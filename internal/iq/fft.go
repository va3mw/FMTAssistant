package iq

import (
	"math"
	"math/bits"
	"math/cmplx"
)

// fft transforms x in place; len(x) must be a power of two.
func fft(x []complex128) {
	n := len(x)
	if n < 2 {
		return
	}
	shift := 64 - bits.TrailingZeros(uint(n))
	for i := 0; i < n; i++ {
		j := int(bits.Reverse64(uint64(i)) >> shift)
		if j > i {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			t := complex(1, 0)
			for k := 0; k < half; k++ {
				a, b := x[start+k], x[start+k+half]*t
				x[start+k], x[start+k+half] = a+b, a-b
				t *= w
			}
		}
	}
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
