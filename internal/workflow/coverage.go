package workflow

import "fmt"

// Coverage compares the stretch of a key-down that WSJT-X captured with the
// whole minute, using a second series that covers it all (IQ segments).
type Coverage struct {
	NCaptured    int     `json:"n_captured"`
	NMissed      int     `json:"n_missed"`
	CapturedMean float64 `json:"captured_mean"` // full series, inside the captured stretch
	MissedMean   float64 `json:"missed_mean"`   // full series, outside it
	FullMean     float64 `json:"full_mean"`
	Shift        float64 `json:"shift"` // FullMean - CapturedMean: what the missing part would change
	Msg          string  `json:"msg"`
}

// CompareCoverage splits full (points over the whole key-down) at the
// captured stretch fromSec..toSec. ok is false when either part is empty.
func CompareCoverage(full []Point, fromSec, toSec float64) (Coverage, bool) {
	var c Coverage
	var in, out, all []float64
	for _, p := range full {
		all = append(all, p.Hz)
		if p.Sec >= fromSec && p.Sec <= toSec {
			in = append(in, p.Hz)
		} else {
			out = append(out, p.Hz)
		}
	}
	if len(in) == 0 || len(out) == 0 {
		return c, false
	}
	c.NCaptured, c.NMissed = len(in), len(out)
	c.CapturedMean, _ = meanSD(in)
	c.MissedMean, _ = meanSD(out)
	c.FullMean, _ = meanSD(all)
	c.Shift = c.FullMean - c.CapturedMean
	c.Msg = fmt.Sprintf("The IQ recording covers the whole key-down. The part WSJT-X missed reads %+.3f Hz from the part it caught, so measuring the whole minute would move this answer by %+.3f Hz.",
		c.MissedMean-c.CapturedMean, c.Shift)
	return c, true
}
