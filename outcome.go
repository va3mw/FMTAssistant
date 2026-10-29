package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
	"github.com/va3mw/FMTAssistant/internal/session"
)

// After the test the ARRL publishes each carrier's true frequency. Typed in,
// it shows how close each answer was, what the other correction choices
// would have given, and feeds a prompt the operator can give an AI to talk
// the result through. None of it changes the answer.

// Outcome compares a run's answer with the published frequency.
type Outcome struct {
	PublishedHz float64 `json:"published_hz"`
	ErrHz       float64 `json:"err_hz"`  // answer - published
	ErrPPB      float64 `json:"err_ppb"` // the same, in parts per billion
	// ByMode is the error each correction choice would have given
	// ("none", "before", "after", "average"; only those available).
	ByMode map[string]float64 `json:"by_mode"`
}

// blunderHz is an error no reference or propagation explains.
const blunderHz = 2.0

var modeNames = map[string]string{"none": "No correction", "before": "Before", "after": "After", "average": "Average"}

// newOutcome works out the errors; raw is the uncorrected carrier
// (dial + slice tuning error + mean fMeas).
func newOutcome(raw, final, published float64, before, after *float64) *Outcome {
	o := &Outcome{PublishedHz: published, ErrHz: final - published, ByMode: map[string]float64{"none": raw - published}}
	o.ErrPPB = o.ErrHz / published * 1e9
	if before != nil {
		o.ByMode["before"] = raw - *before - published
	}
	if after != nil {
		o.ByMode["after"] = raw - *after - published
	}
	if before != nil && after != nil {
		o.ByMode["average"] = raw - (*before+*after)/2 - published
	}
	return o
}

// runDrift is the slope of the readings used, in Hz per minute.
func runDrift(res *fmtdata.RunResult) (float64, bool) {
	var xs, ys []float64
	if res == nil {
		return 0, false
	}
	for _, l := range res.Lines {
		if l.Used {
			xs, ys = append(xs, float64(l.Sec)), append(ys, l.FMeas)
		}
	}
	if len(xs) < 3 {
		return 0, false
	}
	var mx, my float64
	for i := range xs {
		mx, my = mx+xs[i], my+ys[i]
	}
	mx, my = mx/float64(len(xs)), my/float64(len(xs))
	var sxy, sxx float64
	for i := range xs {
		sxy += (xs[i] - mx) * (ys[i] - my)
		sxx += (xs[i] - mx) * (xs[i] - mx)
	}
	if sxx == 0 {
		return 0, false
	}
	return sxy / sxx * 60, true
}

// outcomeNotes explains the errors in plain language: each run against its
// own measurement scatter, which correction would have been closest, and
// whether the runs share one error (the reference) or not (propagation).
func outcomeNotes(runs []RunView, mode string) []string {
	var done []RunView
	for _, r := range runs {
		if r.Outcome != nil {
			done = append(done, r)
		}
	}
	if len(done) == 0 {
		return nil
	}
	var notes []string
	var fair []RunView // runs without a blunder, for the patterns below
	for _, r := range done {
		o := r.Outcome
		n := fmt.Sprintf("%s: off by %+.3f Hz (%+.1f ppb).", r.Title, o.ErrHz, o.ErrPPB)
		if !r.Ready {
			n += " This answer still had unresolved problems in the app."
		}
		if math.Abs(o.ErrHz) > blunderHz {
			// Far beyond anything the reference or the ionosphere does.
			notes = append(notes, n+" That's far more than propagation or a radio's error can explain: check the dial entered for the run, the published frequency you typed, and that the file and station match. It's left out of the patterns below.")
			continue
		}
		fair = append(fair, r)
		if sem := r.Result.SEM; sem > 0 {
			if withinScatter(r) {
				n += fmt.Sprintf(" That's within the measurement's own scatter (σ/√N %.3f Hz).", sem)
			} else {
				n += fmt.Sprintf(" That's %.0f times the measurement's own scatter (σ/√N %.3f Hz), so more readings wouldn't have fixed it: the cause is a bias such as Doppler, the calibration or the reference.",
					math.Abs(o.ErrHz)/sem, sem)
			}
		}
		if d, ok := runDrift(r.Result); ok && math.Abs(d) >= 0.01 {
			n += fmt.Sprintf(" The readings drifted %+.3f Hz per minute during the run: the path's Doppler, or the radio's own frequency, was changing.", d)
		}
		notes = append(notes, n)
	}

	if len(fair) == 0 {
		return notes
	}
	// Which correction would have been closest, over the runs that have it.
	rms := func(m string) (float64, bool) {
		var ss float64
		for _, r := range fair {
			e, ok := r.Outcome.ByMode[m]
			if !ok {
				return 0, false
			}
			ss += e * e
		}
		return math.Sqrt(ss / float64(len(fair))), true
	}
	// The answers' own errors: with too few calibrations, Average falls
	// back to the one there is.
	var ss float64
	for _, r := range fair {
		ss += r.Outcome.ErrHz * r.Outcome.ErrHz
	}
	used := math.Sqrt(ss / float64(len(fair)))
	best, bestRMS := "", math.Inf(1)
	for _, m := range []string{"none", "before", "after", "average"} {
		if v, ok := rms(m); ok && v < bestRMS {
			best, bestRMS = m, v
		}
	}
	if bestRMS < used-0.005 {
		notes = append(notes, fmt.Sprintf("In hindsight, %s would have been closest (RMS error %.3f Hz, against %.3f Hz with the correction you used). One night doesn't prove it will be best next time.",
			modeNames[best], bestRMS, used))
	} else {
		notes = append(notes, fmt.Sprintf("The correction you used was as close as any of the choices (RMS error %.3f Hz).", used))
	}

	// One shared error or several different ones?
	if len(fair) >= 2 {
		var m, s float64
		pos, neg := 0, 0
		for _, r := range fair {
			m += r.Outcome.ErrPPB
			if r.Outcome.ErrPPB >= 0 {
				pos++
			} else {
				neg++
			}
		}
		m /= float64(len(fair))
		for _, r := range fair {
			s += (r.Outcome.ErrPPB - m) * (r.Outcome.ErrPPB - m)
		}
		s = math.Sqrt(s / float64(len(fair)-1))
		allNoise := true
		for _, r := range fair {
			allNoise = allNoise && withinScatter(r)
		}
		switch {
		case allNoise:
			notes = append(notes, fmt.Sprintf("Every error is within its run's own measurement scatter (average %+.1f ppb, spread %.1f ppb), so these runs can't tell a reference error from propagation. A steadier carrier or more readings would sharpen the answer.", m, s))
		case math.Abs(m) >= 3 && s < math.Abs(m)/2:
			notes = append(notes, fmt.Sprintf("All the runs are off in the same direction by a similar fraction (%+.1f ppb on average, spread %.1f ppb). An error that scales with frequency points to the frequency reference or the calibration rather than propagation.", m, s))
		case s >= 3:
			sign := ""
			if pos > 0 && neg > 0 {
				sign = ", and in both directions"
			}
			notes = append(notes, fmt.Sprintf("The runs are off by different amounts (spread %.1f ppb%s). That points to propagation, each path's own Doppler, or to the individual measurements, rather than the frequency reference.", s, sign))
		default:
			notes = append(notes, fmt.Sprintf("All the runs are within a few ppb of the published frequencies (average %+.1f ppb, spread %.1f ppb): about as close as the ionosphere allows.", m, s))
		}
	}
	return notes
}

// withinScatter says a run's error is no bigger than its own measurement
// noise (three times σ/√N, with a floor for very steady carriers).
func withinScatter(r RunView) bool {
	sem := r.Result.SEM
	return sem > 0 && math.Abs(r.Outcome.ErrHz) <= 3*math.Max(sem, 0.003)
}

// SetPublished records the published frequency for a run (Hz, kHz or MHz
// text); empty text clears it.
func (a *App) SetPublished(id, text string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.sess.Run(id)
	if r == nil || id == "practice" {
		return "", fmt.Errorf("unknown run %s", id)
	}
	r.PublishedText = text
	r.PublishedHz = 0
	if strings.TrimSpace(text) != "" {
		hz, err := session.ParseDial(text)
		if err != nil {
			a.saveLocked()
			return "", err
		}
		r.PublishedHz = hz
	}
	a.saveLocked()
	if r.PublishedHz == 0 {
		return "", nil
	}
	return "= " + hzStr(r.PublishedHz), nil
}

// AIPrompt builds a self-contained prompt describing the night (method,
// reference, calibrations, every run's readings and, when entered, the
// published frequencies) for the operator to paste into an AI chat. It's
// saved in the session folder as well.
func (a *App) AIPrompt() (string, error) {
	st := a.GetState()
	s := st.Session
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	published := 0
	for _, r := range st.Runs {
		if r.Outcome != nil {
			published++
		}
	}

	w("I took part in the ARRL Frequency Measuring Test (FMT), where amateurs measure the frequency of unmodulated HF carriers as accurately as they can. ")
	if published > 0 {
		w("The ARRL has published the true frequencies, so I know how close I was. Please help me understand why each measurement was as far off as it was, and what I should change next time.\n\n")
	} else {
		w("The true frequencies aren't published yet. Please review how good these measurements are likely to be, and what I should change next time.\n\n")
	}
	w("Please:\n")
	w("1. For each run, explain the error (or the likely uncertainty) and rank the probable causes, with numbers from the data.\n")
	w("2. Say whether the pattern across the runs looks systematic (the frequency reference or the calibration, which scale with frequency) or run-to-run (propagation Doppler on each path, or the measurement itself).\n")
	w("3. Say whether a different correction choice would have helped, and whether that's likely to hold on another night.\n")
	w("4. Give specific, practical changes for the next FMT.\n")
	w("Use only the data below and say when something can't be told from it. Don't assume equipment or conditions that aren't stated.\n\n")

	w("HOW THE MEASUREMENTS WERE MADE\n")
	w("- WSJT-X in FreqCal mode measures the audio frequency of a carrier received in USB (DIGU) and writes one line per measurement to fmt.all: UTC, kHz (truncated RF frequency), Offset (the Rx audio frequency set in WSJT-X), fMeas (measured audio frequency, Hz), DF = fMeas - Offset, Level and S/N.\n")
	w("- Carrier = dial + slice tuning error (FlexRadio only, when read) + mean fMeas - correction.\n")
	w("- The correction is the radio's own frequency error, A + B*f (f in MHz), from a straight-line fit of DF against frequency over WWV carriers (2.5-25 MHz, transmitted to within about 1e-11 but received with their own path Doppler).\n")
	w("- Correction choice used: %s. The other choices are shown per run for comparison.\n", modeNames[s.CalMode])
	w("- Weak readings (S/N below the threshold, or flagged '*' by WSJT-X) and the carrier's first and last readings are normally left out; wild readings are rejected.\n")
	w("- Rules of thumb: 1 ppb is 0.0035 Hz at 3.5 MHz, 0.007 Hz at 7 MHz and 0.014 Hz at 14 MHz. A locked GPSDO radio is typically within about 1 ppb, so on it a WWV correction mainly adds WWV's own path Doppler. Night-time ionospheric Doppler is typically a few hundredths of a hertz on 80 and 40 m, and more near sunrise or sunset or when the ionosphere is disturbed.\n\n")

	w("STATION\n")
	w("- FMT Assistant %s, session %q", version, s.Name)
	if s.Callsign != "" {
		w(", callsign %s", s.Callsign)
	}
	w("\n- Frequency reference now: %s\n", refReportLine(&st.Ref))
	if s.Flex.Enabled {
		w("- FlexRadio connected to the app (dial and reference read from the radio)\n")
	}
	w("\n")

	w("CALIBRATIONS\n")
	for _, c := range st.Cals {
		cr := c.Result
		if cr == nil || !cr.OK {
			w("- %s: none\n", c.Title)
			continue
		}
		w("- %s, %s-%s UTC: A %+.3f +/- %.3f Hz, B %+.5f +/- %.5f ppm, %d stations fitted, fit SD %.3f Hz\n",
			c.Title, cr.First, cr.Last, cr.A, cr.SigmaA, cr.B, cr.SigmaB, cr.NFit, cr.RMS)
		w("  Reference: %s\n", refReportLine(c.Cal.Slot.Ref))
		for _, stn := range cr.Stations {
			use := "used"
			if !stn.Included {
				use = "not used"
			}
			w("  %s: n=%d, mean DF %+.3f Hz, SD %.3f, residual %+.3f (%s)\n", stn.Label, stn.N, stn.MeanDF, stn.SD, stn.Resid, use)
		}
		writeIssues(w, "  ", c.Issues, cr.Issues)
	}
	w("\n")

	w("RUNS\n")
	for i, r := range st.Runs {
		run := r.Run
		w("Run %d: %s, published as near %.0f kHz, key-down %s UTC (one minute)\n", i+1, r.Title, run.NominalKHz, run.KeyDown.Format("2006-01-02 15:04"))
		res := r.Result
		if res == nil || res.N == 0 {
			w("  No usable measurement.\n")
			writeIssues(w, "  ", r.Issues, resIssues(res))
			continue
		}
		w("  Dial %s", hzStr(run.DialHz))
		if r.SliceErrHz != nil {
			w(", slice tuning error %+.1f mHz", *r.SliceErrHz*1000)
		}
		w("\n  Carrier readings used: n=%d, %s-%s UTC, mean fMeas %.3f Hz, SD %.3f Hz, SD/sqrt(N) %.3f Hz\n", res.N, res.BlockFrom, res.BlockTo, res.MeanFMeas, res.SD, res.SEM)
		if d, ok := runDrift(res); ok {
			w("  Slope of the readings used: %+.4f Hz per minute\n", d)
		}
		if r.CorrBefore != nil {
			w("  Correction from the before calibration: %+.3f Hz\n", *r.CorrBefore)
		}
		if r.CorrAfter != nil {
			w("  Correction from the after calibration: %+.3f Hz\n", *r.CorrAfter)
		}
		if r.CorrHz != nil {
			w("  Correction applied: %+.3f Hz\n", *r.CorrHz)
		}
		w("  Reference: %s\n", refReportLine(run.Slot.Ref))
		if r.FinalHz != nil {
			w("  My answer: %.3f Hz\n", *r.FinalHz)
		}
		if o := r.Outcome; o != nil {
			w("  Published: %.3f Hz. Error %+.3f Hz (%+.1f ppb)\n", o.PublishedHz, o.ErrHz, o.ErrPPB)
			var alt []string
			for _, m := range []string{"none", "before", "after", "average"} {
				if e, ok := o.ByMode[m]; ok {
					alt = append(alt, fmt.Sprintf("%s %+.3f Hz", modeNames[m], e))
				}
			}
			w("  Error with each correction choice: %s\n", strings.Join(alt, "; "))
		}
		writeIssues(w, "  ", r.Issues, resIssues(res))
		// The readings around the carrier, so the AI can see drift and fades.
		from, _ := hmsSec(res.BlockFrom)
		to, _ := hmsSec(res.BlockTo)
		w("  Readings from 30 s before to 30 s after the carrier (UTC fMeas S/N status):\n")
		for _, l := range res.Lines {
			if l.Sec < from-30 || l.Sec > to+30 {
				continue
			}
			status := l.Status
			if l.Used {
				status = "used"
			}
			w("    %s %.3f %.1f %s\n", l.Time, l.FMeas, l.SNR, status)
		}
	}

	if notes := st.OutcomeNotes; len(notes) > 0 {
		w("\nFMT ASSISTANT'S OWN ANALYSIS\n")
		for _, n := range notes {
			w("- %s\n", n)
		}
	}

	text := b.String()
	p := filepath.Join(st.Dir, "FMT AI prompt.txt")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(text, "\n", "\r\n")), 0o644); err != nil {
		return text, err
	}
	a.src.Info("AI prompt saved to %s", p)
	return text, nil
}

// writeIssues lists warnings and errors (not info) for the prompt.
func writeIssues(w func(string, ...any), indent string, lists ...[]fmtdata.Issue) {
	for _, l := range lists {
		for _, is := range l {
			if is.Level != "info" {
				w("%s%s: %s\n", indent, strings.ToUpper(is.Level), is.Msg)
			}
		}
	}
}
