package flex

import (
	"math"
	"math/cmplx"
	"sort"
)

// Spectrum averages power spectra of a complex stream (Welch's method, Hann
// window, no overlap) so the strongest signal in a recording can be found.
// It is a sanity check on the recording, not a measurement: the bins are
// about 1.5 Hz wide.
type Spectrum struct {
	n      int
	rate   int
	win    []float64
	buf    []complex128
	fill   int
	power  []float64
	blocks int
}

// NewSpectrum uses the largest power-of-two block of at most one second.
func NewSpectrum(rate int) *Spectrum {
	n := 1
	for n*2 <= rate {
		n *= 2
	}
	s := &Spectrum{n: n, rate: rate, win: make([]float64, n), buf: make([]complex128, n), power: make([]float64, n)}
	for i := range s.win {
		s.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return s
}

// Add feeds samples.
func (s *Spectrum) Add(x []complex64) {
	for _, v := range x {
		s.buf[s.fill] = complex(float64(real(v))*s.win[s.fill], float64(imag(v))*s.win[s.fill])
		s.fill++
		if s.fill == s.n {
			fft(s.buf)
			for i, c := range s.buf {
				re, im := real(c), imag(c)
				s.power[i] += re*re + im*im
			}
			s.blocks++
			s.fill = 0
		}
	}
}

// Peak is the strongest signal found.
type Peak struct {
	OffsetHz float64 `json:"offset_hz"` // from the centre of the IQ, + is above
	SNRdB    float64 `json:"snr_db"`    // peak bin over the median bin
	BinHz    float64 `json:"bin_hz"`
	Blocks   int     `json:"blocks"`
}

// Peak finds the strongest bin, skipping a few bins around 0 Hz where the
// radio's own DC offset can sit, and refines it by parabolic interpolation.
func (s *Spectrum) Peak() (Peak, bool) {
	if s.blocks == 0 {
		return Peak{}, false
	}
	binHz := float64(s.rate) / float64(s.n)
	skip := int(math.Ceil(3 / binHz))
	best := -1
	for i, p := range s.power {
		k := i
		if k > s.n/2 {
			k -= s.n
		}
		if k >= -skip && k <= skip {
			continue
		}
		if best < 0 || p > s.power[best] {
			best = i
		}
	}
	if best < 0 || s.power[best] <= 0 {
		return Peak{}, false
	}
	at := func(i int) float64 { return math.Log(s.power[(i+s.n)%s.n] + 1e-300) }
	a, b, c := at(best-1), at(best), at(best+1)
	delta := 0.0
	if d := a - 2*b + c; d < 0 {
		delta = 0.5 * (a - c) / d
	}
	k := float64(best) + delta
	if best > s.n/2 {
		k -= float64(s.n)
	}
	sorted := append([]float64(nil), s.power...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	snr := 0.0
	if median > 0 {
		snr = 10 * math.Log10(s.power[best]/median)
	}
	return Peak{OffsetHz: k * binHz, SNRdB: snr, BinHz: binHz, Blocks: s.blocks}, true
}

// fft is an in-place radix-2 FFT; len(x) must be a power of two.
func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Rect(1, -2*math.Pi/float64(size))
		for start := 0; start < n; start += size {
			t := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, v := x[start+k], x[start+k+size/2]*t
				x[start+k], x[start+k+size/2] = u+v, u-v
				t *= w
			}
		}
	}
}
