package tnc

import "math"

// resampler converts a mono float stream between two fixed sample rates
// with a polyphase windowed-sinc filter. It is streaming: Process may be
// called with any number of input samples and returns every output sample
// those inputs fully determine; the last few inputs stay buffered as filter
// lookahead until more arrive or Flush is called.
//
// The router runs at 44.1 kHz and pdn-soundmodem's pipe device at a
// multiple of 12 kHz, so the pdn backend needs one of these in each
// direction. Linear interpolation would be simpler but images badly for
// the wider FM modes (c4fsk19200, the OFDM-FM family), which put energy
// well up the audio band.
type resampler struct {
	up, down int         // output/input rate ratio as up/down, in lowest terms
	half     int         // filter half-width in input samples
	taps     [][]float32 // taps[phase][j], j = 0..2*half-1

	buf   []float32 // pending input; buf[idx] is the sample at or before the next output instant
	idx   int
	phase int // next output instant is idx + phase/up input samples
}

// resampleHalfWidth is the filter half-width in input samples. 32 gives a
// transition band of about 3.5 kHz at these rates with a Kaiser window of
// ~80 dB stopband, so everything below ~18 kHz passes flat.
const resampleHalfWidth = 32

// resampleKaiserBeta sets the window's sidelobe level (~80 dB at 8).
const resampleKaiserBeta = 8.0

func newResampler(inRate, outRate int) *resampler {
	g := gcd(inRate, outRate)
	r := &resampler{up: outRate / g, down: inRate / g, half: resampleHalfWidth}

	// Cutoff sits just inside the lower of the two Nyquist frequencies,
	// expressed as a fraction of the input rate.
	nyq := float64(min(inRate, outRate)) / 2
	fc := (nyq - 2000) / float64(inRate)

	r.taps = make([][]float32, r.up)
	for p := 0; p < r.up; p++ {
		row := make([]float32, 2*r.half)
		frac := float64(p) / float64(r.up)
		sum := 0.0
		vals := make([]float64, 2*r.half)
		for j := range row {
			// Input sample idx-half+1+j sits at offset k from the output instant.
			k := float64(j-r.half+1) - frac
			v := 2 * fc * sinc(2*fc*k) * kaiser(k/float64(r.half), resampleKaiserBeta)
			vals[j] = v
			sum += v
		}
		// Normalise each phase to unity DC gain so there is no
		// phase-dependent ripple on a steady level.
		for j := range row {
			row[j] = float32(vals[j] / sum)
		}
		r.taps[p] = row
	}
	r.reset()
	return r
}

func (r *resampler) reset() {
	r.buf = make([]float32, r.half-1, 4096)
	r.idx = r.half - 1
	r.phase = 0
}

// Process appends in and returns the output samples now computable.
func (r *resampler) Process(in []float32, out []float32) []float32 {
	r.buf = append(r.buf, in...)
	for r.idx+r.half < len(r.buf) {
		row := r.taps[r.phase]
		win := r.buf[r.idx-r.half+1 : r.idx+r.half+1]
		var acc float32
		for j, t := range row {
			acc += win[j] * t
		}
		out = append(out, acc)
		r.phase += r.down
		r.idx += r.phase / r.up
		r.phase %= r.up
	}
	// Drop input the filter can no longer reach.
	if drop := r.idx - (r.half - 1); drop > 0 {
		n := copy(r.buf, r.buf[drop:])
		r.buf = r.buf[:n]
		r.idx -= drop
	}
	return out
}

// Flush pushes the buffered lookahead out by feeding silence, then resets
// so the next burst starts from a clean filter. Used at the end of each
// transmission so its last few milliseconds are not held back until the
// next one.
func (r *resampler) Flush(out []float32) []float32 {
	out = r.Process(make([]float32, r.half+1), out)
	r.reset()
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// kaiser is the Kaiser window at x in [-1, 1] (0 outside).
func kaiser(x, beta float64) float64 {
	if x < -1 || x > 1 {
		return 0
	}
	return besselI0(beta*math.Sqrt(1-x*x)) / besselI0(beta)
}

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 50; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
		if term < 1e-12*sum {
			break
		}
	}
	return sum
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
