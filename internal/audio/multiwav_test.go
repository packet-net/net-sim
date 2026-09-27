package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestMultiWAVWriterStereoHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.wav")

	w, err := NewMultiWAVWriter(path, 2)
	if err != nil {
		t.Fatalf("NewMultiWAVWriter: %v", err)
	}

	// 3 interleaved stereo frames of BlockSamples each.
	const nBlocks = 3
	frameBytes := BlockBytes * 2 // 2 channels
	for i := 0; i < nBlocks; i++ {
		left := mkBlock(int16(1000 * (i + 1)))
		right := mkBlock(int16(-1000 * (i + 1)))
		if _, err := w.Write(Interleave([]Block{left, right})); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	wantData := uint32(nBlocks * frameBytes)
	if got := uint32(len(b)); got != wavHeaderBytes+wantData {
		t.Fatalf("file length = %d, want %d", got, wavHeaderBytes+wantData)
	}
	if got := binary.LittleEndian.Uint16(b[22:24]); got != 2 {
		t.Errorf("num channels = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(b[24:28]); got != SampleRate {
		t.Errorf("sample rate = %d, want %d", got, SampleRate)
	}
	// byteRate = SampleRate * channels * 2
	if got := binary.LittleEndian.Uint32(b[28:32]); got != SampleRate*2*2 {
		t.Errorf("byte rate = %d, want %d", got, SampleRate*2*2)
	}
	// blockAlign = channels * 2
	if got := binary.LittleEndian.Uint16(b[32:34]); got != 4 {
		t.Errorf("block align = %d, want 4", got)
	}
	if got := binary.LittleEndian.Uint32(b[40:44]); got != wantData {
		t.Errorf("data chunk size = %d, want %d", got, wantData)
	}

	// First interleaved frame: L sample = +1000, R sample = -1000.
	l0 := int16(uint16(b[44]) | uint16(b[45])<<8)
	r0 := int16(uint16(b[46]) | uint16(b[47])<<8)
	if l0 != 1000 || r0 != -1000 {
		t.Errorf("first frame = (L=%d,R=%d), want (1000,-1000)", l0, r0)
	}
}

func TestInterleaveStereo(t *testing.T) {
	// Two 2-sample mono blocks: L=[1,2], R=[3,4] → [1,3,2,4].
	left := Block{1, 0, 2, 0}  // int16 LE: 1, 2
	right := Block{3, 0, 4, 0} // int16 LE: 3, 4
	out := Interleave([]Block{left, right})
	want := []int16{1, 3, 2, 4}
	if len(out) != len(want)*2 {
		t.Fatalf("interleaved length = %d bytes, want %d", len(out), len(want)*2)
	}
	for i, w := range want {
		got := int16(uint16(out[i*2]) | uint16(out[i*2+1])<<8)
		if got != w {
			t.Errorf("sample %d = %d, want %d", i, got, w)
		}
	}
}

func TestInterleaveAbsentChannelIsSilence(t *testing.T) {
	// One channel has audio, the other is nil → that channel must still
	// occupy its slot, padded with silence, so channels stay aligned.
	left := mkBlock(5000)
	out := Interleave([]Block{left, nil})
	if len(out) != BlockBytes*2 {
		t.Fatalf("interleaved length = %d, want %d", len(out), BlockBytes*2)
	}
	// Frame 0: L=5000, R=0.
	l0 := int16(uint16(out[0]) | uint16(out[1])<<8)
	r0 := int16(uint16(out[2]) | uint16(out[3])<<8)
	if l0 != 5000 || r0 != 0 {
		t.Errorf("frame 0 = (L=%d,R=%d), want (5000,0)", l0, r0)
	}
}

func TestNewMultiWAVWriterRejectsZeroChannels(t *testing.T) {
	if _, err := NewMultiWAVWriter(filepath.Join(t.TempDir(), "x.wav"), 0); err == nil {
		t.Error("NewMultiWAVWriter(0) should error")
	}
}

func mkBlock(level int16) Block {
	b := make(Block, BlockBytes)
	for i := 0; i+1 < BlockBytes; i += 2 {
		b[i] = byte(uint16(level))
		b[i+1] = byte(uint16(level) >> 8)
	}
	return b
}
