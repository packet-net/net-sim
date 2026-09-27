package audio

import (
	"math"
	"sync"
	"testing"
)

// TestAddNoiseQuietedThreshold checks the FM-quieting model: silent
// block in → noise out at the configured level (idle hiss). Loud
// signal block in → noise output substantially attenuated. Weak
// signal (below quieting margin) → noise unchanged.
func TestAddNoiseQuietedThreshold(t *testing.T) {
	m := NewMixer(6, false, "silence")

	idle := Silence()
	m.AddNoiseQuieted(idle, 22) // floor only
	floor := rms(idle)
	if floor < 1500 || floor > 4500 {
		t.Errorf("idle floor rms %.0f, want ~2600 (noiseDB=22)", floor)
	}

	// Loud carrier (~−1 dB FS sine-ish): fill block with alternating
	// ±29000 samples → peak ~29000, SNR ≈ 21 dB above the 22 dB floor.
	loud := make(Block, BlockBytes)
	for i := 0; i+1 < len(loud); i += 2 {
		v := int16(29000)
		if (i/2)%2 == 0 {
			v = -v
		}
		loud[i] = byte(uint16(v))
		loud[i+1] = byte(uint16(v) >> 8)
	}
	loudBefore := rms(loud)
	m.AddNoiseQuieted(loud, 22)
	loudAfter := rms(loud)
	delta := loudAfter - loudBefore
	if delta > 600 {
		t.Errorf("loud signal: noise added rms ~%.0f, want quieted (< 600 added)", delta)
	}

	// Weak signal (peak ~3000, SNR ~1 dB above noise): noise should
	// be roughly the same as the idle floor (no significant quieting).
	weak := make(Block, BlockBytes)
	for i := 0; i+1 < len(weak); i += 2 {
		v := int16(3000)
		if (i/2)%2 == 0 {
			v = -v
		}
		weak[i] = byte(uint16(v))
		weak[i+1] = byte(uint16(v) >> 8)
	}
	weakBefore := rms(weak)
	m.AddNoiseQuieted(weak, 22)
	weakAfter := rms(weak)
	// RMS adds in quadrature, so weakAfter² ≈ weakBefore² + floor².
	expected := math.Sqrt(weakBefore*weakBefore + floor*floor)
	if math.Abs(weakAfter-expected) > 800 {
		t.Errorf("weak signal noise: got rms %.0f, want ~%.0f (no quieting)", weakAfter, expected)
	}
}

// TestAddNoiseConcurrent is a regression test: every receiver goroutine
// in the router calls Mixer.AddNoise on the same *Mixer. Pre-fix the
// shared *rand.Rand was unprotected and panicked under load
// ("index out of range [-1]" inside math/rand). Race-detector enabled
// at CI catches the data race even without a panic.
func TestAddNoiseConcurrent(t *testing.T) {
	m := NewMixer(6, false, "silence")
	const goroutines = 16
	const iterations = 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				blk := Silence()
				m.AddNoise(blk, 18)
			}
		}()
	}
	wg.Wait()
}

// rms computes root-mean-square sample value (0..32768).
func rms(b Block) float64 {
	var sum float64
	n := 0
	for i := 0; i+1 < len(b); i += 2 {
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		sum += float64(s) * float64(s)
		n++
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

func TestAddNoiseLevels(t *testing.T) {
	m := NewMixer(6, false, "silence")
	cases := []struct {
		noiseDB    float64
		wantSigma  float64
		tolerance  float64
	}{
		{20, 3277, 500},   // -20 dBFS hiss
		{10, 10362, 1500}, // -10 dBFS
		{6, 16412, 2500},  // -6 dBFS, deafening
	}
	for _, c := range cases {
		b := Silence()
		m.AddNoise(b, c.noiseDB)
		got := rms(b)
		if math.Abs(got-c.wantSigma) > c.tolerance {
			t.Errorf("noise_db=%g: rms=%g, want ~%g (±%g)", c.noiseDB, got, c.wantSigma, c.tolerance)
		}
	}
}

func TestAddNoiseZeroIsNop(t *testing.T) {
	m := NewMixer(6, false, "silence")
	for _, nd := range []float64{0, -1, -100} {
		b := Silence()
		m.AddNoise(b, nd)
		if !b.IsSilence() {
			t.Errorf("noise_db=%g: expected silence to remain silent", nd)
		}
	}
}

// A receiver configured quieter than the quieting floor must not get
// noisier when a signal arrives.
func TestAddNoiseQuietedNeverRaisesNoise(t *testing.T) {
	m := NewMixer(6, false, "silence")
	sig := Silence()
	for i := 0; i+1 < len(sig); i += 2 {
		v := int16(16000)
		if (i/2)%2 == 1 {
			v = -16000
		}
		sig[i], sig[i+1] = byte(uint16(v)), byte(uint16(v)>>8)
	}
	idle := Silence()
	m.AddNoiseQuieted(idle, 70)
	withSig := append(Block(nil), sig...)
	m.AddNoiseQuieted(withSig, 70)
	// Noise alone on the signal block = difference from the clean signal.
	var sum float64
	for i := 0; i+1 < len(sig); i += 2 {
		d := float64(int16(uint16(withSig[i])|uint16(withSig[i+1])<<8)) - float64(int16(uint16(sig[i])|uint16(sig[i+1])<<8))
		sum += d * d
	}
	noiseUnderSignal := math.Sqrt(sum / float64(len(sig)/2))
	if noiseUnderSignal > rms(idle)*1.5 {
		t.Errorf("noise under a strong signal (rms %.1f) is louder than idle noise (rms %.1f)", noiseUnderSignal, rms(idle))
	}
}

func TestZeroCaptureDBCapturesAnyMargin(t *testing.T) {
	m := NewMixer(0, false, "silence")
	a := Silence()
	a[0] = 100
	_, dec := m.Mix([]ActiveTX{{Block: a, LossDB: 0}, {Block: a, LossDB: 0.5}})
	if dec != MixCapture {
		t.Errorf("capture_db 0 with a 0.5 dB margin: decision %v, want capture", dec)
	}
}
