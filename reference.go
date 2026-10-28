package main

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/va3mw/FMTAssistant/internal/flex"
	"github.com/va3mw/FMTAssistant/internal/fmtdata"
	"github.com/va3mw/FMTAssistant/internal/session"
)

// Frequency reference handling.
//
// The WWV calibration measures the radio's whole error, whatever its 10 MHz
// reference, so the reference never enters the arithmetic. It decides which
// checks apply: a calibration only holds for the reference it was made on, an
// unlocked GPSDO can wander, and "No correction" is only right for a
// GPS-locked reference. A FlexRadio reports its reference; other radios use
// what the operator declared in Setup. The GPS freq_error a Flex reports is
// shown and logged but never applied: its units and sign are not documented.

// refEntry is a change of what the radio reports. Ref.Known is false when no
// radio reports a reference (Flex control off, or the connection lost), so
// Setup's declaration applies from then on.
type refEntry struct {
	At  time.Time
	Ref flex.Reference
}

func refKindName(kind string) string {
	switch kind {
	case session.RefGPS:
		return "a GPS-disciplined reference"
	case session.RefExternal:
		return "an external 10 MHz reference"
	}
	return "the radio's internal oscillator"
}

// refFromRadio maps a FlexRadio's oscillator status to a reference record.
func refFromRadio(fr flex.Reference, extDisciplined bool) session.RefRecord {
	rec := session.RefRecord{From: "radio", Locked: fr.Locked}
	var name string
	switch fr.State {
	case "gpsdo":
		rec.Kind, name = session.RefGPS, "GPSDO"
		if fr.GNSS && !fr.GPSDO {
			name = "GNSS-disciplined oscillator"
		}
	case "external":
		rec.Kind, name = session.RefExternal, "external 10 MHz"
		if extDisciplined {
			rec.Kind, name = session.RefGPS, "external 10 MHz (GPS-disciplined or rubidium, as set in Setup)"
		}
	default:
		// A TCXO has nothing to lock to.
		rec.Kind, rec.Locked, name = session.RefInternal, true, "TCXO"
	}
	rec.Detail = name
	if rec.Kind != session.RefInternal {
		rec.Detail += map[bool]string{true: ", locked", false: ", NOT locked"}[fr.Locked]
	}
	if fr.State == "gpsdo" && fr.GPSFreqError != "" {
		rec.Detail += ", GPS freq_error " + fr.GPSFreqError
	}
	return rec
}

// currentRefLocked is the reference now: from a connected FlexRadio, or as
// declared in Setup. a.mu must be held.
func (a *App) currentRefLocked() session.RefRecord {
	if a.sess.Flex.Enabled {
		if snap := a.radio.Snapshot(); snap.Connected && snap.Ref.Known {
			return refFromRadio(snap.Ref, a.sess.ExtDisciplined)
		}
	}
	return a.setupRefLocked()
}

// classifyLocked turns a logged radio report into a record, with today's
// Setup choices (an external reference ticked as GPS-disciplined counts as
// such for files measured before the tick, too).
func (a *App) classifyLocked(fr flex.Reference) session.RefRecord {
	if fr.Known {
		return refFromRadio(fr, a.sess.ExtDisciplined)
	}
	return a.setupRefLocked()
}

func (a *App) setupRefLocked() session.RefRecord {
	k := a.sess.Reference
	if k == "" {
		k = session.RefInternal
	}
	return session.RefRecord{Kind: k, Locked: true, From: "setup",
		Detail: strings.ToUpper(refKindName(k)[:1]) + refKindName(k)[1:]}
}

// onReference records each change of the radio's reference, so a capture
// can tell what the reference was while its file was measured.
func (a *App) onReference(fr flex.Reference) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logRefLocked(fr)
}

func (a *App) logRefLocked(fr flex.Reference) {
	a.refLog = append(a.refLog, refEntry{At: time.Now().UTC(), Ref: fr})
	if len(a.refLog) > 2000 {
		a.refLog = a.refLog[len(a.refLog)-2000:]
	}
	a.src.Info("frequency reference: %s", a.classifyLocked(fr).Detail)
}

// refOver summarises the reference over a measurement from fromSec to toSec
// (UTC seconds of day, within the last day) from the change log: the state
// in force at the start plus every change during it. With no log covering
// the measurement (Flex off, or the app started afterwards), cur is used.
func refOver(log []refEntry, fromSec, toSec int, now time.Time, classify func(flex.Reference) session.RefRecord, cur session.RefRecord) session.RefRecord {
	var recs []session.RefRecord
	var before *refEntry
	for i := range log {
		e := &log[i]
		if now.Sub(e.At) > 24*time.Hour {
			continue
		}
		s := secOfDay(e.At)
		if fmtdata.SecDiff(s, fromSec) < 0 {
			before = e
			continue
		}
		if fmtdata.SecDiff(toSec, s) >= 0 {
			recs = append(recs, classify(e.Ref))
		}
	}
	if before != nil {
		recs = append([]session.RefRecord{classify(before.Ref)}, recs...)
	}
	if len(recs) == 0 {
		return cur
	}
	out := recs[0]
	var steps []string
	for _, r := range recs {
		if !r.Locked {
			out.Locked = false
		}
		if r.Kind != out.Kind {
			out.Changed = true
		}
		if len(steps) == 0 || steps[len(steps)-1] != r.Detail {
			steps = append(steps, r.Detail)
		}
	}
	out.Detail = strings.Join(steps, " → ")
	return out
}

// recordRefLocked stores the reference over a captured file in its slot.
func (a *App) recordRefLocked(sl *session.Slot, f *fmtdata.File) {
	cur := a.currentRefLocked()
	rec := cur
	if n := len(f.Lines); n > 0 {
		rec = refOver(a.refLog, f.Lines[0].Sec, f.Lines[n-1].Sec, time.Now().UTC(), a.classifyLocked, cur)
	}
	// GPS details (freq_error) arrive after the lock change that was logged;
	// when nothing changed, the fuller description of now is the same state.
	if !rec.Changed && rec.From == cur.From && rec.Kind == cur.Kind && rec.Locked == cur.Locked {
		rec.Detail = cur.Detail
	}
	sl.Ref = &rec
	a.saveLocked()
}

// refRunIssues checks a run's reference against the calibrations used to
// correct it and the correction mode.
func refRunIssues(run *session.RefRecord, mode string, cals map[string]*session.RefRecord) []fmtdata.Issue {
	var is []fmtdata.Issue
	add := func(level, format string, args ...any) {
		is = append(is, fmtdata.Issue{Level: level, Msg: fmt.Sprintf(format, args...)})
	}
	if run == nil {
		if mode == "none" {
			add("info", "No reference was recorded for this file. No correction is only right if the radio was GPS-locked while it was measured.")
		}
		return is
	}
	if run.Changed {
		add("error", "The radio's frequency reference changed while this run was measured (%s), so no single correction fits it. Keep only the readings from one side of the change.", run.Detail)
	}
	if mode == "none" {
		switch {
		case run.Kind != session.RefGPS && run.From == "radio":
			add("error", "No correction is selected, but this run was measured on the radio's %s. Choose a WWV calibration on the Calibration tab.", run.Detail)
		case run.Kind != session.RefGPS:
			add("warn", "No correction is selected, but Setup says your radio uses %s. No correction is only right for a GPS-locked reference.", refKindName(run.Kind))
		case !run.Locked:
			add("error", "No correction is selected, but the GPS-disciplined reference wasn't locked while this run was measured, so it may be off by tenths of a hertz. Choose a WWV calibration.")
		}
		return is
	}
	if run.Kind == session.RefGPS && !run.Locked && !run.Changed {
		add("warn", "The GPS-disciplined reference wasn't locked while this run was measured. The WWV calibration corrects it only if it stayed steady; compare the before and after calibrations.")
	}
	for _, name := range []string{"before", "after"} {
		c := cals[name]
		if c == nil || c.Kind == run.Kind {
			continue
		}
		add("error", "This run was measured on %s, but the %s calibration was made on %s. A calibration only holds for the reference it was made on: calibrate again on this reference, or choose a calibration made on it.",
			refKindName(run.Kind), name, refKindName(c.Kind))
	}
	return is
}

// refCalIssues checks a calibration against its reference.
func refCalIssues(rec *session.RefRecord, res *fmtdata.CalResult) []fmtdata.Issue {
	if rec == nil {
		return nil
	}
	var is []fmtdata.Issue
	if rec.Changed {
		is = append(is, fmtdata.Issue{Level: "error", Msg: fmt.Sprintf(
			"The radio's frequency reference changed during this calibration (%s), so its stations don't share one error. Calibrate again.", rec.Detail)})
	}
	// A locked GPS reference leaves almost nothing to calibrate, so a large
	// correction points at something else.
	if rec.Kind == session.RefGPS && rec.Locked && !rec.Changed && res != nil && res.OK {
		worst, at := 0.0, 0
		for _, st := range res.Stations {
			if c := math.Abs(res.Correction(float64(st.KHz) / 1000)); st.Included && c > worst {
				worst, at = c, st.KHz
			}
		}
		if worst > 0.3 {
			is = append(is, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf(
				"Your reference is GPS-locked, so this calibration should be close to zero, but it puts the radio %.2f Hz off at %s. Check that WSJT-X's own calibration is zero (Settings > Frequencies) and look at the WWV readings; or the reference isn't what it seems.",
				worst, khzStr(float64(at)*1000))})
		}
	}
	return is
}

var hmsRe = regexp.MustCompile(`\b(\d{1,2}):(\d{2}):(\d{2})\b`)

// gpsClockIssue compares the radio's GPS time with the PC clock, which the
// schedule runs from.
func gpsClockIssue(fr flex.Reference, now time.Time) *fmtdata.Issue {
	if fr.GPSTime == "" || fr.GPSTimeAt.IsZero() || now.Sub(fr.GPSTimeAt) > 10*time.Second || strings.EqualFold(fr.GPSFix, "NO_FIX") {
		return nil
	}
	m := hmsRe.FindStringSubmatch(fr.GPSTime)
	if m == nil {
		return nil
	}
	var h, mi, s int
	fmt.Sscan(m[1], &h)
	fmt.Sscan(m[2], &mi)
	fmt.Sscan(m[3], &s)
	d := fmtdata.SecDiff(h*3600+mi*60+s, secOfDay(fr.GPSTimeAt))
	if d < 2 && d > -2 {
		return nil
	}
	return &fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf(
		"The PC clock is %d s %s the radio's GPS time. Sync Windows time (Settings > Time & language > Date & time > Sync now): the schedule runs from the PC clock.",
		abs(d), map[bool]string{true: "behind", false: "ahead of"}[d > 0])}
}

// flexRefIssues reports the radio's reference state in the Flex panel.
func flexRefIssues(fr flex.Reference, extDisciplined bool, now time.Time) []fmtdata.Issue {
	if !fr.Known {
		return nil
	}
	var is []fmtdata.Issue
	names := map[string]string{"tcxo": "TCXO", "gpsdo": "GPSDO", "external": "external 10 MHz"}
	if fr.Setting != "" && fr.Setting != "auto" && fr.Setting != fr.State {
		is = append(is, fmtdata.Issue{Level: "error", Msg: fmt.Sprintf(
			"The radio is set to use its %s reference but is running on its %s. The %s reference may be missing or lost; a calibration made on one doesn't hold for the other.",
			names[fr.Setting], names[fr.State], names[fr.Setting])})
	}
	switch {
	case fr.State == "gpsdo" && !fr.Locked:
		is = append(is, fmtdata.Issue{Level: "warn", Msg: "The GPSDO isn't locked" +
			map[bool]string{true: " (GPS status " + fr.GPSStatus + ")", false: ""}[fr.GPSStatus != ""] +
			". Wait for it to lock before calibrating or measuring."})
	case fr.State == "external" && !fr.Locked:
		is = append(is, fmtdata.Issue{Level: "warn", Msg: "The radio isn't locked to the external 10 MHz reference. Check the reference and its cable."})
	case fr.State == "external" && !extDisciplined:
		is = append(is, fmtdata.Issue{Level: "info", Msg: "The radio is on an external 10 MHz reference. The app can't tell how accurate it is, so it's treated like an internal oscillator: calibrate on WWV. If it is GPS-disciplined or rubidium, say so under Frequency reference."})
	}
	if i := gpsClockIssue(fr, now); i != nil {
		is = append(is, *i)
	}
	return is
}

// refReportLine describes a recorded reference for the report.
func refReportLine(rec *session.RefRecord) string {
	if rec == nil {
		return "not recorded (file loaded, or captured by an older version)"
	}
	from := map[string]string{"radio": "read from the radio", "setup": "as set in Setup"}[rec.From]
	return rec.Detail + " (" + from + ")"
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// SetReference declares the radio's frequency reference (used when no
// FlexRadio reports one).
func (a *App) SetReference(kind string) {
	switch kind {
	case session.RefInternal, session.RefExternal, session.RefGPS:
	default:
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.Reference = kind
	a.saveLocked()
}

// SetExtDisciplined says whether an external 10 MHz is GPS-disciplined or
// rubidium.
func (a *App) SetExtDisciplined(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.ExtDisciplined = on
	a.saveLocked()
}
