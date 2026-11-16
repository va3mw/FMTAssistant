package workflow

import (
	"fmt"
	"math"
	"strings"
)

// RunSeries is the readings a run uses, for comparing runs.
type RunSeries struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Points []Point `json:"points"`
}

// Fading is the fading and multipath check for one run.
type Fading struct {
	ID      string  `json:"id"`
	SD      float64 `json:"sd"`
	Typical float64 `json:"typical"` // the other runs' median SD (0 if too few)
	Ratio   float64 `json:"ratio"`   // SD / Typical
	Lag1    float64 `json:"lag1"`
	CurvT   float64 `json:"curv_t"` // t statistic of a smooth swing or dip
	Swing   bool    `json:"swing"`
	Suspect bool    `json:"suspect"` // review required
	Msg     string  `json:"msg"`
}

// DetectFading compares each run's scatter with the session's typical
// scatter and looks for readings that move together. A run is a suspect
// when its SD is well above typical AND neighbouring readings move
// together or follow a smooth swing; noise alone doesn't do that.
func DetectFading(runs []RunSeries, th Thresholds) []Fading {
	sds := make([]float64, len(runs))
	for i, r := range runs {
		_, sds[i] = meanSD(values(r.Points))
	}
	out := make([]Fading, len(runs))
	for i, r := range runs {
		f := Fading{ID: r.ID, SD: sds[i], Lag1: Lag1(values(r.Points))}
		var others []float64
		for j := range runs {
			if j != i && len(runs[j].Points) >= 3 {
				others = append(others, sds[j])
			}
		}
		high := false
		if len(others) >= 2 {
			f.Typical = median(others)
			if f.Typical > 0 {
				f.Ratio = f.SD / f.Typical
			}
			high = f.Ratio >= th.FadingSDRatio
		} else {
			high = f.SD > th.FadingSDHz
		}
		if n := len(r.Points); n >= 5 {
			f.CurvT = curvature(r.Points)
			f.Swing = math.Abs(f.CurvT) > TCrit(th.SlopeConfidence, n-3)
		}
		f.Suspect = high && (f.Lag1 >= th.FadingLag1 || f.Swing)
		if f.Suspect {
			var why []string
			if f.Typical > 0 {
				why = append(why, fmt.Sprintf("the readings scatter %.3f Hz, %.1f times the other runs' %.3f Hz", f.SD, f.Ratio, f.Typical))
			} else {
				why = append(why, fmt.Sprintf("the readings scatter %.3f Hz", f.SD))
			}
			if f.Lag1 >= th.FadingLag1 {
				why = append(why, fmt.Sprintf("neighbouring readings move together (lag-1 autocorrelation %.2f)", f.Lag1))
			}
			if f.Swing {
				why = append(why, "they follow a smooth swing or dip")
			}
			f.Msg = "Fading or multipath likely: " + strings.Join(why, ", and ") +
				". Look at the plot and drag across any stretch that is a fade dip or a weak patch to leave it out, or confirm that every reading should be used."
		}
		out[i] = f
	}
	return out
}

// Exclusion rules the operator picks from.
const (
	RuleFadeDip = "fade_dip"
	RuleLowSNR  = "low_snr"
	RuleEdge    = "carrier_edge"
	RuleOther   = "other"
	RuleLegacy  = "legacy" // a per-reading choice carried over from an older session
)

// RuleNames are shown in the picker and the report.
var RuleNames = map[string]string{
	RuleFadeDip: "Dip during fade",
	RuleLowSNR:  "Low S/N",
	RuleEdge:    "Carrier edge",
	RuleOther:   "Other",
	RuleLegacy:  "Other (from an older session)",
}

// Exclusion is a stretch of a run the operator left out.
type Exclusion struct {
	FromSec float64 `json:"from_sec"` // UTC seconds of day
	ToSec   float64 `json:"to_sec"`
	Rule    string  `json:"rule"`
	Note    string  `json:"note,omitempty"` // required for "other"
	At      string  `json:"at"`             // UTC time it was made, RFC 3339
}

// Validate checks an exclusion before it is saved.
func (e Exclusion) Validate() error {
	if _, ok := RuleNames[e.Rule]; !ok || e.Rule == RuleLegacy {
		return fmt.Errorf("pick a reason for leaving these readings out")
	}
	if e.Rule == RuleOther && strings.TrimSpace(e.Note) == "" {
		return fmt.Errorf("say briefly why these readings are left out")
	}
	if e.ToSec < e.FromSec {
		return fmt.Errorf("the stretch ends before it starts")
	}
	return nil
}

// Covers says whether a reading at sec falls in the excluded stretch.
func (e Exclusion) Covers(sec float64) bool { return sec >= e.FromSec && sec <= e.ToSec }

// ApplyExclusions splits points into those kept and those excluded.
func ApplyExclusions(ps []Point, ex []Exclusion) (kept, dropped []Point) {
	for _, p := range ps {
		out := false
		for _, e := range ex {
			if e.Covers(p.Sec) {
				out = true
				break
			}
		}
		if out {
			dropped = append(dropped, p)
		} else {
			kept = append(kept, p)
		}
	}
	return
}
