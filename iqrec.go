package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/va3mw/FMTAssistant/internal/flex"
	"github.com/va3mw/FMTAssistant/internal/fmtdata"
	"github.com/va3mw/FMTAssistant/internal/iq"
)

// DAX IQ recording: while a step is followed live (the same span the UI
// watches fmt.all for), the controlled slice's panadapter is recorded as IQ
// to the session's IQ folder, with a JSON file describing it. This is for
// analysis after the test only; it never changes the answer.
//
// Locking: iqState.mu serialises starting and stopping, which wait on the
// radio, so it is never taken from the radio's callbacks. Those read the
// current recording through cur, an atomic pointer. iqState.mu is taken
// without a.mu or a.liveMu held.

type iqState struct {
	mu     sync.Mutex
	rx     *flex.Receiver
	live   atomic.Pointer[string] // step followed live now ("" or nil: none)
	manual bool                   // a recording started with Record IQ now
	cur    atomic.Pointer[iqRec]

	infoMu  sync.Mutex
	lastErr string
	last    *IQLast
}

type iqRec struct {
	step     string
	rec      *iq.Recorder
	stream   uint32
	pan      string
	rate     int
	setDAX   bool // we gave the panadapter its DAX IQ channel; undone at the end
	centerHz atomic.Int64
	started  time.Time
	moving   atomic.Bool // a re-centre is in progress
}

// IQLast summarises the last finished recording.
type IQLast struct {
	Step    string  `json:"step"`
	File    string  `json:"file"`
	Seconds float64 `json:"seconds"`
	Bytes   int64   `json:"bytes"`
	Lost    int64   `json:"lost"`
	Rate    int     `json:"rate"`
	Err     string  `json:"err"`
}

// IQView is the recording state for the UI.
type IQView struct {
	Recording bool            `json:"recording"`
	Manual    bool            `json:"manual"`
	Step      string          `json:"step"`
	CenterHz  int64           `json:"center_hz"`
	Status    iq.Status       `json:"status"`
	Last      *IQLast         `json:"last"`
	Err       string          `json:"err"`
	Issues    []fmtdata.Issue `json:"issues"`
}

func (a *App) iqView() IQView {
	v := IQView{}
	a.iq.infoMu.Lock()
	v.Err, v.Last = a.iq.lastErr, a.iq.last
	a.iq.infoMu.Unlock()
	if c := a.iq.cur.Load(); c != nil {
		v.Recording, v.Step, v.CenterHz = true, c.step, c.centerHz.Load()
		v.Manual = c.step == "manual"
		v.Status = c.rec.Status()
		if v.Status.Packets == 0 && time.Since(c.started) > 5*time.Second {
			v.Issues = append(v.Issues, fmtdata.Issue{Level: "warn", Msg: "Recording IQ, but no IQ packets have arrived from the radio. " +
				"Allow FMT Assistant through Windows Firewall (private networks), and check that no other program holds DAX IQ channel " +
				strconv.Itoa(a.sess.Flex.IQChannel) + "."})
		}
		// Packets stopped mid-recording: the radio dropped the stream or the
		// connection.
		if lp := v.Status.LastPacket; !lp.IsZero() && time.Since(lp) > 5*time.Second {
			v.Issues = append(v.Issues, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf(
				"No IQ packets for %.0f s: the radio has stopped sending the IQ stream. Check the radio and its network connection.",
				time.Since(lp).Seconds())})
		}
		if v.Status.Err != "" {
			v.Issues = append(v.Issues, fmtdata.Issue{Level: "error", Msg: "IQ recording problem: " + v.Status.Err})
		}
	} else if v.Err != "" && a.sess.Flex.RecordIQ {
		v.Issues = append(v.Issues, fmtdata.Issue{Level: "warn", Msg: "IQ recording: " + v.Err})
	}
	return v
}

// iqFollow is told the step followed live (from LiveFollow). The step is
// stored at once, so however the syncs that follow are ordered, the last
// one sees the latest step.
func (a *App) iqFollow(step string) {
	a.iq.live.Store(&step)
	go a.iqSync()
}

// iqKick re-evaluates after a setting changed.
func (a *App) iqKick() { go a.iqSync() }

// iqSync starts or stops recording to match the live step and settings.
func (a *App) iqSync() {
	a.iq.mu.Lock()
	defer a.iq.mu.Unlock()
	a.mu.Lock()
	on := a.sess.Flex.Enabled && a.sess.Flex.RecordIQ
	flexOn := a.sess.Flex.Enabled
	a.mu.Unlock()
	want := ""
	switch {
	case a.iq.manual && flexOn:
		want = "manual"
	case on:
		if p := a.iq.live.Load(); p != nil {
			want = *p
		}
	}
	cur := a.iq.cur.Load()
	if cur != nil && cur.step == want {
		return
	}
	if cur != nil {
		a.iqStopLocked()
	}
	if want == "" {
		return
	}
	err := a.iqStartLocked(want)
	a.iq.infoMu.Lock()
	a.iq.lastErr = ""
	if err != nil {
		a.iq.lastErr = err.Error()
	}
	a.iq.infoMu.Unlock()
	if err != nil {
		a.src.Warn("IQ recording for %s not started: %v", want, err)
		if want == "manual" {
			a.iq.manual = false
		}
	}
	a.changed()
}

func (a *App) iqStartLocked(step string) error {
	sl, err := a.flexSlice()
	if err != nil {
		return err
	}
	pan, ok := a.radio.Pan(sl.Pan)
	if !ok {
		return fmt.Errorf("the panadapter of slice %s is not known yet", sl.Letter)
	}
	a.mu.Lock()
	f := a.sess.Flex
	dir := filepath.Join(a.sess.Dir(), "IQ")
	call := a.sess.Callsign
	ref := a.currentRefLocked()
	var carrier float64
	title := step
	if r := a.sess.Run(step); r != nil {
		carrier, title = r.CarrierHz, r.Title()
	} else if a.sess.Cal(step) != nil {
		title = map[string]string{"cal_before": "Calibration before", "cal_after": "Calibration after", "practice_cal": "Practice calibration"}[step]
	}
	a.mu.Unlock()
	ch, rate := f.IQChannel, f.IQRate
	if ch < 1 || ch > 4 {
		ch = 1
	}
	if !validIQRate(rate) {
		rate = 24000
	}

	port, err := a.iq.rx.Port()
	if err != nil {
		return err
	}
	if err := a.radio.SetUDPPort(port); err != nil {
		return err
	}

	// The panadapter's DAX IQ channel: use the one it has, or give it ours.
	setDAX := false
	var notes []string
	if pan.DAXIQChannel != 0 {
		if pan.DAXIQChannel != ch {
			notes = append(notes, fmt.Sprintf("The panadapter already had DAX IQ channel %d, so that was used instead of %d.", pan.DAXIQChannel, ch))
		}
		ch = pan.DAXIQChannel
	} else {
		if err := a.radio.SetPanDAXIQ(pan.ID, ch); err != nil {
			return err
		}
		setDAX = true
	}

	// The slice must be inside the IQ bandwidth; if not, centre the pan on it.
	if !iqInBand(sl.FreqHz, pan.CenterHz, rate) {
		a.src.Info("IQ: centring panadapter %s on %s (was %s)", pan.ID, khzStr(float64(sl.FreqHz)), khzStr(float64(pan.CenterHz)))
		if err := a.radio.CenterPan(pan.ID, sl.FreqHz); err != nil {
			a.undoDAX(pan.ID, setDAX)
			return err
		}
		pan.CenterHz = sl.FreqHz
		notes = append(notes, "The panadapter was re-centred on the dial so the carrier is inside the IQ bandwidth.")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.undoDAX(pan.ID, setDAX)
		return err
	}
	now := time.Now().UTC()
	name := fmt.Sprintf("%s %s %s kHz.wav", step, now.Format("20060102-150405Z"), strconv.FormatFloat(float64(sl.FreqHz)/1000, 'f', 3, 64))
	snap := a.radio.Snapshot()
	rec, err := iq.NewRecorder(filepath.Join(dir, name), iq.Meta{
		Step: step, Title: title, Callsign: call, Radio: strings.TrimSpace(snap.Model + " " + snap.Nickname),
		Slice: sl.Letter, DAXIQ: ch, CenterHz: pan.CenterHz, DialHz: sl.FreqHz, CarrierHz: carrier,
		RequestedRate: rate, Reference: ref.Detail, Notes: notes,
	})
	if err != nil {
		a.undoDAX(pan.ID, setDAX)
		return err
	}
	c := &iqRec{step: step, rec: rec, pan: pan.ID, rate: rate, setDAX: setDAX, started: now}
	c.centerHz.Store(pan.CenterHz)

	id, rateErr, err := a.radio.StartDAXIQ(ch, rate)
	if err != nil {
		rec.Update(func(m *iq.Meta) { m.Error = err.Error() })
		rec.Close()
		os.Remove(rec.Path())
		os.Remove(rec.MetaPath())
		a.undoDAX(pan.ID, setDAX)
		return err
	}
	c.stream = id
	rec.Update(func(m *iq.Meta) { m.StreamID = fmt.Sprintf("0x%08X", id) })
	if rateErr != nil {
		a.src.Warn("IQ: %v; recording at the radio's rate", rateErr)
		rec.Update(func(m *iq.Meta) { m.Notes = append(m.Notes, "The radio refused the requested rate: "+rateErr.Error()) })
	}
	a.iq.rx.Subscribe(id, rec.Packet)
	a.iq.cur.Store(c)
	a.src.Info("IQ recording %s: stream 0x%08X, DAX IQ %d, %d Hz, pan centre %s, dial %s, to %s",
		step, id, ch, rate, khzStr(float64(pan.CenterHz)), khzStr(float64(sl.FreqHz)), rec.Path())
	return nil
}

func (a *App) iqStopLocked() {
	c := a.iq.cur.Swap(nil)
	if c == nil {
		return
	}
	if err := a.radio.StopDAXIQ(c.stream); err != nil {
		a.src.Warn("IQ: removing stream: %v", err)
	}
	a.iq.rx.Subscribe(c.stream, nil)
	a.undoDAX(c.pan, c.setDAX)
	a.mu.Lock()
	ref := a.currentRefLocked()
	a.mu.Unlock()
	c.rec.Update(func(m *iq.Meta) {
		if ref.Detail != m.Reference {
			m.ReferenceLater = ref.Detail
		}
	})
	st := c.rec.Status()
	m, err := c.rec.Close()
	last := &IQLast{Step: c.step, File: c.rec.Path(), Seconds: float64(m.Frames) / math.Max(1, float64(m.SampleRate)),
		Bytes: st.Bytes, Lost: m.Lost, Rate: m.SampleRate, Err: m.Error}
	if err != nil {
		a.src.Error("IQ: %v", err)
		last.Err = err.Error()
	}
	a.iq.infoMu.Lock()
	a.iq.last = last
	a.iq.infoMu.Unlock()
	a.src.Info("IQ recording %s stopped: %.1f s at %d Hz (%s), %d packets, %d lost, %s",
		c.step, last.Seconds, m.SampleRate, m.RateSource, m.Packets, m.Lost, c.rec.Path())
}

func (a *App) undoDAX(pan string, set bool) {
	if set {
		if err := a.radio.SetPanDAXIQ(pan, 0); err != nil {
			a.src.Debug("IQ: clearing the panadapter's DAX IQ channel: %v", err)
		}
	}
}

func (a *App) iqShutdown() {
	a.iq.mu.Lock()
	a.iq.manual = false
	a.iq.live.Store(nil)
	a.iqStopLocked()
	a.iq.mu.Unlock()
	a.iq.rx.Close()
}

// onIQPan notes a change of the recorded panadapter's centre. It runs on the
// radio's reader, so it must not wait.
func (a *App) onIQPan(p flex.Pan) {
	c := a.iq.cur.Load()
	if c == nil || p.ID != c.pan || p.CenterHz == 0 {
		return
	}
	if old := c.centerHz.Swap(p.CenterHz); old != p.CenterHz {
		c.rec.Event(iq.Event{What: "center", Hz: p.CenterHz})
		a.src.Info("IQ: panadapter centre now %s", khzStr(float64(p.CenterHz)))
	}
}

// onIQStream takes the rate from the recorded stream's status.
func (a *App) onIQStream(id uint32, kv map[string]string) {
	c := a.iq.cur.Load()
	if c == nil || id != c.stream {
		return
	}
	if v, ok := kv["daxiq_rate"]; ok {
		if r, err := strconv.Atoi(v); err == nil {
			c.rec.SetRate(r)
		}
	}
	if _, ok := kv["removed"]; ok {
		c.rec.Event(iq.Event{What: "note", Text: "the radio removed the stream"})
		a.src.Warn("IQ: the radio removed stream 0x%08X", id)
	}
}

// onIQDial notes a dial change of the controlled slice (called with a.mu
// held) and re-centres the panadapter if the slice left the IQ bandwidth.
func (a *App) onIQDial(hz int64) {
	c := a.iq.cur.Load()
	if c == nil {
		return
	}
	c.rec.Event(iq.Event{What: "dial", Hz: hz})
	if iqInBand(hz, c.centerHz.Load(), c.rate) || !c.moving.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.moving.Store(false)
		a.src.Info("IQ: dial %s is outside the recorded band; centring the panadapter on it", khzStr(float64(hz)))
		if err := a.radio.CenterPan(c.pan, hz); err != nil {
			a.src.Warn("IQ: re-centring: %v", err)
			c.rec.Event(iq.Event{What: "note", Text: "re-centring failed: " + err.Error()})
		}
	}()
}

// iqInBand says whether a DIGU dial's audio passband (up to 3 kHz above
// the dial) is well inside an IQ stream of this rate centred on center.
func iqInBand(dialHz, centerHz int64, rate int) bool {
	half := float64(rate) * 0.45
	lo, hi := float64(dialHz-centerHz), float64(dialHz+3000-centerHz)
	return lo > -half && hi < half
}

func validIQRate(r int) bool {
	return r == 24000 || r == 48000 || r == 96000 || r == 192000
}

// ---------------------------------------------------------------------------
// Bound to the UI

// SetIQRecord turns IQ recording during each live step on or off.
func (a *App) SetIQRecord(on bool) {
	a.mu.Lock()
	a.sess.Flex.RecordIQ = on
	a.saveLocked()
	a.mu.Unlock()
	a.iqKick()
}

// SetIQOptions sets the DAX IQ channel and rate for the next recording.
func (a *App) SetIQOptions(channel, rate int) error {
	if channel < 1 || channel > 4 {
		return fmt.Errorf("DAX IQ channel must be 1 to 4")
	}
	if !validIQRate(rate) {
		return fmt.Errorf("IQ rate must be 24, 48, 96 or 192 kHz")
	}
	a.mu.Lock()
	a.sess.Flex.IQChannel, a.sess.Flex.IQRate = channel, rate
	a.saveLocked()
	a.mu.Unlock()
	return nil
}

// StartIQ records now, until StopIQ, whatever the schedule.
func (a *App) StartIQ() error {
	a.iq.mu.Lock()
	a.iq.manual = true
	a.iq.mu.Unlock()
	a.iqSync()
	a.iq.infoMu.Lock()
	defer a.iq.infoMu.Unlock()
	if a.iq.lastErr != "" && a.iq.cur.Load() == nil {
		return fmt.Errorf("%s", a.iq.lastErr)
	}
	return nil
}

// StopIQ ends a recording started with StartIQ. Recording of the live step
// resumes if it is turned on.
func (a *App) StopIQ() {
	a.iq.mu.Lock()
	a.iq.manual = false
	a.iq.mu.Unlock()
	a.iqSync()
}

// OpenIQFolder opens the session's IQ folder in Explorer.
func (a *App) OpenIQFolder() {
	a.mu.Lock()
	d := filepath.Join(a.sess.Dir(), "IQ")
	a.mu.Unlock()
	_ = os.MkdirAll(d, 0o755)
	_ = exec.Command("explorer", d).Start()
}
