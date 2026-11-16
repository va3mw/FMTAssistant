package workflow

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
)

// The FMT 2026-11-13 regression fixture (see
// internal/fmtdata/testdata/fmt2026-11/gen.py for where every number comes from).

type fxRun struct {
	file      string
	title     string
	keyDown   int
	dial      float64
	sliceErr  float64
	published float64
	errPPB    float64 // uncorrected, from the published Hz errors
	slope     float64 // Hz/min
	slopeSE   float64
	errAvg    float64 // Hz, Average correction
	errInt    float64 // Hz, interpolated correction
}

var fxRuns = []fxRun{
	{"run1_W8RKO_40m.all", "W8RKO 40 m", hms(2, 34, 0), 7062500.00, -0.0059, 7063903.300, -12.3, -0.32, 0.17, -0.011, -0.022},
	{"run2_K5CM_40m.all", "K5CM 40 m", hms(2, 49, 0), 7063500.00, +0.0087, 7065124.320, -13.4, +0.23, 0.10, -0.019, -0.010},
	{"run3_W8RKO_80m.all", "W8RKO 80 m", hms(3, 4, 0), 3596500.00, -0.0018, 3598877.400, -12.2, -0.15, 0.15, -0.016, -0.003},
	{"run4_K5CM_80m.all", "K5CM 80 m", hms(3, 19, 0), 3597500.00, +0.0191, 3599325.170, -48.1, +0.35, 0.39, -0.144, -0.123},
}

func hms(h, m, s int) int { return h*3600 + m*60 + s }

func fxFile(t *testing.T, name string) *fmtdata.File {
	t.Helper()
	f, err := fmtdata.ParseFile(filepath.Join("..", "fmtdata", "testdata", "fmt2026-11", name))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func fxCals(t *testing.T) (before, after *fmtdata.CalResult) {
	t.Helper()
	before = fmtdata.AnalyzeCal(fxFile(t, "cal_before.all"), fmtdata.CalOptions{MinSNR: 20})
	after = fmtdata.AnalyzeCal(fxFile(t, "cal_after.all"), fmtdata.CalOptions{MinSNR: 20, Stations: map[int]bool{20000: false}})
	return
}

// fxRunData is one analysed run of the fixture.
type fxRunData struct {
	fx     fxRun
	file   *fmtdata.File
	res    *fmtdata.RunResult
	points []Point
	raw    float64 // dial + slice error + mean fMeas
}

func fxAll(t *testing.T) []fxRunData {
	t.Helper()
	var out []fxRunData
	for _, r := range fxRuns {
		f := fxFile(t, r.file)
		res := fmtdata.AnalyzeRun(f, fmtdata.RunOptions{MinSNR: 20, TrimEdges: true, KeyDownStart: r.keyDown, KeyDownEnd: r.keyDown + 60})
		var ps []Point
		for _, l := range res.Lines {
			if l.Used {
				ps = append(ps, Point{Sec: float64(l.Sec), Hz: l.FMeas, SNR: l.SNR, Source: "wsjtx"})
			}
		}
		out = append(out, fxRunData{r, f, res, ps, r.dial + r.sliceErr + res.MeanFMeas})
	}
	return out
}

func approx(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f ± %g", what, got, want, tol)
	}
}

func TestFixtureShortCapture(t *testing.T) {
	th := Defaults()
	for i, r := range fxAll(t) {
		c := RunCapture(r.file.Lines, r.fx.keyDown, 20, th)
		if !c.Short || !c.HalfCycle {
			t.Errorf("run %d: short=%v halfCycle=%v (%+v)", i+1, c.Short, c.HalfCycle, c)
		}
		if c.SpanSec < 20 || c.SpanSec > 26 {
			t.Errorf("run %d: span %.0f s, want about 25 s", i+1, c.SpanSec)
		}
		for _, want := range []string{"Only about", "30 second", "T/R cycle", "60 s or longer"} {
			if !strings.Contains(c.Msg, want) {
				t.Errorf("run %d: message lacks %q: %s", i+1, want, c.Msg)
			}
		}
		// Live: 40 s into the key-down, nothing has come in since :29.
		if LiveAlert(r.file.Lines, r.fx.keyDown, r.fx.keyDown+40, 20, th) == "" {
			t.Errorf("run %d: live check did not fire", i+1)
		}
		// Not yet decidable 20 s in.
		if LiveAlert(r.file.Lines, r.fx.keyDown, r.fx.keyDown+20, 20, th) != "" {
			t.Errorf("run %d: live check fired too early", i+1)
		}
	}

	// A full minute of readings is not short and raises no live alert.
	var full []fmtdata.Line
	kd := hms(2, 34, 0)
	for s := 4; s <= 58; s += 2 {
		full = append(full, fmtdata.Line{Sec: kd + s, KHz: 7064, FMeas: 1403.2, SNR: 40})
	}
	if c := RunCapture(full, kd, 20, th); c.Short {
		t.Errorf("full minute flagged short: %+v", c)
	}
	if m := LiveAlert(full, kd, kd+45, 20, th); m != "" {
		t.Errorf("full minute raised a live alert: %s", m)
	}
}

func TestFixtureCalDwellsShort(t *testing.T) {
	th := Defaults()
	for _, name := range []string{"cal_before.all", "cal_after.all"} {
		for _, d := range CalDwells(fxFile(t, name).Lines, 60, th) {
			if !d.Short || !strings.Contains(d.Msg, "T/R cycle") {
				t.Errorf("%s %d kHz: longest %.0f s not flagged short", name, d.KHz, d.Longest)
			}
		}
	}
}

func TestFixtureCalLoadGate(t *testing.T) {
	th := Defaults()
	runs := map[int]string{7064: "W8RKO 40 m", 7065: "K5CM 40 m", 3598: "W8RKO 80 m", 3599: "K5CM 80 m"}
	msg := CalLoadCheck(fxFile(t, "cal_before.all"), runs, th)
	for _, want := range []string{"7.064 MHz", "W8RKO 40 m", "isn't a calibration"} {
		if !strings.Contains(msg, want) {
			t.Errorf("before calibration not rejected properly (lacks %q): %q", want, msg)
		}
	}
	if msg := CalLoadCheck(fxFile(t, "cal_after.all"), runs, th); msg != "" {
		t.Errorf("after calibration rejected: %s", msg)
	}
}

func TestForeignReadingsGate(t *testing.T) {
	th := Defaults()
	all := fxAll(t)
	windows := []Window{}
	for i, r := range all {
		windows = append(windows, Window{ID: string(rune('1' + i)), Title: r.fx.title, From: r.fx.keyDown - 300, To: r.fx.keyDown + 60})
	}
	// Each run's own file is clean.
	for i, r := range all {
		if m := ForeignBlock(ForeignReadings(r.file.Lines, windows[i].ID, windows), th); m != "" {
			t.Errorf("run %d blocked: %s", i+1, m)
		}
	}
	// Run 1's capture with run 2's readings left in: blocked, naming run 2.
	mixed := append(append([]fmtdata.Line(nil), all[0].file.Lines...), all[1].file.Lines...)
	m := ForeignBlock(ForeignReadings(mixed, windows[0].ID, windows), th)
	if !strings.Contains(m, "K5CM 40 m") {
		t.Errorf("mixed capture not blocked with the right slot: %q", m)
	}
}

func TestFixtureCalQuality(t *testing.T) {
	th := Defaults()
	before, after := fxCals(t)
	runMHz := []float64{7.064, 7.065, 3.599, 3.599}

	qa := JudgeCal(after, runMHz, 0.05, th)
	verdicts := map[int][]string{}
	notes := map[int]string{}
	for _, s := range qa.Stations {
		verdicts[s.KHz] = s.Verdicts
		notes[s.KHz] = strings.Join(s.Notes, " ")
		if s.N > 0 {
			approx(t, "SD/sqrt(n)", s.SEM, s.SD/math.Sqrt(float64(s.N)), 1e-12)
		}
	}
	if !containsStr(verdicts[20000], VerdictSteady) || !strings.Contains(notes[20000], "Possibly not WWV, check the signal") ||
		!strings.Contains(notes[20000], "+0.43") {
		t.Errorf("20 MHz not flagged suspiciously steady: %v %q", verdicts[20000], notes[20000])
	}
	if !containsStr(verdicts[10000], VerdictTooNoisy) || !strings.Contains(notes[10000], "WWVH") || !strings.Contains(notes[10000], "longer") {
		t.Errorf("10 MHz not flagged noisy: %v %q", verdicts[10000], notes[10000])
	}
	if len(verdicts[2500])+len(verdicts[5000]) != 0 {
		t.Errorf("2.5/5 MHz flagged: %v %v", verdicts[2500], verdicts[5000])
	}

	qb := JudgeCal(before, runMHz, 0.05, th)
	if !strings.Contains(strings.Join(qb.Slope, " "), "can't be told apart from zero") {
		t.Errorf("before slope not reported as indistinguishable from zero: %v", qb.Slope)
	}
	if !strings.Contains(strings.Join(qb.Slope, " "), "as big as the run errors") {
		t.Errorf("before slope uncertainty at 7 MHz not compared with run errors: %v", qb.Slope)
	}
	// Before: 10 MHz is noisy, leaving 2.5 and 5 MHz: too few good frequencies.
	if !strings.Contains(qb.Coverage, "Only 2 good WWV frequencies") {
		t.Errorf("before coverage: %q", qb.Coverage)
	}
}

func TestFixtureSlopes(t *testing.T) {
	th := Defaults()
	for i, r := range fxAll(t) {
		s := FitSlope(r.points, th.SlopeConfidence)
		approx(t, "slope", s.HzPerMin, r.fx.slope, 0.006)
		approx(t, "slope SE", s.SE, r.fx.slopeSE, 0.006)
		if s.Drifting {
			t.Errorf("run %d counted as drifting: %+v", i+1, s)
		}
	}
	approx(t, "t(0.95, 8)", TCrit(0.95, 8), 2.306, 0.001)
	approx(t, "t(0.95, 7)", TCrit(0.95, 7), 2.365, 0.001)
}

func TestFixtureFading(t *testing.T) {
	th := Defaults()
	var series []RunSeries
	for _, r := range fxAll(t) {
		series = append(series, RunSeries{ID: r.fx.file, Title: r.fx.title, Points: r.points})
	}
	fs := DetectFading(series, th)
	for i, f := range fs {
		if (i == 3) != f.Suspect {
			t.Errorf("run %d: suspect=%v (SD %.3f ratio %.2f lag1 %.2f swing %v)", i+1, f.Suspect, f.SD, f.Ratio, f.Lag1, f.Swing)
		}
	}
	approx(t, "run 4 SD", fs[3].SD, 0.135, 0.001)
	approx(t, "run 4 typical SD", fs[3].Typical, 0.05, 0.01)
	approx(t, "run 4 lag-1", fs[3].Lag1, 0.33, 0.01)
	if !strings.Contains(fs[3].Msg, "Look at the plot") {
		t.Errorf("run 4 message: %s", fs[3].Msg)
	}
}

func TestExclusions(t *testing.T) {
	ps := []Point{{Sec: 10}, {Sec: 11}, {Sec: 13}, {Sec: 16}}
	ex := []Exclusion{{FromSec: 10.5, ToSec: 13.5, Rule: RuleFadeDip}}
	kept, dropped := ApplyExclusions(ps, ex)
	if len(kept) != 2 || len(dropped) != 2 {
		t.Fatalf("kept %v dropped %v", kept, dropped)
	}
	if (Exclusion{FromSec: 1, ToSec: 2}).Validate() == nil {
		t.Error("an exclusion without a rule was accepted")
	}
	if (Exclusion{FromSec: 1, ToSec: 2, Rule: RuleOther}).Validate() == nil {
		t.Error("an \"other\" exclusion without a note was accepted")
	}
	if err := (Exclusion{FromSec: 1, ToSec: 2, Rule: RuleLowSNR}).Validate(); err != nil {
		t.Error(err)
	}
}

func fxCalPoints(t *testing.T) Cals {
	before, after := fxCals(t)
	b, ok1 := PointFrom("before", before)
	a, ok2 := PointFrom("after", after)
	if !ok1 || !ok2 {
		t.Fatal("fixture calibrations unusable")
	}
	approx(t, "before midpoint", float64(b.MidSec), float64(hms(1, 58, 50)), 1)
	approx(t, "after midpoint", float64(a.MidSec), float64(hms(3, 27, 20)), 1)
	return Cals{Before: &b, After: &a}
}

func TestFixtureCorrections(t *testing.T) {
	cals := fxCalPoints(t)
	errs := map[string][]float64{}
	for i, r := range fxAll(t) {
		fMHz := r.raw / 1e6
		all := AllMethods(cals, r.res.MidSec, fMHz)
		for _, m := range Methods {
			if _, ok := all[m]; !ok {
				t.Fatalf("run %d: method %s missing", i+1, m)
			}
			errs[m] = append(errs[m], r.raw-all[m]-r.fx.published)
		}
		approx(t, "average error", errs[MethodAverage][i], r.fx.errAvg, 0.001)
		approx(t, "interpolated error", errs[MethodInterpolated][i], r.fx.errInt, 0.002)
		_, basis, msg := Correct(MethodInterpolated, cals, r.res.MidSec, fMHz)
		if msg != "" || !strings.Contains(basis, "before") || !strings.Contains(basis, "after") {
			t.Errorf("run %d interpolation basis %q %q", i+1, basis, msg)
		}
	}
	approx(t, "average RMS", RMS(errs[MethodAverage]), 0.073, 0.001)
	approx(t, "average RMS without run 4", RMS(errs[MethodAverage][:3]), 0.016, 0.001)

	h := strings.Join(Hindsight(MethodAverage, errs, []int{4}), "\n")
	for _, want := range []string{"Hindsight", "stay as submitted", "Average: RMS error 0.073 Hz, 0.016 Hz without the outlier (your choice)"} {
		if !strings.Contains(h, want) {
			t.Errorf("hindsight lacks %q:\n%s", want, h)
		}
	}
	if strings.Contains(strings.ToLower(h), "resubmit") {
		t.Errorf("hindsight offers to resubmit:\n%s", h)
	}

	// Interpolation falls back to one side when only one calibration exists.
	v, basis, msg := Correct(MethodInterpolated, Cals{Before: cals.Before}, hms(2, 34, 18), 7.0)
	if msg != "" || !strings.Contains(basis, "no calibration after") || v != cals.Before.At(7.0) {
		t.Errorf("one-sided interpolation: %v %q %q", v, basis, msg)
	}
}

func TestFixtureAnalysisText(t *testing.T) {
	th := Defaults()
	var series []RunSeries
	all := fxAll(t)
	for _, r := range all {
		series = append(series, RunSeries{ID: r.fx.file, Points: r.points})
	}
	fading := DetectFading(series, th)
	var in []RunInput
	for i, r := range all {
		e := r.raw - r.fx.published // no correction: the station's GNSS-locked answer
		c := RunCapture(r.file.Lines, r.fx.keyDown, 20, th)
		in = append(in, RunInput{N: i + 1, Title: r.fx.title, Points: r.points, AudioHz: r.res.MeanFMeas,
			FreqHz: r.fx.published, ErrHz: &e, Capture: &c, Fading: &fading[i]})
	}
	an := Analyze(in, nil, th)

	for i, rn := range an.Runs {
		approx(t, "error ppb", *rn.ErrPPB, fxRuns[i].errPPB, 0.15)
		text := strings.Join(rn.Lines, " ")
		if !strings.Contains(text, "half the available data") || !strings.Contains(text, "first part of the carrier") {
			t.Errorf("run %d: short capture not mentioned: %s", i+1, text)
		}
		if strings.Contains(text, "Drifting") {
			t.Errorf("run %d called drifting: %s", i+1, text)
		}
		if !strings.Contains(text, "± ") {
			t.Errorf("run %d: slope without its standard error: %s", i+1, text)
		}
	}
	// Run 4: autocorrelated, so effective N and a wider uncertainty.
	r4 := an.Runs[3]
	approx(t, "run 4 effective N", r4.NEff, 5.0, 0.2)
	if r4.SEMEff <= r4.SEM || !strings.Contains(strings.Join(r4.Lines, " "), "independent ones") {
		t.Errorf("run 4 effective N not reported: %+v", r4)
	}

	if an.Offset == nil {
		t.Fatalf("no systematic offset found: %v", an.Pattern)
	}
	approx(t, "systematic offset", *an.Offset, -12.65, 0.2)
	if len(an.Outliers) != 1 || an.Outliers[0] != 4 {
		t.Errorf("outliers %v, want [4]", an.Outliers)
	}
	p := strings.Join(an.Pattern, "\n")
	for _, want := range []string{
		"Systematic offset of -12.7 ppb, plus one outlier (run 4)",
		"proportional to frequency", "reflecting layer", "Only the reference check",
		"doesn't line up, so the sound card's clock isn't the cause",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("pattern lacks %q:\n%s", want, p)
		}
	}
	// The old conclusion: one spread dominated by run 4, read as propagation.
	for _, bad := range []string{"spread 17", "rather than the frequency reference"} {
		if strings.Contains(p, bad) {
			t.Errorf("pattern still says %q:\n%s", bad, p)
		}
	}

	// With a reference check near 0 ppb, the offset is put down to the ionosphere.
	an = Analyze(in, &RefCheck{PPB: 0.4, What: "a GPS-locked 10 MHz carrier"}, th)
	if p := strings.Join(an.Pattern, "\n"); !strings.Contains(p, "reference is fine") {
		t.Errorf("reference check not used:\n%s", p)
	}
	// Before the answers are locked there is no error and no pattern.
	for i := range in {
		in[i].ErrHz = nil
	}
	an = Analyze(in, nil, th)
	if len(an.Pattern) != 0 || an.Runs[0].ErrHz != nil {
		t.Errorf("analysis used published values before lock: %+v", an)
	}
}
