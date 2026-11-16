package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// IQ segments measured from the night's recording by cmd/iqfixture: 4 s
// segments over each whole key-down minute.

type fxIQRun struct {
	ID       string `json:"id"`
	Segments []struct {
		T       time.Time `json:"t"`
		AudioHz float64   `json:"audio_hz"`
		SNR     float64   `json:"snr"`
		OK      bool      `json:"ok"`
		In      bool      `json:"in"`
	} `json:"segments"`
}

func fxIQ(t *testing.T) [][]Point {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "fmtdata", "testdata", "fmt2026-11", "iq_segments.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runs []fxIQRun
	if err := json.Unmarshal(b, &runs); err != nil {
		t.Fatal(err)
	}
	var out [][]Point
	for _, r := range runs {
		var ps []Point
		for _, s := range r.Segments {
			if s.OK && s.In {
				u := s.T.UTC()
				ps = append(ps, Point{Sec: float64(u.Hour()*3600+u.Minute()*60+u.Second()) + float64(u.Nanosecond())/1e9,
					Hz: s.AudioHz, SNR: s.SNR, Source: "iq"})
			}
		}
		out = append(out, ps)
	}
	if len(out) != 4 {
		t.Fatalf("%d IQ runs, want 4", len(out))
	}
	return out
}

func TestFixtureIQCoverage(t *testing.T) {
	th := Defaults()
	iq := fxIQ(t)
	// Expected: IQ mean over the whole minute, and the shift from the
	// stretch WSJT-X caught to the whole minute.
	want := []struct{ full, shift float64 }{
		{1403.225, -0.012}, {1624.250, +0.030}, {2377.354, +0.048}, {1825.009, +0.019},
	}
	for i, r := range fxAll(t) {
		ps := iq[i]
		if len(ps) != 15 {
			t.Fatalf("run %d: %d IQ segments, want 15", i+1, len(ps))
		}
		// The IQ segments cover the whole minute, unlike WSJT-X.
		if span := ps[len(ps)-1].Sec - ps[0].Sec; span < 50 {
			t.Errorf("run %d: IQ spans %.0f s", i+1, span)
		}
		// The stretch WSJT-X caught: its strong readings in the minute, each
		// the end of a ~5 s analysis window.
		cc := RunCapture(r.file.Lines, r.fx.keyDown, 20, th)
		c, ok := CompareCoverage(ps, float64(cc.FirstSec-5), float64(cc.LastSec))
		if !ok {
			t.Fatalf("run %d: no coverage comparison", i+1)
		}
		approx(t, "IQ full-minute mean", c.FullMean, want[i].full, 0.001)
		approx(t, "shift to the whole minute", c.Shift, want[i].shift, 0.003)
		// Over the same stretch, IQ and WSJT-X agree within a few hundredths.
		approx(t, "IQ vs WSJT-X, captured stretch", c.CapturedMean, r.res.MeanFMeas, 0.06)
		if !strings.Contains(c.Msg, "whole key-down") {
			t.Errorf("run %d message: %s", i+1, c.Msg)
		}
	}
}

func TestFixtureIQAnalysis(t *testing.T) {
	th := Defaults()
	iq := fxIQ(t)
	var in []RunInput
	for i, r := range fxAll(t) {
		var m float64
		for _, p := range iq[i] {
			m += p.Hz
		}
		m /= float64(len(iq[i]))
		raw := r.fx.dial + r.fx.sliceErr + m
		e := raw - r.fx.published
		in = append(in, RunInput{N: i + 1, Title: r.fx.title, Points: iq[i], AudioHz: m, FreqHz: r.fx.published, ErrHz: &e})
	}
	an := Analyze(in, nil, th)
	want := []float64{-11.4, -8.7, -13.3, -39.5}
	for i, rn := range an.Runs {
		approx(t, "IQ error ppb", *rn.ErrPPB, want[i], 0.1)
	}
	// The whole minutes tell the same story: an offset shared by runs 1-3
	// and run 4 as the outlier.
	if an.Offset == nil || len(an.Outliers) != 1 || an.Outliers[0] != 4 {
		t.Fatalf("IQ pattern: offset %v outliers %v\n%s", an.Offset, an.Outliers, strings.Join(an.Pattern, "\n"))
	}
	approx(t, "IQ systematic offset", *an.Offset, -11.1, 0.15)

	// Fading on the IQ series: run 4 scatters most.
	var series []RunSeries
	for i := range iq {
		series = append(series, RunSeries{Points: iq[i]})
	}
	fs := DetectFading(series, th)
	for i, f := range fs {
		t.Logf("IQ run %d: SD %.3f ratio %.2f lag1 %.2f curvT %.2f suspect %v", i+1, f.SD, f.Ratio, f.Lag1, f.CurvT, f.Suspect)
	}
	// IQ segments are 4 s averages: run 4 still scatters most (over twice
	// the typical SD), but its segments don't move together, so the fading
	// gate stays on the WSJT-X readings (see TestFixtureFading).
	if fs[3].Ratio < 2 || fs[3].Suspect {
		t.Errorf("run 4 IQ: ratio %.2f suspect %v", fs[3].Ratio, fs[3].Suspect)
	}
}
