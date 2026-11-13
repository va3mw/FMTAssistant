package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// IQ cross-check for the PDF report: for each run, the IQ recording that
// covers its key-down minute is measured, compared with the WSJT-X answer,
// and the report recommends a value to submit to 0.01 Hz.

// iqCheck is the IQ measurement of one run, or why there is none.
type iqCheck struct {
	View *IQAnalysisView
	Err  string
}

// recommendation is the value the report recommends for one run.
type recommendation struct {
	Hz      float64
	SEM     float64
	Basis   string // "WSJT-X and IQ (weighted)", "WSJT-X only", ...
	Short   string // for the summary table
	HasIQ   bool
	IQHz    float64 // IQ carrier with the same correction as WSJT-X
	IQSEM   float64
	WHz     float64 // WSJT-X carrier
	WSEM    float64
	Delta   float64 // IQ - WSJT-X
	Sigma   float64 // combined uncertainty of the difference
	Limit   float64 // agreement limit
	Agree   bool
	Warning string
}

const (
	semFloor   = 0.01 // Hz: neither method's SEM is taken below this
	agreeK     = 2.0  // agree if |delta| <= agreeK * combined sigma ...
	agreeFloor = 0.05 // ... or this many Hz, whichever is larger
)

// recommend works out what to submit for a run from its WSJT-X answer and,
// if there is one, its IQ cross-check.
func recommend(wHz, wSEM float64, iq *IQAnalysisView) recommendation {
	rc := recommendation{WHz: wHz, WSEM: math.Max(wSEM, semFloor), Hz: wHz, SEM: math.Max(wSEM, semFloor),
		Basis: "WSJT-X only (no IQ recording covers this run)", Short: "WSJT-X"}
	if iq == nil || iq.A == nil || iq.A.Summary == nil || iq.IQFinalHz == nil {
		return rc
	}
	s := iq.A.Summary
	if s.N < 3 {
		rc.Basis = fmt.Sprintf("WSJT-X only (the IQ has only %d usable segment(s))", s.N)
		return rc
	}
	rc.HasIQ = true
	rc.IQHz, rc.IQSEM = *iq.IQFinalHz, math.Max(s.SEM, semFloor)
	rc.Delta = rc.IQHz - rc.WHz
	rc.Sigma = math.Hypot(rc.WSEM, rc.IQSEM)
	rc.Limit = math.Max(agreeK*rc.Sigma, agreeFloor)
	rc.Agree = math.Abs(rc.Delta) <= rc.Limit
	if !rc.Agree {
		rc.Basis = "WSJT-X only: the IQ disagrees, check before submitting"
		rc.Short = "WSJT-X (check)"
		rc.Warning = fmt.Sprintf("IQ and WSJT-X differ by %+.3f Hz, more than the %.3f Hz they should agree within. Look at both charts before submitting.", rc.Delta, rc.Limit)
		return rc
	}
	ww, wi := 1/(rc.WSEM*rc.WSEM), 1/(rc.IQSEM*rc.IQSEM)
	rc.Hz = (rc.WHz*ww + rc.IQHz*wi) / (ww + wi)
	rc.SEM = 1 / math.Sqrt(ww+wi)
	rc.Basis = "WSJT-X and IQ agree: weighted average"
	rc.Short = "WSJT-X + IQ"
	return rc
}

// iqChecks measures each run from the IQ recording that covers its key-down
// minute: the run's own recording if there is one, else the shortest that
// covers it.
func (a *App) iqChecks(st State) map[string]*iqCheck {
	out := map[string]*iqCheck{}
	files, err := a.ListIQFiles()
	if err != nil || len(files) == 0 {
		return out
	}
	for _, rv := range st.Runs {
		if rv.Run.KeyDown.IsZero() || rv.FinalHz == nil {
			continue
		}
		kd := rv.Run.KeyDown
		type cand struct {
			path string
			own  bool
			dur  float64
		}
		var cs []cand
		for _, f := range files {
			m := f.Meta
			if m == nil || m.StartUTC.IsZero() || m.SampleRate == 0 || f.Recording {
				continue
			}
			dur := float64(m.Frames) / float64(m.SampleRate)
			end := m.StartUTC.Add(time.Duration(dur * float64(time.Second)))
			if m.StartUTC.After(kd.Add(5*time.Second)) || end.Before(kd.Add(55*time.Second)) {
				continue
			}
			cs = append(cs, cand{f.Path, m.Step == rv.Run.ID, dur})
		}
		if len(cs) == 0 {
			continue
		}
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].own != cs[j].own {
				return cs[i].own
			}
			return cs[i].dur < cs[j].dur
		})
		v, err := a.AnalyzeIQ(cs[0].path, IQAnalyzeOptions{RunID: rv.Run.ID})
		c := &iqCheck{View: v}
		if err != nil {
			c.Err = err.Error()
		}
		out[rv.Run.ID] = c
	}
	return out
}

// ---------------------------------------------------------------- PDF parts

func (r *pdfReport) recSummary() {
	var rows [][]string
	any := false
	for _, rv := range r.st.Runs {
		if rv.FinalHz == nil {
			rows = append(rows, []string{rv.Title, "-", "-", "-", "-", "-", "-"})
			continue
		}
		rc := r.rec[rv.Run.ID]
		iqs, d, ag := "-", "-", "-"
		if rc.HasIQ {
			any = true
			iqs, d = commaHz(rc.IQHz, 3), fmt.Sprintf("%+.3f", rc.Delta)
			ag = map[bool]string{true: "yes", false: "NO"}[rc.Agree]
		}
		rows = append(rows, []string{rv.Title, commaHz(rc.WHz, 3), iqs, d, ag, commaHz(rc.Hz, 2), rc.Short})
	}
	r.h2("IQ cross-check and recommendation")
	r.table([]string{"Run", "WSJT-X Hz", "IQ Hz (whole minute)", "IQ - WSJT-X", "Agree", "Submit (0.01 Hz)", "Basis"},
		[]float64{24, 32, 32, 19, 12, 31, 30}, []string{"L", "R", "R", "R", "C", "R", "L"}, rows,
		func(i int) bool { return r.st.Runs[i].FinalHz != nil })
	if any {
		r.note(fmt.Sprintf("IQ Hz is the carrier measured from the FlexRadio IQ recording over the whole key-down minute, with the same correction as WSJT-X. "+
			"The two agree when they differ by no more than %.0f x their combined uncertainty or %.2f Hz, whichever is larger; then the recommendation is their "+
			"uncertainty-weighted average (each SEM taken as at least %.2f Hz). If they disagree, the WSJT-X answer is recommended and flagged. Each run's section shows the working.",
			agreeK, agreeFloor, semFloor))
	} else {
		r.note("No IQ recording covers the runs, so the recommendation is the WSJT-X answer.")
	}
}

// iqSection is the cross-check working for one run.
func (r *pdfReport) iqSection(rv RunView) {
	rc := r.rec[rv.Run.ID]
	c := r.iq[rv.Run.ID]
	r.h2("IQ cross-check")
	switch {
	case c == nil:
		r.para("No IQ recording covers this run's key-down minute.")
	case c.Err != "":
		r.para("The IQ recording could not be measured: " + c.Err)
	case c.View == nil || c.View.A == nil || c.View.A.Summary == nil:
		r.para("The IQ recording shows no clear carrier near the WSJT-X answer during key-down.")
	}
	if c != nil && c.View != nil && c.View.A != nil {
		v, a := c.View, c.View.A
		rows := [][2]string{
			{"Recording", v.Name},
			{"IQ", fmt.Sprintf("%d kHz rate, panadapter centre %s Hz, I/Q %s", a.Rate/1000, commaHz(float64(a.CenterHz), 0),
				map[bool]string{true: "swapped", false: "as recorded"}[a.Mirrored])},
			{"Looked", fmt.Sprintf("%s Hz +/- %.0f Hz (where WSJT-X found the carrier), in %.0f s segments", commaHz(a.ExpectHz, 3), a.SearchHz, a.SegSec)},
		}
		if s := a.Summary; s != nil {
			rows = append(rows, [2]string{"Segments", fmt.Sprintf("%d used, %s to %s UTC (%s)", s.N, s.From.Format("15:04:05"), s.To.Format("15:04:05"), s.Basis)})
		}
		r.kv(rows)
		for _, n := range a.Notes {
			r.note(n)
		}
	}
	rows := []string{}
	if rc.HasIQ {
		v := c.View
		s := v.A.Summary
		corr := 0.0
		if v.CorrHz != nil {
			corr = *v.CorrHz
		}
		rows = append(rows,
			"IQ carrier = mean of the segments' carrier (each from an FFT peak refined by a phase fit)",
			fmt.Sprintf("           = %s Hz   SD %.4f Hz, SEM %.4f Hz, drift %+.4f Hz/min", commaHz(s.MeanRFHz, 4), s.SD, s.SEM, s.DriftHzMin),
			fmt.Sprintf("IQ answer  = IQ carrier - correction = %s - (%+.4f) = %s Hz", commaHz(s.MeanRFHz, 4), corr, commaHz(rc.IQHz, 4)))
		if v.SameSpanHz != nil {
			rows = append(rows,
				fmt.Sprintf("Same secs  = IQ over only %s-%s UTC (%d segments), as WSJT-X measured:", v.SameSpanFrom, v.SameSpanTo, v.SameSpanN),
				fmt.Sprintf("           = %s Hz; the whole minute is %+.4f Hz from it", commaHz(*v.SameSpanHz-corr, 4), s.MeanRFHz-*v.SameSpanHz))
		}
		rows = append(rows,
			fmt.Sprintf("Delta      = IQ - WSJT-X = %s - %s = %+.4f Hz", commaHz(rc.IQHz, 4), commaHz(rc.WHz, 4), rc.Delta),
			"Combined   = sqrt( SEM_wsjtx^2 + SEM_iq^2 )   (each at least 0.01 Hz)",
			fmt.Sprintf("           = sqrt( %.4f^2 + %.4f^2 ) = %.4f Hz", rc.WSEM, rc.IQSEM, rc.Sigma),
			fmt.Sprintf("Limit      = max( %.0f x %.4f, %.2f ) = %.4f Hz:  |delta| %.4f  ->  %s", agreeK, rc.Sigma, agreeFloor, rc.Limit, math.Abs(rc.Delta),
				map[bool]string{true: "AGREE", false: "DISAGREE"}[rc.Agree]))
		if rc.Agree {
			ww, wi := 1/(rc.WSEM*rc.WSEM), 1/(rc.IQSEM*rc.IQSEM)
			rows = append(rows,
				"Weighted   = ( W / SEM_w^2 + IQ / SEM_iq^2 ) / ( 1/SEM_w^2 + 1/SEM_iq^2 )",
				fmt.Sprintf("           = ( %s x %.1f + %s x %.1f ) / %.1f", commaHz(rc.WHz, 4), ww, commaHz(rc.IQHz, 4), wi, ww+wi),
				fmt.Sprintf("           = %s Hz  +/- %.4f Hz", commaHz(rc.Hz, 4), rc.SEM))
		}
	}
	rows = append(rows, fmt.Sprintf("Recommended to submit: %s Hz", commaHz(rc.Hz, 2)), "  "+rc.Basis)
	r.math(rows...)
	if rc.Warning != "" {
		r.note("CHECK: " + rc.Warning)
	}
	if !rc.HasIQ {
		return
	}

	// Chart: WSJT-X readings and IQ segments on the same carrier scale.
	run, res := rv.Run, rv.Result
	corr, se := 0.0, 0.0
	if rv.CorrHz != nil {
		corr = *rv.CorrHz
	}
	if rv.SliceErrHz != nil {
		se = *rv.SliceErrHz
	}
	kd := float64(secOfDay(run.KeyDown))
	var wpts, ipts []chartPt
	var ys []float64
	for _, l := range res.Lines {
		x := unwrap(l.Sec, kd)
		if !l.Used || x < kd-60 || x > kd+120 {
			continue
		}
		y := run.DialHz + se + l.FMeas - corr - rc.Hz
		wpts = append(wpts, chartPt{x: x, y: y, filled: true})
		ys = append(ys, y)
	}
	for _, s := range c.View.A.Segments {
		if s.T.IsZero() {
			continue
		}
		x := kd + s.T.Sub(run.KeyDown).Seconds()
		if x < kd-60 || x > kd+120 {
			continue
		}
		y := s.RFHz - corr - rc.Hz
		ipts = append(ipts, chartPt{x: x, y: y, filled: s.OK && s.In})
		if s.OK && s.In {
			ys = append(ys, y)
		}
	}
	y0, y1 := yRange(ys, 0.1)
	zero := 0.0
	r.chart(chartOpts{title: fmt.Sprintf("WSJT-X readings and IQ segments, Hz from the recommended %s Hz", commaHz(rc.Hz, 2)), ylabel: "Hz from recommended",
		x0: kd - 60, x1: kd + 120, y0: y0, y1: y1, shade: [][2]float64{{kd, kd + 60}}, hline: &zero, legend: true, height: 56},
		[]chartSeries{{name: "WSJT-X readings used", rgb: palette[0], pts: wpts}, {name: "IQ segments (hollow: left out)", rgb: palette[1], pts: ipts}})
}
