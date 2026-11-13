package main

import (
	"testing"

	"github.com/va3mw/FMTAssistant/internal/session"
)

func TestOutFileName(t *testing.T) {
	s := session.Default()
	if got := outFileName(s, "results", ".txt"); got != "FMT Nov 2026 results.txt" {
		t.Errorf("no callsign: %q", got)
	}
	s.Callsign = "VA3MW"
	if got := outFileName(s, "report", ".pdf"); got != "FMT Nov 2026 VA3MW report.pdf" {
		t.Errorf("with callsign: %q", got)
	}
	s.Callsign = "VA3MW/P"
	if got := outFileName(s, "results", ".txt"); got != "FMT Nov 2026 VA3MW_P results.txt" {
		t.Errorf("slash: %q", got)
	}
}
