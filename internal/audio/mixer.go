package audio

import (
	"math"
	"math/rand"
	"sort"
	"sync"
)

// MixDecision describes what the mixer chose to emit, for verbose logging.
type MixDecision int

const (
	MixSilence MixDecision = iota
	MixSingle
	MixCapture   // strongest signal captured demodulator
	MixCollision // two or more signals within capture margin → garbage
	MixSum       // linear-sum mode (SSB stub)
)

// ActiveTX is one transmitter currently audible at a receiver port.
type ActiveTX struct {
	Block   Block
	LossDB  float64 // attenuation 0..N, larger = quieter
	NoiseDB float64 // optional noise level for this link (Phase 4)
}

// Level returns the receiver-side dB level of the TX. The mixer compares
// these to decide capture: stronger = larger Level (less negative).
//
// We use −LossDB rather than measuring peak amplitude on the block, because
// real FM capture cares about RF power on the channel, not modulated audio
// peaks (which are roughly constant for a digital signal anyway).
func (a ActiveTX) Level() float64 { return -a.LossDB }

// Mixer is the receive-side capture-effect mixer (PLAN Phase 3).
//
// Pure linear-sum mixing is wrong for FM. Real receivers exhibit the
// capture effect: when two signals arrive simultaneously the stronger one
// takes over the demodulator and the weaker is suppressed, *provided* the
// level difference exceeds the capture ratio (≈6 dB on narrow-band 2m FM).
// The default config sets CaptureDB = 6.0.
//
// One Mixer is shared by every receiving port; it holds no per-port state.
type Mixer struct {
	CaptureDB     float64
	CollisionMode string // "silence" | "sum" | "noise"
	LinearSum     bool   // true = SSB stub mode

	// rng is the noise source for AddNoise. *rand.Rand is NOT
	// goroutine-safe; rxFeeder goroutines call AddNoise concurrently,
	// so all access is serialised behind rngMu.
	rngMu sync.Mutex
	rng   *rand.Rand
}

// NewMixer constructs a mixer. captureDB 0 means the strongest signal
// always captures (the config supplies 6 when capture_db is absent).
func NewMixer(captureDB float64, linearSum bool, collisionMode string) *Mixer {
	return &Mixer{
		CaptureDB:     captureDB,
		CollisionMode: collisionMode,
		LinearSum:     linearSum,
		rng:           rand.New(rand.NewSource(1)),
	}
}

// Mix produces one block of receive audio from the list of active TX
// streams reaching this port. Returns the audio plus a tag describing the
// decision for verbose logging.
//
//	0 active   → silence
//	1 active   → that stream attenuated
//	2+ active, margin >= captureDB → strongest, attenuated (capture)
//	2+ active, margin <  captureDB → "collision garbage" per CollisionMode
func (m *Mixer) Mix(active []ActiveTX) (Block, MixDecision) {
	if m.LinearSum {
		// SSB stub. v1 only needs FM path working.
		return m.linearSum(active), MixSum
	}
	switch len(active) {
	case 0:
		return Silence(), MixSilence
	case 1:
		amp := AmplitudeFromDB(active[0].LossDB)
		return active[0].Block.Scale(amp), MixSingle
	}
	// >= 2: sort by RX level (strongest first), check capture margin.
	sorted := make([]ActiveTX, len(active))
	copy(sorted, active)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Level() > sorted[j].Level()
	})
	margin := sorted[0].Level() - sorted[1].Level()
	if margin >= m.CaptureDB {
		amp := AmplitudeFromDB(sorted[0].LossDB)
		return sorted[0].Block.Scale(amp), MixCapture
	}
	return m.collision(sorted), MixCollision
}

// collision renders the receiver's output when 2+ signals land inside the
// capture margin and none of them takes the demodulator. sorted is the
// active set ordered strongest-first (by receiver-side Level).
func (m *Mixer) collision(sorted []ActiveTX) Block {
	switch m.CollisionMode {
	case "noise":
		// FM capture below threshold does NOT yield silence: with two
		// comparable carriers on channel, the discriminator output is
		// loud garble — the heterodyne beat between the carriers plus
		// wideband noise — at an amplitude comparable to the signals
		// themselves. Pure digital silence is unrealistically clean: it
		// gives receiving modems a perfectly quiet channel exactly when
		// a real one would be hammering their DCD and false-sync
		// behaviour. Model the garble as gaussian noise with RMS equal
		// to the strongest signal's post-loss level, so a hot collision
		// is loud garble and a weak distant one is quiet garble.
		return m.noiseBlock(sorted[0].Block.RMS() * AmplitudeFromDB(sorted[0].LossDB))
	case "sum":
		// stub — behaves as silence until the SSB path needs it
		return Silence()
	}
	// "silence" (the default): the v1 model, kept for backwards
	// compatibility.
	return Silence()
}

// noiseBlock returns a fresh block of gaussian noise with the given RMS
// amplitude (sigma, in sample units, 0..32767). Routed through AddNoise so
// the RNG locking and int16 clamping live in one place.
func (m *Mixer) noiseBlock(sigma float64) Block {
	b := Silence()
	if sigma <= 0 {
		return b
	}
	// AddNoise expresses level as positive dB below full-scale; convert,
	// clamping at "full-scale garble" for sigma at/above full-scale.
	noiseDB := 20 * math.Log10(float64(math.MaxInt16)/sigma)
	if noiseDB <= 0 {
		noiseDB = 0.01
	}
	m.AddNoise(b, noiseDB)
	return b
}

func (m *Mixer) linearSum(active []ActiveTX) Block {
	if len(active) == 0 {
		return Silence()
	}
	out := make(Block, BlockBytes)
	// accumulate as int32 to avoid clipping mid-sum, then clamp to int16
	acc := make([]int32, BlockSamples)
	for _, tx := range active {
		amp := AmplitudeFromDB(tx.LossDB)
		for i := 0; i+1 < len(tx.Block) && i/2 < BlockSamples; i += 2 {
			s := int16(uint16(tx.Block[i]) | uint16(tx.Block[i+1])<<8)
			acc[i/2] += int32(float64(s) * amp)
		}
	}
	for i, v := range acc {
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		s := int16(v)
		out[i*2] = byte(uint16(s))
		out[i*2+1] = byte(uint16(s) >> 8)
	}
	return out
}

// AddNoise mixes white noise at the given dB-level into the block in place.
//
// noiseDB is the noise level expressed as positive dB below full-scale,
// applied to the gaussian noise's standard deviation (RMS amplitude):
//
//	noiseDB=20 → sigma ≈ 3277  (gentle hiss, well below 1200-baud signal)
//	noiseDB=10 → sigma ≈ 10362 (audible hiss, frame errors start to creep in)
//	noiseDB=6  → sigma ≈ 16412 (deafening — channel barely usable)
//
// 0 (or negative) is treated as "no noise". v1 generates flat gaussian
// noise — see PLAN Phase 4 ("flat white noise is the right model for
// channel noise floor at this layer").
func (m *Mixer) AddNoise(b Block, noiseDB float64) {
	if noiseDB <= 0 {
		return
	}
	sigma := AmplitudeFromDB(noiseDB) * float64(math.MaxInt16)
	// *rand.Rand is not goroutine-safe and many rxFeeder goroutines
	// call AddNoise concurrently. Take the lock for the duration of
	// one block — a few hundred RNG calls — rather than once per
	// sample, to keep contention low.
	m.rngMu.Lock()
	defer m.rngMu.Unlock()
	for i := 0; i+1 < len(b); i += 2 {
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		n := m.rng.NormFloat64() * sigma
		v := float64(s) + n
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		s2 := int16(v)
		b[i] = byte(uint16(s2))
		b[i+1] = byte(uint16(s2) >> 8)
	}
}

// fmQuietingMargin is the SNR (dB) below which no quieting occurs.
// Above this threshold each dB of SNR above the floor knocks the
// effective noise level down by fmQuietingSlope dB. This is the
// standard FM "threshold knee" — below threshold the receiver hears
// mostly noise; above it the carrier captures the limiter and the
// output rapidly cleans up.
const (
	fmQuietingMargin = 6.0  // dB headroom before quieting kicks in
	fmQuietingSlope  = 2.0  // dB of noise drop per dB of SNR above margin
	fmQuietingFloor  = 60.0 // quieting stops at this many dB below FS
)

// AddNoiseQuieted is AddNoise with FM threshold quieting baked in.
// The block's existing signal peak is read first; if it's strong
// enough relative to noiseDB, the effective noise level applied is
// reduced. Silence in → full noise out (idle channel hisses). Loud
// signal in → near-silent noise (signal "captures" the demod).
//
// Use this from the rxFeeder for receiver-side band noise. Use the
// plain AddNoise when you specifically want a fixed-level noise
// addition regardless of signal content (test fixtures, the
// collision-mode "noise" output).
func (m *Mixer) AddNoiseQuieted(b Block, noiseDB float64) {
	if noiseDB <= 0 {
		return
	}
	signalPeak := b.PeakAbs()
	noiseAmp := AmplitudeFromDB(noiseDB) * float64(math.MaxInt16)
	effectiveDB := noiseDB
	if signalPeak > 0 && noiseAmp > 0 {
		snrDB := 20 * math.Log10(float64(signalPeak)/noiseAmp)
		if snrDB > fmQuietingMargin {
			// Quieting only ever lowers the noise. A configured floor
			// already below fmQuietingFloor stays where it is rather
			// than being raised to it.
			effectiveDB += (snrDB - fmQuietingMargin) * fmQuietingSlope
			effectiveDB = math.Max(noiseDB, math.Min(effectiveDB, fmQuietingFloor))
		}
	}
	m.AddNoise(b, effectiveDB)
}
