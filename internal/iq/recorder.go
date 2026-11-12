package iq

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// Meta describes a recording. It is written next to the WAV file as JSON so
// the IQ can be analysed later without the app.
type Meta struct {
	Format string `json:"format"` // what the WAV holds
	WAV    string `json:"wav"`    // file name of the samples

	Step     string `json:"step"`     // cal_before, run1..run4, cal_after, practice, manual
	Title    string `json:"title"`    // e.g. "W8RKO 40 m"
	Callsign string `json:"callsign"` // the operator
	Radio    string `json:"radio"`    // model and nickname
	Slice    string `json:"slice"`    // slice letter
	DAXIQ    int    `json:"daxiq_channel"`
	StreamID string `json:"stream_id"`

	// The IQ is centred on the panadapter, not on the slice: a signal at
	// RF frequency f appears at f - CenterHz in the recording.
	CenterHz       int64   `json:"center_hz"`
	DialHz         int64   `json:"dial_hz"`                     // slice (DIGU dial) at the start
	CarrierHz      float64 `json:"carrier_hz"`                  // expected carrier, if known (0 otherwise)
	CarrierOffset  float64 `json:"carrier_offset_hz,omitempty"` // CarrierHz - CenterHz
	RequestedRate  int     `json:"requested_rate"`
	SampleRate     int     `json:"sample_rate"`   // rate written in the WAV header
	RateSource     string  `json:"rate_source"`   // radio, measured or requested
	MeasuredRate   float64 `json:"measured_rate"` // samples / host elapsed time
	ByteOrder      string  `json:"byte_order"`    // of the radio's floats, as detected
	ChannelOrder   string  `json:"channel_order"` // left/right of the WAV
	Reference      string  `json:"reference"`     // frequency reference at the start
	ReferenceLater string  `json:"reference_later,omitempty"`

	StartUTC   time.Time `json:"start_utc"` // host clock, first packet
	StopUTC    time.Time `json:"stop_utc"`  // host clock, last packet
	FirstVITA  *VITATime `json:"first_vita_timestamp,omitempty"`
	Frames     int64     `json:"frames"` // IQ pairs
	Packets    int64     `json:"packets"`
	Lost       int64     `json:"lost_packets"` // from the 4-bit VITA packet counter
	LostFrames int64     `json:"lost_frames_estimate"`
	Gaps       []Gap     `json:"gaps,omitempty"`
	Events     []Event   `json:"events,omitempty"`
	Notes      []string  `json:"notes,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// VITATime is the radio's timestamp of the first packet.
type VITATime struct {
	TSI     int    `json:"tsi"` // 1: UTC, 2: GPS, 3: other
	TSF     int    `json:"tsf"` // 1: sample count, 2: picoseconds, 3: free running
	Integer uint32 `json:"integer"`
	Frac    uint64 `json:"fractional"`
	UTC     string `json:"utc,omitempty"` // Integer as a UTC time, when TSI is UTC
}

// Gap is where packets went missing: before Frame, about Packets packets.
type Gap struct {
	Frame   int64 `json:"frame"`
	Packets int   `json:"packets"`
}

// Event is something that changed during the recording, at a frame index.
type Event struct {
	Frame int64     `json:"frame"`
	At    time.Time `json:"at"`
	What  string    `json:"what"`           // center, dial, reference, note
	Hz    int64     `json:"hz,omitempty"`   // for center and dial
	Text  string    `json:"text,omitempty"` // for reference and note
}

// Recorder writes packets of one stream to a WAV file and keeps the metadata.
type Recorder struct {
	mu      sync.Mutex
	path    string // WAV path; the sidecar is the same with .json
	wav     *WAVWriter
	meta    Meta
	order   Order
	lastCnt uint8
	gotPkt  bool
	perPkt  int64 // frames in a full packet, for the loss estimate
	scratch []float32
	err     error
	closed  bool
	t0      time.Time
}

// NewRecorder creates the WAV at path. meta is the description so far; the
// recorder fills in the counts, times and rate.
func NewRecorder(path string, meta Meta) (*Recorder, error) {
	now := time.Now().UTC()
	w, err := CreateWAV(path, now)
	if err != nil {
		return nil, err
	}
	meta.Format = "WAV, 32-bit float, 2 channels: left I, right Q, as sent by the radio"
	meta.ChannelOrder = "left=I right=Q"
	meta.WAV = baseName(path)
	return &Recorder{path: path, wav: w, meta: meta, t0: now}, nil
}

// Path is the WAV file.
func (r *Recorder) Path() string { return r.path }

// MetaPath is the JSON sidecar.
func (r *Recorder) MetaPath() string { return strings.TrimSuffix(r.path, ".wav") + ".json" }

// Packet takes one VITA packet of this stream.
func (r *Recorder) Packet(p Packet, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil {
		return
	}
	if r.order == OrderUnknown {
		r.order = DetectOrder(p.Payload)
		if r.order == OrderUnknown {
			// All zeros (or undecidable): keep the time base, assume network order.
			r.scratch = Decode(r.scratch, p.Payload, OrderBig)
			for i := range r.scratch {
				r.scratch[i] = 0
			}
			r.write(p, at)
			return
		}
		r.meta.ByteOrder = r.order.String()
	}
	r.scratch = Decode(r.scratch, p.Payload, r.order)
	r.write(p, at)
}

func (r *Recorder) write(p Packet, at time.Time) {
	frames := int64(len(r.scratch) / 2)
	if !r.gotPkt {
		r.gotPkt = true
		r.meta.StartUTC = at
		if p.TSI != 0 {
			vt := &VITATime{TSI: int(p.TSI), TSF: int(p.TSF), Integer: p.IntTS, Frac: p.FracTS}
			if p.TSI == 1 {
				vt.UTC = time.Unix(int64(p.IntTS), 0).UTC().Format(time.RFC3339)
			}
			r.meta.FirstVITA = vt
		}
	} else if gap := CountGap(r.lastCnt, p.Count); gap > 0 {
		r.meta.Lost += int64(gap)
		est := int64(gap) * r.perPkt
		r.meta.LostFrames += est
		if len(r.meta.Gaps) < 1000 {
			r.meta.Gaps = append(r.meta.Gaps, Gap{Frame: r.wav.Frames(), Packets: gap})
		}
	}
	if frames > r.perPkt {
		r.perPkt = frames
	}
	r.lastCnt = p.Count
	r.meta.Packets++
	r.meta.StopUTC = at
	if err := r.wav.Write(r.scratch); err != nil {
		r.err = err
		r.meta.Error = err.Error()
	}
}

// Event notes a change at the current position of the recording.
func (r *Recorder) Event(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	e.Frame = r.wav.Frames()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if len(r.meta.Events) < 10000 {
		r.meta.Events = append(r.meta.Events, e)
	}
}

// SetRate records the rate the radio reports for the stream.
func (r *Recorder) SetRate(rate int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rate > 0 {
		r.meta.SampleRate, r.meta.RateSource = rate, "radio"
	}
}

// Update changes the metadata under the lock (e.g. a note).
func (r *Recorder) Update(f func(m *Meta)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.meta)
}

// Status is a running summary for the UI.
type Status struct {
	File    string  `json:"file"`
	Frames  int64   `json:"frames"`
	Seconds float64 `json:"seconds"`
	Bytes   int64   `json:"bytes"`
	Packets int64   `json:"packets"`
	Lost    int64   `json:"lost"`
	Rate    int     `json:"rate"`
	Err     string  `json:"err"`
}

func (r *Recorder) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	rate := r.rateLocked()
	s := Status{File: r.path, Frames: r.wav.Frames(), Bytes: r.wav.Bytes(), Packets: r.meta.Packets,
		Lost: r.meta.Lost, Rate: rate, Err: r.meta.Error}
	if rate > 0 {
		s.Seconds = float64(s.Frames) / float64(rate)
	}
	return s
}

// rateLocked is the best known sample rate: the radio's, else the measured
// rate snapped to a DAX IQ rate, else the requested one.
func (r *Recorder) rateLocked() int {
	if r.meta.RateSource == "radio" && r.meta.SampleRate > 0 {
		return r.meta.SampleRate
	}
	if el := r.meta.StopUTC.Sub(r.meta.StartUTC).Seconds(); el > 2 {
		m := float64(r.wav.Frames()+r.meta.LostFrames) / el
		return SnapRate(m)
	}
	return r.meta.RequestedRate
}

// SnapRate returns the DAX IQ rate nearest a measured one.
func SnapRate(m float64) int {
	best := 0
	for _, v := range []int{24000, 48000, 96000, 192000} {
		if best == 0 || math.Abs(float64(v)-m) < math.Abs(float64(best)-m) {
			best = v
		}
	}
	return best
}

// Close finishes the WAV and writes the sidecar. It is safe to call twice.
func (r *Recorder) Close() (Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.meta, nil
	}
	r.closed = true
	m := &r.meta
	m.Frames = r.wav.Frames()
	if el := m.StopUTC.Sub(m.StartUTC).Seconds(); el > 0 {
		m.MeasuredRate = math.Round(float64(m.Frames+m.LostFrames)/el*10) / 10
	}
	switch {
	case m.RateSource == "radio" && m.SampleRate > 0:
	case m.MeasuredRate > 0 && m.StopUTC.Sub(m.StartUTC) > 2*time.Second:
		m.SampleRate, m.RateSource = SnapRate(m.MeasuredRate), "measured"
	default:
		m.SampleRate, m.RateSource = m.RequestedRate, "requested"
	}
	if m.ByteOrder == "" {
		m.ByteOrder = "unknown (no non-zero samples)"
	}
	if m.CarrierHz > 0 {
		m.CarrierOffset = m.CarrierHz - float64(m.CenterHz)
	}
	if m.Packets == 0 {
		m.Notes = append(m.Notes, "No IQ packets arrived. Check that the firewall lets FMT Assistant receive UDP from the radio.")
	}
	stop := m.StopUTC
	if stop.IsZero() {
		stop = time.Now().UTC()
	}
	err := r.wav.Close(m.SampleRate, m.CenterHz, stop)
	b, _ := json.MarshalIndent(m, "", "  ")
	if e := os.WriteFile(r.MetaPath(), b, 0o644); err == nil {
		err = e
	}
	if err != nil {
		err = fmt.Errorf("finishing %s: %w", r.path, err)
	}
	return *m, err
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
