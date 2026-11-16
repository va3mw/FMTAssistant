package workflow

import (
	"math"
	"sort"
)

// Point is one frequency reading on a common time scale. WSJT-X readings
// and IQ segments both become Points, so the plot, the exclusions and the
// fading check treat them alike.
type Point struct {
	Sec    float64 `json:"sec"`    // UTC seconds of day
	Hz     float64 `json:"hz"`     // audio frequency (WSJT-X fMeas, or IQ audio_hz)
	SNR    float64 `json:"snr"`    // dB, on the source's own scale
	Source string  `json:"source"` // "wsjtx" or "iq"
}

func values(ps []Point) []float64 {
	out := make([]float64, len(ps))
	for i, p := range ps {
		out[i] = p.Hz
	}
	return out
}

func meanSD(v []float64) (mean, sd float64) {
	if len(v) == 0 {
		return 0, 0
	}
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	if len(v) > 1 {
		var s float64
		for _, x := range v {
			s += (x - mean) * (x - mean)
		}
		sd = math.Sqrt(s / float64(len(v)-1))
	}
	return
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Lag1 is the lag-1 autocorrelation of v about its mean. Readings that
// move together from one to the next (a slow fade) give a positive value.
func Lag1(v []float64) float64 {
	if len(v) < 3 {
		return 0
	}
	m, _ := meanSD(v)
	var num, den float64
	for i, x := range v {
		den += (x - m) * (x - m)
		if i > 0 {
			num += (x - m) * (v[i-1] - m)
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// EffectiveN is how many independent readings n readings with lag-1
// autocorrelation r are worth: n(1-r)/(1+r), never more than n or below 1.
func EffectiveN(n int, r float64) float64 {
	if n < 1 {
		return 0
	}
	if r <= 0 {
		return float64(n)
	}
	return math.Max(1, float64(n)*(1-r)/(1+r))
}

// Slope is a straight-line fit of frequency against time.
type Slope struct {
	HzPerMin float64 `json:"hz_per_min"`
	SE       float64 `json:"se"`       // standard error, Hz/min
	T        float64 `json:"t"`        // HzPerMin / SE
	TCrit    float64 `json:"t_crit"`   // Student t at the chosen confidence, N-2 df
	Drifting bool    `json:"drifting"` // |T| > TCrit
	OK       bool    `json:"ok"`
}

// FitSlope fits the points and tests the slope against zero.
func FitSlope(ps []Point, conf float64) Slope {
	n := len(ps)
	if n < 3 {
		return Slope{}
	}
	var mx, my float64
	for _, p := range ps {
		mx, my = mx+p.Sec, my+p.Hz
	}
	mx, my = mx/float64(n), my/float64(n)
	var sxx, sxy float64
	for _, p := range ps {
		sxx += (p.Sec - mx) * (p.Sec - mx)
		sxy += (p.Sec - mx) * (p.Hz - my)
	}
	if sxx == 0 {
		return Slope{}
	}
	b := sxy / sxx
	var ss float64
	for _, p := range ps {
		r := p.Hz - (my + b*(p.Sec-mx))
		ss += r * r
	}
	se := math.Sqrt(ss / float64(n-2) / sxx)
	s := Slope{HzPerMin: b * 60, SE: se * 60, TCrit: TCrit(conf, n-2), OK: true}
	if se > 0 {
		s.T = b / se
		s.Drifting = math.Abs(s.T) > s.TCrit
	}
	return s
}

// curvature fits y = a + b t + c t² and returns c's t statistic (0 when it
// can't be fitted). A significant c is a smooth swing or dip.
func curvature(ps []Point) float64 {
	n := len(ps)
	if n < 5 {
		return 0
	}
	var mx float64
	for _, p := range ps {
		mx += p.Sec
	}
	mx /= float64(n)
	// Normal equations for (1, t, t²) with t centred.
	var s [5]float64 // sums of t^0..t^4
	var sy [3]float64
	for _, p := range ps {
		t := p.Sec - mx
		tp := 1.0
		for k := 0; k < 5; k++ {
			s[k] += tp
			if k < 3 {
				sy[k] += tp * p.Hz
			}
			tp *= t
		}
	}
	m := [3][3]float64{{s[0], s[1], s[2]}, {s[1], s[2], s[3]}, {s[2], s[3], s[4]}}
	inv, ok := inverse3(m)
	if !ok {
		return 0
	}
	var c [3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			c[i] += inv[i][j] * sy[j]
		}
	}
	var ss float64
	for _, p := range ps {
		t := p.Sec - mx
		r := p.Hz - (c[0] + c[1]*t + c[2]*t*t)
		ss += r * r
	}
	v := ss / float64(n-3)
	se := math.Sqrt(v * inv[2][2])
	if se == 0 {
		return 0
	}
	return c[2] / se
}

func inverse3(m [3][3]float64) ([3][3]float64, bool) {
	var r [3][3]float64
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	if math.Abs(det) < 1e-300 {
		return r, false
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			a, b := (j+1)%3, (j+2)%3
			c, d := (i+1)%3, (i+2)%3
			r[i][j] = (m[a][c]*m[b][d] - m[a][d]*m[b][c]) / det
		}
	}
	return r, true
}

// TCrit is the two-sided Student t critical value for confidence conf
// (e.g. 0.95) with df degrees of freedom.
func TCrit(conf float64, df int) float64 {
	if df < 1 {
		return math.Inf(1)
	}
	target := 1 - (1-conf)/2 // one-sided CDF value
	lo, hi := 0.0, 1000.0
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if tCDF(mid, float64(df)) < target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// tCDF is the Student t cumulative distribution for t >= 0.
func tCDF(t, df float64) float64 {
	x := df / (df + t*t)
	return 1 - 0.5*regIncBeta(df/2, 0.5, x)
}

// regIncBeta is the regularized incomplete beta function I_x(a, b).
func regIncBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return front * betaCF(a, b, x) / a
	}
	return 1 - front*betaCF(b, a, 1-x)/b
}

// betaCF is the continued fraction for the incomplete beta (Lentz).
func betaCF(a, b, x float64) float64 {
	const tiny = 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 300; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < 1e-14 {
			break
		}
	}
	return h
}
