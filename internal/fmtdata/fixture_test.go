package fmtdata

import (
	"math"
	"path/filepath"
	"testing"
)

// Regression fixture: FMT 2026-11-13 at VA3MW, a FlexRadio locked to a
// GNSS-disciplined oscillator, WSJT-X FreqCal on its 30 s default.
// testdata/fmt2026-11/gen.py writes the files and says where every number
// comes from. Later tests (gates, capture length, fading, interpolated
// correction, analysis text) check against the same table.

type fixtureRun struct {
	File      string
	KeyDown   int     // UTC seconds of day
	DialHz    float64 // dial typed in
	SliceErr  float64 // Flex slice tuning error, Hz
	Published float64 // ARRL published frequency, Hz
	NUsed     int     // readings the night's analysis used

	// Expected results.
	ErrNoneHz  float64 // with no correction (published by the operator)
	ErrNonePPB float64
	SlopeHzMin float64 // slope of the readings used, Hz per minute
	SlopeSE    float64
	SD         float64
	ErrAvgHz   float64 // with the Average correction
	ErrIntHz   float64 // with the time-interpolated correction
}

var fixtureRuns = []fixtureRun{
	{"run1_W8RKO_40m.all", hmsT(2, 34, 0), 7062500.00, -0.0059, 7063903.300, 9,
		-0.087, -12.3, -0.32, 0.17, 0.058, -0.011, -0.022},
	{"run2_K5CM_40m.all", hmsT(2, 49, 0), 7063500.00, +0.0087, 7065124.320, 10,
		-0.095, -13.4, +0.23, 0.10, 0.044, -0.019, -0.010},
	{"run3_W8RKO_80m.all", hmsT(3, 4, 0), 3596500.00, -0.0018, 3598877.400, 9,
		-0.044, -12.2, -0.15, 0.15, 0.044, -0.016, -0.003},
	{"run4_K5CM_80m.all", hmsT(3, 19, 0), 3597500.00, +0.0191, 3599325.170, 10,
		-0.173, -48.1, +0.35, 0.39, 0.135, -0.144, -0.123},
}

// Calibration midpoints used by the time-interpolated correction.
var (
	fixtureCalBeforeMid = hmsT(1, 58, 50)
	fixtureCalAfterMid  = hmsT(3, 27, 20)
)

func hmsT(h, m, s int) int { return h*3600 + m*60 + s }

func fixturePath(name string) string { return filepath.Join("testdata", "fmt2026-11", name) }

func loadFixture(t *testing.T, name string) *File {
	t.Helper()
	f, err := ParseFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Bad) > 0 {
		t.Fatalf("%s: unreadable lines %+v", name, f.Bad)
	}
	return f
}

// fixtureCals analyses both calibrations the way the night's session did:
// the after calibration's 20 MHz station was left out of the fit.
func fixtureCals(t *testing.T) (before, after *CalResult) {
	t.Helper()
	before = AnalyzeCal(loadFixture(t, "cal_before.all"), CalOptions{MinSNR: 20})
	after = AnalyzeCal(loadFixture(t, "cal_after.all"), CalOptions{MinSNR: 20, Stations: map[int]bool{20000: false}})
	return
}

func fixtureRunResult(t *testing.T, r fixtureRun) *RunResult {
	t.Helper()
	return AnalyzeRun(loadFixture(t, r.File), RunOptions{MinSNR: 20, TrimEdges: true,
		KeyDownStart: r.KeyDown, KeyDownEnd: r.KeyDown + 60})
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.5f, want %.5f ± %g", what, got, want, tol)
	}
}

func TestFixtureCalibrations(t *testing.T) {
	before, after := fixtureCals(t)

	// Session summary: A +0.012 ± 0.051 Hz, B -0.00386 ± 0.00776 ppm.
	near(t, "before A", before.A, 0.012, 0.002)
	near(t, "before σA", before.SigmaA, 0.051, 0.002)
	near(t, "before B", before.B, -0.00386, 0.0002)
	near(t, "before σB", before.SigmaB, 0.00776, 0.0003)
	near(t, "before mid", float64(before.MidSec), float64(fixtureCalBeforeMid), 1)
	want := map[int]struct {
		n        int
		mean, sd float64
	}{2500: {13, 0.024, 0.020}, 5000: {48, -0.041, 0.058}, 10000: {24, -0.016, 0.310}}
	nonRef := 0
	for _, st := range before.Stations {
		if w, ok := want[st.KHz]; ok {
			if st.N != w.n {
				t.Errorf("before %d kHz n = %d, want %d", st.KHz, st.N, w.n)
			}
			near(t, "before mean DF", st.MeanDF, w.mean, 0.0006)
			near(t, "before SD", st.SD, w.sd, 0.001)
		} else {
			nonRef += st.Lines
		}
	}
	// Mostly not WWV: the 7.064 MHz readings outnumber the WWV ones.
	if nonRef*2 <= before.Total {
		t.Errorf("before: %d of %d readings are non-WWV; the fixture must be mostly non-WWV", nonRef, before.Total)
	}

	// Session summary: A +0.030 ± 0.021 Hz, B -0.02362 ± 0.00311 ppm.
	near(t, "after A", after.A, 0.030, 0.002)
	near(t, "after σA", after.SigmaA, 0.021, 0.002)
	near(t, "after B", after.B, -0.02362, 0.0002)
	near(t, "after σB", after.SigmaB, 0.00311, 0.0002)
	near(t, "after mid", float64(after.MidSec), float64(fixtureCalAfterMid), 1)
	for _, st := range after.Stations {
		if st.KHz == 20000 {
			if st.N != 12 {
				t.Errorf("after 20 MHz n = %d, want 12", st.N)
			}
			near(t, "after 20 MHz SD", st.SD, 0.003, 0.0006)
			near(t, "after 20 MHz residual", st.Resid, 0.438, 0.003)
		}
	}
}

func TestFixtureRuns(t *testing.T) {
	before, after := fixtureCals(t)
	var ss float64
	for i, r := range fixtureRuns {
		res := fixtureRunResult(t, r)
		if !res.OK || res.N != r.NUsed {
			t.Fatalf("run %d: ok=%v n=%d want %d; issues %+v", i+1, res.OK, res.N, r.NUsed, res.Issues)
		}
		near(t, "SD", res.SD, r.SD, 0.001)

		raw := r.DialHz + r.SliceErr + res.MeanFMeas
		errNone := raw - r.Published
		near(t, "error, no correction (Hz)", errNone, r.ErrNoneHz, 0.001)
		near(t, "error, no correction (ppb)", errNone/r.Published*1e9, r.ErrNonePPB, 0.15)

		fMHz := raw / 1e6
		avg := (before.Correction(fMHz) + after.Correction(fMHz)) / 2
		errAvg := errNone - avg
		near(t, "error, average correction", errAvg, r.ErrAvgHz, 0.001)
		ss += errAvg * errAvg
	}
	near(t, "RMS, average correction", math.Sqrt(ss/4), 0.073, 0.001)
}
