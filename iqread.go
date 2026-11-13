package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/FMTAssistant/internal/iq"
)

// Reading IQ recordings back: list them, and measure the carrier in one
// directly from the IQ, as a cross-check on WSJT-X. Nothing here changes the
// answer.

// IQFileView is one recording in the session's IQ folder.
type IQFileView struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	Bytes     int64     `json:"bytes"`
	Modified  time.Time `json:"modified"`
	Meta      *iq.Meta  `json:"meta"`
	Recording bool      `json:"recording"` // still being written
}

// ListIQFiles lists the recordings in the session's IQ folder, newest first.
func (a *App) ListIQFiles() ([]IQFileView, error) {
	a.mu.Lock()
	dir := filepath.Join(a.sess.Dir(), "IQ")
	a.mu.Unlock()
	cur := ""
	if c := a.iq.cur.Load(); c != nil {
		cur = c.rec.Path()
	}
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []IQFileView{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []IQFileView{}
	for _, e := range ents {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".wav") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		v := IQFileView{Path: p, Name: e.Name(), Recording: p == cur}
		if fi, err := e.Info(); err == nil {
			v.Bytes, v.Modified = fi.Size(), fi.ModTime()
		}
		if m, ok, err := iq.LoadMeta(p); err == nil && ok {
			m.Events, m.Gaps = nil, nil // not needed for the list
			v.Meta = &m
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// BrowseIQFile asks for a WAV anywhere (e.g. one copied from another PC).
func (a *App) BrowseIQFile() (string, error) {
	a.mu.Lock()
	dir := filepath.Join(a.sess.Dir(), "IQ")
	a.mu.Unlock()
	return wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:            "Choose an IQ recording",
		DefaultDirectory: dir,
		Filters: []wruntime.FileFilter{
			{DisplayName: "IQ recordings (*.wav)", Pattern: "*.wav"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
}

// sameSpan averages the usable IQ segments that fall within the seconds
// WSJT-X's used readings cover (a reading's time is the end of its ~4.6 s
// window).
func sameSpan(v *IQAnalysisView, res *iq.Analysis, rv *RunView) {
	var first, last = -1, -1
	for _, l := range rv.Result.Lines {
		if !l.Used {
			continue
		}
		if first < 0 {
			first = l.Sec
		}
		last = l.Sec
	}
	if first < 0 {
		return
	}
	day := rv.Run.KeyDown.UTC().Truncate(24 * time.Hour)
	at := func(sec int) time.Time {
		t := day.Add(time.Duration(sec) * time.Second)
		// across midnight, keep it next to key-down
		if d := t.Sub(rv.Run.KeyDown); d > 12*time.Hour {
			t = t.Add(-24 * time.Hour)
		} else if d < -12*time.Hour {
			t = t.Add(24 * time.Hour)
		}
		return t
	}
	from, to := at(first).Add(-4600*time.Millisecond), at(last)
	sum, n := 0.0, 0
	for _, s := range res.Segments {
		if s.OK && !s.T.Before(from) && !s.T.After(to) {
			sum += s.RFHz
			n++
		}
	}
	if n == 0 {
		return
	}
	m := sum / float64(n)
	v.SameSpanHz, v.SameSpanN = &m, n
	v.SameSpanFrom, v.SameSpanTo = from.Format("15:04:05"), to.Format("15:04:05")
}

// IQAnalyzeOptions are the operator's choices; zero means default.
type IQAnalyzeOptions struct {
	ExpectHz float64 `json:"expect_hz"` // RF frequency to look near
	SearchHz float64 `json:"search_hz"`
	MinSNR   float64 `json:"min_snr"`
	SegSec   float64 `json:"seg_sec"`
	Mirror   int     `json:"mirror"` // 0 detect, 1 as recorded, -1 swapped
	// RunID measures that run's key-down minute from this recording, even if
	// it was made for another step (e.g. one long recording of the night).
	RunID string `json:"run_id"`
}

// IQAnalysisView is the measurement with what it compares to.
type IQAnalysisView struct {
	Path    string       `json:"path"`
	Name    string       `json:"name"`
	Meta    *iq.Meta     `json:"meta"`
	A       *iq.Analysis `json:"a"`
	Title   string       `json:"title"`
	KeyDown *time.Time   `json:"key_down"`
	Partial bool         `json:"partial"` // still being recorded
	Elapsed float64      `json:"elapsed"` // seconds the analysis took
	// For a run: the app's answer from WSJT-X and the correction it used.
	AppFinalHz *float64 `json:"app_final_hz"`
	CorrHz     *float64 `json:"corr_hz"`
	IQFinalHz  *float64 `json:"iq_final_hz"` // IQ carrier with the same correction
	DiffHz     *float64 `json:"diff_hz"`     // IQ minus app
	WSJTXMean  *float64 `json:"wsjtx_mean"`  // WSJT-X mean fMeas, to compare with the IQ audio frequency
	// The IQ carrier over only the seconds WSJT-X measured, to compare with
	// the whole minute (WSJT-X may measure only part of it).
	SameSpanHz   *float64 `json:"same_span_hz"`
	SameSpanN    int      `json:"same_span_n"`
	SameSpanFrom string   `json:"same_span_from"`
	SameSpanTo   string   `json:"same_span_to"`
	RunID        string   `json:"run_id"`
	// For a WWV recording: the standard frequency and how far off it reads.
	WWVHz    float64  `json:"wwv_hz"`
	WWVErrHz *float64 `json:"wwv_err_hz"`
	WWVPPM   *float64 `json:"wwv_ppm"`
}

// AnalyzeIQ measures the carrier in a recording.
func (a *App) AnalyzeIQ(path string, o IQAnalyzeOptions) (*IQAnalysisView, error) {
	if path == "" {
		return nil, fmt.Errorf("choose a recording")
	}
	t0 := time.Now()
	v := &IQAnalysisView{Path: path, Name: filepath.Base(path)}
	m, ok, err := iq.LoadMeta(path)
	if err != nil {
		return nil, err
	}
	var meta *iq.Meta
	if ok {
		meta = &m
		v.Meta = &m
		v.Title = m.Title
	}
	if c := a.iq.cur.Load(); c != nil && c.rec.Path() == path {
		v.Partial = true
	}
	opt := iq.Options{ExpectHz: o.ExpectHz, SearchHz: o.SearchHz, MinSNR: o.MinSNR, SegSec: o.SegSec, Mirror: o.Mirror}

	// A run's key-down minute is what gets averaged, as WSJT-X's is.
	var runID string
	if o.RunID != "" {
		runID = o.RunID
		opt.OnlyWindow = true
	} else if meta != nil {
		a.mu.Lock()
		if r := a.sess.Run(meta.Step); r != nil && !r.KeyDown.IsZero() {
			runID = r.ID
		}
		a.mu.Unlock()
	}
	var rv *RunView
	if runID != "" {
		st := a.GetState()
		for _, x := range append(st.Runs, st.Practice) {
			if x.Run.ID == runID {
				x := x
				rv = &x
			}
		}
		if rv == nil || rv.Run.KeyDown.IsZero() {
			return nil, fmt.Errorf("run %s has no key-down time", runID)
		}
		v.RunID = runID
		kd := rv.Run.KeyDown
		v.KeyDown = &kd
		opt.From, opt.To = kd.Add(5*time.Second), rv.KeyUp.Add(-5*time.Second)
		if o.RunID != "" {
			v.Title = rv.Title + " (from " + v.Name + ")"
		}
		// Look where WSJT-X found the carrier, if it did; else near the dial.
		if opt.ExpectHz == 0 {
			switch {
			case rv.Run.DialHz > 0 && rv.Result != nil && rv.Result.N > 0:
				opt.ExpectHz = rv.Run.DialHz + rv.Result.MeanFMeas
				if opt.SearchHz == 0 {
					opt.SearchHz = 20
				}
			case rv.Run.DialHz > 0:
				opt.ExpectHz = rv.Run.DialHz + 1500
			default:
				opt.ExpectHz = rv.Run.NominalKHz * 1000
				if opt.SearchHz == 0 {
					opt.SearchHz = 1500
				}
			}
		}
	}

	res, err := iq.Analyze(path, meta, opt)
	if err != nil {
		return nil, err
	}
	v.A = res
	v.Elapsed = time.Since(t0).Seconds()
	if res.Summary == nil {
		return v, nil
	}
	mean := res.Summary.MeanRFHz

	if rv != nil {
		v.CorrHz, v.AppFinalHz = rv.CorrHz, rv.FinalHz
		if rv.Result != nil && rv.Result.N > 0 {
			w := rv.Result.MeanFMeas
			v.WSJTXMean = &w
			sameSpan(v, res, rv)
		}
		if rv.CorrHz != nil {
			f := mean - *rv.CorrHz
			v.IQFinalHz = &f
			if rv.FinalHz != nil {
				d := f - *rv.FinalHz
				v.DiffHz = &d
			}
		}
	}
	for _, mhz := range []float64{2.5, 5, 10, 15, 20, 25} {
		if hz := mhz * 1e6; math.Abs(mean-hz) < 500 {
			e, ppm := mean-hz, (mean-hz)/hz*1e6
			v.WWVHz, v.WWVErrHz, v.WWVPPM = hz, &e, &ppm
		}
	}
	a.src.Info("IQ analysis of %s: %s, %d segments, %.4f Hz ± %.4f (%.1f s)",
		v.Name, res.Summary.Basis, res.Summary.N, mean, res.Summary.SEM, v.Elapsed)
	return v, nil
}
