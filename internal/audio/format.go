// Package audio holds the simulator's PCM types: the sample rate, the 10 ms
// block the router moves audio in, and the WAV writers and audio tap. The
// FM channel itself is in internal/fm.
package audio

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

// PeakAbs returns the peak absolute amplitude in the block (0..32768).
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
