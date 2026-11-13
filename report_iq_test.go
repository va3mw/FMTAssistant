package main

import (
	"math"
	"testing"

	"github.com/va3mw/FMTAssistant/internal/iq"
)

func iqView(hz, sem float64, n int) *IQAnalysisView {
	return &IQAnalysisView{A: &iq.Analysis{Summary: &iq.Summary{N: n, SEM: sem, MeanRFHz: hz}}, IQFinalHz: &hz}
}

func TestRecommend(t *testing.T) {
	// No IQ: the WSJT-X answer.
	if rc := recommend(3599324.997, 0.043, nil); rc.HasIQ || rc.Hz != 3599324.997 {
		t.Fatalf("no IQ: %+v", rc)
	}
	// K5CM 80 m on the night: agree, weighted average between the two.
	rc := recommend(3599324.997, 0.043, iqView(3599324.945, 0.0464, 12))
	if !rc.Agree || rc.Hz <= 3599324.945 || rc.Hz >= 3599324.997 {
		t.Fatalf("agree: %+v", rc)
	}
	ww, wi := 1/(0.043*0.043), 1/(0.0464*0.0464)
	if want := (3599324.997*ww + 3599324.945*wi) / (ww + wi); math.Abs(rc.Hz-want) > 1e-9 {
		t.Fatalf("weighted %.6f want %.6f", rc.Hz, want)
	}
	// Disagree by 0.3 Hz: WSJT-X, flagged.
	rc = recommend(7065124.225, 0.014, iqView(7065124.528, 0.010, 13))
	if rc.Agree || rc.Hz != 7065124.225 || rc.Warning == "" {
		t.Fatalf("disagree: %+v", rc)
	}
	// Tiny SEMs are floored, so a 0.04 Hz difference still agrees (0.05 Hz floor).
	if rc = recommend(100.00, 0.001, iqView(100.04, 0.001, 13)); !rc.Agree || rc.WSEM != semFloor {
		t.Fatalf("floor: %+v", rc)
	}
	// Too few IQ segments: WSJT-X only.
	if rc = recommend(100, 0.02, iqView(100.01, 0.01, 2)); rc.HasIQ {
		t.Fatalf("few segments: %+v", rc)
	}
}
