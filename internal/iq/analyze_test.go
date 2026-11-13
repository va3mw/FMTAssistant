package iq

import (
	"math"
	"math/cmplx"
	"math/rand"
	"path/filepath"
	"testing"
	"time"
)

// synth writes a recording of a carrier at rfHz with noise. If recentre is
// set, the centre moves by that much halfway through.
func synth(t *testing.T, rfHz float64, mirror bool, recentre int64) (string, *Meta) {
	t.Helper()
	const rate, secs = 24000, 40
	center := int64(7062500)
	path := filepath.Join(t.TempDir(), "x.wav")
	start := time.Date(2026, 11, 13, 2, 33, 40, 0, time.UTC)
	w, err := CreateWAV(path, start, rate, center)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	meta := &Meta{CenterHz: center, DialHz: 7062500, StartUTC: start, SampleRate: rate}
	buf := make([]float32, 0, 2*rate)
	ph := 0.3
	c := center
	for i := 0; i < rate*secs; i++ {
		if recentre != 0 && i == rate*secs/2+12345 {
			c += recentre
			meta.Events = append(meta.Events, Event{Frame: int64(i), What: "center", Hz: c})
			ph += 1.1 // the radio's retune breaks phase continuity
		}
		ph += 2 * math.Pi * (rfHz - float64(c)) / rate
		v := 0.1*cmplx.Exp(complex(0, ph)) + complex(rng.NormFloat64()*0.02, rng.NormFloat64()*0.02)
		if mirror {
			v = cmplx.Conj(v)
		}
		buf = append(buf, float32(real(v)), float32(imag(v)))
		if len(buf) == cap(buf) {
			w.Write(buf)
			buf = buf[:0]
		}
	}
	w.Write(buf)
	if err := w.Close(rate, center, start.Add(secs*time.Second)); err != nil {
		t.Fatal(err)
	}
	return path, meta
}

func TestAnalyzeCarrier(t *testing.T) {
	const rf = 7064000.137
	for _, c := range []struct {
		name     string
		mirror   bool
		recentre int64
	}{{"plain", false, 0}, {"mirrored", true, 0}, {"recentred", false, 700}} {
		t.Run(c.name, func(t *testing.T) {
			path, meta := synth(t, rf, c.mirror, c.recentre)
			a, err := Analyze(path, meta, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if a.Mirrored != c.mirror {
				t.Fatalf("mirrored %v, notes %v", a.Mirrored, a.Notes)
			}
			if a.Summary == nil {
				t.Fatalf("no summary: %+v", a.Segments)
			}
			if d := a.Summary.MeanRFHz - rf; math.Abs(d) > 0.002 {
				t.Fatalf("mean %.4f Hz, off by %.4f; segs %+v", a.Summary.MeanRFHz, d, a.Segments)
			}
			if d := a.Summary.MeanAudio - 1500.137; math.Abs(d) > 0.002 {
				t.Fatalf("audio %.4f", a.Summary.MeanAudio)
			}
			bad := 0
			for _, s := range a.Segments {
				if !s.OK {
					bad++
				}
			}
			if c.recentre != 0 && bad != 1 || c.recentre == 0 && bad != 0 {
				t.Fatalf("%d segments left out: %+v", bad, a.Segments)
			}
			if len(a.Spectrum) == 0 {
				t.Fatal("no spectrum")
			}
		})
	}
}

func TestAnalyzeWindow(t *testing.T) {
	path, meta := synth(t, 7064000.5, false, 0)
	kd := meta.StartUTC.Add(20 * time.Second)
	a, err := Analyze(path, meta, Options{From: kd, To: kd.Add(10 * time.Second)})
	if err != nil || a.Summary == nil {
		t.Fatal(err)
	}
	if a.Summary.N < 2 || a.Summary.N > 3 || a.Summary.Basis != "segments inside the key-down minute" {
		t.Fatalf("summary %+v", a.Summary)
	}
}

func TestReadWAVRoundTrip(t *testing.T) {
	path, _ := synth(t, 7064000, false, 0)
	w, err := OpenWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Rate != 24000 || w.Frames != 24000*40 || w.CenterHz != 7062500 || w.Start.Hour() != 2 || !w.Float {
		t.Fatalf("%+v", w)
	}
}

// A run measured from inside a longer recording: only its window is read,
// and the centre in effect there is used.
func TestAnalyzeOnlyWindow(t *testing.T) {
	const rf = 7064000.137
	path, meta := synth(t, rf, false, 700) // re-centred 32.3 s in
	from := meta.StartUTC.Add(34 * time.Second)
	a, err := Analyze(path, meta, Options{ExpectHz: rf, SearchHz: 20, From: from, To: from.Add(5 * time.Second), OnlyWindow: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.CenterHz != 7062500+700 {
		t.Fatalf("centre %d", a.CenterHz)
	}
	if len(a.Segments) > 4 || a.Summary == nil || math.Abs(a.Summary.MeanRFHz-rf) > 0.002 {
		t.Fatalf("segs %d summary %+v", len(a.Segments), a.Summary)
	}
	if a.Segments[0].Sec < 20 {
		t.Fatalf("read from the start: first segment at %.1f s", a.Segments[0].Sec)
	}
	if _, err := Analyze(path, meta, Options{ExpectHz: rf, From: from.Add(time.Hour), To: from.Add(time.Hour + time.Minute), OnlyWindow: true}); err == nil {
		t.Fatal("window outside the recording accepted")
	}
}
