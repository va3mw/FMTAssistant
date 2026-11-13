package iq

import (
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"sort"
	"time"
)

// Measuring a carrier from IQ, independently of WSJT-X:
//
//  1. The recording is mixed down by the expected carrier (relative to the
//     panadapter centre in effect at each sample, so re-centring is
//     followed) and decimated to about 4 kHz.
//  2. It is cut into segments of a few seconds. In each, the strongest
//     peak within ±SearchHz is found with a windowed FFT (parabolic
//     interpolation), then refined by fitting a straight line to the phase
//     of the signal mixed down by that peak.
//  3. Segments that are weak, hold lost packets or a re-centre are left
//     out; the rest are averaged.
//
// Both the recording and its mirror image are measured; unless told
// otherwise, the one with the clearer carrier is used, so swapped I and Q
// are noticed and corrected.

// Options steer an analysis. Zero values take the defaults.
type Options struct {
	ExpectHz float64   // RF frequency to look near (default: dial + 1500)
	SearchHz float64   // ± search around ExpectHz (default 150)
	SegSec   float64   // segment length in seconds (default 4)
	MinSNR   float64   // dB, segments below are left out (default 15)
	Mirror   int       // 0 detect, 1 as recorded, -1 mirrored (I and Q swapped)
	From, To time.Time // average only segments inside this window, if any are
	// Only reads the window (plus a margin), e.g. one run's key-down from a
	// recording that spans the whole night. Needs From, To and a start time.
	OnlyWindow bool
}

// Segment is one measurement.
type Segment struct {
	T       time.Time `json:"t"`
	Sec     float64   `json:"sec"`      // from the start of the file
	RFHz    float64   `json:"rf_hz"`    // carrier frequency, as the radio sees it (uncorrected)
	AudioHz float64   `json:"audio_hz"` // RF minus the dial in effect: comparable to WSJT-X fMeas
	SNR     float64   `json:"snr"`      // peak over median noise in the search band, dB (not WSJT-X's scale)
	SigmaHz float64   `json:"sigma_hz"` // standard error of the phase fit
	OK      bool      `json:"ok"`
	Why     string    `json:"why,omitempty"` // why it was left out
	In      bool      `json:"in"`            // inside the averaging window
}

// SpecPoint is one point of the average spectrum around the expected carrier.
type SpecPoint struct {
	Hz float64 `json:"hz"` // RF offset from ExpectHz
	DB float64 `json:"db"`
}

// Summary is the averaged carrier.
type Summary struct {
	N          int       `json:"n"`
	MeanRFHz   float64   `json:"mean_rf_hz"`
	MeanAudio  float64   `json:"mean_audio_hz"`
	SD         float64   `json:"sd"`
	SEM        float64   `json:"sem"`
	DriftHzMin float64   `json:"drift_hz_min"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Basis      string    `json:"basis"`
}

// Analysis is the result for one recording.
type Analysis struct {
	Rate     int         `json:"rate"`
	Frames   int64       `json:"frames"`
	Seconds  float64     `json:"seconds"`
	Start    time.Time   `json:"start"`
	CenterHz int64       `json:"center_hz"`
	DialHz   int64       `json:"dial_hz"`
	ExpectHz float64     `json:"expect_hz"`
	SearchHz float64     `json:"search_hz"`
	SegSec   float64     `json:"seg_sec"`
	MinSNR   float64     `json:"min_snr"`
	Mirrored bool        `json:"mirrored"`
	Detected bool        `json:"mirror_detected"` // Mirrored was decided from the data
	Segments []Segment   `json:"segments"`
	Spectrum []SpecPoint `json:"spectrum"`
	Summary  *Summary    `json:"summary"`
	Notes    []string    `json:"notes"`
}

const decimRate = 4000.0 // Hz, after the first decimation
const fineRate = 25.0    // Hz, for the phase fit

// Analyze measures the carrier in a recording. meta may be nil for a file
// from another program; then the centre comes from the WAV and opt.ExpectHz
// must be given.
func Analyze(path string, meta *Meta, opt Options) (*Analysis, error) {
	w, err := OpenWAV(path)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	a := &Analysis{Rate: w.Rate, Frames: w.Frames, Seconds: float64(w.Frames) / float64(w.Rate)}
	if meta != nil {
		if meta.SampleRate > 0 && meta.SampleRate != w.Rate {
			a.Notes = append(a.Notes, fmt.Sprintf("The WAV says %d Hz but the description says %d Hz; using the WAV.", w.Rate, meta.SampleRate))
		}
		a.CenterHz, a.DialHz, a.Start = meta.CenterHz, meta.DialHz, meta.StartUTC
	}
	if a.CenterHz == 0 {
		a.CenterHz = w.CenterHz
	}
	if a.Start.IsZero() {
		a.Start = w.Start
	}
	if a.CenterHz == 0 {
		return nil, fmt.Errorf("the recording's centre frequency is unknown")
	}
	if opt.ExpectHz == 0 {
		switch {
		case a.DialHz > 0:
			opt.ExpectHz = float64(a.DialHz) + 1500
		default:
			return nil, fmt.Errorf("give the frequency to look for")
		}
	}
	if opt.SearchHz <= 0 {
		opt.SearchHz = 150
	}
	if opt.SegSec <= 0 {
		opt.SegSec = 4
	}
	if opt.MinSNR == 0 {
		opt.MinSNR = 15
	}
	a.ExpectHz, a.SearchHz, a.SegSec, a.MinSNR = opt.ExpectHz, opt.SearchHz, opt.SegSec, opt.MinSNR

	tl := newTimeline(meta, a.CenterHz, a.DialHz)
	var frame, end int64 = 0, w.Frames
	if opt.OnlyWindow {
		if a.Start.IsZero() || opt.From.IsZero() || opt.To.IsZero() {
			return nil, fmt.Errorf("the recording's start time is unknown, so a time window can't be found in it")
		}
		const margin = 10 * time.Second
		from := int64(opt.From.Add(-margin).Sub(a.Start).Seconds() * float64(w.Rate))
		end = int64(opt.To.Add(margin).Sub(a.Start).Seconds() * float64(w.Rate))
		if from < 0 {
			from = 0
		}
		if end > w.Frames {
			end = w.Frames
		}
		if from >= end {
			return nil, fmt.Errorf("the recording (%s to %s UTC) doesn't cover %s to %s UTC",
				a.Start.Format("15:04:05"), a.Start.Add(time.Duration(a.Seconds*float64(time.Second))).Format("15:04:05"),
				opt.From.Format("15:04:05"), opt.To.Format("15:04:05"))
		}
		if err := w.Skip(from); err != nil {
			return nil, err
		}
		frame = from
		a.CenterHz, _ = tl.at(from) // the centre in effect there
		a.DialHz = tl.dialAt(from)
		if lost := lostBefore(meta, from); lost > 0 {
			a.Notes = append(a.Notes, fmt.Sprintf("About %d samples (%.2f s) were lost earlier in the recording, so times here may be early by up to that much.",
				lost, float64(lost)/float64(w.Rate)))
		}
	}
	if off := opt.ExpectHz - float64(a.CenterHz); math.Abs(off)+opt.SearchHz > float64(w.Rate)/2 {
		a.Notes = append(a.Notes, fmt.Sprintf("%.0f Hz is outside the recorded band (centre %d Hz, ±%d Hz).", opt.ExpectHz, a.CenterHz, w.Rate/2))
	}
	chains := []*chain{newChain(a, opt, w.Rate, false), newChain(a, opt, w.Rate, true)}
	buf := make([]complex64, 1<<16)
	for frame < end {
		if rest := end - frame; rest < int64(len(buf)) {
			buf = buf[:rest]
		}
		n, err := w.Read(buf)
		if n > 0 {
			for i := 0; i < n; i++ {
				f := frame + int64(i)
				center, ev := tl.at(f)
				for _, c := range chains {
					c.sample(buf[i], f, center, ev)
				}
			}
			frame += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	for _, c := range chains {
		c.flush()
	}

	pick := chains[0]
	switch {
	case opt.Mirror == -1:
		pick = chains[1]
	case opt.Mirror == 0:
		a.Detected = true
		if chains[1].score() > chains[0].score()+3 {
			pick = chains[1]
			a.Notes = append(a.Notes, "The carrier is clearer in the mirror image, so I and Q look swapped; measured as mirrored.")
		}
	}
	a.Mirrored = pick.mirror
	a.Segments = pick.segs
	a.Spectrum = pick.spectrum()
	for i := range a.Segments {
		s := &a.Segments[i]
		if !a.Start.IsZero() {
			s.T = a.Start.Add(time.Duration(s.Sec * float64(time.Second)))
		}
		s.AudioHz = s.RFHz - float64(tl.dialAt(int64(s.Sec*float64(w.Rate))))
		if s.OK && s.SNR < opt.MinSNR {
			s.OK, s.Why = false, "weak"
		}
	}
	a.Summary = summarise(a.Segments, opt, a.Start)
	if a.Summary == nil {
		a.Notes = append(a.Notes, "No segment has a clear carrier near the expected frequency.")
	}
	return a, nil
}

// lostBefore estimates the samples lost before a frame (from the packet gaps).
func lostBefore(m *Meta, frame int64) int64 {
	if m == nil || m.Packets == 0 || m.Frames == 0 {
		return 0
	}
	perPkt := m.Frames / m.Packets
	var n int64
	for _, g := range m.Gaps {
		if g.Frame <= frame {
			n += int64(g.Packets) * perPkt
		}
	}
	return n
}

// timeline gives the panadapter centre and the dial at a frame.
type timeline struct {
	centers []Event
	dials   []Event
	center0 int64
	dial0   int64
	ci      int
	gaps    []Gap
	gi      int
}

func newTimeline(m *Meta, center, dial int64) *timeline {
	t := &timeline{center0: center, dial0: dial}
	if m != nil {
		for _, e := range m.Events {
			switch e.What {
			case "center":
				t.centers = append(t.centers, e)
			case "dial":
				t.dials = append(t.dials, e)
			}
		}
		t.gaps = m.Gaps
	}
	return t
}

// at returns the centre at frame f (called with increasing f) and whether
// the stream has a discontinuity there (a re-centre or lost packets).
func (t *timeline) at(f int64) (int64, bool) {
	ev := false
	for t.ci < len(t.centers) && t.centers[t.ci].Frame <= f {
		t.center0 = t.centers[t.ci].Hz
		t.ci++
		ev = true
	}
	for t.gi < len(t.gaps) && t.gaps[t.gi].Frame <= f {
		t.gi++
		ev = true
	}
	return t.center0, ev
}

func (t *timeline) dialAt(f int64) int64 {
	d := t.dial0
	for _, e := range t.dials {
		if e.Frame > f {
			break
		}
		d = e.Hz
	}
	return d
}

// chain measures one image (as recorded, or mirrored).
type chain struct {
	a       *Analysis
	opt     Options
	rate    float64
	mirror  bool
	d1      int        // first decimation factor
	r1      float64    // rate after it
	osc     complex128 // mixer phasor
	step    complex128 // its rotation per sample
	stepOff float64    // offset step was made for
	oscN    int
	acc     complex128
	accN    int
	seg     []complex128
	segLen  int
	segFrom int64
	broken  string // why the current segment can't be used
	segs    []Segment
	specSum []float64
	specN   int
	nfft    int
}

func newChain(a *Analysis, opt Options, rate int, mirror bool) *chain {
	d1 := int(math.Round(float64(rate) / decimRate))
	if d1 < 1 {
		d1 = 1
	}
	c := &chain{a: a, opt: opt, rate: float64(rate), mirror: mirror, d1: d1, r1: float64(rate) / float64(d1)}
	c.segLen = int(opt.SegSec * c.r1)
	c.nfft = nextPow2(c.segLen) * 4
	return c
}

func (c *chain) sample(x complex64, f int64, center int64, ev bool) {
	if ev && len(c.seg) > 0 {
		c.broken = "re-centred or packets lost"
	}
	off := c.opt.ExpectHz - float64(center)
	if c.osc == 0 || off != c.stepOff {
		if c.osc == 0 {
			c.osc = 1
		}
		c.step, c.stepOff = cmplx.Exp(complex(0, -2*math.Pi*off/c.rate)), off
	}
	v := complex128(x)
	if c.mirror {
		v = cmplx.Conj(v)
	}
	c.acc += v * c.osc
	c.osc *= c.step
	if c.oscN++; c.oscN&1023 == 0 {
		c.osc /= complex(cmplx.Abs(c.osc), 0)
	}
	c.accN++
	if c.accN < c.d1 {
		return
	}
	if len(c.seg) == 0 {
		c.segFrom = f - int64(c.d1) + 1
	}
	c.seg = append(c.seg, c.acc/complex(float64(c.d1), 0))
	c.acc, c.accN = 0, 0
	if len(c.seg) >= c.segLen {
		c.measure()
	}
}

func (c *chain) flush() {
	if len(c.seg) >= c.segLen/2 {
		c.measure()
	}
}

func (c *chain) measure() {
	seg := c.seg
	s := Segment{Sec: (float64(c.segFrom) + float64(len(seg)*c.d1)/2) / c.rate, OK: true}
	defer func() {
		c.segs = append(c.segs, s)
		c.seg, c.broken = c.seg[:0], ""
	}()
	if c.broken != "" {
		s.OK, s.Why = false, c.broken
	}
	n := len(seg)
	x := make([]complex128, c.nfft)
	for i, v := range seg {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		x[i] = v * complex(w, 0)
	}
	fft(x)
	p := make([]float64, c.nfft)
	for i, v := range x {
		p[i] = real(v)*real(v) + imag(v)*imag(v)
	}
	binHz := c.r1 / float64(c.nfft)
	bin := func(hz float64) int { return int(math.Round(hz/binHz)+float64(c.nfft)) % c.nfft }
	kmax := int(c.opt.SearchHz / binHz)
	best, bestK := -1.0, 0
	var band []float64
	for k := -kmax; k <= kmax; k++ {
		v := p[bin(float64(k)*binHz)]
		band = append(band, v)
		if v > best {
			best, bestK = v, k
		}
	}
	// average spectrum, ±1.5 × search
	span := kmax * 3 / 2
	if c.specSum == nil {
		c.specSum = make([]float64, 2*span+1)
	}
	if len(seg) == c.segLen {
		for k := -span; k <= span; k++ {
			c.specSum[k+span] += p[bin(float64(k)*binHz)]
		}
		c.specN++
	}
	sort.Float64s(band)
	noise := band[len(band)/2]
	if noise <= 0 {
		noise = 1e-30
	}
	s.SNR = 10 * math.Log10(best/noise)
	// parabolic interpolation on log power
	lm, l0, lp := math.Log(p[bin(float64(bestK-1)*binHz)]+1e-300), math.Log(best+1e-300), math.Log(p[bin(float64(bestK+1)*binHz)]+1e-300)
	delta := 0.0
	if den := lm - 2*l0 + lp; den < 0 {
		delta = 0.5 * (lm - lp) / den
	}
	coarse := (float64(bestK) + delta) * binHz
	if bestK == kmax || bestK == -kmax {
		s.OK, s.Why = false, "peak at the edge of the search"
	}
	// phase fit
	d2 := int(math.Round(c.r1 / fineRate))
	if d2 < 1 {
		d2 = 1
	}
	var ts, ph []float64
	var acc complex128
	prev, unwrap := 0.0, 0.0
	for i, v := range seg {
		t := float64(i) / c.r1
		acc += v * cmplx.Exp(complex(0, -2*math.Pi*coarse*t))
		if (i+1)%d2 == 0 {
			a := cmplx.Phase(acc)
			if len(ph) > 0 {
				d := a - prev
				d -= 2 * math.Pi * math.Round(d/(2*math.Pi))
				unwrap += d
			} else {
				unwrap = a
			}
			prev = a
			ts = append(ts, (float64(i)-float64(d2-1)/2)/c.r1)
			ph = append(ph, unwrap)
			acc = 0
		}
	}
	slope, se := linfit(ts, ph)
	fine := slope / (2 * math.Pi)
	if math.Abs(fine) > binHz*2 { // the fit slipped; keep the FFT estimate
		fine, se = 0, binHz*2*math.Pi
	}
	s.RFHz = c.opt.ExpectHz + coarse + fine
	s.SigmaHz = se / (2 * math.Pi)
}

// score says how clearly this image shows a carrier: the median SNR.
func (c *chain) score() float64 {
	var v []float64
	for _, s := range c.segs {
		v = append(v, s.SNR)
	}
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

func (c *chain) spectrum() []SpecPoint {
	if c.specN == 0 {
		return nil
	}
	n := len(c.specSum)
	span := n / 2
	binHz := c.r1 / float64(c.nfft)
	step := 1
	if n > 1200 {
		step = (n + 1199) / 1200
	}
	var out []SpecPoint
	for i := 0; i < n; i += step {
		m := 0.0
		for j := i; j < i+step && j < n; j++ {
			m = math.Max(m, c.specSum[j])
		}
		out = append(out, SpecPoint{Hz: float64(i+step/2-span) * binHz, DB: 10 * math.Log10(m/float64(c.specN)+1e-30)})
	}
	return out
}

// linfit returns the least-squares slope of y on x and its standard error.
func linfit(x, y []float64) (slope, se float64) {
	n := float64(len(x))
	if n < 3 {
		return 0, math.Inf(1)
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx, my = mx/n, my/n
	var sxx, sxy float64
	for i := range x {
		sxx += (x[i] - mx) * (x[i] - mx)
		sxy += (x[i] - mx) * (y[i] - my)
	}
	slope = sxy / sxx
	var ss float64
	for i := range x {
		r := y[i] - my - slope*(x[i]-mx)
		ss += r * r
	}
	return slope, math.Sqrt(ss / (n - 2) / sxx)
}

// summarise averages the usable segments, those in the window if any are,
// leaving out ones more than 4 robust SDs from the median.
func summarise(segs []Segment, opt Options, start time.Time) *Summary {
	win := !opt.From.IsZero() && !opt.To.IsZero() && !start.IsZero()
	any := false
	for i := range segs {
		s := &segs[i]
		s.In = !win || (!s.T.Before(opt.From) && !s.T.After(opt.To))
		if s.In && s.OK {
			any = true
		}
	}
	basis := "all segments with a clear carrier"
	if win {
		if any {
			basis = "segments inside the key-down minute"
		} else {
			basis = "all segments with a clear carrier (none inside the key-down minute)"
			for i := range segs {
				segs[i].In = true
			}
		}
	}
	var v []float64
	for _, s := range segs {
		if s.OK && s.In {
			v = append(v, s.RFHz)
		}
	}
	if len(v) == 0 {
		return nil
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	med := sorted[len(sorted)/2]
	var dev []float64
	for _, x := range v {
		dev = append(dev, math.Abs(x-med))
	}
	sort.Float64s(dev)
	lim := math.Max(4*1.4826*dev[len(dev)/2], 0.05)
	sum := &Summary{Basis: basis}
	var xs, ys []float64
	for i := range segs {
		s := &segs[i]
		if !s.OK || !s.In {
			continue
		}
		if math.Abs(s.RFHz-med) > lim {
			s.OK, s.Why = false, "outlier"
			continue
		}
		sum.N++
		sum.MeanRFHz += s.RFHz
		sum.MeanAudio += s.AudioHz
		xs, ys = append(xs, s.Sec/60), append(ys, s.RFHz)
		if sum.From.IsZero() || s.T.Before(sum.From) {
			sum.From = s.T
		}
		if s.T.After(sum.To) {
			sum.To = s.T
		}
	}
	if sum.N == 0 {
		return nil
	}
	n := float64(sum.N)
	sum.MeanRFHz /= n
	sum.MeanAudio /= n
	for _, y := range ys {
		sum.SD += (y - sum.MeanRFHz) * (y - sum.MeanRFHz)
	}
	if sum.N > 1 {
		sum.SD = math.Sqrt(sum.SD / (n - 1))
		sum.SEM = sum.SD / math.Sqrt(n)
	} else {
		sum.SD = 0
	}
	if sum.N >= 3 {
		sum.DriftHzMin, _ = linfit(xs, ys)
	}
	return sum
}
