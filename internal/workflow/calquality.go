package workflow

import (
	"fmt"
	"math"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
)

// Station verdicts.
const (
	VerdictTooFew   = "too_few"
	VerdictTooNoisy = "too_noisy"
	VerdictSteady   = "too_steady"
)

// StationQuality is one WWV frequency in a calibration, judged.
type StationQuality struct {
	KHz      int      `json:"khz"`
	Label    string   `json:"label"`
	N        int      `json:"n"`
	SD       float64  `json:"sd"`
	SEM      float64  `json:"sem"` // SD/sqrt(n)
	Resid    float64  `json:"resid"`
	Included bool     `json:"included"`
	Verdicts []string `json:"verdicts"`
	Notes    []string `json:"notes"`
	Good     bool     `json:"good"`
}

// CalQuality is the judgement of a whole calibration.
type CalQuality struct {
	Stations []StationQuality `json:"stations"`
	Coverage string           `json:"coverage"` // "" when 3+ frequencies are well spread
	Slope    []string         `json:"slope"`    // notes on the slope B
	Pass     bool             `json:"pass"`     // nothing that should block the step
}

// sharedWithWWVH are the frequencies WWVH also transmits on.
var sharedWithWWVH = map[int]bool{2500: true, 5000: true, 10000: true, 15000: true}

// JudgeCal checks each WWV frequency and the fit as a whole. runMHz are the
// run frequencies the correction will be used at; scaleHz is the size of
// the errors to correct (for example the runs' typical SD/sqrt(N)), or 0.
func JudgeCal(res *fmtdata.CalResult, runMHz []float64, scaleHz float64, th Thresholds) CalQuality {
	q := CalQuality{Pass: res != nil && res.OK}
	if res == nil {
		return q
	}
	var usedMHz []float64
	for _, st := range res.Stations {
		if !st.IsRef || st.N == 0 {
			continue
		}
		s := StationQuality{KHz: st.KHz, Label: st.Label, N: st.N, SD: st.SD, Resid: st.Resid, Included: st.Included, Good: true}
		if st.N > 0 {
			s.SEM = st.SD / math.Sqrt(float64(st.N))
		}
		if st.N < th.CalMinReadings {
			s.Verdicts = append(s.Verdicts, VerdictTooFew)
			s.Notes = append(s.Notes, fmt.Sprintf("Only %d readings; at least %d are needed for a steady average. Stay on this frequency longer.", st.N, th.CalMinReadings))
		}
		if st.N > 1 && st.SD > th.CalNoisySD {
			s.Verdicts = append(s.Verdicts, VerdictTooNoisy)
			n := fmt.Sprintf("Too noisy: the readings scatter %.2f Hz (SD). Stay longer on this frequency so more fade cycles average out.", st.SD)
			if sharedWithWWVH[st.KHz] {
				n += " WWV and WWVH both transmit here; when both are heard, their different paths make the reading wander."
			}
			s.Notes = append(s.Notes, n)
		}
		if st.N > 1 && st.SD < th.CalSteadySD {
			s.Verdicts = append(s.Verdicts, VerdictSteady)
			n := fmt.Sprintf("Possibly not WWV, check the signal: the readings scatter only %.3f Hz, far steadier than a skywave path allows", st.SD)
			if res.NFit >= 2 && math.Abs(st.Resid) > 3*th.CalSteadySD {
				n += fmt.Sprintf(", and the reading sits %+.3f Hz off the line through the other frequencies", st.Resid)
			}
			s.Notes = append(s.Notes, n+". It may be a local spur or a different station. Leave it out unless you've confirmed it's WWV.")
		}
		s.Good = len(s.Verdicts) == 0
		if st.Included && s.Good {
			usedMHz = append(usedMHz, float64(st.KHz)/1000)
		}
		if st.Included && !s.Good && containsStr(s.Verdicts, VerdictSteady) {
			q.Pass = false
		}
		q.Stations = append(q.Stations, s)
	}

	// Coverage of the band.
	lo, hi := math.Inf(1), 0.0
	for _, f := range usedMHz {
		lo, hi = math.Min(lo, f), math.Max(hi, f)
	}
	switch {
	case len(usedMHz) == 0:
		q.Coverage = "No good WWV frequency in this calibration."
		q.Pass = false
	case len(usedMHz) < th.CalMinFreqs:
		q.Coverage = fmt.Sprintf("Only %d good WWV frequenc%s. Measure at least %d, spread from 2.5 to 20 MHz, so the slope can be measured and one bad frequency can be spotted.",
			len(usedMHz), plural(len(usedMHz), "y", "ies"), th.CalMinFreqs)
	case hi/lo < th.CalMinSpanRatio:
		q.Coverage = fmt.Sprintf("The good frequencies only span %.1f to %.1f MHz. Spread them from 2.5 to 20 MHz so the slope is measured over a wide range.", lo, hi)
	}

	// Is the slope measured, and does it matter at the run frequencies?
	if res.OK && res.NFit >= 3 && res.SigmaB > 0 {
		if math.Abs(res.B) < th.CalSlopeSigmas*res.SigmaB {
			q.Slope = append(q.Slope, fmt.Sprintf("The slope B (%+.5f ± %.5f ppm) can't be told apart from zero.", res.B, res.SigmaB))
		}
		fmax := 0.0
		for _, f := range runMHz {
			fmax = math.Max(fmax, f)
		}
		if fmax > 0 && scaleHz > 0 {
			if u := res.SigmaB * fmax; u >= scaleHz {
				q.Slope = append(q.Slope, fmt.Sprintf("At %.3f MHz the slope's uncertainty alone is ±%.3f Hz, as big as the run errors being corrected (about %.3f Hz). The correction may add as much error as it removes.", fmax, u, scaleHz))
			}
		}
	} else if res.OK && res.NFit == 2 {
		q.Slope = append(q.Slope, "With only two frequencies the slope has no uncertainty estimate.")
	}
	return q
}

// BracketHint encourages short WWV checks right before and after each run.
const BracketHint = "Tip: a short WWV check (one frequency near the run's band, about a minute) right before and after each run tracks the ionosphere and your radio far better than one calibration at the start and one at the end. With the interpolated correction, each bracket is used for the runs next to it."

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
