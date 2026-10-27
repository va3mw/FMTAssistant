package fmtdata

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// line formats a measurement exactly like freqcal.f90:
// (i2.2,':',i2.2,':',i2.2,i7,i3,i6,2f10.3,2f7.1,2x,a1)
func line(sec, khz, offset int, fmeas, snr float64) string {
	flag := " "
	if snr < 20 {
		flag = "*"
	}
	return fmt.Sprintf("%02d:%02d:%02d%7d%3d%6d%10.3f%10.3f%7.1f%7.1f  %s",
		sec/3600, (sec/60)%60, sec%60, khz, 1, offset, fmeas, fmeas-float64(offset), -30.0, snr, flag)
}

func TestParseLine(t *testing.T) {
	l, ok := ParseLine(line(2*3600+34*60+5, 7064, 1500, 1503.271, 35.2))
	if !ok {
		t.Fatal("not parsed")
	}
	if l.Time != "02:34:05" || l.KHz != 7064 || l.Offset != 1500 || math.Abs(l.FMeas-1503.271) > 1e-9 || l.Flag {
		t.Fatalf("bad parse %+v", l)
	}
	l, ok = ParseLine(line(100, 10000, 1500, 1499.9, 12.0))
	if !ok || !l.Flag {
		t.Fatalf("flag not seen %+v", l)
	}
	if _, ok := ParseLine("02:34:05  7064  1  1500   -99.900   -99.900  -99.9  -99.9  *"); ok {
		t.Fatal("no-peak line should be rejected")
	}
}

func TestCalibrationFit(t *testing.T) {
	// Radio with A = 0.5 Hz, B = -0.1 ppm; one broadcast station 2 Hz off.
	var b strings.Builder
	sec := 2*3600 + 10*60
	for _, khz := range []int{2500, 5000, 10000, 15000, 1010} {
		for i := 0; i < 6; i++ {
			df := 0.5 - 0.1*float64(khz)/1000
			if khz == 1010 {
				df = 2
			}
			jit := []float64{0.05, -0.05, 0.02, -0.02, 0.0, 0.0}[i]
			snr := 30.0
			if i == 5 && khz == 15000 {
				snr, jit = 10, 3 // flagged junk must be ignored
			}
			b.WriteString(line(sec, khz, 1500, 1500+df+jit, snr) + "\n")
			sec += 6
		}
	}
	f, _ := Parse(strings.NewReader(b.String()))
	r := AnalyzeCal(f, CalOptions{MinSNR: 20})
	if !r.OK || r.NFit != 4 {
		t.Fatalf("fit failed: %+v", r)
	}
	if math.Abs(r.A-0.5) > 0.02 || math.Abs(r.B+0.1) > 0.005 {
		t.Fatalf("A=%v B=%v", r.A, r.B)
	}
	// Including the broadcast station must be a deliberate choice.
	r2 := AnalyzeCal(f, CalOptions{MinSNR: 20, Stations: map[int]bool{1010: true}})
	if r2.NFit != 5 {
		t.Fatalf("override ignored: %d", r2.NFit)
	}
}

func TestRunFindsCarrier(t *testing.T) {
	var b strings.Builder
	sec := 2*3600 + 29*60
	// Five minutes of CW ID: alternating weak/strong.
	for i := 0; i < 50; i++ {
		snr := 15.0
		if i%3 == 0 {
			snr = 25
		}
		v := 1503.0 + float64(i%5)*0.3
		if i >= 47 { // strong CW right up to key-down must not count
			snr, v = 30, 1503.6
		}
		b.WriteString(line(sec, 7064, 1500, v, snr) + "\n")
		sec += 6
	}
	// Key-down: 10 strong readings at 1503.27, first and last slightly off.
	for i := 0; i < 10; i++ {
		v := 1503.27
		if i == 0 || i == 9 {
			v = 1503.9
		}
		b.WriteString(line(sec, 7064, 1500, v, 38) + "\n")
		sec += 6
	}
	for i := 0; i < 5; i++ {
		b.WriteString(line(sec, 7064, 1500, 1480, 5) + "\n")
		sec += 6
	}
	f, _ := Parse(strings.NewReader(b.String()))
	ks := 2*3600 + 34*60
	r := AnalyzeRun(f, RunOptions{MinSNR: 20, TrimEdges: true, KeyDownStart: ks, KeyDownEnd: ks + 60})
	if !r.OK || r.N != 8 || math.Abs(r.MeanFMeas-1503.27) > 1e-6 {
		t.Fatalf("run: ok=%v n=%d mean=%v issues=%v", r.OK, r.N, r.MeanFMeas, r.Issues)
	}
	if r.DialMin != 7062500 || r.DialMax != 7063499 {
		t.Fatalf("dial range %d..%d", r.DialMin, r.DialMax)
	}
}

func TestRunRejectsCalibrationFile(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		b.WriteString(line(3600+i*6, 10000, 1500, 1500.1, 40) + "\n")
	}
	f, _ := Parse(strings.NewReader(b.String()))
	r := AnalyzeRun(f, RunOptions{MinSNR: 20, TrimEdges: true, KeyDownStart: -1, KeyDownEnd: -1})
	if r.OK || !HasErrors(r.Issues) {
		t.Fatal("calibration file accepted as a run")
	}
}
