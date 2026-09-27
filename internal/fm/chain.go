package fm

import (
	"math"
	"math/rand/v2"
)

// TxPath is a transmitter's audio path and modulator settings.
type TxPath struct {
	AudioLowHz, AudioHighHz float64 // the path's passband
	PreEmphasisUs           float64 // 0 = flat (a data port)

	// DeviationPerUnitHz is the deviation produced by audio of amplitude
	// 1.0 at the modulator input (after pre-emphasis): the drive. The
	// C# package's fixed-gain mode uses the link's peak deviation here.
	DeviationPerUnitHz float64

	// LimitHz, if non-zero, hard-clips the deviation there (the radio's
	// limiter). Applied after pre-emphasis and before the audio filter,
	// which is the order Tait document (MMA-00005-05 p.58).
	LimitHz float64
}

// Transmitter turns a stream of audio into a stream of unit-amplitude
// complex carrier samples at the IF rate (audio rate times Factor).
type Transmitter struct {
	rate, factor int
	path         TxPath

	preA, prevIn float64
	band         *fir
	interp       *fir
	phase        float64

	dev  []float64
	fine []complex128
}

// NewTransmitter builds a transmitter running at audioRate with IF
// oversampling factor.
func NewTransmitter(p TxPath, audioRate, factor int) *Transmitter {
	t := &Transmitter{rate: audioRate, factor: factor, path: p}
	if p.PreEmphasisUs > 0 {
		t.preA = emphasisCoefficient(float64(audioRate), p.PreEmphasisUs)
	}
	t.band = newFIR(bandLimitKernel(float64(audioRate), p.AudioLowHz, p.AudioHighHz))
	t.interp = newFIR(resampleKernel(float64(audioRate*factor), factor))
	return t
}

// Factor is the IF oversampling factor.
func (t *Transmitter) Factor() int { return t.factor }

// Process modulates audio (full scale +/-1) and appends
// len(audio)*Factor carrier samples to out.
func (t *Transmitter) Process(audio []float32, out []complex128) []complex128 {
	t.dev = t.dev[:0]
	for _, a := range audio {
		v := float64(a)
		if t.preA > 0 {
			// Inverse of the receiver's one-pole de-emphasis.
			in := v
			v = (in - (1-t.preA)*t.prevIn) / t.preA
			t.prevIn = in
		}
		hz := v * t.path.DeviationPerUnitHz
		if l := t.path.LimitHz; l > 0 {
			hz = math.Max(-l, math.Min(l, hz))
		}
		t.dev = append(t.dev, hz)
	}
	// Filter the audio path, in hertz of deviation (linear, so the same as
	// filtering the audio and scaling after).
	t.fine = t.band.processReal(t.dev, t.fine)

	// Interpolate: zero-stuff by factor (with gain factor) and low-pass.
	n := len(t.dev) * t.factor
	if cap(t.fine) < n {
		t.fine = make([]complex128, n)
	}
	fine := t.fine[:n]
	clear(fine)
	for i, v := range t.dev {
		fine[i*t.factor] = complex(v*float64(t.factor), 0)
	}
	t.interp.process(fine)

	// Integrate frequency into phase and walk the unit circle.
	k := 2 * math.Pi / float64(t.rate*t.factor)
	for _, f := range fine {
		t.phase += k * float64(float32(real(f)))
		if t.phase > math.Pi {
			t.phase -= 2 * math.Pi
		} else if t.phase < -math.Pi {
			t.phase += 2 * math.Pi
		}
		s, c := math.Sincos(t.phase)
		out = append(out, complex(c, s))
	}
	return out
}

// RxPath is a receiver's IF and audio path settings.
type RxPath struct {
	IFBandwidthHz           float64 // -6 dB total width of the channel filter
	AudioLowHz, AudioHighHz float64
	DeEmphasisUs            float64

	// FullScaleDeviationHz is the deviation that the discriminator output
	// reports as 1.0 (the C# package uses the link's peak deviation).
	FullScaleDeviationHz float64
}

// Receiver turns the sum of whatever carriers reach it, plus its own
// noise, into audio.
type Receiver struct {
	rate, factor int
	path         RxPath

	noiseSigma float64 // per I and Q, for unit noise power in the IF bandwidth
	rng        *rand.Rand

	ifFilter *fir
	prev     complex128
	started  bool
	scale    float64
	dec      *fir
	decPhase int
	deA, deY float64
	band     *fir

	disc    []float64
	scratch []complex128
	audio   []float64
}

// NewReceiver builds a receiver. With noise false it adds nothing (a
// noiseless link, for conformance tests); otherwise its noise has unit
// power in the IF bandwidth, and carrier amplitudes are set relative to
// that: a carrier of amplitude sqrt(10^(cnr/10)) is at cnr dB.
func NewReceiver(p RxPath, audioRate, factor int, noise bool, seed uint64) *Receiver {
	ifRate := float64(audioRate * factor)
	r := &Receiver{
		rate: audioRate, factor: factor, path: p,
		rng:      rand.New(rand.NewPCG(seed, 0x6e65742d73696d)),
		ifFilter: newFIR(ifKernel(ifRate, p.IFBandwidthHz)),
		scale:    ifRate / (2 * math.Pi) / p.FullScaleDeviationHz,
		dec:      newFIR(resampleKernel(ifRate, factor)),
		band:     newFIR(bandLimitKernel(float64(audioRate), p.AudioLowHz, p.AudioHighHz)),
	}
	if noise {
		r.noiseSigma = math.Sqrt(ifRate / p.IFBandwidthHz / 2)
	}
	if p.DeEmphasisUs > 0 {
		r.deA = emphasisCoefficient(float64(audioRate), p.DeEmphasisUs)
	}
	return r
}

// Amplitude is the carrier amplitude for a carrier-to-noise ratio.
func Amplitude(cnrDB float64) float64 { return math.Pow(10, cnrDB/20) }

// Process takes the summed carriers at the IF rate (len a multiple of
// Factor; they are modified in place) and appends the receiver's audio,
// len/Factor samples with full-scale deviation at +/-1, to out.
func (r *Receiver) Process(iq []complex128, out []float32) []float32 {
	if r.noiseSigma > 0 {
		for i := range iq {
			iq[i] += complex(r.noiseSigma*r.rng.NormFloat64(), r.noiseSigma*r.rng.NormFloat64())
		}
	}
	r.ifFilter.process(iq)

	// Limiter and discriminator in one: the angle between successive
	// samples is the instantaneous frequency, whatever the amplitude.
	r.disc = r.disc[:0]
	for _, z := range iq {
		if !r.started {
			r.prev, r.started = z, true
		}
		d := z * complex(real(r.prev), -imag(r.prev))
		r.disc = append(r.disc, float64(float32(math.Atan2(imag(d), real(d))*r.scale)))
		r.prev = z
	}

	// Decimate back to the audio rate.
	r.scratch = r.dec.processReal(r.disc, r.scratch)
	r.audio = r.audio[:0]
	for i, v := range r.disc {
		if (r.decPhase+i)%r.factor == 0 {
			r.audio = append(r.audio, v)
		}
	}
	r.decPhase = (r.decPhase + len(r.disc)) % r.factor

	if r.deA > 0 {
		for i, v := range r.audio {
			r.deY += r.deA * (v - r.deY)
			r.audio[i] = r.deY
		}
	}
	r.scratch = r.band.processReal(r.audio, r.scratch)
	for _, v := range r.audio {
		out = append(out, float32(v))
	}
	return out
}
