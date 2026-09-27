package fm

import (
	"math"
	"math/bits"
	"math/cmplx"
	"sync"
)

// fir is a streaming FIR filter on a complex signal (a real signal just
// uses the real part). Each call filters the samples it is given against
// the history of the previous calls, so a stream cut into blocks of any
// size comes out exactly as the whole stream would, with no added delay:
// the filter is causal, like the C# package's, so its group delay is
// (taps-1)/2 samples.
//
// Short kernels are applied directly. Long ones, which is most of them at
// IF rates, go through the FFT (overlap-save sized to each call), which is
// what makes a 320-tap filter at 192 kHz affordable per receiver.
type fir struct {
	h    []float64
	hist []complex128 // the last len(h)-1 inputs
	buf  []complex128
	work []complex128

	spectra map[int][]complex128 // kernel FFT per transform size
}

const directTaps = 48

func newFIR(h []float64) *fir {
	return &fir{h: h, hist: make([]complex128, len(h)-1), spectra: map[int][]complex128{}}
}

// process filters x in place.
func (f *fir) process(x []complex128) {
	m := len(f.h)
	if len(x) == 0 {
		return
	}
	f.buf = append(append(f.buf[:0], f.hist...), x...)
	if m <= directTaps {
		for n := range x {
			var acc complex128
			base := n + m - 1
			for k, c := range f.h {
				acc += complex(c, 0) * f.buf[base-k]
			}
			x[n] = acc
		}
	} else {
		p := len(f.buf)
		size := 1 << bits.Len(uint(p-1))
		if cap(f.work) < size {
			f.work = make([]complex128, size)
		}
		work := f.work[:size]
		n := copy(work, f.buf)
		clear(work[n:])
		fft(work, false)
		hs := f.spectrum(size)
		for i := range work {
			work[i] *= hs[i]
		}
		fft(work, true)
		copy(x, work[m-1:m-1+len(x)])
	}
	copy(f.hist, f.buf[len(f.buf)-(m-1):])
}

func (f *fir) spectrum(size int) []complex128 {
	if s, ok := f.spectra[size]; ok {
		return s
	}
	s := make([]complex128, size)
	for i, c := range f.h {
		s[i] = complex(c, 0)
	}
	fft(s, false)
	f.spectra[size] = s
	return s
}

// processReal filters a real signal in place.
func (f *fir) processReal(x []float64, scratch []complex128) []complex128 {
	scratch = scratch[:0]
	for _, v := range x {
		scratch = append(scratch, complex(v, 0))
	}
	f.process(scratch)
	for i := range x {
		x[i] = real(scratch[i])
	}
	return scratch
}

// fft is an in-place iterative radix-2 transform; len(a) must be a power
// of two. inverse includes the 1/N scaling.
func fft(a []complex128, inverse bool) {
	n := len(a)
	shift := 64 - bits.Len(uint(n-1))
	for i := range a {
		j := int(bits.Reverse64(uint64(i)) >> shift)
		if j > i {
			a[i], a[j] = a[j], a[i]
		}
	}
	sign := -1.0
	if inverse {
		sign = 1
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		w := twiddles(size, sign)
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				t := w[k] * a[start+k+half]
				a[start+k+half] = a[start+k] - t
				a[start+k] += t
			}
		}
	}
	if inverse {
		scale := complex(1/float64(n), 0)
		for i := range a {
			a[i] *= scale
		}
	}
}

var twiddleCache sync.Map // [2]int{size, sign} -> []complex128

func twiddles(size int, sign float64) []complex128 {
	key := [2]int{size, int(sign)}
	if w, ok := twiddleCache.Load(key); ok {
		return w.([]complex128)
	}
	w := make([]complex128, size/2)
	for k := range w {
		w[k] = cmplx.Exp(complex(0, sign*2*math.Pi*float64(k)/float64(size)))
	}
	twiddleCache.Store(key, w)
	return w
}
