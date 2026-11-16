package workflow

import (
	"fmt"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
)

// Correction methods. The operator picks one in Setup, before any run.
const (
	MethodNone         = "none"
	MethodBefore       = "before"
	MethodAfter        = "after"
	MethodAverage      = "average"
	MethodInterpolated = "interpolated"
)

// Methods lists every method in display order.
var Methods = []string{MethodNone, MethodBefore, MethodAfter, MethodAverage, MethodInterpolated}

// MethodNames are the operator-facing names.
var MethodNames = map[string]string{
	MethodNone:         "No correction",
	MethodBefore:       "Before",
	MethodAfter:        "After",
	MethodAverage:      "Average",
	MethodInterpolated: "Interpolated in time",
}

// MethodWhy explains each method for the Setup step.
var MethodWhy = map[string]string{
	MethodNone:         "Use the radio's reading as it is. Best when the radio is locked to GPS: a WWV correction then mostly adds WWV's own path Doppler.",
	MethodBefore:       "Correct with the WWV calibration made before the runs. Simple, but the ionosphere and the radio can change over an hour.",
	MethodAfter:        "Correct with the WWV calibration made after the runs. Same weakness as Before.",
	MethodAverage:      "Correct with the average of the before and after calibrations. Evens out a steady drift, but gives every run the same correction.",
	MethodInterpolated: "Correct each run with the calibrations on either side of it, weighted by how close in time they are. With short WWV brackets around each run, this follows changes through the evening best.",
}

// CalPoint is one usable calibration (the main ones or a bracket).
type CalPoint struct {
	ID     string  `json:"id"`
	MidSec int     `json:"mid_sec"` // UTC seconds of day, middle of the readings used
	A      float64 `json:"a"`
	B      float64 `json:"b"`
}

// At is the radio's error in Hz at fMHz.
func (c CalPoint) At(fMHz float64) float64 { return c.A + c.B*fMHz }

// PointFrom turns a calibration fit into a CalPoint (false if unusable).
func PointFrom(id string, r *fmtdata.CalResult) (CalPoint, bool) {
	if r == nil || !r.OK || fmtdata.HasErrors(r.Issues) {
		return CalPoint{}, false
	}
	return CalPoint{ID: id, MidSec: r.MidSec, A: r.A, B: r.B}, true
}

// Cals are the calibrations available to a correction.
type Cals struct {
	Before   *CalPoint  `json:"before"`
	After    *CalPoint  `json:"after"`
	Brackets []CalPoint `json:"brackets"`
}

// Correct works out the correction for a run measured around runSec at
// fMHz. It returns the correction in Hz, a plain description of how it was
// made, and an error message when the method can't be applied.
func Correct(method string, c Cals, runSec int, fMHz float64) (float64, string, string) {
	switch method {
	case MethodNone:
		return 0, "no correction", ""
	case MethodBefore:
		if c.Before == nil {
			return 0, "", "The calibration before the runs is missing or has errors."
		}
		return c.Before.At(fMHz), "before calibration", ""
	case MethodAfter:
		if c.After == nil {
			return 0, "", "The calibration after the runs is missing or has errors."
		}
		return c.After.At(fMHz), "after calibration", ""
	case MethodAverage:
		if c.Before == nil || c.After == nil {
			return 0, "", "Average needs both the before and the after calibration."
		}
		return (c.Before.At(fMHz) + c.After.At(fMHz)) / 2, "average of before and after", ""
	case MethodInterpolated:
		return interpolate(c, runSec, fMHz)
	}
	return 0, "", fmt.Sprintf("Unknown correction method %q.", method)
}

// interpolate is linear in time between the nearest calibration before
// the run and the nearest one after it (brackets included).
func interpolate(c Cals, runSec int, fMHz float64) (float64, string, string) {
	var all []CalPoint
	if c.Before != nil {
		all = append(all, *c.Before)
	}
	all = append(all, c.Brackets...)
	if c.After != nil {
		all = append(all, *c.After)
	}
	var prev, next *CalPoint
	for i := range all {
		p := &all[i]
		d := fmtdata.SecDiff(p.MidSec, runSec)
		if d <= 0 && (prev == nil || d > fmtdata.SecDiff(prev.MidSec, runSec)) {
			prev = p
		}
		if d >= 0 && (next == nil || d < fmtdata.SecDiff(next.MidSec, runSec)) {
			next = p
		}
	}
	switch {
	case prev == nil && next == nil:
		return 0, "", "Interpolation needs a calibration before and after the run."
	case prev == nil:
		return next.At(fMHz), fmt.Sprintf("only %s (no calibration before the run)", next.ID), ""
	case next == nil:
		return prev.At(fMHz), fmt.Sprintf("only %s (no calibration after the run)", prev.ID), ""
	case prev == next:
		return prev.At(fMHz), prev.ID, ""
	}
	span := float64(fmtdata.SecDiff(next.MidSec, prev.MidSec))
	w := float64(fmtdata.SecDiff(runSec, prev.MidSec)) / span
	v := prev.At(fMHz) + w*(next.At(fMHz)-prev.At(fMHz))
	return v, fmt.Sprintf("%.0f%% %s, %.0f%% %s", 100*(1-w), prev.ID, 100*w, next.ID), ""
}

// AllMethods is every method's correction, for the hindsight comparison
// shown after the answers are locked. Methods that can't be applied are
// left out.
func AllMethods(c Cals, runSec int, fMHz float64) map[string]float64 {
	out := map[string]float64{}
	for _, m := range Methods {
		if v, _, err := Correct(m, c, runSec, fMHz); err == "" {
			out[m] = v
		}
	}
	return out
}
