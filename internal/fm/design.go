// Package fm is a physical FM link, streamed: transmit audio really is
// frequency-modulated onto a carrier, receivers really add noise at the
// carrier, filter it to their IF bandwidth and put it through a limiter and
// discriminator, and the radios' audio paths shape both ends.
//
// It is a Go port of M0LTE.FmChannel (https://github.com/M0LTE/M0LTE.FmChannel),
// which pdn-soundmodem measures its FM modes through. The stages, filter
// designs and conventions are that package's; what is new here is that they
// run continuously in blocks, several carriers can reach one receiver, and a
// receiver with no carrier at all still produces output (open-squelch hiss).
// The testdata directory holds reference vectors made from the C# package by
// tools/fmref, and the tests hold this port to them.
//
// Carrier-to-noise ratios follow the package's convention: carrier power over
// noise power in the receiver's IF bandwidth, which is a -6 dB total width.
package fm

import "math"

// skirtFraction is how wide a filter's transition is as a fraction of the
// passband it describes. Filters are specified in hertz and the tap count is
// derived, so they are the same filter whatever rate they run at.
const skirtFraction = 0.15

// tapsFor gives an odd tap count for roughly transitionHz of transition at
// rate: the usual windowed-sinc rule, clamped to 31..2047.
func tapsFor(rate float64, transitionHz float64) int {
	taps := int(math.Ceil(4 * rate / math.Max(transitionHz, 1)))
	taps |= 1
	if taps < 31 {
		taps = 31
	}
	if taps > 2047 {
		taps = 2047
	}
	return taps
}

// lowPass is a Hann-windowed sinc low-pass normalised to unity DC gain,
// half amplitude at cutoffHz (M0LTE.Dsp FilterDesign.LowPass).
func lowPass(cutoffHz, rate float64, taps int) []float64 {
	h := make([]float64, taps)
	fc := cutoffHz / rate
	centre := float64(taps-1) / 2
	sum := 0.0
	for i := range h {
		x := float64(i) - centre
		sinc := 2 * fc
		if x != 0 {
			sinc = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		window := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(taps-1))
		h[i] = sinc * window
		sum += h[i]
	}
	for i := range h {
		// The C# design rounds its taps to float32; so do we, so the two
		// filters are the same filter.
		h[i] = float64(float32(h[i] / sum))
	}
	return h
}

// bandPass is the difference of two low-passes (FilterDesign.BandPass).
func bandPass(lowHz, highHz, rate float64, taps int) []float64 {
	upper := lowPass(highHz, rate, taps)
	lower := lowPass(lowHz, rate, taps)
	for i := range upper {
		upper[i] = float64(float32(upper[i] - lower[i]))
	}
	return upper
}

// bandLimitKernel is an audio path's passband filter: band-pass when the
// low edge is above 20 Hz, otherwise a low-pass (FmChannel.BandLimit).
func bandLimitKernel(rate, lowHz, highHz float64) []float64 {
	high := math.Min(highHz, rate/2*0.95)
	taps := tapsFor(rate, math.Max(high-math.Max(lowHz, 0), 1)*skirtFraction)
	if lowHz > 20 {
		return bandPass(lowHz, high, rate, taps)
	}
	return lowPass(high, rate, taps)
}

// resampleKernel is the low-pass used both to interpolate audio up to the
// IF rate and to decimate the discriminator's output back down.
func resampleKernel(ifRate float64, factor int) []float64 {
	cutoff := ifRate / (2 * float64(factor)) * 0.9
	return lowPass(cutoff, ifRate, tapsFor(ifRate, cutoff*skirtFraction))
}

// ifKernel is the receiver's channel filter on the complex envelope: a
// low-pass at half the IF bandwidth on each of I and Q.
func ifKernel(ifRate, ifBandwidthHz float64) []float64 {
	return lowPass(ifBandwidthHz/2, ifRate, tapsFor(ifRate, ifBandwidthHz*skirtFraction))
}

// emphasisCoefficient is the one-pole coefficient shared by pre- and
// de-emphasis, so a matched pair cancels exactly.
func emphasisCoefficient(rate, microseconds float64) float64 {
	return 1 - math.Exp(-1/(rate*microseconds*1e-6))
}

// InterpolationFactor is the IF oversampling for a transmit path: the
// smallest power of two (2 to 16) that puts the IF rate at four times
// Carson's bandwidth, as the C# package chooses.
func InterpolationFactor(audioRate int, peakDeviationHz, audioHighHz float64) int {
	carson := 2 * (peakDeviationHz + audioHighHz)
	f := 2
	for f < 16 && float64(audioRate*f) < 4*carson {
		f *= 2
	}
	return f
}
