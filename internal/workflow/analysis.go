package workflow

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// RunInput is what the analysis needs to know about one run.
type RunInput struct {
	N       int     // run number, 1-based
	Title   string  // e.g. "K5CM 80 m"
	Points  []Point // the readings used
	AudioHz float64 // mean audio frequency (fMeas) of the readings used
	FreqHz  float64 // the answer (carrier frequency)
	// ErrHz is answer minus published. Nil until the answers are locked and
	// the published frequency is entered.
	ErrHz   *float64
	Capture *CaptureCheck // nil when unknown
	Fading  *Fading       // nil when not checked
	// Coverage compares the captured stretch with the whole minute when an
	// IQ recording covers it; nil otherwise.
	Coverage *Coverage
}

// RunNote is the analysis of one run.
type RunNote struct {
	N      int      `json:"n"`
	Title  string   `json:"title"`
	ErrHz  *float64 `json:"err_hz,omitempty"`
	ErrPPB *float64 `json:"err_ppb,omitempty"`
	SD     float64  `json:"sd"`
	SEM    float64  `json:"sem"`     // SD/sqrt(N)
	Lag1   float64  `json:"lag1"`    // lag-1 autocorrelation
	NEff   float64  `json:"n_eff"`   // effective N
	SEMEff float64  `json:"sem_eff"` // SD/sqrt(NEff)
	Slope  Slope    `json:"slope"`
	Lines  []string `json:"lines"`
}

// RefCheck is the reading of a signal with no skywave path.
type RefCheck struct {
	PPB  float64 `json:"ppb"`
	What string  `json:"what"` // e.g. "ground-wave AM station 1010 kHz"
}

// Analysis is the plain-language reading of a session.
type Analysis struct {
	Runs    []RunNote `json:"runs"`
	Pattern []string  `json:"pattern"` // across runs (needs the published values)
	// Offset is the systematic offset of the agreeing runs, in ppb, and
	// Outliers the run numbers that don't share it.
	Offset   *float64 `json:"offset_ppb,omitempty"`
	Outliers []int    `json:"outliers,omitempty"`
}

// Analyze explains each run, and, once errors are known, the pattern
// across runs.
func Analyze(runs []RunInput, ref *RefCheck, th Thresholds) Analysis {
	var an Analysis
	for _, r := range runs {
		an.Runs = append(an.Runs, runNote(r, th))
	}

	// Runs with a known, sensible error take part in the pattern.
	type fair struct {
		in   RunInput
		note RunNote
		ppb  float64
	}
	var fs []fair
	for i, r := range runs {
		if r.ErrHz == nil || math.Abs(*r.ErrHz) > th.BlunderHz {
			continue
		}
		fs = append(fs, fair{r, an.Runs[i], *an.Runs[i].ErrPPB})
	}
	if len(fs) < 2 {
		return an
	}

	// The largest group of runs that agree within AgreePPB.
	idx := make([]int, len(fs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return fs[idx[a]].ppb < fs[idx[b]].ppb })
	bestLo, bestHi := 0, 0
	for lo, hi := 0, 0; lo < len(idx); lo++ {
		for hi+1 < len(idx) && fs[idx[hi+1]].ppb-fs[idx[lo]].ppb <= th.AgreePPB {
			hi++
		}
		if hi-lo > bestHi-bestLo {
			bestLo, bestHi = lo, hi
		}
	}
	group := map[int]bool{}
	var gp []float64
	for k := bestLo; k <= bestHi; k++ {
		group[idx[k]] = true
		gp = append(gp, fs[idx[k]].ppb)
	}
	mean, _ := meanSD(gp)
	spread := gp[len(gp)-1] - gp[0]
	majority := len(gp) >= 2 && 2*len(gp) > len(fs)

	if majority {
		an.Offset = &mean
		var outs []string
		for i, f := range fs {
			if !group[i] {
				an.Outliers = append(an.Outliers, f.in.N)
				outs = append(outs, fmt.Sprintf("run %d", f.in.N))
			}
		}
		sort.Ints(an.Outliers)
		switch len(outs) {
		case 0:
			an.Pattern = append(an.Pattern, fmt.Sprintf("Systematic offset of %+.1f ppb: all %d runs agree within %.1f ppb.", mean, len(fs), spread))
		case 1:
			an.Pattern = append(an.Pattern, fmt.Sprintf("Systematic offset of %+.1f ppb, plus one outlier (%s). The other %d runs agree within %.1f ppb.", mean, outs[0], len(gp), spread))
		default:
			an.Pattern = append(an.Pattern, fmt.Sprintf("Systematic offset of %+.1f ppb, plus outliers (%s). The other %d runs agree within %.1f ppb.", mean, strings.Join(outs, " and "), len(gp), spread))
		}
		for i, f := range fs {
			if group[i] {
				continue
			}
			d := f.ppb - mean
			n := fmt.Sprintf("Run %d (%s) is %+.1f ppb (%+.3f Hz) from that offset.", f.in.N, f.in.Title, d, d*f.in.FreqHz/1e9)
			if u := f.note.SEMEff; u > 0 {
				k := math.Abs(d*f.in.FreqHz/1e9) / u
				n += fmt.Sprintf(" That's %.1f times its own measurement uncertainty (%.3f Hz).", k, u)
			}
			if f.in.Fading != nil && f.in.Fading.Suspect {
				n += " Its readings show fading, so the path itself is the likely cause: look at its plot."
			} else {
				n += " Check its plot, its dial entry and its file."
			}
			an.Pattern = append(an.Pattern, n)
		}
		if math.Abs(mean) >= th.AgreePPB/2 {
			an.Pattern = append(an.Pattern, "An error that is the same in ppb on every run is proportional to frequency. That can come from the frequency reference, or from the ionosphere: a reflecting layer moving up or down shifts every frequency by the same fraction. "+refVerdict(ref, mean, th))
		}
	} else {
		an.Pattern = append(an.Pattern, fmt.Sprintf("The runs don't share one offset (%+.1f to %+.1f ppb, no majority within %.0f ppb of each other). That points to each path's own Doppler or to the individual measurements rather than one cause.",
			fs[idx[0]].ppb, fs[idx[len(idx)-1]].ppb, th.AgreePPB))
	}

	// Audio clock: its error would be a fixed fraction of the audio frequency.
	var ratios []string
	var rv []float64
	for _, f := range fs {
		if f.in.AudioHz > 0 {
			x := *f.in.ErrHz / f.in.AudioHz * 1e6
			rv = append(rv, x)
			ratios = append(ratios, fmt.Sprintf("%+.0f", x))
		}
	}
	if len(rv) >= 2 {
		m, _ := meanSD(rv)
		lo, hi := rv[0], rv[0]
		for _, x := range rv {
			lo, hi = math.Min(lo, x), math.Max(hi, x)
		}
		if m == 0 || (hi-lo)/math.Abs(m) > th.AudioClockSpread {
			an.Pattern = append(an.Pattern, fmt.Sprintf("Error divided by audio frequency is %s ppm across the runs. That doesn't line up, so the sound card's clock isn't the cause.", joinAnd(ratios)))
		} else {
			an.Pattern = append(an.Pattern, fmt.Sprintf("Error divided by audio frequency lines up at about %+.0f ppm on every run. A sound card clock off by that much would explain it: check the sound card's sample rate.", m))
		}
	}
	return an
}

func refVerdict(ref *RefCheck, offset float64, th Thresholds) string {
	if ref == nil {
		return "Only the reference check (a signal with no skywave path) can tell which. Do one next time."
	}
	switch {
	case math.Abs(ref.PPB) <= th.RefCheckOKPPB:
		return fmt.Sprintf("Your reference check on %s read %+.1f ppb, so the reference is fine: the offset most likely comes from the ionosphere, which the WWV corrections may also carry.", ref.What, ref.PPB)
	case math.Abs(ref.PPB-offset) <= th.AgreePPB:
		return fmt.Sprintf("Your reference check on %s read %+.1f ppb, close to the runs' offset: the reference is the likely cause.", ref.What, ref.PPB)
	default:
		return fmt.Sprintf("Your reference check on %s read %+.1f ppb, which matches neither a good reference nor the runs' offset. Repeat it to be sure.", ref.What, ref.PPB)
	}
}

func runNote(r RunInput, th Thresholds) RunNote {
	v := values(r.Points)
	n := RunNote{N: r.N, Title: r.Title, Lag1: Lag1(v)}
	_, n.SD = meanSD(v)
	if len(v) > 1 {
		n.SEM = n.SD / math.Sqrt(float64(len(v)))
	}
	n.NEff = EffectiveN(len(v), n.Lag1)
	if n.NEff > 0 {
		n.SEMEff = n.SD / math.Sqrt(n.NEff)
	}
	n.Slope = FitSlope(r.Points, th.SlopeConfidence)
	head := fmt.Sprintf("Run %d, %s:", r.N, r.Title)

	if r.ErrHz != nil && r.FreqHz > 0 {
		e := *r.ErrHz
		ppb := e / r.FreqHz * 1e9
		n.ErrHz, n.ErrPPB = &e, &ppb
		s := fmt.Sprintf("%s off by %+.3f Hz (%+.1f ppb).", head, e, ppb)
		switch {
		case math.Abs(e) > th.BlunderHz:
			s += " That's far more than the reference or the ionosphere can do: check the dial you entered, the published frequency you typed, and that the file is this run's. It's left out of the pattern across runs."
		case n.SEMEff > 0 && math.Abs(e) <= 2*n.SEMEff:
			s += fmt.Sprintf(" That's within the measurement's own uncertainty (%.1f times %.3f Hz), so this run alone can't show a bias.", math.Abs(e)/n.SEMEff, n.SEMEff)
		case n.SEMEff > 0 && math.Abs(e) <= 3*n.SEMEff:
			s += fmt.Sprintf(" That's %.1f times the measurement's own uncertainty (%.3f Hz): probably a bias, but noise could account for part of it.", math.Abs(e)/n.SEMEff, n.SEMEff)
		case n.SEMEff > 0:
			s += fmt.Sprintf(" That's %.1f times the measurement's own uncertainty (%.3f Hz), so more readings wouldn't have fixed it: the cause is a bias (the reference, the calibration or the path's Doppler).", math.Abs(e)/n.SEMEff, n.SEMEff)
		}
		n.Lines = append(n.Lines, s)
	} else {
		n.Lines = append(n.Lines, fmt.Sprintf("%s %d readings, SD %.3f Hz, σ/√N %.3f Hz.", head, len(v), n.SD, n.SEM))
	}

	if s := n.Slope; s.OK {
		if s.Drifting {
			n.Lines = append(n.Lines, fmt.Sprintf("Drifting: %+.2f ± %.2f Hz per minute, %.1f standard errors from zero (noise alone stays under %.1f). The path's Doppler or the radio was changing during the run.", s.HzPerMin, s.SE, math.Abs(s.T), s.TCrit))
		} else {
			n.Lines = append(n.Lines, fmt.Sprintf("Slope %+.2f ± %.2f Hz per minute, %.1f standard errors from zero. Noise alone can give up to %.1f with this many readings, so no real drift can be seen.", s.HzPerMin, s.SE, math.Abs(s.T), s.TCrit))
		}
	}
	if n.Lag1 > th.AutocorrNote && len(v) > 2 {
		n.Lines = append(n.Lines, fmt.Sprintf("Neighbouring readings move together (lag-1 autocorrelation %.2f), so the %d readings are worth about %.1f independent ones. The uncertainty widens from %.3f to %.3f Hz.",
			n.Lag1, len(v), n.NEff, n.SEM, n.SEMEff))
	}
	if r.Fading != nil && r.Fading.Suspect {
		n.Lines = append(n.Lines, r.Fading.Msg)
	}
	if c := r.Capture; c != nil && c.Short && c.FirstSec >= 0 {
		share := "less than half"
		if c.Fraction >= 0.45 {
			share = "about half"
		}
		n.Lines = append(n.Lines, fmt.Sprintf("Only about %.0f s of the %.0f s key-down was captured, so this result rests on %s the available data, from the first part of the carrier only.", c.SpanSec, c.ExpectedSec, share))
	}
	if r.Coverage != nil {
		n.Lines = append(n.Lines, r.Coverage.Msg)
	}
	return n
}

func joinAnd(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// Hindsight compares every correction method once the answers are locked.
// It is for learning only: the submitted answers keep the method chosen in
// Setup. errs maps method to each run's error (Hz), in run order; outliers
// are run numbers (1-based) to also leave out in a second RMS.
func Hindsight(chosen string, errs map[string][]float64, outliers []int) []string {
	out := []string{fmt.Sprintf("Hindsight, for learning only. Your answers used %s, chosen before the test; they stay as submitted.", MethodNames[chosen])}
	skip := map[int]bool{}
	for _, o := range outliers {
		skip[o-1] = true
	}
	for _, m := range Methods {
		e, ok := errs[m]
		if !ok || len(e) == 0 {
			continue
		}
		var all, kept []float64
		for i, x := range e {
			all = append(all, x)
			if !skip[i] {
				kept = append(kept, x)
			}
		}
		s := fmt.Sprintf("%s: RMS error %.3f Hz", MethodNames[m], rms(all))
		if len(outliers) > 0 && len(kept) > 0 {
			s += fmt.Sprintf(", %.3f Hz without the outlier%s", rms(kept), plural(len(outliers), "", "s"))
		}
		if m == chosen {
			s += " (your choice)"
		}
		out = append(out, s+".")
	}
	out = append(out, "One night doesn't show which method will be best next time.")
	return out
}

// RMS is the root mean square of v.
func RMS(v []float64) float64 { return rms(v) }

func rms(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s / float64(len(v)))
}
