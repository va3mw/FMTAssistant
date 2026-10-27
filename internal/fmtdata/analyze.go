package fmtdata

import (
	"fmt"
	"math"
	"sort"
)

// Issue is a problem or note found while checking a file.
type Issue struct {
	Level string `json:"level"` // "error", "warn" or "info"
	Msg   string `json:"msg"`
}

func errorf(f string, a ...any) Issue { return Issue{"error", fmt.Sprintf(f, a...)} }
func warnf(f string, a ...any) Issue  { return Issue{"warn", fmt.Sprintf(f, a...)} }
func infof(f string, a ...any) Issue  { return Issue{"info", fmt.Sprintf(f, a...)} }

// HasErrors reports whether any issue is an error.
func HasErrors(is []Issue) bool {
	for _, i := range is {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

// Reference is a standard-frequency station usable for calibration.
type Reference struct {
	KHz   int
	Label string
}

// References are the time-and-frequency stations WSJT-X lists for FreqCal.
var References = []Reference{
	{2500, "WWV 2.5 MHz"}, {5000, "WWV 5 MHz"}, {10000, "WWV 10 MHz"},
	{15000, "WWV 15 MHz"}, {20000, "WWV 20 MHz"}, {25000, "WWV 25 MHz"},
	{3330, "CHU 3.33 MHz"}, {7850, "CHU 7.85 MHz"}, {14670, "CHU 14.67 MHz"},
}

// LookupReference finds the reference station for a kHz column value.
// kHz is truncated, so allow one kHz either side.
func LookupReference(khz int) (Reference, bool) {
	for _, r := range References {
		if khz >= r.KHz-1 && khz <= r.KHz+1 {
			return r, true
		}
	}
	return Reference{}, false
}

// ---------------------------------------------------------------------------
// Calibration

// CalOptions control which calibration measurements are used.
type CalOptions struct {
	MinSNR float64 `json:"min_snr"` // lines below this (or flagged "*") are dropped
	// Station toggles keyed by kHz: true forces include, false forces exclude.
	// Without a toggle, reference stations are included and others excluded.
	Stations map[int]bool `json:"stations"`
}

// CalStation is one station's averaged error.
type CalStation struct {
	KHz      int     `json:"khz"`
	Label    string  `json:"label"`
	IsRef    bool    `json:"is_ref"`
	Lines    int     `json:"lines"`    // all lines for this station
	LowSNR   int     `json:"low_snr"`  // dropped for S/N
	Outliers int     `json:"outliers"` // dropped as far from the station median
	N        int     `json:"n"`        // lines used
	MeanDF   float64 `json:"mean_df"`
	SD       float64 `json:"sd"`
	Included bool    `json:"included"`
	Resid    float64 `json:"resid"`
	Note     string  `json:"note"`
}

// CalResult is a straight-line fit DF = A + B*f (f in MHz, B in ppm).
type CalResult struct {
	Stations []CalStation `json:"stations"`
	A        float64      `json:"a"`
	B        float64      `json:"b"`
	SigmaA   float64      `json:"sigma_a"`
	SigmaB   float64      `json:"sigma_b"`
	RMS      float64      `json:"rms"`
	NFit     int          `json:"n_fit"`
	OK       bool         `json:"ok"`
	First    string       `json:"first"`
	Last     string       `json:"last"`
	MidSec   int          `json:"mid_sec"`
	Total    int          `json:"total"`
	Used     int          `json:"used"`
	Issues   []Issue      `json:"issues"`
	BadLines int          `json:"bad_lines"`
}

// Correction returns A + B*f, the radio's error in Hz at fMHz.
func (c *CalResult) Correction(fMHz float64) float64 { return c.A + c.B*fMHz }

// AnalyzeCal groups measurements by station, drops weak and wild readings,
// and fits a line through the station means (as WSJT-X's fitcal does).
func AnalyzeCal(f *File, opt CalOptions) *CalResult {
	res := &CalResult{Total: len(f.Lines), BadLines: len(f.Bad)}
	if len(f.Lines) == 0 {
		res.Issues = append(res.Issues, errorf("The file has no measurements. Was the Measure box checked in WSJT-X?"))
		return res
	}
	if len(f.Bad) > 0 {
		res.Issues = append(res.Issues, warnf("%d line(s) could not be read and were ignored (first is line %d).", len(f.Bad), f.Bad[0].N))
	}
	res.First, res.Last = f.Lines[0].Time, f.Lines[len(f.Lines)-1].Time
	res.MidSec = (f.Lines[0].Sec + SecDiff(f.Lines[len(f.Lines)-1].Sec, f.Lines[0].Sec)/2 + 86400) % 86400

	byKHz := map[int][]Line{}
	var order []int
	for _, l := range f.Lines {
		if _, ok := byKHz[l.KHz]; !ok {
			order = append(order, l.KHz)
		}
		byKHz[l.KHz] = append(byKHz[l.KHz], l)
	}
	sort.Ints(order)

	nonRef := 0
	for _, khz := range order {
		ls := byKHz[khz]
		st := CalStation{KHz: khz, Lines: len(ls)}
		if ref, ok := LookupReference(khz); ok {
			st.Label, st.IsRef = ref.Label, true
		} else {
			st.Label = fmt.Sprintf("%.3f MHz (not a WWV/CHU frequency)", float64(khz)/1000)
			nonRef++
		}
		var dfs []float64
		for _, l := range ls {
			if l.Flag || l.SNR < opt.MinSNR {
				st.LowSNR++
				continue
			}
			dfs = append(dfs, l.DF)
		}
		kept, out := rejectOutliers(dfs, 1.0)
		st.Outliers = out
		st.N = len(kept)
		st.MeanDF, st.SD = meanSD(kept)

		st.Included = st.IsRef
		if v, ok := opt.Stations[khz]; ok {
			st.Included = v
		}
		switch {
		case st.N == 0:
			st.Included = false
			st.Note = "no usable readings (all below S/N limit)"
		case !st.IsRef && st.Included:
			st.Note = "included by you; broadcast carriers are often off frequency"
		case !st.IsRef:
			st.Note = "excluded: not a standard-frequency station"
		case st.SD > 0.5:
			st.Note = "noisy: readings scatter more than 0.5 Hz"
		}
		res.Stations = append(res.Stations, st)
	}

	var xs, ys []float64
	for _, st := range res.Stations {
		if st.Included {
			xs = append(xs, float64(st.KHz)/1000)
			ys = append(ys, st.MeanDF)
			res.Used += st.N
		}
	}
	res.NFit = len(xs)
	if nonRef > 0 {
		res.Issues = append(res.Issues, infof("%d station(s) are not WWV/CHU and are excluded unless you include them.", nonRef))
	}
	if res.NFit == 0 {
		res.Issues = append(res.Issues, errorf("No usable calibration stations. Check the S/N limit and that the file is a WWV calibration run."))
		return res
	}
	if res.NFit == 1 {
		// One station: no slope, just an offset at that frequency.
		res.A, res.B = ys[0], 0
		res.OK = true
		res.Issues = append(res.Issues, warnf("Only one station was usable, so the slope is assumed to be zero. Calibrate on at least three WWV frequencies if you can."))
		return res
	}
	res.A, res.B, res.SigmaA, res.SigmaB, res.RMS = fitLine(xs, ys)
	res.OK = true
	for i := range res.Stations {
		st := &res.Stations[i]
		st.Resid = st.MeanDF - res.Correction(float64(st.KHz)/1000)
	}
	if res.NFit == 2 {
		res.Issues = append(res.Issues, warnf("Only two stations were usable; the fit has no uncertainty estimate. Three or more is better."))
	}
	if res.RMS > 1.0 {
		res.Issues = append(res.Issues, errorf("The fit's standard deviation is %.2f Hz (WSJT-X rejects anything over 1 Hz). Look for a bad station.", res.RMS))
	} else if res.RMS > 0.3 {
		res.Issues = append(res.Issues, warnf("The fit's standard deviation is %.2f Hz; a good calibration is usually under 0.3 Hz.", res.RMS))
	}
	return res
}

// fitLine is WSJT-X's fitcal: unweighted least squares y = a + b*x.
func fitLine(x, y []float64) (a, b, sigA, sigB, rms float64) {
	n := float64(len(x))
	var sx, sy, sxy, sx2 float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxy += x[i] * y[i]
		sx2 += x[i] * x[i]
	}
	delta := n*sx2 - sx*sx
	if delta == 0 {
		m, _ := meanSD(y)
		return m, 0, 0, 0, 0
	}
	a = (sx2*sy - sx*sxy) / delta
	b = (n*sxy - sx*sy) / delta
	if len(x) >= 3 {
		var sq float64
		for i := range x {
			r := y[i] - (a + b*x[i])
			sq += r * r
		}
		rms = math.Sqrt(sq / (n - 2))
		sigA = math.Sqrt(rms * rms * sx2 / delta)
		sigB = math.Sqrt(n * rms * rms / delta)
	}
	return
}

// ---------------------------------------------------------------------------
// Carrier runs

// RunOptions control how a run's readings are selected.
type RunOptions struct {
	MinSNR    float64 `json:"min_snr"`
	TrimEdges bool    `json:"trim_edges"` // drop first and last reading of the carrier
	// Per-line toggles keyed by line number: true forces use, false forces drop.
	Overrides map[int]bool `json:"overrides"`
	// Key-down window in UTC seconds of day; -1 when unknown.
	KeyDownStart int `json:"-"`
	KeyDownEnd   int `json:"-"`
	// ForceWindow always keeps only readings inside the key-down window
	// (practice on a continuous carrier, where the whole file is strong).
	ForceWindow bool `json:"-"`
	// AllowReference accepts a run measured on a WWV/CHU frequency.
	AllowReference bool `json:"-"`
}

// RunLine is a line with the reason it was or wasn't used.
type RunLine struct {
	Line
	Used   bool   `json:"used"`
	Status string `json:"status"`
	Forced bool   `json:"forced"`
}

// RunResult is the averaged measurement of one carrier.
type RunResult struct {
	Lines     []RunLine `json:"lines"`
	N         int       `json:"n"`
	MeanFMeas float64   `json:"mean_fmeas"`
	SD        float64   `json:"sd"`
	SEM       float64   `json:"sem"`
	KHz       int       `json:"khz"`
	Offset    int       `json:"offset"`
	BlockFrom string    `json:"block_from"`
	BlockTo   string    `json:"block_to"`
	MidSec    int       `json:"mid_sec"`
	// The dial frequencies (Hz) consistent with the file's kHz column.
	DialMin int64   `json:"dial_min"`
	DialMax int64   `json:"dial_max"`
	Issues  []Issue `json:"issues"`
	OK      bool    `json:"ok"`
}

const blockGap = 20 // seconds; readings further apart start a new block

// AnalyzeRun finds the key-down carrier in a run file and averages it.
//
// A one-minute key-down shows up as an unbroken stretch of strong readings;
// CW identification and dead air show up as weak or flagged readings. The
// longest strong stretch is taken as the carrier (preferring one inside the
// scheduled key-down window), its first and last readings are dropped because
// they may straddle key-up/key-down, and wild readings are rejected.
func AnalyzeRun(f *File, opt RunOptions) *RunResult {
	res := &RunResult{}
	if len(f.Lines) == 0 {
		res.Issues = append(res.Issues, errorf("The file has no measurements. Was the Measure box checked in WSJT-X?"))
		return res
	}
	if len(f.Bad) > 0 {
		res.Issues = append(res.Issues, warnf("%d line(s) could not be read and were ignored (first is line %d).", len(f.Bad), f.Bad[0].N))
	}
	lines := make([]RunLine, len(f.Lines))
	for i, l := range f.Lines {
		lines[i] = RunLine{Line: l}
	}
	strong := func(l Line) bool { return !l.Flag && l.SNR >= opt.MinSNR }

	// Blocks of consecutive strong readings.
	type block struct{ from, to int }
	var blocks []block
	for i := 0; i < len(lines); i++ {
		if !strong(lines[i].Line) {
			continue
		}
		j := i
		for j+1 < len(lines) && strong(lines[j+1].Line) && SecDiff(lines[j+1].Sec, lines[j].Sec) <= blockGap {
			j++
		}
		blocks = append(blocks, block{i, j})
		i = j
	}
	inWindow := func(b block) bool {
		if opt.KeyDownStart < 0 || opt.KeyDownEnd < 0 {
			return false
		}
		s, e := lines[b.from].Sec, lines[b.to].Sec
		return SecDiff(e, opt.KeyDownStart-60) >= 0 && SecDiff(opt.KeyDownEnd+60, s) >= 0
	}
	best := -1
	for i, b := range blocks {
		if best < 0 {
			best = i
			continue
		}
		bl, cl := blocks[best].to-blocks[best].from, b.to-b.from
		bw, cw := inWindow(blocks[best]), inWindow(b)
		// Prefer the window when lengths are comparable, else the longer block.
		if (cw && !bw && float64(cl+1) >= 0.6*float64(bl+1)) || (cw == bw && cl > bl) ||
			(!cw && bw && float64(cl+1) > 1.0/0.6*float64(bl+1)) {
			best = i
		}
	}

	for i := range lines {
		if !strong(lines[i].Line) {
			lines[i].Status = "weak signal"
			if lines[i].Flag {
				lines[i].Status = "weak signal (*)"
			}
		} else {
			lines[i].Status = "not part of the carrier"
		}
	}
	if best >= 0 {
		b := blocks[best]
		for i := b.from; i <= b.to; i++ {
			lines[i].Used, lines[i].Status = true, "used"
		}
		// Strong CW-ID readings can run straight into the key-down. When the
		// carrier sits on its scheduled time, drop readings outside it. A
		// reading's time is the end of its ~4.6 s analysis window.
		if inWindow(b) || (opt.ForceWindow && opt.KeyDownStart >= 0) {
			within := func(s int) bool {
				return SecDiff(s, opt.KeyDownStart) >= 0 && SecDiff(opt.KeyDownEnd+5, s) >= 0
			}
			in := 0
			for i := b.from; i <= b.to; i++ {
				if within(lines[i].Sec) {
					in++
				}
			}
			if in*2 >= b.to-b.from+1 || (opt.ForceWindow && in > 0) {
				for i := b.from; i <= b.to; i++ {
					if !within(lines[i].Sec) {
						lines[i].Used, lines[i].Status = false, "outside key-down time"
					}
				}
			}
		}
		var idx []int
		for i := b.from; i <= b.to; i++ {
			if lines[i].Used {
				idx = append(idx, i)
			}
		}
		if opt.TrimEdges && len(idx) >= 5 {
			lines[idx[0]].Used, lines[idx[0]].Status = false, "edge of carrier"
			lines[idx[len(idx)-1]].Used, lines[idx[len(idx)-1]].Status = false, "edge of carrier"
		}
		// Reject readings far from the rest of the carrier.
		var vals []float64
		for _, l := range lines {
			if l.Used {
				vals = append(vals, l.FMeas)
			}
		}
		med, mad := medianMAD(vals)
		lim := math.Max(0.75, 5*1.4826*mad)
		for i := range lines {
			if lines[i].Used && math.Abs(lines[i].FMeas-med) > lim {
				lines[i].Used, lines[i].Status = false, fmt.Sprintf("outlier (%+.2f Hz from median)", lines[i].FMeas-med)
			}
		}
		other := 0
		for i, ob := range blocks {
			if i != best && ob.to-ob.from+1 >= 5 && float64(ob.to-ob.from+1) >= 0.6*float64(b.to-b.from+1) {
				other++
			}
		}
		if other > 0 {
			res.Issues = append(res.Issues, warnf("The file has %d other long stretch(es) of strong signal. Check that the highlighted readings are the key-down carrier.", other))
		}
		if opt.KeyDownStart >= 0 && !inWindow(b) {
			res.Issues = append(res.Issues, warnf("The carrier found (%s to %s UTC) is outside the scheduled key-down time. Is this the right file?", lines[b.from].Time, lines[b.to].Time))
		}
	}
	for i := range lines {
		if v, ok := opt.Overrides[lines[i].N]; ok {
			lines[i].Used, lines[i].Forced = v, true
			if v {
				lines[i].Status = "used (your choice)"
			} else {
				lines[i].Status = "dropped (your choice)"
			}
		}
	}
	res.Lines = lines

	var vals []float64
	first, last := -1, -1
	khzCount, offCount := map[int]int{}, map[int]int{}
	for i, l := range lines {
		if !l.Used {
			continue
		}
		vals = append(vals, l.FMeas)
		khzCount[l.KHz]++
		offCount[l.Offset]++
		if first < 0 {
			first = i
		}
		last = i
	}
	res.N = len(vals)
	if res.N == 0 {
		res.Issues = append(res.Issues, errorf("No readings are strong enough to use (S/N limit %.0f dB). The carrier may have been missed.", opt.MinSNR))
		return res
	}
	res.MeanFMeas, res.SD = meanSD(vals)
	if res.N > 1 {
		res.SEM = res.SD / math.Sqrt(float64(res.N))
	}
	res.KHz, res.Offset = mode(khzCount), mode(offCount)
	if len(khzCount) > 1 || len(offCount) > 1 {
		res.Issues = append(res.Issues, errorf("The readings used come from more than one dial setting (kHz %v, offsets %v). The dial was moved during the carrier; keep only one setting.", keys(khzCount), keys(offCount)))
	}
	res.BlockFrom, res.BlockTo = lines[first].Time, lines[last].Time
	res.MidSec = (lines[first].Sec + SecDiff(lines[last].Sec, lines[first].Sec)/2 + 86400) % 86400
	// WSJT-X: kHz = (dial + offset) / 1000, truncated.
	res.DialMin = int64(res.KHz)*1000 - int64(res.Offset)
	res.DialMax = res.DialMin + 999

	if _, isRef := LookupReference(res.KHz); isRef && !opt.AllowReference {
		res.Issues = append(res.Issues, errorf("This file was measured at %s, a calibration frequency. It looks like a calibration file, not a test run.", fmtKHz(res.KHz)))
	}
	if res.N < 3 {
		res.Issues = append(res.Issues, warnf("Only %d reading(s) used; the answer is less certain.", res.N))
	}
	if res.SD > 1.0 {
		res.Issues = append(res.Issues, warnf("Readings scatter by %.2f Hz (std dev). Check for interference or a moved dial.", res.SD))
	}
	res.OK = !HasErrors(res.Issues)
	return res
}

// ---------------------------------------------------------------------------
// helpers

func meanSD(v []float64) (mean, sd float64) {
	if len(v) == 0 {
		return 0, 0
	}
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	if len(v) > 1 {
		var s float64
		for _, x := range v {
			s += (x - mean) * (x - mean)
		}
		sd = math.Sqrt(s / float64(len(v)-1))
	}
	return
}

func medianMAD(v []float64) (med, mad float64) {
	if len(v) == 0 {
		return 0, 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	med = median(s)
	d := make([]float64, len(s))
	for i, x := range s {
		d[i] = math.Abs(x - med)
	}
	sort.Float64s(d)
	return med, median(d)
}

func median(s []float64) float64 {
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// rejectOutliers drops values further than max(floor, 5*sigma_MAD) from the median.
func rejectOutliers(v []float64, floor float64) (kept []float64, dropped int) {
	if len(v) < 3 {
		return v, 0
	}
	med, mad := medianMAD(v)
	lim := math.Max(floor, 5*1.4826*mad)
	for _, x := range v {
		if math.Abs(x-med) > lim {
			dropped++
		} else {
			kept = append(kept, x)
		}
	}
	return
}

func mode(m map[int]int) int {
	best, bc := 0, -1
	for k, c := range m {
		if c > bc || (c == bc && k < best) {
			best, bc = k, c
		}
	}
	return best
}

func keys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func fmtKHz(khz int) string { return fmt.Sprintf("%d kHz", khz) }
