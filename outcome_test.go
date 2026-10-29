package main

import (
	"math"
	"strings"
	"testing"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
)

func ptr(v float64) *float64 { return &v }

func TestNewOutcome(t *testing.T) {
	// Raw 7062500.30, before 0.10, after 0.20: average answer 7062500.15.
	o := newOutcome(7062500.30, 7062500.15, 7062500.00, ptr(0.10), ptr(0.20))
	if math.Abs(o.ErrHz-0.15) > 1e-6 || math.Abs(o.ErrPPB-0.15/7062500*1e9) > 1e-6 {
		t.Errorf("error %+v", o)
	}
	want := map[string]float64{"none": 0.30, "before": 0.20, "after": 0.10, "average": 0.15}
	for m, v := range want {
		if math.Abs(o.ByMode[m]-v) > 1e-6 {
			t.Errorf("%s: got %.4f, want %.4f", m, o.ByMode[m], v)
		}
	}
	o = newOutcome(7062500.30, 7062500.20, 7062500.00, ptr(0.10), nil)
	if _, ok := o.ByMode["average"]; ok || len(o.ByMode) != 2 {
		t.Errorf("only none and before should be there: %v", o.ByMode)
	}
}

func TestRunDrift(t *testing.T) {
	res := &fmtdata.RunResult{}
	for i := 0; i < 10; i++ {
		l := fmtdata.RunLine{Used: i != 9}
		l.Sec, l.FMeas = 7200+6*i, 1500+0.001*float64(6*i) // 0.06 Hz/min
		if i == 9 {
			l.FMeas = 1600 // not used, so ignored
		}
		res.Lines = append(res.Lines, l)
	}
	d, ok := runDrift(res)
	if !ok || math.Abs(d-0.06) > 1e-9 {
		t.Errorf("drift %v %v", d, ok)
	}
	if _, ok := runDrift(&fmtdata.RunResult{}); ok {
		t.Error("no readings should give no drift")
	}
}

func TestOutcomeNotes(t *testing.T) {
	run := func(title string, khz, errHz, sem float64, before, after float64) RunView {
		pub := khz * 1000
		raw := pub + errHz + (before+after)/2
		return RunView{Title: title, Ready: true, Result: &fmtdata.RunResult{SEM: sem},
			Outcome: newOutcome(raw, raw-(before+after)/2, pub, ptr(before), ptr(after))}
	}
	has := func(notes []string, s string) bool {
		for _, n := range notes {
			if strings.Contains(n, s) {
				return true
			}
		}
		return false
	}

	// Same fraction on every band: the reference.
	notes := outcomeNotes([]RunView{run("A 80 m", 3580, 0.035, 0.002, 0, 0), run("B 40 m", 7060, 0.070, 0.002, 0, 0)}, "average")
	if !has(notes, "same direction") || !has(notes, "times the measurement's own scatter") {
		t.Errorf("shared error not explained: %q", notes)
	}

	// Different errors, both signs: propagation.
	notes = outcomeNotes([]RunView{run("A 80 m", 3580, 0.030, 0.01, 0, 0), run("B 40 m", 7060, -0.060, 0.01, 0, 0)}, "average")
	if !has(notes, "different amounts") || !has(notes, "both directions") {
		t.Errorf("spread not explained: %q", notes)
	}

	// Tiny errors on steady carriers.
	notes = outcomeNotes([]RunView{run("A 80 m", 3580, 0.007, 0.002, 0, 0), run("B 40 m", 7060, -0.01, 0.002, 0, 0)}, "average")
	if !has(notes, "within a few ppb") {
		t.Errorf("good result not explained: %q", notes)
	}

	// Errors inside each run's own noise can't be told apart.
	notes = outcomeNotes([]RunView{run("A 40 m", 7060, -0.016, 0.128, 0, 0), run("B 40 m", 7060, 0.016, 0.015, 0, 0)}, "average")
	if !has(notes, "can't tell a reference error from propagation") || has(notes, "different amounts") {
		t.Errorf("noise not explained: %q", notes)
	}

	// A WWV correction of 0.05 Hz on a radio that needed none: No correction
	// would have been closest.
	notes = outcomeNotes([]RunView{run("A 40 m", 7060, -0.05, 0.002, 0.05, 0.05)}, "average")
	if !has(notes, "In hindsight, No correction") {
		t.Errorf("hindsight not given: %q", notes)
	}
	// A blunder is called one, and kept out of the patterns.
	notes = outcomeNotes([]RunView{run("A 40 m", 7060, 1488, 0.01, 0, 0), run("B 40 m", 7060, 0.002, 0.01, 0, 0)}, "average")
	if !has(notes, "far more than propagation") || has(notes, "different amounts") || has(notes, "same direction") {
		t.Errorf("blunder not handled: %q", notes)
	}
	if outcomeNotes([]RunView{{Title: "x"}}, "average") != nil {
		t.Error("no published frequencies should give no notes")
	}
}
