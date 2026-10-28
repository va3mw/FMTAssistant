package main

import (
	"strings"
	"testing"
	"time"

	"github.com/va3mw/FMTAssistant/internal/flex"
	"github.com/va3mw/FMTAssistant/internal/fmtdata"
	"github.com/va3mw/FMTAssistant/internal/session"
)

func TestRefFromRadio(t *testing.T) {
	cases := []struct {
		fr     flex.Reference
		ext    bool
		kind   string
		locked bool
	}{
		{flex.Reference{Known: true, State: "tcxo", Locked: false}, false, session.RefInternal, true},
		{flex.Reference{Known: true, State: "gpsdo", Locked: true, GPSFreqError: "0.12"}, false, session.RefGPS, true},
		{flex.Reference{Known: true, State: "gpsdo", Locked: false}, false, session.RefGPS, false},
		{flex.Reference{Known: true, State: "external", Locked: true}, false, session.RefExternal, true},
		{flex.Reference{Known: true, State: "external", Locked: true}, true, session.RefGPS, true},
	}
	for _, c := range cases {
		got := refFromRadio(c.fr, c.ext)
		if got.Kind != c.kind || got.Locked != c.locked || got.From != "radio" {
			t.Errorf("%+v ext=%v: got %+v", c.fr, c.ext, got)
		}
	}
	if d := refFromRadio(flex.Reference{State: "gpsdo", Locked: true, GPSFreqError: "0.12"}, false).Detail; !strings.Contains(d, "freq_error 0.12") {
		t.Errorf("detail %q", d)
	}
}

func TestRefOver(t *testing.T) {
	day := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	gps := flex.Reference{Known: true, State: "gpsdo", Locked: true}
	unl := flex.Reference{Known: true, State: "gpsdo", Locked: false}
	tcxo := flex.Reference{Known: true, State: "tcxo"}
	ext := flex.Reference{Known: true, State: "external", Locked: true}
	setup := session.RefRecord{Kind: session.RefInternal, Locked: true, From: "setup", Detail: "setup"}
	disc := false
	classify := func(fr flex.Reference) session.RefRecord {
		if !fr.Known {
			return setup
		}
		return refFromRadio(fr, disc)
	}
	log := []refEntry{{at(2, 0), gps}, {at(2, 50), unl}, {at(2, 52), gps}, {at(3, 18), tcxo},
		{at(3, 25), ext}, {at(3, 35), flex.Reference{}}}
	now := at(3, 40)
	sec := func(h, m int) int { return h*3600 + m*60 }

	// Before calibration 02:09-02:25: GPSDO locked throughout.
	if r := refOver(log, sec(2, 9), sec(2, 25), now, classify, setup); r.Kind != session.RefGPS || !r.Locked || r.Changed {
		t.Errorf("cal: %+v", r)
	}
	// Run 2, 02:44-02:51: the GPSDO lost lock during it.
	if r := refOver(log, sec(2, 44), sec(2, 51), now, classify, setup); r.Kind != session.RefGPS || r.Locked || r.Changed {
		t.Errorf("run 2: %+v", r)
	}
	// Run 4, 03:14-03:20: fell back to the TCXO during it.
	if r := refOver(log, sec(3, 14), sec(3, 20), now, classify, setup); !r.Changed || !strings.Contains(r.Detail, "→ TCXO") {
		t.Errorf("run 4: %+v", r)
	}
	// External 10 MHz at 03:26-03:30: plain external, until it's ticked as
	// GPS-disciplined, which then applies to files measured before the tick too.
	if r := refOver(log, sec(3, 26), sec(3, 30), now, classify, setup); r.Kind != session.RefExternal {
		t.Errorf("external: %+v", r)
	}
	disc = true
	if r := refOver(log, sec(3, 26), sec(3, 30), now, classify, setup); r.Kind != session.RefGPS {
		t.Errorf("external ticked as disciplined: %+v", r)
	}
	disc = false
	// Flex turned off at 03:35: Setup's reference applies after that.
	if r := refOver(log, sec(3, 36), sec(3, 38), now, classify, setup); r != setup {
		t.Errorf("after Flex off: %+v", r)
	}
	// Nothing logged: the reference now is used.
	if r := refOver(nil, sec(2, 9), sec(2, 25), now, classify, setup); r != setup {
		t.Errorf("empty log: %+v", r)
	}
	// Entries older than a day don't count.
	if r := refOver(log, sec(2, 9), sec(2, 25), now.Add(48*time.Hour), classify, setup); r != setup {
		t.Errorf("stale log: %+v", r)
	}
}

func levels(is []fmtdata.Issue) string {
	var s []string
	for _, i := range is {
		s = append(s, i.Level)
	}
	return strings.Join(s, ",")
}

func TestRefRunIssues(t *testing.T) {
	gps := &session.RefRecord{Kind: session.RefGPS, Locked: true, From: "radio"}
	gpsUnl := &session.RefRecord{Kind: session.RefGPS, Locked: false, From: "radio"}
	tcxo := &session.RefRecord{Kind: session.RefInternal, Locked: true, From: "radio"}
	tcxoSetup := &session.RefRecord{Kind: session.RefInternal, Locked: true, From: "setup"}
	changed := &session.RefRecord{Kind: session.RefGPS, Changed: true, From: "radio", Detail: "GPSDO, locked → TCXO"}
	cases := []struct {
		name string
		run  *session.RefRecord
		mode string
		cals map[string]*session.RefRecord
		want string
	}{
		{"same reference", tcxo, "average", map[string]*session.RefRecord{"before": tcxo, "after": tcxo}, ""},
		{"cal on another reference", tcxo, "average", map[string]*session.RefRecord{"before": gps, "after": tcxo}, "error"},
		{"no record on the cal", tcxo, "before", map[string]*session.RefRecord{"before": nil}, ""},
		{"none on GPS locked", gps, "none", nil, ""},
		{"none on TCXO from radio", tcxo, "none", nil, "error"},
		{"none on internal from Setup", tcxoSetup, "none", nil, "warn"},
		{"none on unlocked GPSDO", gpsUnl, "none", nil, "error"},
		{"calibrated, unlocked GPSDO", gpsUnl, "before", map[string]*session.RefRecord{"before": gps}, "warn"},
		{"changed during the run", changed, "before", map[string]*session.RefRecord{"before": gps}, "error"},
		{"loaded file, calibrated", nil, "average", nil, ""},
		{"loaded file, no correction", nil, "none", nil, "info"},
	}
	for _, c := range cases {
		if got := levels(refRunIssues(c.run, c.mode, c.cals)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestRefCalIssues(t *testing.T) {
	gps := &session.RefRecord{Kind: session.RefGPS, Locked: true}
	res := func(a, b float64) *fmtdata.CalResult {
		return &fmtdata.CalResult{OK: true, A: a, B: b, Stations: []fmtdata.CalStation{{KHz: 5000, Included: true}, {KHz: 15000, Included: true}}}
	}
	if is := refCalIssues(gps, res(0.02, 0.005)); len(is) != 0 {
		t.Errorf("small fit on GPS: %+v", is)
	}
	if is := refCalIssues(gps, res(0.1, 0.05)); levels(is) != "warn" {
		t.Errorf("large fit on GPS (0.85 Hz at 15 MHz): %+v", is)
	}
	if is := refCalIssues(&session.RefRecord{Kind: session.RefInternal, Locked: true}, res(2, 0.3)); len(is) != 0 {
		t.Errorf("large fit on TCXO is normal: %+v", is)
	}
	if is := refCalIssues(&session.RefRecord{Kind: session.RefGPS, Changed: true}, res(0, 0)); levels(is) != "error" {
		t.Errorf("changed during cal: %+v", is)
	}
	if is := refCalIssues(nil, res(5, 1)); len(is) != 0 {
		t.Errorf("no record: %+v", is)
	}
}

func TestFlexRefIssues(t *testing.T) {
	now := time.Date(2026, 11, 13, 2, 34, 10, 0, time.UTC)
	ok := flex.Reference{Known: true, Setting: "auto", State: "gpsdo", Locked: true, GPSTime: "02:34:10", GPSTimeAt: now}
	if is := flexRefIssues(ok, false, now); len(is) != 0 {
		t.Errorf("all good: %+v", is)
	}
	lost := flex.Reference{Known: true, Setting: "external", State: "tcxo", Locked: false}
	if is := flexRefIssues(lost, false, now); levels(is) != "error" {
		t.Errorf("external lost: %+v", is)
	}
	if is := flexRefIssues(flex.Reference{Known: true, Setting: "auto", State: "gpsdo"}, false, now); levels(is) != "warn" {
		t.Errorf("GPSDO unlocked: %+v", is)
	}
	ext := flex.Reference{Known: true, Setting: "auto", State: "external", Locked: true}
	if is := flexRefIssues(ext, false, now); levels(is) != "info" {
		t.Errorf("external, accuracy unknown: %+v", is)
	}
	if is := flexRefIssues(ext, true, now); len(is) != 0 {
		t.Errorf("external marked disciplined: %+v", is)
	}
	// PC clock 5 s ahead of GPS time.
	skew := ok
	skew.GPSTime = "02:34:05"
	if is := flexRefIssues(skew, false, now); levels(is) != "warn" || !strings.Contains(is[0].Msg, "5 s ahead of") {
		t.Errorf("clock skew: %+v", is)
	}
	// A stale GPS time is ignored.
	if is := flexRefIssues(skew, false, now.Add(time.Minute)); len(is) != 0 {
		t.Errorf("stale GPS time: %+v", is)
	}
	if is := flexRefIssues(flex.Reference{}, false, now); len(is) != 0 {
		t.Errorf("unknown reference: %+v", is)
	}
}
