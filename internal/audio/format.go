// Package audio holds the simulator's PCM types and the receive-side mixer
// (FM capture-effect). The router is otherwise opaque to audio content —
// modulation/demodulation lives in samoyed.
package audio

import "math"

// SampleRate is the simulator's audio rate. 48 kHz is pdn-soundmodem's
// native rate, what USB sound cards on real stations run at, and the rate
// the FM receive-path measurements the channel model is calibrated against
// were taken at. samoyed and direwolf are told to use it (ARATE).
const SampleRate = 48000

// BlockSamples is the audio block size used for routing decisions, in
// samples. 10 ms at 48 kHz = 480 samples = 960 bytes. Small enough that
// PTT-on/off transitions are tracked at frame-level granularity; large
// enough that goroutine scheduling overhead doesn't dominate.
const BlockSamples = 480

// BlockBytes is BlockSamples × 2 (mono int16 LE).
const BlockBytes = BlockSamples * 2

// Block is one audio block as raw bytes (LE int16 mono).
type Block []byte

// Silence returns a fresh zero-filled block.
func Silence() Block { return make(Block, BlockBytes) }

// AmplitudeFromDB returns the linear scaling factor for an attenuation
// expressed in dB (positive = quieter).
func AmplitudeFromDB(lossDB float64) float64 {
	return math.Pow(10, -lossDB/20.0)
}

// Scale returns a new block with each sample scaled by amp. amp is the
// linear scaling factor (0..1 typical).
func (b Block) Scale(amp float64) Block {
	if len(b) < 2 {
		return Silence()
	}
	out := make(Block, len(b))
	for i := 0; i+1 < len(b); i += 2 {
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		v := float64(s) * amp
		if v > math.MaxInt16 {
			v = math.MaxInt16
		}
		if v < math.MinInt16 {
			v = math.MinInt16
		}
		v16 := int16(v)
		out[i] = byte(uint16(v16))
		out[i+1] = byte(uint16(v16) >> 8)
	}
	return out
}

// IsSilence is true if every sample in the block is zero. Used by the TX
// detector to spot inter-frame quiet.
func (b Block) IsSilence() bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// RMS returns the root-mean-square sample amplitude of the block
// (0..32768). Used by the collision "noise" model to size the garble to
// the colliding signals' level.
func (b Block) RMS() float64 {
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

// PeakAbs returns the peak absolute amplitude in the block (0..32768).
// Used as a rough RX-level estimator for the capture-effect mixer.
func (b Block) PeakAbs() int {
	max := 0
	for i := 0; i+1 < len(b); i += 2 {
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		v := int(s)
		if v < 0 {
			v = -v
		}
		if v > max {
			max = v
		}
	}
	return max
}
