package workflow

import (
	"fmt"
	"sort"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
)

// TRCycleFix is the WSJT-X setting to change when captures stop halfway.
const TRCycleFix = "In WSJT-X, set the T/R cycle to 60 s or longer while in FreqCal mode, so it stays on the run frequency for the whole key-down instead of moving on after 30 s."

// CaptureCheck compares how long readings actually came in with how long
// the carrier (or WWV dwell) lasted.
type CaptureCheck struct {
	FirstSec    int     `json:"first_sec"` // UTC seconds of day, -1 if no readings
	LastSec     int     `json:"last_sec"`
	SpanSec     float64 `json:"span_sec"`     // seconds covered by readings
	ExpectedSec float64 `json:"expected_sec"` // key-down or planned dwell
	Fraction    float64 `json:"fraction"`
	Short       bool    `json:"short"`
	HalfCycle   bool    `json:"half_cycle"` // readings stop near :30, the 30 s T/R cycle
	Msg         string  `json:"msg"`
}

// RunCapture checks a run's readings against its key-down minute. Only
// strong readings (not flagged, S/N at least minSNR) in the minute count.
func RunCapture(lines []fmtdata.Line, keyDownSec int, minSNR float64, th Thresholds) CaptureCheck {
	c := CaptureCheck{FirstSec: -1, LastSec: -1, ExpectedSec: th.KeyDownSec}
	end := keyDownSec + int(th.KeyDownSec)
	for _, l := range lines {
		if l.Flag || l.SNR < minSNR {
			continue
		}
		if fmtdata.SecDiff(l.Sec, keyDownSec) < 0 || fmtdata.SecDiff(end, l.Sec) < 0 {
			continue
		}
		if c.FirstSec < 0 {
			c.FirstSec = l.Sec
		}
		c.LastSec = l.Sec
	}
	if c.FirstSec < 0 {
		c.Short = true
		c.Msg = "No strong readings during the key-down minute. The carrier was missed."
		return c
	}
	c.SpanSec = float64(fmtdata.SecDiff(c.LastSec, c.FirstSec))
	c.Fraction = c.SpanSec / c.ExpectedSec
	if c.Fraction >= th.ShortCaptureFraction {
		c.Msg = fmt.Sprintf("Captured %.0f s of the %.0f s key-down.", c.SpanSec, c.ExpectedSec)
		return c
	}
	c.Short = true
	lastInMinute := float64(fmtdata.SecDiff(c.LastSec, keyDownSec))
	c.HalfCycle = lastInMinute < th.HalfCycleLastSec
	if c.HalfCycle {
		c.Msg = fmt.Sprintf("Only about %.0f s captured (readings stop at :%02.0f of the key-down minute). WSJT-X FreqCal is probably still on its 30 second T/R cycle. %s",
			c.SpanSec, lastInMinute, TRCycleFix)
	} else {
		c.Msg = fmt.Sprintf("Only about %.0f s of the %.0f s key-down captured. Check that WSJT-X was measuring for the whole minute.", c.SpanSec, c.ExpectedSec)
	}
	return c
}

// LiveAlert checks, while a run is on, whether readings stopped halfway
// through the key-down. nowSec is the current UTC second of day. It returns
// a message once enough of the minute has passed to tell, or "".
func LiveAlert(lines []fmtdata.Line, keyDownSec, nowSec int, minSNR float64, th Thresholds) string {
	into := fmtdata.SecDiff(nowSec, keyDownSec)
	if float64(into) < th.LiveCheckAfterSec || float64(into) > th.KeyDownSec+30 {
		return ""
	}
	early, late := 0, 0
	for _, l := range lines {
		if l.Flag || l.SNR < minSNR {
			continue
		}
		d := float64(fmtdata.SecDiff(l.Sec, keyDownSec))
		switch {
		case d >= 0 && d < th.HalfCycleLastSec:
			early++
		case d >= th.HalfCycleLastSec && d <= th.KeyDownSec:
			late++
		}
	}
	if early >= th.LiveCheckMinReadings && late == 0 {
		return "Readings stopped about 30 s into the key-down. WSJT-X FreqCal is probably still on its 30 second T/R cycle. " + TRCycleFix + " Change it now, before the next run."
	}
	return ""
}

// Window is a slot's time span in UTC seconds of day.
type Window struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	From  int    `json:"from"`
	To    int    `json:"to"`
}

// Foreign counts a capture's readings that fall in other slots' windows.
type Foreign struct {
	Window Window `json:"window"`
	N      int    `json:"n"`
}

// ForeignReadings finds readings that belong to another slot. self is the
// capture's own slot id; windows lists every slot with a known time.
func ForeignReadings(lines []fmtdata.Line, self string, windows []Window) []Foreign {
	var out []Foreign
	for _, w := range windows {
		if w.ID == self {
			continue
		}
		n := 0
		for _, l := range lines {
			if fmtdata.SecDiff(l.Sec, w.From) >= 0 && fmtdata.SecDiff(w.To, l.Sec) >= 0 {
				n++
			}
		}
		if n > 0 {
			out = append(out, Foreign{w, n})
		}
	}
	return out
}

// ForeignBlock returns the message that blocks a capture holding another
// slot's readings, or "" when there are too few to matter.
func ForeignBlock(fs []Foreign, th Thresholds) string {
	for _, f := range fs {
		if f.N >= th.ForeignReadingsBlock {
			return fmt.Sprintf("This capture holds %d readings from %s's time window. They belong to that step, not this one. Start a fresh capture for this step.", f.N, f.Window.Title)
		}
	}
	return ""
}

// CalLoadCheck rejects a calibration capture that is mostly not WWV/WWVH.
// runs maps a kHz value to a run title so the message can say whose
// frequency the stray readings are on. It returns "" when the file is fine.
func CalLoadCheck(f *fmtdata.File, runs map[int]string, th Thresholds) string {
	if len(f.Lines) == 0 {
		return "The file has no readings. Was the Measure box ticked in WSJT-X?"
	}
	wwv := 0
	other := map[int]int{}
	for _, l := range f.Lines {
		if _, ok := fmtdata.LookupReference(l.KHz); ok {
			wwv++
		} else {
			other[l.KHz]++
		}
	}
	if float64(wwv) >= th.CalMinWWVFraction*float64(len(f.Lines)) {
		return ""
	}
	top, n := 0, -1
	for k, c := range other {
		if c > n || (c == n && k < top) {
			top, n = k, c
		}
	}
	whose, bestD := "", 2
	for k, title := range runs {
		d := top - k
		if d < 0 {
			d = -d
		}
		if d < bestD || (d == bestD && whose != "" && title < whose) {
			whose, bestD = title, d
		}
	}
	if whose != "" {
		whose = fmt.Sprintf(" (%s's frequency)", whose)
	}
	return fmt.Sprintf("Only %d of %d readings are on WWV/WWVH frequencies. Most are on %.3f MHz%s, so this isn't a calibration capture. Capture WWV again into a fresh file.",
		wwv, len(f.Lines), float64(top)/1000, whose)
}

// Dwell is one continuous stay on a WWV frequency.
type Dwell struct {
	KHz      int     `json:"khz"`
	FromSec  int     `json:"from_sec"`
	ToSec    int     `json:"to_sec"`
	SpanSec  float64 `json:"span_sec"`
	Readings int     `json:"readings"`
}

// DwellCheck is the capture length of each WWV frequency in a calibration.
type DwellCheck struct {
	KHz     int     `json:"khz"`
	Dwells  []Dwell `json:"dwells"`
	Longest float64 `json:"longest_sec"`
	Short   bool    `json:"short"`
	Msg     string  `json:"msg"`
}

// CalDwells splits a calibration's WWV readings into dwells and compares
// each frequency's longest dwell with the planned dwell (planSec, or the
// default when 0).
func CalDwells(lines []fmtdata.Line, planSec float64, th Thresholds) []DwellCheck {
	if planSec <= 0 {
		planSec = th.CalDwellSec
	}
	by := map[int][]fmtdata.Line{}
	for _, l := range lines {
		if r, ok := fmtdata.LookupReference(l.KHz); ok {
			by[r.KHz] = append(by[r.KHz], l)
		}
	}
	var out []DwellCheck
	for khz, ls := range by {
		dc := DwellCheck{KHz: khz}
		start := 0
		for i := 1; i <= len(ls); i++ {
			if i < len(ls) && float64(fmtdata.SecDiff(ls[i].Sec, ls[i-1].Sec)) <= th.DwellGapSec {
				continue
			}
			d := Dwell{KHz: khz, FromSec: ls[start].Sec, ToSec: ls[i-1].Sec, Readings: i - start}
			d.SpanSec = float64(fmtdata.SecDiff(d.ToSec, d.FromSec))
			dc.Dwells = append(dc.Dwells, d)
			if d.SpanSec > dc.Longest {
				dc.Longest = d.SpanSec
			}
			start = i
		}
		if dc.Longest < th.CalDwellShort*planSec {
			dc.Short = true
			dc.Msg = fmt.Sprintf("%s: the longest stretch of readings is %.0f s, against the %.0f s you planned to stay. If WSJT-X moved on after 30 s, its T/R cycle is still at 30 s. %s",
				refLabel(khz), dc.Longest, planSec, TRCycleFix)
		}
		out = append(out, dc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KHz < out[j].KHz })
	return out
}

func refLabel(khz int) string {
	if r, ok := fmtdata.LookupReference(khz); ok {
		return r.Label
	}
	return fmt.Sprintf("%.3f MHz", float64(khz)/1000)
}
