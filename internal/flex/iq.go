package flex

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// IQ recording: a DAX IQ stream from the panadapter a slice is on, saved as
// a float32 stereo WAV file (I left, Q right) with a JSON file beside it that
// says what was recorded. A recording is for reference and later analysis;
// it never changes what the radio is doing apart from the DAX IQ channel of
// that panadapter, which is put back when the recording stops.

// IQOptions says what to record and where.
type IQOptions struct {
	Slice  string // slice letter on the bound station, e.g. "A"
	Rate   int    // 24000, 48000, 96000 or 192000
	Path   string // file path without extension; .wav and .json are added
	SwapIQ bool   // take the first float of each pair as Q instead of I
	Note   string // free text kept in the JSON file
}

// IQGap is a run of lost packets, filled with silence in the WAV file so the
// samples after it stay at the right time.
type IQGap struct {
	AtSample       int64     `json:"at_sample"`
	MissingPackets int       `json:"missing_packets"`
	MissingSamples int       `json:"missing_samples"`
	UTC            time.Time `json:"utc"`
}

// IQCenterChange records the panadapter moving while recording. The samples
// after AtSample are centred on CenterHz.
type IQCenterChange struct {
	AtSample int64     `json:"at_sample"`
	CenterHz float64   `json:"center_hz"`
	UTC      time.Time `json:"utc"`
}

// VitaStamp is a packet's VITA-49 timestamp as received, with the PC's
// clock when it arrived.
type VitaStamp struct {
	TSI    uint8     `json:"tsi"` // 1 UTC, 2 GPS, 3 other
	TSF    uint8     `json:"tsf"` // 1 sample count, 2 picoseconds, 3 free running
	Int    uint32    `json:"integer"`
	Frac   uint64    `json:"fractional"`
	Sample int64     `json:"at_sample"`
	PCTime time.Time `json:"pc_time_utc"`
}

// IQMeta is written as the JSON file beside the WAV file.
type IQMeta struct {
	Format        string           `json:"format"`
	Wav           string           `json:"wav"`
	Note          string           `json:"note,omitempty"`
	RadioModel    string           `json:"radio_model"`
	RadioNickname string           `json:"radio_nickname"`
	RadioIP       string           `json:"radio_ip"`
	Station       string           `json:"station"`
	StreamID      string           `json:"stream_id"`
	PanID         string           `json:"pan_id"`
	DAXIQChannel  int              `json:"daxiq_channel"`
	ChannelSet    bool             `json:"channel_set_by_us"`
	RequestedRate int              `json:"requested_rate"`
	SampleRate    int              `json:"sample_rate"`
	CenterHz      float64          `json:"center_hz"`
	PanBandwidth  float64          `json:"pan_bandwidth_hz"`
	IQOrder       string           `json:"iq_order"`
	SwapIQ        bool             `json:"swap_iq"`
	SliceStart    Slice            `json:"slice_start"`
	SliceEnd      Slice            `json:"slice_end"`
	SliceErrStart *float64         `json:"slice_error_start_millihz,omitempty"`
	SliceErrEnd   *float64         `json:"slice_error_end_millihz,omitempty"`
	RefStart      Reference        `json:"reference_start"`
	RefEnd        Reference        `json:"reference_end"`
	StartedUTC    time.Time        `json:"started_utc"`
	StoppedUTC    time.Time        `json:"stopped_utc"`
	FirstPacket   *VitaStamp       `json:"first_packet,omitempty"`
	LastPacket    *VitaStamp       `json:"last_packet,omitempty"`
	Packets       int64            `json:"packets"`
	Samples       int64            `json:"samples"`
	ZeroFilled    int64            `json:"zero_filled_samples"`
	Gaps          []IQGap          `json:"gaps"`
	CenterChanges []IQCenterChange `json:"center_changes"`
	OtherPackets  int64            `json:"other_packets"`
	BadPackets    int64            `json:"bad_packets"`
	Peak          *Peak            `json:"peak,omitempty"`
	PeakHz        float64          `json:"peak_hz,omitempty"`
	Problems      []string         `json:"problems"`
}

// IQStatus is a recording's progress.
type IQStatus struct {
	Packets    int64
	Samples    int64
	Gaps       int
	SampleRate int
	LastPacket time.Time
	Err        error
}

// IQRecording is a recording in progress.
type IQRecording struct {
	r        *Radio
	opt      IQOptions
	udp      *net.UDPConn
	streamID uint32
	panID    uint32
	stopping atomic.Bool
	done     chan struct{}
	stopOnce sync.Once
	result   IQMeta
	resErr   error

	mu        sync.Mutex
	meta      IQMeta
	wav       *IQWav
	spec      *Spectrum
	lastCount int
	perPacket int
	lastAt    time.Time
	err       error
	center    float64
}

// StartIQ starts recording the DAX IQ stream of the panadapter that the bound
// station's slice opt.Slice is on.
func (r *Radio) StartIQ(opt IQOptions) (rec *IQRecording, err error) {
	if opt.Rate == 0 {
		opt.Rate = 24000
	}
	if DAXIQRate(rateClass(opt.Rate)) == 0 {
		return nil, fmt.Errorf("DAX IQ rate must be 24000, 48000, 96000 or 192000, not %d", opt.Rate)
	}
	snap := r.Snapshot()
	if !snap.Connected {
		return nil, fmt.Errorf("not connected to a FlexRadio")
	}
	sl, ok := snap.SliceByLetter(opt.Slice)
	if !ok {
		return nil, fmt.Errorf("slice %s is not open on the bound station %q", opt.Slice, snap.BoundStation)
	}
	if sl.Pan == 0 {
		return nil, fmt.Errorf("slice %s hasn't reported its panadapter", opt.Slice)
	}
	pan, ok := snap.PanByID(sl.Pan)
	if !ok {
		return nil, fmt.Errorf("no status for panadapter 0x%08X (does this firmware accept \"sub pan all\"?)", sl.Pan)
	}

	rec = &IQRecording{r: r, opt: opt, panID: pan.ID, done: make(chan struct{}), lastCount: -1, center: pan.CenterHz}
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()

	// The DAX IQ channel: use the pan's if it has one, else take a free one
	// and put it back afterwards.
	ch, set := pan.DAXIQChannel, false
	if ch == 0 {
		used := map[int]bool{}
		for _, p := range snap.Pans {
			used[p.DAXIQChannel] = true
		}
		for c := 1; c <= 4; c++ {
			if !used[c] {
				ch = c
				break
			}
		}
		if ch == 0 {
			return nil, fmt.Errorf("all four DAX IQ channels are in use by other panadapters")
		}
		if err := r.CommandWait(fmt.Sprintf("display pan set 0x%08X daxiq_channel=%d", pan.ID, ch), 3*time.Second); err != nil {
			return nil, fmt.Errorf("could not give panadapter 0x%08X DAX IQ channel %d: %w", pan.ID, ch, err)
		}
		set = true
		undo = append(undo, func() { rec.restoreChannel() })
	}
	r.mu.Lock()
	for _, st := range r.streams {
		if st.Type == "dax_iq" && st.DAXIQChannel == ch {
			r.log.Warn("DAX IQ channel %d already has stream 0x%08X (client 0x%08X); adding our own", ch, st.ID, st.ClientHandle)
		}
	}
	r.mu.Unlock()

	// A UDP port for the stream. The radio sends to the address of our TCP
	// connection, on the port we tell it.
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("could not open a UDP port for the IQ stream: %w", err)
	}
	_ = udp.SetReadBuffer(4 << 20)
	rec.udp = udp
	undo = append(undo, func() { udp.Close() })
	port := udp.LocalAddr().(*net.UDPAddr).Port
	if err := r.CommandWait(fmt.Sprintf("client udpport %d", port), 3*time.Second); err != nil {
		return nil, fmt.Errorf("could not tell the radio our UDP port: %w", err)
	}

	// The stream itself. Firmware v3 and later take the first form, v2 the second.
	reply, err := r.CommandReply(fmt.Sprintf("stream create type=dax_iq daxiq_channel=%d", ch), 3*time.Second)
	if err != nil {
		r.log.Debug("%v; trying the v2 form", err)
		if reply, err = r.CommandReply(fmt.Sprintf("stream create daxiq=%d", ch), 3*time.Second); err != nil {
			return nil, fmt.Errorf("could not create a DAX IQ stream: %w", err)
		}
	}
	id, perr := parseHex32(reply)
	if perr != nil || id == 0 {
		return nil, fmt.Errorf("unexpected reply to stream create: %q", reply)
	}
	rec.streamID = id
	undo = append(undo, func() { r.Command(fmt.Sprintf("stream remove 0x%08X", id), nil) })
	if err := r.CommandWait(fmt.Sprintf("stream set 0x%08X daxiq_rate=%d", id, opt.Rate), 3*time.Second); err != nil {
		r.log.Debug("%v; trying the v2 form", err)
		if err := r.CommandWait(fmt.Sprintf("dax iq set %d rate=%d", ch, opt.Rate), 3*time.Second); err != nil {
			r.log.Warn("could not set the DAX IQ rate to %d; recording at the radio's rate", opt.Rate)
		}
	}

	now := time.Now().UTC()
	rec.meta = IQMeta{
		Format:        "FMT Assistant IQ recording 1",
		Note:          opt.Note,
		RadioModel:    snap.Model,
		RadioNickname: snap.Nickname,
		RadioIP:       snap.IP,
		Station:       snap.BoundStation,
		StreamID:      fmt.Sprintf("0x%08X", id),
		PanID:         fmt.Sprintf("0x%08X", pan.ID),
		DAXIQChannel:  ch,
		ChannelSet:    set,
		RequestedRate: opt.Rate,
		CenterHz:      pan.CenterHz,
		PanBandwidth:  pan.BandwidthHz,
		IQOrder:       "WAV left = I, right = Q; frequency = center_hz + offset",
		SwapIQ:        opt.SwapIQ,
		SliceStart:    sl,
		RefStart:      snap.Ref,
		StartedUTC:    now,
		Gaps:          []IQGap{},
		CenterChanges: []IQCenterChange{},
		Problems:      []string{},
	}
	if e, err := r.SliceError(sl.Index, 2*time.Second); err == nil {
		rec.meta.SliceErrStart = &e
	}
	r.log.Info("IQ recording: stream 0x%08X, DAX IQ channel %d, pan 0x%08X centred on %.0f Hz, UDP port %d",
		id, ch, pan.ID, pan.CenterHz, port)
	go rec.read()
	return rec, nil
}

// rateClass is the packet class for a DAX IQ rate.
func rateClass(rate int) uint16 {
	switch rate {
	case 24000:
		return 0x02E3
	case 48000:
		return 0x02E4
	case 96000:
		return 0x02E5
	case 192000:
		return 0x02E6
	}
	return 0
}

func (rec *IQRecording) restoreChannel() {
	rec.r.Command(fmt.Sprintf("display pan set 0x%08X daxiq_channel=0", rec.panID), nil)
}

// read receives packets until Stop.
func (rec *IQRecording) read() {
	defer close(rec.done)
	buf := make([]byte, 65536)
	var scratch []complex64
	for !rec.stopping.Load() {
		rec.udp.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := rec.udp.ReadFromUDP(buf)
		rec.watchCenter()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if !rec.stopping.Load() {
				rec.fail(fmt.Errorf("UDP receive failed: %w", err))
			}
			return
		}
		p, err := ParseVita(buf[:n])
		if err != nil {
			rec.mu.Lock()
			rec.meta.BadPackets++
			rec.mu.Unlock()
			continue
		}
		if p.StreamID != rec.streamID {
			rec.mu.Lock()
			rec.meta.OtherPackets++
			rec.mu.Unlock()
			continue
		}
		scratch = IQSamples(p.Payload, rec.opt.SwapIQ, scratch)
		if err := rec.add(p, scratch); err != nil {
			rec.fail(err)
			return
		}
	}
}

func (rec *IQRecording) add(p VitaPacket, s []complex64) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	now := time.Now().UTC()
	rate := DAXIQRate(p.PacketClass)
	if rate == 0 {
		rec.meta.OtherPackets++
		return nil
	}
	if rec.wav == nil {
		w, err := CreateIQWav(rec.opt.Path+".wav", rate)
		if err != nil {
			return err
		}
		rec.wav, rec.spec = w, NewSpectrum(rate)
		rec.meta.SampleRate = rate
		if rate != rec.opt.Rate {
			rec.problem("the radio sends %d samples/s, not the %d asked for", rate, rec.opt.Rate)
		}
	} else if rate != rec.meta.SampleRate {
		rec.problem("a packet at %d samples/s in a %d samples/s recording was dropped", rate, rec.meta.SampleRate)
		return nil
	}
	if rec.lastCount >= 0 {
		want := uint8(rec.lastCount+1) & 0xF
		if p.Count != want {
			missing := int((p.Count - want) & 0xF)
			zeros := missing * rec.perPacket
			rec.meta.Gaps = append(rec.meta.Gaps, IQGap{AtSample: rec.wav.Frames(), MissingPackets: missing, MissingSamples: zeros, UTC: now})
			rec.meta.ZeroFilled += int64(zeros)
			if err := rec.wav.WriteZeros(zeros); err != nil {
				return err
			}
		}
	}
	stamp := &VitaStamp{TSI: p.TSI, TSF: p.TSF, Int: p.IntTS, Frac: p.FracTS, Sample: rec.wav.Frames(), PCTime: now}
	if rec.meta.FirstPacket == nil {
		rec.meta.FirstPacket = stamp
	}
	rec.meta.LastPacket = stamp
	if err := rec.wav.Write(s); err != nil {
		return err
	}
	rec.spec.Add(s)
	rec.meta.Packets++
	rec.lastCount, rec.perPacket, rec.lastAt = int(p.Count), len(s), now
	return nil
}

// watchCenter notes the panadapter moving, which moves the IQ's centre.
func (rec *IQRecording) watchCenter() {
	rec.r.mu.Lock()
	var c float64
	known := false
	if p := rec.r.pans[rec.panID]; p != nil {
		c, known = p.CenterHz, true
	}
	rec.r.mu.Unlock()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !known || math.Abs(c-rec.center) < 0.5 {
		return
	}
	at := int64(0)
	if rec.wav != nil {
		at = rec.wav.Frames()
	}
	rec.r.log.Warn("IQ recording: the panadapter moved from %.0f to %.0f Hz at sample %d", rec.center, c, at)
	rec.meta.CenterChanges = append(rec.meta.CenterChanges, IQCenterChange{AtSample: at, CenterHz: c, UTC: time.Now().UTC()})
	rec.center = c
}

func (rec *IQRecording) problem(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	for _, p := range rec.meta.Problems {
		if p == msg {
			return
		}
	}
	rec.meta.Problems = append(rec.meta.Problems, msg)
	rec.r.log.Warn("IQ recording: %s", msg)
}

func (rec *IQRecording) fail(err error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.err == nil {
		rec.err = err
		rec.meta.Problems = append(rec.meta.Problems, err.Error())
		rec.r.log.Error("IQ recording stopped: %v", err)
	}
}

// Status reports progress.
func (rec *IQRecording) Status() IQStatus {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st := IQStatus{Packets: rec.meta.Packets, Gaps: len(rec.meta.Gaps), SampleRate: rec.meta.SampleRate, LastPacket: rec.lastAt, Err: rec.err}
	if rec.wav != nil {
		st.Samples = rec.wav.Frames()
	}
	return st
}

// Stop ends the recording, removes the stream, puts the panadapter's DAX IQ
// channel back, and writes the JSON file. It is safe to call more than once.
func (rec *IQRecording) Stop() (IQMeta, error) {
	rec.stopOnce.Do(func() { rec.result, rec.resErr = rec.stop() })
	return rec.result, rec.resErr
}

func (rec *IQRecording) stop() (IQMeta, error) {
	r := rec.r
	if err := r.CommandWait(fmt.Sprintf("stream remove 0x%08X", rec.streamID), 3*time.Second); err != nil {
		r.log.Warn("IQ recording: %v", err)
	}
	rec.stopping.Store(true)
	<-rec.done
	rec.udp.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	m := &rec.meta
	if m.ChannelSet {
		rec.restoreChannel()
	}
	m.StoppedUTC = time.Now().UTC()
	snap := r.Snapshot()
	m.RefEnd = snap.Ref
	if sl, ok := snap.SliceByLetter(rec.opt.Slice); ok {
		m.SliceEnd = sl
		if e, err := r.SliceError(sl.Index, 2*time.Second); err == nil {
			m.SliceErrEnd = &e
		}
	}
	var werr error
	if rec.wav != nil {
		m.Samples = rec.wav.Frames()
		m.Wav = filepath.Base(rec.opt.Path) + ".wav"
		werr = rec.wav.Close()
		if pk, ok := rec.spec.Peak(); ok {
			m.Peak = &pk
			m.PeakHz = m.CenterHz + pk.OffsetHz
		}
	} else {
		m.Problems = append(m.Problems, "no IQ packets arrived (is the UDP port blocked by a firewall?)")
	}
	if len(m.Gaps) > 0 {
		m.Problems = append(m.Problems, fmt.Sprintf("%d gaps, %d samples filled with silence", len(m.Gaps), m.ZeroFilled))
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = os.WriteFile(rec.opt.Path+".json", b, 0o644)
	}
	if err == nil {
		err = werr
	}
	if err == nil {
		err = rec.err
	}
	r.log.Info("IQ recording stopped: %d packets, %d samples at %d/s, %d gaps", m.Packets, m.Samples, m.SampleRate, len(m.Gaps))
	return *m, err
}
