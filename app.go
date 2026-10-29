package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/FMTAssistant/internal/flex"
	"github.com/va3mw/FMTAssistant/internal/fmtdata"
	"github.com/va3mw/FMTAssistant/internal/logx"
	"github.com/va3mw/FMTAssistant/internal/session"
	"github.com/va3mw/FMTAssistant/internal/speech"
)

// App is bound to the UI. Every exported method is callable from JavaScript.
type App struct {
	ctx   context.Context
	log   *logx.Logger
	src   *logx.Source
	voice *speech.Speaker

	mu     sync.Mutex
	sess   *session.Session
	notice string // shown in Setup, e.g. why sessions aren't in Documents
	cache  map[string]cached
	ready  chan struct{} // closed when startup has loaded the session

	disc  *flex.Discovery
	radio *flex.Radio
	// dial history of the controlled slice, to check the dial during a carrier
	freqLog []freqEntry
	// changes of the radio's frequency reference (reference.go)
	refLog []refEntry

	// live follow of fmt.all (live.go); liveMu is taken before mu, never after
	liveMu sync.Mutex
	live   *liveFollow
}

type cached struct {
	mod  time.Time
	size int64
	f    *fmtdata.File
	err  error
}

type freqEntry struct {
	At time.Time
	Hz int64
}

func NewApp(l *logx.Logger) *App {
	return &App{log: l, src: l.Src("app"), cache: map[string]cached{}, ready: make(chan struct{})}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.voice = speech.New(a.log)
	a.radio = flex.NewRadio(a.log, a.changed)
	a.radio.OnSliceFreq = a.onSliceFreq
	a.radio.OnReference = a.onReference
	a.disc = flex.NewDiscovery(a.log, a.changed)

	dir := loadLastDir()
	s, err := session.Load(dir)
	if err != nil {
		if dir != "" && !os.IsNotExist(err) {
			a.src.Warn("could not open last session %s: %v", dir, err)
		}
		s = session.Default()
		if dir == "" {
			dir = session.DefaultDir(s.Name)
		}
		s.SetDir(dir)
		if old, err := session.Load(s.Dir()); err == nil {
			s = old
		}
	}
	if root, fallback := session.SessionsRoot(); fallback {
		a.notice = fmt.Sprintf("Windows Security's Controlled folder access stops FMT Assistant writing to Documents, so sessions are kept in %s. "+
			"To use Documents instead: Windows Security > Virus & threat protection > Ransomware protection > Allow an app through Controlled folder access, add FMTAssistant.exe, then restart FMT Assistant.", root)
		a.src.Warn("Documents is not writable; sessions go in %s", root)
	}
	if !session.Writable(s.Dir()) {
		nd := session.DefaultDir(filepath.Base(s.Dir()))
		a.src.Warn("session folder %s is not writable; using %s", s.Dir(), nd)
		if old, err := session.Load(nd); err == nil {
			s = old
		} else {
			s.SetDir(nd)
		}
	}
	a.sess = s
	a.saveLocked()
	a.src.Info("session %q in %s", s.Name, s.Dir())
	a.startFlexLocked()
	close(a.ready)
}

func (a *App) shutdown(context.Context) {
	a.LiveFollow("")
	a.radio.Disconnect()
	a.disc.Stop()
	a.log.Close()
}

// changed tells the UI to refresh.
func (a *App) changed() {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "changed")
	}
}

func (a *App) saveLocked() {
	if err := a.sess.Save(); err != nil {
		a.src.Error("saving session: %v", err)
	}
	saveLastDir(a.sess.Dir())
}

// ---------------------------------------------------------------------------
// State for the UI

type WSJTXView struct {
	Dir       string          `json:"dir"`
	Dirs      []string        `json:"dirs"`
	DirExists bool            `json:"dir_exists"`
	FmtExists bool            `json:"fmt_exists"`
	FmtLines  int             `json:"fmt_lines"`
	FmtAgeSec float64         `json:"fmt_age_sec"`
	Last      []fmtdata.Line  `json:"last"`
	FmtKHz    []int           `json:"fmt_khz"`
	CalA      float64         `json:"cal_a"`
	CalB      float64         `json:"cal_b"`
	IniFound  bool            `json:"ini_found"`
	Issues    []fmtdata.Issue `json:"issues"`
}

type CalView struct {
	ID      string             `json:"id"`
	Title   string             `json:"title"`
	Cal     session.Cal        `json:"cal"`
	Present bool               `json:"present"`
	Result  *fmtdata.CalResult `json:"result"`
	Issues  []fmtdata.Issue    `json:"issues"`
	Lines   []fmtdata.Line     `json:"lines"`
}

type RunView struct {
	Run           session.Run        `json:"run"`
	Title         string             `json:"title"`
	CallUp        time.Time          `json:"call_up"`
	KeyUp         time.Time          `json:"key_up"`
	FileName      string             `json:"file_name"`
	Present       bool               `json:"present"`
	SuggestDialHz float64            `json:"suggest_dial_hz"`
	Result        *fmtdata.RunResult `json:"result"`
	CorrHz        *float64           `json:"corr_hz"`
	CorrBefore    *float64           `json:"corr_before"`
	CorrAfter     *float64           `json:"corr_after"`
	FinalHz       *float64           `json:"final_hz"`
	SliceErrHz    *float64           `json:"slice_err_hz"` // Flex slice tuning error added to the dial
	Issues        []fmtdata.Issue    `json:"issues"`
	Ready         bool               `json:"ready"`
}

type FlexView struct {
	Enabled bool             `json:"enabled"`
	Radios  []flex.RadioInfo `json:"radios"`
	Snap    flex.Snapshot    `json:"snap"`
	Slice   *flex.Slice      `json:"slice"`
	Issues  []fmtdata.Issue  `json:"issues"`
}

type State struct {
	Session     *session.Session  `json:"session"`
	Dir         string            `json:"dir"`
	WSJTX       WSJTXView         `json:"wsjtx"`
	Cals        []CalView         `json:"cals"`
	Runs        []RunView         `json:"runs"`
	Practice    RunView           `json:"practice"`
	PracticeCal CalView           `json:"practice_cal"`
	Flex        FlexView          `json:"flex"`
	Ref         session.RefRecord `json:"ref"` // the frequency reference now
	VoiceOK     bool              `json:"voice_ok"`
	Problems    int               `json:"problems"`
	Notice      string            `json:"notice"`
	Version     string            `json:"version"`
}

// GetState analyses everything and returns it for display.
func (a *App) GetState() State {
	// The window can ask before startup has loaded the session.
	<-a.ready
	a.mu.Lock()
	defer a.mu.Unlock()
	// Wails drops a call whose method panics without answering it, which
	// would leave the UI waiting forever; log the stack and re-panic.
	defer func() {
		if r := recover(); r != nil {
			a.src.Error("GetState panic: %v\n%s", r, debug.Stack())
			panic(r)
		}
	}()
	s := a.sess
	st := State{Session: s, Dir: s.Dir(), VoiceOK: a.voice.OK(), Version: version, Notice: a.notice}
	st.WSJTX = a.wsjtxView()
	st.Flex = a.flexView()
	st.Ref = a.currentRefLocked()

	// Calibration files.
	files := map[string]*fmtdata.File{}
	for _, c := range []*session.Cal{&s.CalBefore, &s.CalAfter, &s.PracticeCal} {
		cv := CalView{ID: c.ID, Cal: *c}
		cv.Title = map[string]string{"cal_before": "Calibration before the runs", "cal_after": "Calibration after the runs",
			"practice_cal": "Practice calibration"}[c.ID]
		if f, err := a.loadSlot(c.Slot); err != nil {
			cv.Issues = append(cv.Issues, fmtdata.Issue{Level: "error", Msg: err.Error()})
		} else if f != nil {
			cv.Present = true
			files[c.ID] = f
			cv.Lines = f.Lines
			cv.Result = fmtdata.AnalyzeCal(f, fmtdata.CalOptions{MinSNR: c.MinSNR, Stations: c.Stations})
			cv.Issues = append(cv.Issues, refCalIssues(c.Slot.Ref, cv.Result)...)
			ref := 0
			for _, l := range f.Lines {
				if _, ok := fmtdata.LookupReference(l.KHz); ok {
					ref++
				}
			}
			if len(f.Lines) > 0 && ref*2 < len(f.Lines) {
				cv.Issues = append(cv.Issues, fmtdata.Issue{Level: "error", Msg: "Most readings in this file are not on WWV/WWVH frequencies. Is this a test-run file loaded into a calibration slot?"})
			}
		}
		if c.ID == "practice_cal" {
			st.PracticeCal = cv
			continue
		}
		st.Cals = append(st.Cals, cv)
	}
	before, after := st.Cals[0].Result, st.Cals[1].Result
	if rb, ra := s.CalBefore.Slot.Ref, s.CalAfter.Slot.Ref; rb != nil && ra != nil && rb.Kind != ra.Kind {
		st.Cals[1].Issues = append(st.Cals[1].Issues, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf(
			"The before and after calibrations were made on different references (%s, then %s). Don't average them: choose the one made on the same reference as the runs.",
			refKindName(rb.Kind), refKindName(ra.Kind))})
	}

	// Runs.
	for i := range s.Runs {
		rv, f := a.runView(&s.Runs[i], false, before, after, nil)
		if f != nil {
			files[s.Runs[i].ID] = f
		}
		st.Runs = append(st.Runs, rv)
	}
	pv, pf := a.runView(&s.Practice, true, before, after, st.PracticeCal.Result)
	if pf != nil {
		files["practice"] = pf
	}

	// The same file must not be used twice.
	bySHA := map[string][]string{}
	for id, f := range files {
		bySHA[f.SHA] = append(bySHA[f.SHA], id)
	}
	for _, ids := range bySHA {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		msg := strings.Join(a.slotTitles(ids), " and ") + " have the same file loaded. Each slot needs its own file."
		for i := range st.Cals {
			if contains(ids, st.Cals[i].ID) {
				st.Cals[i].Issues = append(st.Cals[i].Issues, fmtdata.Issue{Level: "error", Msg: msg})
			}
		}
		for i := range st.Runs {
			if contains(ids, st.Runs[i].Run.ID) {
				st.Runs[i].Issues = append(st.Runs[i].Issues, fmtdata.Issue{Level: "error", Msg: msg})
				st.Runs[i].Ready = false
			}
		}
		if contains(ids, "practice") {
			pv.Issues = append(pv.Issues, fmtdata.Issue{Level: "error", Msg: msg})
			pv.Ready = false
		}
		if contains(ids, "practice_cal") {
			st.PracticeCal.Issues = append(st.PracticeCal.Issues, fmtdata.Issue{Level: "error", Msg: msg})
		}
	}
	st.Practice = pv
	for _, c := range st.Cals {
		st.Problems += countErrors(c.Issues)
		if c.Result != nil {
			st.Problems += countErrors(c.Result.Issues)
		}
	}
	for _, r := range st.Runs {
		st.Problems += countErrors(r.Issues)
		if r.Result != nil {
			st.Problems += countErrors(r.Result.Issues)
		}
	}
	return st
}

// runView analyses one run (or the practice run) and returns it with the
// parsed file. pcal is the practice calibration: the practice run uses it
// when it's usable, and the test-night calibrations otherwise.
func (a *App) runView(r *session.Run, practice bool, before, after, pcal *fmtdata.CalResult) (RunView, *fmtdata.File) {
	s := a.sess
	calOK := func(c *fmtdata.CalResult) bool { return c != nil && c.OK && !fmtdata.HasErrors(c.Issues) }
	mode, beforeName, beforeRef, afterRef := s.CalMode, "before", s.CalBefore.Slot.Ref, s.CalAfter.Slot.Ref
	usePcal := practice && calOK(pcal)
	if usePcal {
		before, after, beforeName, beforeRef, afterRef = pcal, nil, "practice", s.PracticeCal.Slot.Ref, nil
		if mode != "none" {
			mode = "before"
		}
	}
	rv := RunView{Run: *r, Title: r.Title(), CallUp: r.CallUp(), KeyUp: r.KeyUp(),
		FileName: s.SlotFileName(r.ID), SuggestDialHz: math.Round(r.CarrierHz - 1500)}
	f, err := a.loadSlot(r.Slot)
	if err != nil {
		rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "error", Msg: err.Error()})
	}
	if f != nil {
		rv.Present = true
		kd := -1
		if !r.KeyDown.IsZero() {
			kd = secOfDay(r.KeyDown)
		}
		res := fmtdata.AnalyzeRun(f, fmtdata.RunOptions{MinSNR: r.MinSNR, TrimEdges: r.TrimEdges,
			Overrides: r.Overrides, KeyDownStart: kd, KeyDownEnd: kd + 60,
			ForceWindow: practice, AllowReference: practice})
		if kd < 0 {
			res.Issues = append(res.Issues, fmtdata.Issue{Level: "info", Msg: "No measuring minute was set, so the whole file was searched for the carrier."})
		}
		rv.Result = res
		rv.Issues = append(rv.Issues, a.checkRun(r, res, f, practice)...)
		if res.N > 0 {
			fMHz := (r.DialHz + res.MeanFMeas) / 1e6
			if r.DialHz == 0 {
				fMHz = float64(res.KHz) / 1000
			}
			if calOK(before) {
				v := before.Correction(fMHz)
				rv.CorrBefore = &v
			}
			if calOK(after) {
				v := after.Correction(fMHz)
				rv.CorrAfter = &v
			}
			corr, cerr := pickCorrection(mode, rv.CorrBefore, rv.CorrAfter)
			switch {
			case cerr != "" && practice:
				corr = 0
				rv.CorrHz = &corr
				rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: "No usable calibration, so the practice result is not corrected for your radio's error. Capture a practice calibration on WWV first if you want it corrected."})
			case practice && usePcal && mode != "none":
				rv.CorrHz = &corr
				rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: "Corrected with the practice calibration."})
			case practice && mode != "none":
				rv.CorrHz = &corr
				rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: "Corrected with the test-night calibration, because there's no practice calibration."})
			case cerr != "":
				rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "error", Msg: cerr})
			default:
				rv.CorrHz = &corr
			}
			used := map[string]*session.RefRecord{}
			if rv.CorrBefore != nil && (mode == "before" || mode == "average") {
				used[beforeName] = beforeRef
			}
			if rv.CorrAfter != nil && (mode == "after" || mode == "average") {
				used["after"] = afterRef
			}
			var applied *float64
			if mode != "none" && rv.CorrHz != nil && (rv.CorrBefore != nil || rv.CorrAfter != nil) {
				applied = rv.CorrHz
			}
			rv.Issues = append(rv.Issues, refRunIssues(r.Slot.Ref, mode, used, applied)...)
			if rv.CorrBefore != nil && rv.CorrAfter != nil && math.Abs(*rv.CorrBefore-*rv.CorrAfter) > 0.3 {
				rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf(
					"The before and after calibrations differ by %.2f Hz here, so the radio drifted. Averaging them is usually best.",
					math.Abs(*rv.CorrBefore-*rv.CorrAfter))})
			}
			if r.DialHz > 0 && rv.CorrHz != nil {
				dial := r.DialHz
				if e := sliceErrApplied(r); e != nil {
					rv.SliceErrHz = e
					dial += *e
					rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: fmt.Sprintf(
						"Includes the radio's slice tuning error, %+.1f mHz, read at key-down.", *e*1000)})
				} else if r.SliceErrMHz != nil {
					rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: fmt.Sprintf(
						"The radio's slice tuning error was read on %s, not the dial you entered, so it isn't applied.", khzStr(float64(r.SliceErrDialHz)))})
				}
				v := dial + res.MeanFMeas - *rv.CorrHz
				rv.FinalHz = &v
				if math.Abs(v-r.NominalKHz*1000) > 1500 {
					rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf(
						"The answer is %.3f kHz from the published %.0f kHz. Check the dial entry and that this is the right file.",
						(v-r.NominalKHz*1000)/1000, r.NominalKHz)})
				}
			}
		}
	} else if err == nil {
		rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: "No file yet. After the carrier ends, press Capture (or Load to pick a saved file)."})
	}
	// A missing dial only becomes a problem once there is a file to work out;
	// before that it is just the next thing to do.
	if r.DialHz == 0 && f != nil {
		rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "error", Msg: "Enter the exact dial frequency you used for this run."})
	} else if r.DialHz == 0 {
		rv.Issues = append(rv.Issues, fmtdata.Issue{Level: "info", Msg: "Type the exact dial into Dial you used once you've centred the tone, about two minutes before key-down."})
	}
	rv.Ready = rv.FinalHz != nil && !fmtdata.HasErrors(rv.Issues) && !fmtdata.HasErrors(resIssues(rv.Result))
	return rv, f
}

// checkRun cross-checks a run file against its schedule, the dial entry,
// the radio's dial log and the other runs.
func (a *App) checkRun(r *session.Run, res *fmtdata.RunResult, f *fmtdata.File, practice bool) []fmtdata.Issue {
	var is []fmtdata.Issue
	add := func(level, format string, args ...any) {
		is = append(is, fmtdata.Issue{Level: level, Msg: fmt.Sprintf(format, args...)})
	}
	if res.N == 0 {
		return is
	}
	if math.Abs(float64(res.KHz)-r.NominalKHz) > 3 {
		hint := ""
		for _, o := range a.sess.Runs {
			if o.ID != r.ID && math.Abs(float64(res.KHz)-o.NominalKHz) <= 3 {
				hint = " It matches " + o.Title() + "."
				break
			}
		}
		add("error", "This file was measured near %d kHz but %s is near %.0f kHz.%s Is it the right file?", res.KHz, r.Title(), r.NominalKHz, hint)
	}
	if r.DialHz > 0 {
		d := int64(math.Round(r.DialHz))
		if d < res.DialMin || d > res.DialMax {
			add("error", "The dial %s does not match the file. WSJT-X logged %d kHz with the tone at %d Hz, so the dial must be between %s and %s. Check for a typo.",
				khzStr(r.DialHz), res.KHz, res.Offset, khzStr(float64(res.DialMin)), khzStr(float64(res.DialMax)))
		}
		if r.RadioDialHz != 0 && math.Abs(float64(r.RadioDialHz)-r.DialHz) >= 1 {
			add("warn", "The radio's dial during the carrier was %s, but you entered %s.", khzStr(float64(r.RadioDialHz)), khzStr(r.DialHz))
		}
	}
	// Readings that fall in another run's time slot mean fmt.all was not
	// captured between runs.
	for _, o := range a.sess.Runs {
		if o.ID == r.ID || practice {
			continue
		}
		cu, ku := secOfDay(o.CallUp()), secOfDay(o.KeyUp())
		n := 0
		for _, l := range f.Lines {
			if fmtdata.SecDiff(l.Sec, cu) >= 0 && fmtdata.SecDiff(ku+60, l.Sec) >= 0 {
				n++
			}
		}
		if n > 3 {
			add("warn", "%d readings fall in the %s time slot. fmt.all may not have been captured between runs; check the highlighted carrier is %s.", n, o.Title(), r.Title())
		}
	}
	// Did the dial move while the carrier was measured?
	if dials := a.dialsDuring(res); len(dials) > 1 {
		add("error", "The radio's dial changed during the carrier (%s). Use only readings from one dial setting.", strings.Join(dials, ", "))
	}
	return is
}

// dialsDuring lists the distinct radio dial settings logged during the
// carrier block of a run measured in the last day.
func (a *App) dialsDuring(res *fmtdata.RunResult) []string {
	if len(a.freqLog) == 0 || res.BlockFrom == "" {
		return nil
	}
	from, _ := hmsSec(res.BlockFrom)
	to, _ := hmsSec(res.BlockTo)
	seen := map[int64]bool{}
	var out []string
	var before *freqEntry
	for i := range a.freqLog {
		e := &a.freqLog[i]
		if time.Since(e.At) > 24*time.Hour {
			continue
		}
		s := secOfDay(e.At)
		if fmtdata.SecDiff(s, from) < 0 {
			before = e
			continue
		}
		if fmtdata.SecDiff(to, s) < 0 {
			continue
		}
		if !seen[e.Hz] {
			seen[e.Hz] = true
			out = append(out, khzStr(float64(e.Hz)))
		}
	}
	if before != nil && len(out) > 0 && !seen[before.Hz] {
		out = append([]string{khzStr(float64(before.Hz))}, out...)
	}
	return out
}

func pickCorrection(mode string, before, after *float64) (float64, string) {
	switch mode {
	case "none":
		return 0, ""
	case "before":
		if before == nil {
			return 0, "The \"before\" calibration is missing or has errors."
		}
		return *before, ""
	case "after":
		if after == nil {
			return 0, "The \"after\" calibration is missing or has errors."
		}
		return *after, ""
	default:
		switch {
		case before != nil && after != nil:
			return (*before + *after) / 2, ""
		case before != nil:
			return *before, ""
		case after != nil:
			return *after, ""
		}
		return 0, "No usable calibration yet. Capture a WWV calibration, or choose \"No correction\" if your radio is GPS-locked."
	}
}

func (a *App) wsjtxView() WSJTXView {
	v := WSJTXView{Dir: a.sess.WSJTXDir, Dirs: session.WSJTXDirs()}
	if fi, err := os.Stat(v.Dir); err == nil && fi.IsDir() {
		v.DirExists = true
	} else {
		v.Issues = append(v.Issues, fmtdata.Issue{Level: "error", Msg: "WSJT-X log folder not found. In WSJT-X use File > Open log directory, then choose that folder here."})
		return v
	}
	p := filepath.Join(v.Dir, "fmt.all")
	if fi, err := os.Stat(p); err == nil {
		v.FmtExists = true
		v.FmtAgeSec = time.Since(fi.ModTime()).Seconds()
		if f, err := a.parseCached(p); err == nil {
			v.FmtLines = len(f.Lines)
			if n := len(f.Lines); n > 8 {
				v.Last = f.Lines[n-8:]
			} else {
				v.Last = f.Lines
			}
			seen := map[int]bool{}
			for _, l := range f.Lines {
				if !seen[l.KHz] {
					seen[l.KHz] = true
					v.FmtKHz = append(v.FmtKHz, l.KHz)
				}
			}
		}
	}
	v.CalA, v.CalB, v.IniFound = readWSJTXCal(v.Dir)
	if v.CalA != 0 || v.CalB != 0 {
		v.Issues = append(v.Issues, fmtdata.Issue{Level: "error", Msg: fmt.Sprintf(
			"WSJT-X has a calibration applied (intercept %.2f Hz, slope %.3f ppm). WSJT-X then moves the radio away from the dial it shows. Set both to 0 in WSJT-X Settings > Frequencies; this app applies the correction itself.", v.CalA, v.CalB)})
	}
	return v
}

// readWSJTXCal reads the calibration WSJT-X applies to the rig frequency.
func readWSJTXCal(dir string) (a, b float64, found bool) {
	m, _ := filepath.Glob(filepath.Join(dir, "WSJT-X*.ini"))
	if len(m) == 0 {
		return
	}
	fh, err := os.Open(m[0])
	if err != nil {
		return
	}
	defer fh.Close()
	found = true
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		k, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "CalibrationIntercept":
			a, _ = strconv.ParseFloat(val, 64)
		case "CalibrationSlopePPM":
			b, _ = strconv.ParseFloat(val, 64)
		}
	}
	return
}

func (a *App) loadSlot(sl session.Slot) (*fmtdata.File, error) {
	if sl.File == "" {
		return nil, nil
	}
	f, err := a.parseCached(a.sess.Path(sl.File))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", sl.File, err)
	}
	return f, nil
}

func (a *App) parseCached(p string) (*fmtdata.File, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if c, ok := a.cache[p]; ok && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.f, c.err
	}
	f, err := fmtdata.ParseFile(p)
	a.cache[p] = cached{fi.ModTime(), fi.Size(), f, err}
	return f, err
}

// ---------------------------------------------------------------------------
// Files

// CaptureSlot copies WSJT-X's fmt.all into a slot, then renames fmt.all so
// the next measurement starts in an empty file. With force false it refuses
// to replace a file already in the slot.
func (a *App) CaptureSlot(id string, force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sl := a.sess.SlotFor(id)
	if sl == nil {
		return "", fmt.Errorf("unknown slot %s", id)
	}
	if sl.File != "" && !force {
		return "", fmt.Errorf("REPLACE")
	}
	src := filepath.Join(a.sess.WSJTXDir, "fmt.all")
	f, err := fmtdata.ParseFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("there is no fmt.all in %s. Is the Measure box checked in WSJT-X, and is this the right folder?", a.sess.WSJTXDir)
		}
		return "", err
	}
	if len(f.Lines) == 0 {
		return "", fmt.Errorf("fmt.all has no measurements. Is the Measure box checked in WSJT-X?")
	}
	if err := a.storeLocked(id, sl, src); err != nil {
		return "", err
	}
	a.recordRefLocked(sl, f)
	bak := filepath.Join(a.sess.WSJTXDir, fmt.Sprintf("fmt-%s-%s.bak", id, time.Now().UTC().Format("20060102-150405")))
	if err := os.Rename(src, bak); err != nil {
		a.src.Warn("captured, but could not rename fmt.all: %v", err)
		return fmt.Sprintf("Captured %d readings, but fmt.all could not be cleared (%v). Rename it by hand before the next run.", len(f.Lines), err), nil
	}
	a.src.Info("captured %s (%d readings); fmt.all moved to %s", id, len(f.Lines), filepath.Base(bak))
	return fmt.Sprintf("Captured %d readings (a dated copy is in the session's Archive folder). fmt.all was moved to %s so the next run starts clean.", len(f.Lines), filepath.Base(bak)), nil
}

// LoadSlot asks for a saved file and copies it into a slot.
func (a *App) LoadSlot(id string) (string, error) {
	a.mu.Lock()
	dir := a.sess.WSJTXDir
	a.mu.Unlock()
	p, err := wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:            "Choose the measurement file",
		DefaultDirectory: dir,
		Filters: []wruntime.FileFilter{
			{DisplayName: "FreqCal files (*.all;*.bak;*.txt)", Pattern: "*.all;*.bak;*.txt"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
	if err != nil || p == "" {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	sl := a.sess.SlotFor(id)
	if sl == nil {
		return "", fmt.Errorf("unknown slot %s", id)
	}
	f, err := fmtdata.ParseFile(p)
	if err != nil {
		return "", err
	}
	if len(f.Lines) == 0 {
		return "", fmt.Errorf("%s has no FreqCal measurements", filepath.Base(p))
	}
	// Refuse a file that is already in another slot.
	for _, other := range a.allSlotIDs() {
		if other == id {
			continue
		}
		if o := a.sess.SlotFor(other); o.File != "" {
			if of, err := a.parseCached(a.sess.Path(o.File)); err == nil && of.SHA == f.SHA {
				return "", fmt.Errorf("that file is already loaded as %s", a.slotTitles([]string{other})[0])
			}
		}
	}
	if err := a.storeLocked(id, sl, p); err != nil {
		return "", err
	}
	return fmt.Sprintf("Loaded %d readings from %s (a dated copy is in the session's Archive folder).", len(f.Lines), filepath.Base(p)), nil
}

func (a *App) storeLocked(id string, sl *session.Slot, src string) error {
	name := a.sess.SlotFileName(id)
	dst := a.sess.Path(name)
	if err := os.MkdirAll(a.sess.Dir(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		old := strings.TrimSuffix(dst, ".all") + "-replaced-" + time.Now().UTC().Format("20060102-150405") + ".all"
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	// Keep a never-overwritten copy of every file processed, named by the UTC
	// time it came in, so the original readings survive re-captures and edits.
	arch := filepath.Join(a.sess.Dir(), "Archive")
	if err := os.MkdirAll(arch, 0o755); err == nil {
		an := time.Now().UTC().Format("20060102-150405Z") + "_" + name
		if err := copyFile(src, filepath.Join(arch, an)); err != nil {
			a.src.Warn("could not archive %s: %v", name, err)
		} else {
			a.src.Info("archived %s as Archive\\%s", filepath.Base(src), an)
		}
	} else {
		a.src.Warn("could not create the archive folder: %v", err)
	}
	*sl = session.Slot{File: name, Source: src, CapturedAt: time.Now().UTC().Format(time.RFC3339)}
	if r := a.sess.Run(id); r != nil {
		r.Overrides = nil
	}
	a.saveLocked()
	return nil
}

// ClearSlot removes a slot's file from the session (it is kept on disk).
func (a *App) ClearSlot(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sl := a.sess.SlotFor(id); sl != nil && sl.File != "" {
		p := a.sess.Path(sl.File)
		_ = os.Rename(p, strings.TrimSuffix(p, ".all")+"-removed-"+time.Now().UTC().Format("20060102-150405")+".all")
		*sl = session.Slot{}
		a.saveLocked()
	}
}

// ClearFmtAll renames WSJT-X's fmt.all so measurements start fresh.
func (a *App) ClearFmtAll() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	src := filepath.Join(a.sess.WSJTXDir, "fmt.all")
	if _, err := os.Stat(src); err != nil {
		return "fmt.all is already empty.", nil
	}
	bak := filepath.Join(a.sess.WSJTXDir, "fmt-cleared-"+time.Now().UTC().Format("20060102-150405")+".bak")
	if err := os.Rename(src, bak); err != nil {
		return "", err
	}
	a.src.Info("fmt.all moved to %s", filepath.Base(bak))
	return "fmt.all moved to " + filepath.Base(bak) + ".", nil
}

// ---------------------------------------------------------------------------
// Settings and entries

// SetDial records the dial for a run; it returns how the text was read.
func (a *App) SetDial(id, text string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.sess.Run(id)
	if r == nil {
		return "", fmt.Errorf("unknown run %s", id)
	}
	r.DialText = text
	if strings.TrimSpace(text) == "" {
		r.DialHz = 0
		a.saveLocked()
		return "", nil
	}
	hz, err := session.ParseDial(text)
	if err != nil {
		r.DialHz = 0
		a.saveLocked()
		return "", err
	}
	r.DialHz = hz
	a.saveLocked()
	return "= " + hzStr(hz), nil
}

// UseRadioDial copies the controlled slice's frequency into a run's dial.
func (a *App) UseRadioDial(id string) (string, error) {
	sl, err := a.flexSlice()
	if err != nil {
		return "", err
	}
	if sl.Mode != "DIGU" && sl.Mode != "USB" {
		return "", fmt.Errorf("slice %s is in %s; set DIGU first", sl.Letter, sl.Mode)
	}
	text := fmt.Sprintf("%.3f", float64(sl.FreqHz)/1000)
	a.mu.Lock()
	if r := a.sess.Run(id); r != nil {
		r.RadioDialHz = sl.FreqHz
	}
	a.mu.Unlock()
	return a.SetDial(id, text)
}

// SetCarrier records the operator's estimate of a carrier (kHz text).
func (a *App) SetCarrier(id, text string) error {
	hz, err := session.ParseDial(text)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.sess.Run(id); r != nil {
		r.CarrierHz = hz
		a.saveLocked()
	}
	return nil
}

// StartPractice sets up a practice run on a steady carrier (an AM broadcast
// station or WWV): it clears the previous practice, optionally starts a fresh
// fmt.all, and schedules a one-minute measuring window leadSec from now.
func (a *App) StartPractice(freqText string, leadSec int, freshFmt bool) (string, error) {
	hz, err := session.ParseDial(freqText)
	if err != nil {
		return "", err
	}
	if leadSec < 30 || leadSec > 3600 {
		return "", fmt.Errorf("the lead time must be between 30 seconds and an hour")
	}
	msg := ""
	if freshFmt {
		if msg, err = a.ClearFmtAll(); err != nil {
			return "", err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := &a.sess.Practice
	if p.Slot.File != "" {
		old := a.sess.Path(p.Slot.File)
		_ = os.Rename(old, strings.TrimSuffix(old, ".all")+"-removed-"+time.Now().UTC().Format("20060102-150405")+".all")
	}
	*p = session.Run{ID: "practice", Station: "Practice", NominalKHz: hz / 1000, CarrierHz: hz,
		KeyDown: time.Now().UTC().Add(time.Duration(leadSec) * time.Second).Truncate(time.Second),
		MinSNR:  p.MinSNR, TrimEdges: p.TrimEdges}
	if p.MinSNR == 0 {
		p.MinSNR, p.TrimEdges = 20, true
	}
	a.saveLocked()
	a.src.Info("practice on %s, measuring from %s UTC", khzStr(hz), p.KeyDown.Format("15:04:05"))
	return strings.TrimSpace(fmt.Sprintf("Practice on %s; measuring starts at %s UTC. %s", khzStr(hz), p.KeyDown.Format("15:04:05"), msg)), nil
}

// CancelPractice removes the practice schedule (its file is kept).
func (a *App) CancelPractice() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.Practice.KeyDown = time.Time{}
	a.saveLocked()
}

type RunEdit struct {
	Station    string  `json:"station"`
	Band       string  `json:"band"`
	NominalKHz float64 `json:"nominal_khz"`
	KeyDown    string  `json:"key_down"` // "2026-11-13 02:34" UTC
}

// EditRun changes a run's schedule entry.
func (a *App) EditRun(id string, e RunEdit) error {
	t, err := time.Parse("2006-01-02 15:04", strings.TrimSpace(strings.Replace(e.KeyDown, "T", " ", 1)))
	if err != nil {
		return fmt.Errorf("key-down time must look like 2026-11-13 02:34 (UTC)")
	}
	if e.NominalKHz < 1700 || e.NominalKHz > 30000 {
		return fmt.Errorf("frequency must be in kHz, e.g. 7064")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.sess.Run(id)
	if r == nil {
		return fmt.Errorf("unknown run %s", id)
	}
	if r.NominalKHz != e.NominalKHz {
		r.CarrierHz = e.NominalKHz * 1000
	}
	r.Station, r.Band, r.NominalKHz, r.KeyDown = strings.TrimSpace(e.Station), strings.TrimSpace(e.Band), e.NominalKHz, t.UTC()
	a.saveLocked()
	return nil
}

func (a *App) SetRunOptions(id string, minSNR float64, trim bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.sess.Run(id); r != nil {
		r.MinSNR, r.TrimEdges = minSNR, trim
		a.saveLocked()
	}
}

// ToggleRunLine forces a reading in or out; state "auto" clears the choice.
func (a *App) ToggleRunLine(id string, n int, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.sess.Run(id)
	if r == nil {
		return
	}
	if r.Overrides == nil {
		r.Overrides = map[int]bool{}
	}
	switch state {
	case "use":
		r.Overrides[n] = true
	case "drop":
		r.Overrides[n] = false
	default:
		delete(r.Overrides, n)
	}
	a.saveLocked()
}

func (a *App) ResetRunLines(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.sess.Run(id); r != nil {
		r.Overrides = nil
		a.saveLocked()
	}
}

func (a *App) SetCalSNR(id string, minSNR float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.sess.Cal(id); c != nil {
		c.MinSNR = minSNR
		a.saveLocked()
	}
}

// SetCalStation includes or excludes a calibration station; "auto" resets.
func (a *App) SetCalStation(id string, khz int, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.sess.Cal(id)
	if c == nil {
		return
	}
	if c.Stations == nil {
		c.Stations = map[int]bool{}
	}
	switch state {
	case "use":
		c.Stations[khz] = true
	case "drop":
		c.Stations[khz] = false
	default:
		delete(c.Stations, khz)
	}
	a.saveLocked()
}

func (a *App) SetCalMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.CalMode = mode
	a.saveLocked()
}

func (a *App) SetCallsign(c string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.Callsign = strings.ToUpper(strings.TrimSpace(c))
	a.saveLocked()
}

// SetAutoCapture turns automatic capture one minute after key-up on or off.
func (a *App) SetAutoCapture(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.AutoCapture = on
	a.saveLocked()
}

func (a *App) SetVoice(on bool, volume int, beeps bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.Voice, a.sess.Volume, a.sess.Beeps = on, volume, beeps
	a.saveLocked()
}

// Speak says text with the Windows voice (if voice prompts are on).
func (a *App) Speak(text string) {
	a.mu.Lock()
	on, vol := a.sess.Voice, a.sess.Volume
	a.mu.Unlock()
	if on {
		a.src.Info("voice: %s", text)
		a.voice.Say(text, vol)
	}
}

func (a *App) SetWSJTXDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.WSJTXDir = dir
	a.saveLocked()
}

func (a *App) BrowseWSJTXDir() (string, error) {
	a.mu.Lock()
	cur := a.sess.WSJTXDir
	a.mu.Unlock()
	p, err := wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{Title: "WSJT-X log folder (File > Open log directory)", DefaultDirectory: cur})
	if err != nil || p == "" {
		return "", err
	}
	a.SetWSJTXDir(p)
	return p, nil
}

// NewSession starts an empty session in a new folder.
func (a *App) NewSession(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("give the session a name")
	}
	dir := session.DefaultDir(name)
	if _, err := os.Stat(filepath.Join(dir, "session.json")); err == nil {
		return "", fmt.Errorf("a session called %q already exists; use Open", name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.sess
	s := session.Default()
	s.Name, s.Callsign, s.WSJTXDir, s.Flex = name, old.Callsign, old.WSJTXDir, old.Flex
	s.Voice, s.Volume, s.Beeps, s.CalMode = old.Voice, old.Volume, old.Beeps, old.CalMode
	s.AutoCapture = old.AutoCapture
	s.Reference, s.ExtDisciplined = old.Reference, old.ExtDisciplined
	// Keep the schedule the operator may have edited.
	for i := range s.Runs {
		if i < len(old.Runs) {
			o := old.Runs[i]
			s.Runs[i].Station, s.Runs[i].Band, s.Runs[i].NominalKHz, s.Runs[i].KeyDown = o.Station, o.Band, o.NominalKHz, o.KeyDown
			s.Runs[i].CarrierHz = o.NominalKHz * 1000
		}
	}
	s.SetDir(dir)
	a.sess = s
	a.saveLocked()
	return dir, nil
}

// OpenSession opens an existing session folder.
func (a *App) OpenSession() (string, error) {
	p, err := wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "Open a session folder", DefaultDirectory: sessionsRoot(),
	})
	if err != nil || p == "" {
		return "", err
	}
	s, err := session.Load(p)
	if err != nil {
		return "", fmt.Errorf("%s is not a session folder (%v)", p, err)
	}
	a.mu.Lock()
	a.sess = s
	a.saveLocked()
	a.mu.Unlock()
	a.restartFlex()
	return p, nil
}

func (a *App) OpenSessionFolder() {
	a.mu.Lock()
	d := a.sess.Dir()
	a.mu.Unlock()
	_ = exec.Command("explorer", d).Start()
}

// ---------------------------------------------------------------------------
// Report

// Report builds the results text and saves it in the session folder.
func (a *App) Report() (string, error) {
	st := a.GetState()
	var b strings.Builder
	s := st.Session
	fmt.Fprintf(&b, "ARRL Frequency Measuring Test - %s\n", s.Name)
	if s.Callsign != "" {
		fmt.Fprintf(&b, "Callsign: %s\n", s.Callsign)
	}
	fmt.Fprintf(&b, "Prepared by FMT Assistant %s on %s UTC\n\n", version, time.Now().UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "RESULTS\n")
	for _, r := range st.Runs {
		if r.FinalHz != nil {
			flag := ""
			if !r.Ready {
				flag = "   (CHECK: has unresolved problems)"
			}
			fmt.Fprintf(&b, "  %-12s %15s Hz%s\n", r.Title, fmt.Sprintf("%.2f", *r.FinalHz), flag)
		} else {
			fmt.Fprintf(&b, "  %-12s   not available\n", r.Title)
		}
	}
	fmt.Fprintf(&b, "\nCALIBRATION (mode: %s)\n", s.CalMode)
	for _, c := range st.Cals {
		if c.Result == nil || !c.Result.OK {
			fmt.Fprintf(&b, "  %s: none\n", c.Title)
			continue
		}
		cr := c.Result
		fmt.Fprintf(&b, "  %s (%s-%s UTC, file %s)\n", c.Title, cr.First, cr.Last, c.Cal.Slot.File)
		fmt.Fprintf(&b, "    Slope %.4f +/- %.4f ppm, Intercept %.3f +/- %.3f Hz, N %d, StdDev %.3f Hz\n", cr.B, cr.SigmaB, cr.A, cr.SigmaA, cr.NFit, cr.RMS)
		fmt.Fprintf(&b, "    Reference: %s\n", refReportLine(c.Cal.Slot.Ref))
		for _, stn := range cr.Stations {
			use := "used"
			if !stn.Included {
				use = "not used"
			}
			fmt.Fprintf(&b, "    %-34s n=%-3d DF %+8.3f Hz  sd %.3f  resid %+.3f  %s\n", stn.Label, stn.N, stn.MeanDF, stn.SD, stn.Resid, use)
		}
	}
	fmt.Fprintf(&b, "\nRUN DETAILS\n")
	for _, r := range st.Runs {
		fmt.Fprintf(&b, "  %s (key-down %s UTC, file %s)\n", r.Title, r.Run.KeyDown.Format("15:04"), r.Run.Slot.File)
		if r.Result != nil && r.Result.N > 0 {
			rr := r.Result
			fmt.Fprintf(&b, "    Dial %s  mean fMeas %.3f Hz (n=%d, sd %.3f, sem %.3f, %s-%s UTC)\n",
				hzStr(r.Run.DialHz), rr.MeanFMeas, rr.N, rr.SD, rr.SEM, rr.BlockFrom, rr.BlockTo)
			if r.CorrHz != nil {
				fmt.Fprintf(&b, "    Correction A+B*f %+.3f Hz\n", *r.CorrHz)
			}
			if r.FinalHz != nil {
				formula := "dial + fMeas - correction"
				if r.SliceErrHz != nil {
					formula = "dial + slice tuning error + fMeas - correction"
				}
				fmt.Fprintf(&b, "    Carrier = %s = %.2f Hz\n", formula, *r.FinalHz)
			}
			fmt.Fprintf(&b, "    Reference: %s\n", refReportLine(r.Run.Slot.Ref))
			if r.SliceErrHz != nil {
				fmt.Fprintf(&b, "    Slice tuning error %+.1f mHz (added to the dial)\n", *r.SliceErrHz*1000)
			}
		}
		for _, is := range append(append([]fmtdata.Issue{}, r.Issues...), resIssues(r.Result)...) {
			if is.Level != "info" {
				fmt.Fprintf(&b, "    %s: %s\n", strings.ToUpper(is.Level), is.Msg)
			}
		}
	}
	text := b.String()
	p := filepath.Join(st.Dir, "FMT results.txt")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(text, "\n", "\r\n")), 0o644); err != nil {
		return text, err
	}
	a.src.Info("report saved to %s", p)
	return text, nil
}

// ---------------------------------------------------------------------------
// FlexRadio (optional)

func (a *App) startFlexLocked() {
	f := a.sess.Flex
	if !f.Enabled {
		return
	}
	a.disc.Start()
	if f.IP != "" {
		a.radio.Connect(f.IP, "", f.Station)
	}
}

func (a *App) restartFlex() {
	a.radio.Disconnect()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startFlexLocked()
}

// SetFlexEnabled turns FlexRadio control on or off.
func (a *App) SetFlexEnabled(on bool) {
	a.mu.Lock()
	was := a.sess.Flex.Enabled
	a.sess.Flex.Enabled = on
	if was && !on {
		a.logRefLocked(flex.Reference{}) // Setup's reference applies from now
	}
	a.saveLocked()
	a.mu.Unlock()
	if on && !was {
		a.restartFlex()
	} else if !on {
		a.radio.Disconnect()
		a.disc.Stop()
	}
}

// FlexConnect connects to a discovered radio (by serial) or an IP address.
func (a *App) FlexConnect(serialOrIP string) error {
	ip, serial := serialOrIP, ""
	if info, ok := a.disc.Find(serialOrIP); ok {
		ip, serial = info.IP, info.Serial
	}
	if ip == "" {
		return fmt.Errorf("choose a radio or type its IP address")
	}
	a.mu.Lock()
	a.sess.Flex.IP, a.sess.Flex.Serial = ip, serial
	station := a.sess.Flex.Station
	a.saveLocked()
	a.mu.Unlock()
	a.radio.Connect(ip, "", station)
	return nil
}

func (a *App) FlexDisconnect() {
	a.radio.Disconnect()
	a.mu.Lock()
	a.sess.Flex.IP = ""
	a.saveLocked()
	a.mu.Unlock()
}

func (a *App) FlexSetStation(name string) {
	a.mu.Lock()
	a.sess.Flex.Station = name
	a.saveLocked()
	a.mu.Unlock()
	a.radio.SetStation(name)
}

func (a *App) FlexSetSlice(letter string) {
	a.mu.Lock()
	a.sess.Flex.Slice = letter
	a.saveLocked()
	a.mu.Unlock()
	a.changed()
}

func (a *App) FlexSetAutoTune(on bool) {
	a.mu.Lock()
	a.sess.Flex.AutoTune = on
	a.saveLocked()
	a.mu.Unlock()
}

// FlexTune sets the controlled slice to DIGU at dialHz.
func (a *App) FlexTune(dialHz float64) (string, error) {
	sl, err := a.flexSlice()
	if err != nil {
		return "", err
	}
	if dialHz < 500e3 || dialHz > 30e6 {
		return "", fmt.Errorf("dial %.0f Hz is out of range", dialHz)
	}
	hz := int64(math.Round(dialHz))
	a.src.Info("tuning slice %s to DIGU %s", sl.Letter, hzStr(float64(hz)))
	if err := a.radio.TuneDigital(sl.Index, hz); err != nil {
		a.src.Error("tuning slice %s: %v", sl.Letter, err)
		return "", err
	}
	return fmt.Sprintf("Slice %s set to DIGU, dial %s (confirmed by the radio).", sl.Letter, khzStr(float64(hz))), nil
}

func (a *App) flexSlice() (flex.Slice, error) {
	a.mu.Lock()
	f := a.sess.Flex
	a.mu.Unlock()
	if !f.Enabled {
		return flex.Slice{}, fmt.Errorf("FlexRadio control is off")
	}
	snap := a.radio.Snapshot()
	if !snap.Connected {
		return flex.Slice{}, fmt.Errorf("not connected to a FlexRadio")
	}
	if snap.BoundHandle == 0 {
		return flex.Slice{}, fmt.Errorf("choose the SmartSDR station to control")
	}
	if f.Slice == "" {
		return flex.Slice{}, fmt.Errorf("choose the slice to control")
	}
	sl, ok := snap.SliceByLetter(f.Slice)
	if !ok {
		return flex.Slice{}, fmt.Errorf("slice %s is not open on station %s", f.Slice, snap.BoundStation)
	}
	return sl, nil
}

func (a *App) flexView() FlexView {
	v := FlexView{Enabled: a.sess.Flex.Enabled}
	if !v.Enabled {
		return v
	}
	v.Radios = a.disc.List()
	v.Snap = a.radio.Snapshot()
	v.Issues = append(v.Issues, flexRefIssues(v.Snap.Ref, a.sess.ExtDisciplined, time.Now())...)
	if sl, ok := v.Snap.SliceByLetter(a.sess.Flex.Slice); ok {
		v.Slice = &sl
		if sl.Mode != "DIGU" {
			v.Issues = append(v.Issues, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf("Slice %s is in %s, not DIGU.", sl.Letter, sl.Mode)})
		}
		if sl.Mode == "DIGU" && sl.DiguOffset != 0 {
			v.Issues = append(v.Issues, fmtdata.Issue{Level: "warn", Msg: fmt.Sprintf("Slice %s has a DIGU offset of %d Hz. Set radio clears it so the slice frequency is the true dial.", sl.Letter, sl.DiguOffset)})
		}
		if sl.RITOn && sl.RITFreq != 0 {
			v.Issues = append(v.Issues, fmtdata.Issue{Level: "error", Msg: fmt.Sprintf("RIT is on (%+d Hz) on slice %s. Turn it off; it shifts what you hear.", sl.RITFreq, sl.Letter)})
		}
	}
	return v
}

// onSliceFreq logs dial changes of the controlled slice.
func (a *App) onSliceFreq(letter string, hz int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.EqualFold(letter, a.sess.Flex.Slice) {
		return
	}
	a.freqLog = append(a.freqLog, freqEntry{At: time.Now().UTC(), Hz: hz})
	if len(a.freqLog) > 5000 {
		a.freqLog = a.freqLog[len(a.freqLog)-5000:]
	}
	a.src.Debug("slice %s dial %s", letter, hzStr(float64(hz)))
	// While a run's carrier is on, remember the dial for the cross-check.
	now := time.Now().UTC()
	runs := []*session.Run{&a.sess.Practice}
	for i := range a.sess.Runs {
		runs = append(runs, &a.sess.Runs[i])
	}
	for _, r := range runs {
		if !now.Before(r.KeyDown) && now.Before(r.KeyUp()) {
			r.RadioDialHz = hz
			a.saveLocked()
		}
	}
}

// NoteRadioDial records the radio's dial for a run (called at key-down).
// It also reads the slice's tuning error, a few millihertz the radio adds to
// the dial; that's read only, and changes after every retune.
func (a *App) NoteRadioDial(id string) {
	sl, err := a.flexSlice()
	if err != nil {
		return
	}
	errMHz, eerr := a.radio.SliceError(sl.Index, 3*time.Second)
	if eerr != nil {
		a.src.Warn("slice tuning error: %v", eerr)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.sess.Run(id); r != nil {
		r.RadioDialHz = sl.FreqHz
		r.SliceErrMHz, r.SliceErrDialHz = nil, 0
		if eerr == nil {
			r.SliceErrMHz, r.SliceErrDialHz = &errMHz, sl.FreqHz
			a.src.Info("%s: slice %s tuning error %+.3f mHz at %s", id, sl.Letter, errMHz, khzStr(float64(sl.FreqHz)))
		}
		a.saveLocked()
	}
}

// sliceErrApplied is the slice tuning error, in Hz, to add to a run's dial:
// only when it was read on the dial the operator entered.
func sliceErrApplied(r *session.Run) *float64 {
	if r.SliceErrMHz == nil || r.DialHz == 0 || r.SliceErrDialHz != int64(math.Round(r.DialHz)) {
		return nil
	}
	v := *r.SliceErrMHz / 1000
	return &v
}

// ---------------------------------------------------------------------------
// helpers

func (a *App) allSlotIDs() []string {
	ids := []string{"cal_before"}
	for _, r := range a.sess.Runs {
		ids = append(ids, r.ID)
	}
	return append(ids, "cal_after", "practice_cal", "practice")
}

func (a *App) slotTitles(ids []string) []string {
	var out []string
	for _, id := range ids {
		switch id {
		case "cal_before":
			out = append(out, "Calibration before")
		case "cal_after":
			out = append(out, "Calibration after")
		case "practice":
			out = append(out, "Practice")
		case "practice_cal":
			out = append(out, "Practice calibration")
		default:
			if r := a.sess.Run(id); r != nil {
				out = append(out, r.Title())
			} else {
				out = append(out, id)
			}
		}
	}
	return out
}

func resIssues(r *fmtdata.RunResult) []fmtdata.Issue {
	if r == nil {
		return nil
	}
	return r.Issues
}

func countErrors(is []fmtdata.Issue) int {
	n := 0
	for _, i := range is {
		if i.Level == "error" {
			n++
		}
	}
	return n
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func secOfDay(t time.Time) int {
	t = t.UTC()
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

func hmsSec(s string) (int, bool) {
	t, err := time.Parse("15:04:05", s)
	if err != nil {
		return 0, false
	}
	return secOfDay(t), true
}

// khzStr formats Hz as "7,062.500 kHz".
func khzStr(hz float64) string {
	k := hz / 1000
	whole := int64(k)
	frac := k - float64(whole)
	return fmt.Sprintf("%s.%03d kHz", commas(whole), int64(math.Round(frac*1000))%1000)
}

// hzStr formats Hz as "7,062,500.00 Hz".
func hzStr(hz float64) string {
	whole := int64(math.Floor(hz))
	cents := int64(math.Round((hz - float64(whole)) * 100))
	if cents == 100 {
		whole, cents = whole+1, 0
	}
	return fmt.Sprintf("%s.%02d Hz", commas(whole), cents)
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------------------
// app config: remembers the last session folder

func configPath() string {
	return filepath.Join(appDataDir(), "config.json")
}

// appDataDir is %APPDATA%\FMTAssistant, or <FMTASSISTANT_HOME>\AppData for a
// development copy kept apart from the real settings and sessions.
func appDataDir() string {
	if h := session.Home(); h != "" {
		return filepath.Join(h, "AppData")
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "FMTAssistant")
}

func loadLastDir() string {
	var c struct {
		SessionDir string `json:"session_dir"`
	}
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c.SessionDir
}

func saveLastDir(dir string) {
	p := configPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(map[string]string{"session_dir": dir})
	_ = os.WriteFile(p, b, 0o644)
}

func sessionsRoot() string {
	r, _ := session.SessionsRoot()
	return r
}
