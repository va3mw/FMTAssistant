package main

import (
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
	"github.com/va3mw/FMTAssistant/internal/iq"
	"github.com/va3mw/FMTAssistant/internal/session"
)

// The PDF report: every result with all of its arithmetic, the calibration
// fits, and charts of the readings over time (WWV Doppler during the
// calibrations, the station's Doppler during each carrier), so a test night
// can be shared and checked by someone else.

// SavePDFReport writes the report to the session folder and returns its path.
func (a *App) SavePDFReport() (string, error) {
	st := a.GetState()
	a.mu.Lock()
	snap := a.radio.Snapshot()
	flexOn := a.sess.Flex.Enabled
	a.mu.Unlock()
	radio := ""
	if flexOn && snap.Connected {
		radio = strings.TrimSpace(snap.Model + ` "` + snap.Nickname + `"`)
	}
	r := newPDFReport(st, radio)
	r.iq = a.iqChecks(st)
	r.build()
	p := filepath.Join(st.Dir, outFileName(st.Session, "report", ".pdf"))
	if err := r.pdf.OutputFileAndClose(p); err != nil {
		return "", fmt.Errorf("writing the PDF: %w", err)
	}
	a.src.Info("PDF report saved to %s", p)
	return p, nil
}

// OpenFile opens a file with its default program (e.g. the PDF report).
func (a *App) OpenFile(p string) {
	_ = exec.Command("explorer", p).Start()
}

// fmtEvent names the FMT event a session is for, e.g. "Nov 2026", from the
// earliest run key-down (the test night), falling back to today.
func fmtEvent(s *session.Session) string {
	var t time.Time
	for _, r := range s.Runs {
		if !r.KeyDown.IsZero() && (t.IsZero() || r.KeyDown.Before(t)) {
			t = r.KeyDown
		}
	}
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format("Jan 2006")
}

// outFileName builds a saved file's name from the event and the operator's
// callsign, e.g. "FMT Nov 2026 VA3MW results.txt".
func outFileName(s *session.Session, kind, ext string) string {
	parts := []string{"FMT", fmtEvent(s)}
	if c := strings.TrimSpace(s.Callsign); c != "" {
		parts = append(parts, c)
	}
	parts = append(parts, kind)
	return safeName(strings.Join(parts, " ")) + ext
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
}

type pdfReport struct {
	st    State
	radio string
	pdf   *fpdf.Fpdf
	tr    func(string) string
	w     float64                   // usable width
	iq    map[string]*iqCheck       // IQ cross-check per run ID
	rec   map[string]recommendation // what to submit per run ID
}

const (
	pdfMargin = 15.0
	pdfLine   = 4.6
)

func newPDFReport(st State, radio string) *pdfReport {
	p := fpdf.New("P", "mm", "A4", "")
	p.SetMargins(pdfMargin, pdfMargin, pdfMargin)
	p.SetAutoPageBreak(true, 16)
	p.AliasNbPages("")
	r := &pdfReport{st: st, radio: radio, pdf: p, tr: p.UnicodeTranslatorFromDescriptor("")}
	pw, _ := p.GetPageSize()
	r.w = pw - 2*pdfMargin
	title := "FMT measurement report"
	if c := st.Session.Callsign; c != "" {
		title += " - " + c
	}
	p.SetTitle(title, true)
	p.SetCreator("FMT Assistant "+version, true)
	p.SetFooterFunc(func() {
		p.SetY(-11)
		p.SetFont("Helvetica", "", 7.5)
		p.SetTextColor(120, 120, 120)
		p.CellFormat(r.w/2, 4, r.tr(title), "", 0, "L", false, 0, "")
		p.CellFormat(r.w/2, 4, fmt.Sprintf("Page %d of {nb}", p.PageNo()), "", 0, "R", false, 0, "")
		p.SetTextColor(0, 0, 0)
	})
	return r
}

// ---------------------------------------------------------------- text helpers

func (r *pdfReport) h1(s string) {
	r.need(40) // keep a heading with what follows it
	r.pdf.Ln(2)
	r.pdf.SetFont("Helvetica", "B", 14)
	r.pdf.SetTextColor(20, 60, 130)
	r.pdf.CellFormat(r.w, 7, r.tr(s), "", 1, "L", false, 0, "")
	r.pdf.SetDrawColor(20, 60, 130)
	r.pdf.SetLineWidth(0.4)
	y := r.pdf.GetY()
	r.pdf.Line(pdfMargin, y, pdfMargin+r.w, y)
	r.pdf.SetTextColor(0, 0, 0)
	r.pdf.Ln(2)
}

func (r *pdfReport) h2(s string) {
	r.need(10)
	r.pdf.Ln(1.5)
	r.pdf.SetFont("Helvetica", "B", 11)
	r.pdf.CellFormat(r.w, 6, r.tr(s), "", 1, "L", false, 0, "")
}

func (r *pdfReport) para(s string) {
	r.pdf.SetFont("Helvetica", "", 9)
	r.pdf.MultiCell(r.w, pdfLine, r.tr(s), "", "L", false)
}

func (r *pdfReport) note(s string) {
	r.pdf.SetFont("Helvetica", "I", 8)
	r.pdf.SetTextColor(90, 90, 90)
	r.pdf.MultiCell(r.w, 3.8, r.tr(s), "", "L", false)
	r.pdf.SetTextColor(0, 0, 0)
}

// math prints lines of working in a fixed-width font on a light panel.
func (r *pdfReport) math(lines ...string) {
	h := float64(len(lines))*4.2 + 3
	r.need(h)
	y := r.pdf.GetY()
	r.pdf.SetFillColor(244, 246, 250)
	r.pdf.SetDrawColor(215, 220, 230)
	r.pdf.SetLineWidth(0.2)
	r.pdf.Rect(pdfMargin, y, r.w, h, "FD")
	r.pdf.SetXY(pdfMargin+2.5, y+1.5)
	r.pdf.SetFont("Courier", "", 8.6)
	for _, l := range lines {
		r.pdf.SetX(pdfMargin + 2.5)
		r.pdf.CellFormat(r.w-5, 4.2, r.tr(l), "", 1, "L", false, 0, "")
	}
	r.pdf.SetY(y + h + 1.5)
}

// kv prints label: value rows.
func (r *pdfReport) kv(rows [][2]string) {
	for _, kv := range rows {
		r.need(pdfLine)
		r.pdf.SetFont("Helvetica", "B", 9)
		r.pdf.CellFormat(42, pdfLine, r.tr(kv[0]), "", 0, "L", false, 0, "")
		r.pdf.SetFont("Helvetica", "", 9)
		r.pdf.MultiCell(r.w-42, pdfLine, r.tr(kv[1]), "", "L", false)
	}
}

// table prints a table; widths in mm, align per column ("L", "R", "C").
func (r *pdfReport) table(head []string, widths []float64, align []string, rows [][]string, shade func(i int) bool) {
	p := r.pdf
	header := func() {
		p.SetFont("Helvetica", "B", 7.8)
		p.SetFillColor(225, 231, 242)
		p.SetDrawColor(200, 205, 215)
		p.SetLineWidth(0.15)
		for i, h := range head {
			p.CellFormat(widths[i], 5, r.tr(h), "1", 0, align[i], true, 0, "")
		}
		p.Ln(-1)
	}
	r.need(10)
	header()
	p.SetFont("Helvetica", "", 7.8)
	for i, row := range rows {
		if r.need(4.4) {
			header()
			p.SetFont("Helvetica", "", 7.8)
		}
		fill := shade != nil && shade(i)
		p.SetFillColor(226, 244, 230)
		for j, c := range row {
			p.CellFormat(widths[j], 4.4, r.fit(r.tr(c), widths[j]-1.5), "1", 0, align[j], fill, 0, "")
		}
		p.Ln(-1)
	}
	p.Ln(1.5)
}

// fit shortens s with "..." so it fits w mm in the current font.
func (r *pdfReport) fit(s string, w float64) string {
	if r.pdf.GetStringWidth(s) <= w {
		return s
	}
	for len(s) > 1 && r.pdf.GetStringWidth(s+"...") > w {
		s = s[:len(s)-1]
	}
	return s + "..."
}

// need starts a new page if h mm don't fit; it says whether it did.
func (r *pdfReport) need(h float64) bool {
	_, ph := r.pdf.GetPageSize()
	if r.pdf.GetY()+h > ph-17 {
		r.pdf.AddPage()
		return true
	}
	return false
}

// ---------------------------------------------------------------- charts

type chartPt struct {
	x, y   float64 // x in seconds (UTC seconds of day, unwrapped), y in Hz
	filled bool
}

type chartSeries struct {
	name string
	rgb  [3]int
	pts  []chartPt
}

type chartOpts struct {
	title  string
	ylabel string
	x0, x1 float64      // seconds
	y0, y1 float64      // Hz
	shade  [][2]float64 // shaded x spans (key-down)
	band   *[2]float64  // shaded y band (±1 SD)
	hline  *float64     // reference line
	trend  *[4]float64  // x0, y0, x1, y1 of a fitted line
	legend bool
	height float64
}

var palette = [][3]int{{31, 95, 191}, {214, 96, 30}, {30, 150, 90}, {150, 60, 170}, {200, 40, 60}, {90, 90, 90}}

func (r *pdfReport) chart(o chartOpts, series []chartSeries) {
	p := r.pdf
	h := o.height
	if h == 0 {
		h = 62
	}
	r.need(h + 12)
	p.SetFont("Helvetica", "B", 9)
	p.CellFormat(r.w, 5, r.tr(o.title), "", 1, "L", false, 0, "")
	top := p.GetY() + 1
	L, R := pdfMargin+14, pdfMargin+r.w-2
	T, B := top, top+h-10
	if o.x1 <= o.x0 {
		o.x1 = o.x0 + 60
	}
	if o.y1 <= o.y0 {
		o.y0, o.y1 = o.y0-0.5, o.y0+0.5
	}
	X := func(x float64) float64 { return L + (x-o.x0)/(o.x1-o.x0)*(R-L) }
	Y := func(y float64) float64 {
		y = math.Max(o.y0, math.Min(o.y1, y))
		return B - (y-o.y0)/(o.y1-o.y0)*(B-T)
	}
	p.SetLineWidth(0.15)
	p.SetDrawColor(180, 185, 195)
	p.Rect(L, T, R-L, B-T, "D")
	for _, s := range o.shade {
		a, b := math.Max(L, X(s[0])), math.Min(R, X(s[1]))
		if b > a {
			p.SetFillColor(250, 228, 228)
			p.Rect(a, T, b-a, B-T, "F")
			p.SetFont("Helvetica", "", 6.5)
			p.SetTextColor(170, 60, 60)
			p.Text(a+1, T+3, "key-down")
			p.SetTextColor(0, 0, 0)
		}
	}
	if o.band != nil {
		p.SetFillColor(222, 232, 248)
		y1, y2 := Y(o.band[1]), Y(o.band[0])
		p.Rect(L, y1, R-L, y2-y1, "F")
	}
	// y grid
	step := niceStep((o.y1 - o.y0) / 5)
	p.SetFont("Helvetica", "", 6.5)
	dec := 0
	for s := step; s < 1 && dec < 4; s *= 10 {
		dec++
	}
	for y := math.Ceil(o.y0/step) * step; y <= o.y1+step/1e6; y += step {
		yy := Y(y)
		p.SetDrawColor(225, 228, 235)
		p.Line(L, yy, R, yy)
		p.SetTextColor(90, 90, 90)
		lbl := fmt.Sprintf("%+.*f", dec, y)
		if math.Abs(y) < step/1e6 {
			lbl = "0"
		}
		p.Text(L-1-p.GetStringWidth(lbl), yy+1, lbl)
	}
	// x ticks every nice number of minutes
	span := o.x1 - o.x0
	tick := 60.0
	for _, t := range []float64{15, 30, 60, 120, 300, 600, 900, 1800} {
		if span/t <= 8 {
			tick = t
			break
		}
	}
	for x := math.Ceil(o.x0/tick) * tick; x <= o.x1+0.01; x += tick {
		xx := X(x)
		p.SetDrawColor(235, 237, 242)
		p.Line(xx, T, xx, B)
		lbl := hmsOf(x)
		if tick >= 60 {
			lbl = lbl[:5]
		}
		p.Text(xx-p.GetStringWidth(lbl)/2, B+3.2, lbl)
	}
	p.Text(R-p.GetStringWidth("UTC"), B+6.3, "UTC")
	p.TransformBegin()
	p.TransformRotate(90, pdfMargin+3, (T+B)/2)
	lbl := r.tr(o.ylabel)
	p.Text(pdfMargin+3-p.GetStringWidth(lbl)/2, (T+B)/2, lbl)
	p.TransformEnd()
	p.SetTextColor(0, 0, 0)
	if o.hline != nil {
		p.SetDrawColor(40, 40, 40)
		p.SetLineWidth(0.3)
		p.Line(L, Y(*o.hline), R, Y(*o.hline))
	}
	if o.trend != nil {
		p.SetDrawColor(200, 120, 20)
		p.SetLineWidth(0.25)
		p.SetDashPattern([]float64{1.2, 0.8}, 0)
		p.Line(X(o.trend[0]), Y(o.trend[1]), X(o.trend[2]), Y(o.trend[3]))
		p.SetDashPattern(nil, 0)
	}
	p.SetLineWidth(0.25)
	for _, s := range series {
		p.SetDrawColor(s.rgb[0], s.rgb[1], s.rgb[2])
		p.SetFillColor(s.rgb[0], s.rgb[1], s.rgb[2])
		for _, pt := range s.pts {
			if pt.x < o.x0 || pt.x > o.x1 {
				continue
			}
			style := "D"
			if pt.filled {
				style = "FD"
			}
			p.Circle(X(pt.x), Y(pt.y), 0.65, style)
		}
	}
	y := B + 5
	if o.legend && len(series) > 0 {
		x := L
		p.SetFont("Helvetica", "", 6.8)
		for _, s := range series {
			p.SetFillColor(s.rgb[0], s.rgb[1], s.rgb[2])
			p.Circle(x+1, y+1.7, 0.8, "F")
			t := r.tr(s.name)
			p.Text(x+2.8, y+2.6, t)
			x += 6 + p.GetStringWidth(t)
		}
		y += 4
	}
	p.SetY(y + 1)
}

func niceStep(x float64) float64 {
	if x <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(x)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*e >= x {
			return m * e
		}
	}
	return 10 * e
}

func hmsOf(sec float64) string {
	s := ((int(math.Round(sec)) % 86400) + 86400) % 86400
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s/60)%60, s%60)
}

// unwrap puts seconds-of-day near ref, across midnight.
func unwrap(sec int, ref float64) float64 {
	return ref + float64(fmtdata.SecDiff(sec, int(ref)))
}

// yRange is a robust range for the values: median ± a spread that covers
// nearly all of them, never narrower than ±minHalf.
func yRange(v []float64, minHalf float64) (float64, float64) {
	if len(v) == 0 {
		return -minHalf, minHalf
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	med := s[len(s)/2]
	lo, hi := s[int(float64(len(s)-1)*0.02)], s[int(math.Ceil(float64(len(s)-1)*0.98))]
	half := math.Max(minHalf, math.Max(med-lo, hi-med)*1.15)
	return med - half, med + half
}

// ---------------------------------------------------------------- content

func (r *pdfReport) build() {
	p := r.pdf
	st := r.st
	r.rec = map[string]recommendation{}
	for _, rv := range st.Runs {
		if rv.FinalHz == nil {
			continue
		}
		sem := 0.0
		if rv.Result != nil {
			sem = rv.Result.SEM
		}
		var v *IQAnalysisView
		if c := r.iq[rv.Run.ID]; c != nil && c.Err == "" {
			v = c.View
		}
		r.rec[rv.Run.ID] = recommend(*rv.FinalHz, sem, v)
	}
	s := st.Session
	p.AddPage()
	p.SetFont("Helvetica", "B", 18)
	p.CellFormat(r.w, 9, r.tr("ARRL Frequency Measuring Test"), "", 1, "L", false, 0, "")
	p.SetFont("Helvetica", "", 11)
	sub := s.Name
	if s.Callsign != "" {
		sub = s.Callsign + " - " + sub
	}
	p.CellFormat(r.w, 6, r.tr("Measurement report: "+sub), "", 1, "L", false, 0, "")
	p.Ln(2)
	rows := [][2]string{
		{"Prepared", fmt.Sprintf("FMT Assistant %s, %s UTC", version, time.Now().UTC().Format("2006-01-02 15:04"))},
		{"Method", "WSJT-X FreqCal: the receiver in USB (DIGU) measures the audio frequency (fMeas) of each carrier about every 2 s. Carrier = dial + fMeas, corrected for the radio's own error measured on WWV."},
		{"Frequency reference", st.Ref.Detail},
		{"Correction mode", calModeText(s.CalMode)},
	}
	if r.radio != "" {
		rows = append(rows, [2]string{"Radio", r.radio})
	}
	r.kv(rows)

	r.h1("Results")
	var res [][]string
	for _, rv := range st.Runs {
		row := []string{rv.Title, fmt.Sprintf("%.0f", rv.Run.NominalKHz), hzOrDash(rv.Run.DialHz, 0), "-", "-", "-", "-"}
		if rr := rv.Result; rr != nil && rr.N > 0 {
			row[3] = fmt.Sprintf("%.3f", rr.MeanFMeas)
			row[4] = fmt.Sprintf("%d", rr.N)
		}
		if rv.CorrHz != nil {
			row[5] = fmt.Sprintf("%+.3f", *rv.CorrHz)
		}
		if rv.FinalHz != nil {
			row[6] = fmt.Sprintf("%s (%s)", commaHz(*rv.FinalHz, 2), commaHz(math.Round(*rv.FinalHz), 0))
		}
		if !rv.Ready && rv.FinalHz != nil {
			row[6] += " *"
		}
		res = append(res, row)
	}
	r.table([]string{"Run", "Near kHz", "Dial Hz", "Mean fMeas Hz", "n", "Correction Hz", "Carrier Hz (nearest Hz)"},
		[]float64{30, 16, 24, 24, 8, 22, 56}, []string{"L", "R", "R", "R", "R", "R", "R"}, res, nil)
	r.note("Carrier = dial + slice tuning error (FlexRadio, when read) + mean fMeas - correction. Rounded to the nearest hertz in brackets. * = the run still has a warning or error in the app; see its section.")
	r.recSummary()
	var outs [][]string
	for _, rv := range st.Runs {
		if o := rv.Outcome; o != nil {
			outs = append(outs, []string{rv.Title, commaHz(o.PublishedHz, 2), fmt.Sprintf("%+.3f", o.ErrHz), fmt.Sprintf("%+.1f", o.ErrPPB)})
		}
	}
	if len(outs) > 0 {
		r.h2("Against the published frequencies")
		r.table([]string{"Run", "Published Hz", "Error Hz", "Error ppb"}, []float64{40, 50, 30, 30}, []string{"L", "R", "R", "R"}, outs, nil)
		for _, n := range st.OutcomeNotes {
			r.note(n)
		}
	}

	for _, c := range st.Cals {
		r.calSection(c)
	}
	for _, rv := range st.Runs {
		r.runSection(rv)
	}
}

func calModeText(m string) string {
	switch m {
	case "none":
		return "No correction (GPS-locked reference): correction = 0 Hz."
	case "before":
		return "Before calibration: correction = A + B x f from the WWV calibration before the runs."
	case "after":
		return "After calibration: correction = A + B x f from the WWV calibration after the runs."
	}
	return "Average: correction = mean of the before and after calibrations' A + B x f (one of them if only one is usable)."
}

func hzOrDash(hz float64, dec int) string {
	if hz == 0 {
		return "-"
	}
	return commaHz(hz, dec)
}

// commaHz formats Hz with thousands separators and dec decimals.
func commaHz(hz float64, dec int) string {
	sign := ""
	if hz < 0 {
		sign, hz = "-", -hz
	}
	ip, fp, _ := strings.Cut(fmt.Sprintf("%.*f", dec, hz), ".")
	whole, _ := strconv.ParseInt(ip, 10, 64)
	if fp != "" {
		fp = "." + fp
	}
	return sign + commas(whole) + fp
}

func (r *pdfReport) calSection(c CalView) {
	r.h1(c.Title)
	cr := c.Result
	if !c.Present || cr == nil {
		r.para("No calibration file.")
		return
	}
	r.kv([][2]string{
		{"File", c.Cal.Slot.File},
		{"Readings", fmt.Sprintf("%s to %s UTC, %d lines, %d used in the fit", cr.First, cr.Last, cr.Total, cr.Used)},
		{"Reference", refReportLine(c.Cal.Slot.Ref)},
	})
	var rows [][]string
	for _, s := range cr.Stations {
		use := "no"
		if s.Included {
			use = "yes"
		}
		rows = append(rows, []string{s.Label, fmt.Sprintf("%d", s.KHz), fmt.Sprintf("%d", s.Lines), fmt.Sprintf("%d", s.LowSNR),
			fmt.Sprintf("%d", s.Outliers), fmt.Sprintf("%d", s.N), fmt.Sprintf("%+.3f", s.MeanDF), fmt.Sprintf("%.3f", s.SD),
			fmt.Sprintf("%+.3f", s.Resid), use})
	}
	r.table([]string{"Station", "kHz", "Lines", "Weak", "Outl.", "Used", "Mean DF Hz", "SD Hz", "Resid. Hz", "In fit"},
		[]float64{50, 14, 11, 11, 11, 11, 20, 15, 18, 13}, []string{"L", "R", "R", "R", "R", "R", "R", "R", "R", "C"}, rows,
		func(i int) bool { return cr.Stations[i].Included })
	r.note(fmt.Sprintf("Readings below S/N %.0f dB or marked * are weak and dropped; outliers are dropped per station. DF = fMeas - Offset is how far each WWV carrier is heard from where it should be: the radio's error plus Doppler on the path.", c.Cal.MinSNR))

	lines := []string{"Straight-line fit through the station means (f in MHz):", "  DF = A + B x f"}
	if cr.OK {
		lines = append(lines,
			fmt.Sprintf("  A = %+.4f Hz  (+/- %.4f)", cr.A, cr.SigmaA),
			fmt.Sprintf("  B = %+.5f Hz/MHz = %+.5f ppm  (+/- %.5f)", cr.B, cr.B, cr.SigmaB),
			fmt.Sprintf("  RMS residual %.4f Hz from %d station(s)", cr.RMS, cr.NFit))
		for _, rv := range r.st.Runs {
			if rv.Result == nil || rv.Result.N == 0 {
				continue
			}
			f := (rv.Run.DialHz + rv.Result.MeanFMeas) / 1e6
			if rv.Run.DialHz == 0 {
				f = float64(rv.Result.KHz) / 1000
			}
			lines = append(lines, fmt.Sprintf("  at %-11s f = %.6f MHz: %+.4f + (%+.5f x %.6f) = %+.4f Hz",
				rv.Title, f, cr.A, cr.B, f, cr.Correction(f)))
		}
	} else {
		lines = append(lines, "  No usable fit (see the app's calibration warnings).")
	}
	r.math(lines...)

	// WWV Doppler chart: DF of every reading over time, a colour per station.
	ref := float64(c.Lines[0].Sec)
	byKHz := map[int]*chartSeries{}
	var order []int
	var ys []float64
	x0, x1 := math.Inf(1), math.Inf(-1)
	min := c.Cal.MinSNR
	if len(c.Lines) == 0 {
		return
	}
	usable := map[int]bool{}
	var left []string
	for _, s := range cr.Stations {
		if s.IsRef && s.N > 0 {
			usable[s.KHz] = true
		} else {
			left = append(left, s.Label)
		}
	}
	for _, l := range c.Lines {
		if !usable[l.KHz] {
			continue
		}
		sr := byKHz[l.KHz]
		if sr == nil {
			name := fmt.Sprintf("%d kHz", l.KHz)
			for _, s := range cr.Stations {
				if s.KHz == l.KHz {
					name = s.Label
				}
			}
			sr = &chartSeries{name: name, rgb: palette[len(order)%len(palette)]}
			byKHz[l.KHz] = sr
			order = append(order, l.KHz)
		}
		x := unwrap(l.Sec, ref)
		strong := !l.Flag && l.SNR >= min
		sr.pts = append(sr.pts, chartPt{x: x, y: l.DF, filled: strong})
		if strong {
			ys = append(ys, l.DF)
		}
		x0, x1 = math.Min(x0, x), math.Max(x1, x)
	}
	var series []chartSeries
	for _, k := range order {
		series = append(series, *byKHz[k])
	}
	y0, y1 := yRange(ys, 0.5)
	zero := 0.0
	r.chart(chartOpts{title: "WWV Doppler: DF of each reading (Hz) over time", ylabel: "DF Hz",
		x0: x0 - 15, x1: x1 + 15, y0: y0, y1: y1, hline: &zero, legend: true}, series)
	note := "Filled dots are strong readings, hollow dots weak ones (not used). With a GPS-locked radio, what remains is mostly ionospheric Doppler and noise."
	if len(left) > 0 {
		note += " Not shown (not WWV/WWVH, or no usable readings): " + strings.Join(left, ", ") + "."
	}
	r.note(note)
}

func (r *pdfReport) runSection(rv RunView) {
	run := rv.Run
	r.h1(rv.Title + fmt.Sprintf(" (near %.0f kHz)", run.NominalKHz))
	rows := [][2]string{
		{"Key-down", fmt.Sprintf("%s to %s UTC", run.KeyDown.UTC().Format("15:04:05"), rv.KeyUp.UTC().Format("15:04:05"))},
		{"File", run.Slot.File},
		{"Reference", refReportLine(run.Slot.Ref)},
	}
	res := rv.Result
	if res == nil || res.N == 0 || run.DialHz == 0 {
		r.kv(rows)
		r.para("Not worked out yet (no file, no usable readings or no dial).")
		r.issues(rv)
		return
	}
	rows = append(rows, [2]string{"Dial entered", commaHz(run.DialHz, 2) + " Hz (" + run.DialText + ")"})
	if run.RadioDialHz != 0 {
		rows = append(rows, [2]string{"Radio dial", commaHz(float64(run.RadioDialHz), 0) + " Hz, read from the FlexRadio during the carrier"})
	}
	r.kv(rows)

	// Readings used.
	var used []fmtdata.RunLine
	why := map[string]int{}
	for _, l := range res.Lines {
		if l.Used {
			used = append(used, l)
		} else {
			k := l.Status
			if i := strings.Index(k, " ("); i > 0 && strings.HasPrefix(k, "outlier") {
				k = "outlier"
			}
			why[k]++
		}
	}
	var whys []string
	for k, n := range why {
		whys = append(whys, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(whys)
	r.h2(fmt.Sprintf("Readings used: %d of %d in the file", len(used), len(res.Lines)))
	if len(whys) > 0 {
		r.note("Left out: " + strings.Join(whys, ", ") + ".")
	}
	var rows2 [][]string
	sum := 0.0
	for _, l := range used {
		sum += l.FMeas
	}
	mean := sum / float64(len(used))
	ss := 0.0
	for i, l := range used {
		d := l.FMeas - mean
		ss += d * d
		st := ""
		if l.Forced {
			st = "your choice"
		}
		rows2 = append(rows2, []string{fmt.Sprintf("%d", i+1), l.Time, fmt.Sprintf("%d", l.KHz), fmt.Sprintf("%d", l.Offset),
			fmt.Sprintf("%.3f", l.FMeas), fmt.Sprintf("%+.3f", l.DF), fmt.Sprintf("%.1f", l.SNR), fmt.Sprintf("%+.3f", d), st})
	}
	r.table([]string{"#", "UTC", "kHz", "Offset", "fMeas Hz", "DF Hz", "S/N dB", "fMeas - mean", "Note"},
		[]float64{8, 18, 14, 14, 22, 18, 16, 24, 26}, []string{"R", "C", "R", "R", "R", "R", "R", "R", "L"}, rows2, nil)

	n := float64(len(used))
	sd, sem := 0.0, 0.0
	if n > 1 {
		sd = math.Sqrt(ss / (n - 1))
		sem = sd / math.Sqrt(n)
	}
	math1 := []string{
		"Mean fMeas = sum of fMeas / n",
		fmt.Sprintf("           = %.3f / %d = %.4f Hz", sum, len(used), mean),
	}
	if n > 1 {
		math1 = append(math1,
			"SD         = sqrt( sum (fMeas - mean)^2 / (n - 1) )",
			fmt.Sprintf("           = sqrt( %.6f / %d ) = %.4f Hz", ss, len(used)-1, sd),
			"SEM        = SD / sqrt(n)",
			fmt.Sprintf("           = %.4f / %.4f = %.4f Hz   (statistical uncertainty of the mean)", sd, math.Sqrt(n), sem))
	}
	var xs, ys []float64
	ref := float64(used[0].Sec)
	for _, l := range used {
		xs, ys = append(xs, (unwrap(l.Sec, ref)-ref)/60), append(ys, l.FMeas)
	}
	slope, icpt, _, okFit := iq.LinFit(xs, ys)
	if okFit && len(used) >= 3 {
		math1 = append(math1, fmt.Sprintf("Drift      = least-squares slope of fMeas over time = %+.4f Hz/min", slope))
	}
	r.math(math1...)

	// Dial check.
	r.math(
		"Dial check: WSJT-X logs kHz = (dial + Offset) / 1000, truncated,",
		"            so each reading puts the dial in kHz x 1000 - Offset ... +999 Hz.",
		fmt.Sprintf("            All readings used: %s ... %s Hz", commaHz(float64(res.DialMin), 0), commaHz(float64(res.DialMax), 0)),
		fmt.Sprintf("            Dial entered %s Hz: %s", commaHz(run.DialHz, 0),
			map[bool]string{true: "consistent", false: "NOT consistent"}[run.DialHz >= float64(res.DialMin) && run.DialHz <= float64(res.DialMax)]),
	)

	// Correction.
	mode := r.st.Session.CalMode
	corr := []string{"Correction: " + calModeText(mode)}
	f := (run.DialHz + res.MeanFMeas) / 1e6
	cal := func(name string, c *fmtdata.CalResult, v *float64) {
		if v != nil && c != nil {
			tail := ""
			if mode == "none" || (mode == "before" && name == "after") || (mode == "after" && name == "before") {
				tail = "  (not applied)"
			}
			corr = append(corr, fmt.Sprintf("  %-7s A + B x f = %+.4f + (%+.5f x %.6f) = %+.4f Hz%s", name, c.A, c.B, f, *v, tail))
		}
	}
	cal("before", r.st.Cals[0].Result, rv.CorrBefore)
	cal("after", r.st.Cals[1].Result, rv.CorrAfter)
	if rv.CorrHz != nil {
		if r.st.Session.CalMode == "average" && rv.CorrBefore != nil && rv.CorrAfter != nil {
			corr = append(corr, fmt.Sprintf("  average  (%+.4f + %+.4f) / 2 = %+.4f Hz", *rv.CorrBefore, *rv.CorrAfter, *rv.CorrHz))
		}
		corr = append(corr, fmt.Sprintf("  correction used = %+.4f Hz", *rv.CorrHz))
	} else {
		corr = append(corr, "  no usable correction, so no carrier was worked out")
	}
	r.math(corr...)

	// Carrier.
	if rv.FinalHz != nil {
		c := *rv.CorrHz
		lines := []string{}
		if rv.SliceErrHz != nil {
			lines = append(lines,
				"Carrier = dial + slice tuning error + mean fMeas - correction",
				fmt.Sprintf("        = %s + %.4f + %.4f - (%+.4f)", commaHz(run.DialHz, 2), *rv.SliceErrHz, res.MeanFMeas, c))
		} else {
			lines = append(lines,
				"Carrier = dial + mean fMeas - correction",
				fmt.Sprintf("        = %s + %.4f - (%+.4f)", commaHz(run.DialHz, 2), res.MeanFMeas, c))
		}
		lines = append(lines,
			fmt.Sprintf("        = %s Hz   (+/- %.3f Hz statistical)", commaHz(*rv.FinalHz, 3), res.SEM),
			fmt.Sprintf("Nearest hertz: %s Hz", commaHz(math.Round(*rv.FinalHz), 0)))
		if o := rv.Outcome; o != nil {
			lines = append(lines, fmt.Sprintf("Published %s Hz: error %+.3f Hz (%+.1f ppb)", commaHz(o.PublishedHz, 2), o.ErrHz, o.ErrPPB))
		}
		r.math(lines...)
	}

	// Station Doppler chart: the readings around key-down, offset from the mean.
	kd := float64(secOfDay(run.KeyDown))
	a, b := kd-60, kd+120
	var pts []chartPt
	var dev []float64
	all := false
	for _, l := range res.Lines {
		if l.Used {
			x := unwrap(l.Sec, kd)
			if x < a || x > b {
				all = true
			}
		}
	}
	x0, x1 := a, b
	for _, l := range res.Lines {
		x := unwrap(l.Sec, kd)
		if !all && (x < a || x > b) {
			continue
		}
		if all {
			x0, x1 = math.Min(x0, x), math.Max(x1, x)
		}
		strong := !l.Flag && l.SNR >= run.MinSNR
		if !strong && !l.Used {
			continue
		}
		pts = append(pts, chartPt{x: x, y: l.FMeas - res.MeanFMeas, filled: l.Used})
		if l.Used {
			dev = append(dev, l.FMeas-res.MeanFMeas)
		}
	}
	y0, y1 := yRange(dev, 0.1)
	zero := 0.0
	o := chartOpts{title: fmt.Sprintf("Station Doppler: fMeas - mean (%.3f Hz) over time", res.MeanFMeas), ylabel: "Hz from mean",
		x0: x0, x1: x1, y0: y0, y1: y1, shade: [][2]float64{{kd, kd + 60}}, hline: &zero, band: &[2]float64{-sd, sd},
		legend: true}
	if okFit && len(used) >= 3 {
		t0, t1 := unwrap(used[0].Sec, kd), unwrap(used[len(used)-1].Sec, kd)
		m0 := (t0-unwrap(used[0].Sec, kd))/60*slope + icpt - res.MeanFMeas
		m1 := (t1-unwrap(used[0].Sec, kd))/60*slope + icpt - res.MeanFMeas
		o.trend = &[4]float64{t0, m0, t1, m1}
	}
	r.chart(o, []chartSeries{{name: "used (filled) / strong but left out (hollow)", rgb: palette[0], pts: pts}})
	r.note("Shaded band: +/- 1 SD around the mean. Dashed line: drift fit. The spread and drift of the readings during the carrier are mostly Doppler on the path and measurement noise.")
	r.iqSection(rv)
	r.issues(rv)
}

func (r *pdfReport) issues(rv RunView) {
	var list []string
	for _, is := range append(append([]fmtdata.Issue{}, rv.Issues...), resIssues(rv.Result)...) {
		list = append(list, strings.ToUpper(is.Level)+": "+is.Msg)
	}
	if len(list) > 0 {
		r.h2("Notes from the app")
		for _, l := range list {
			r.note(l)
		}
	}
}
