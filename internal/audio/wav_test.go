package audio

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWAVWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.wav")

	w, err := NewWAVWriter(path)
	if err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}

	const nBlocks = 7
	for i := 0; i < nBlocks; i++ {
		blk := mkBlock(int16(i*1000) - 3000)
		if _, err := w.Write(blk); err != nil {
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

	wantData := uint32(nBlocks * BlockBytes)
	if got := uint32(len(b)); got != wavHeaderBytes+wantData {
		t.Fatalf("file length = %d, want %d", got, wavHeaderBytes+wantData)
	}

	if string(b[0:4]) != "RIFF" {
		t.Errorf("RIFF magic = %q", b[0:4])
	}
	if got := binary.LittleEndian.Uint32(b[4:8]); got != 36+wantData {
		t.Errorf("RIFF chunk size = %d, want %d", got, 36+wantData)
	}
	if string(b[8:12]) != "WAVE" {
		t.Errorf("WAVE magic = %q", b[8:12])
	}
	if string(b[12:16]) != "fmt " {
		t.Errorf("fmt magic = %q", b[12:16])
	}
	if got := binary.LittleEndian.Uint16(b[20:22]); got != 1 {
		t.Errorf("audio format = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(b[22:24]); got != 1 {
		t.Errorf("num channels = %d, want 1 (mono)", got)
	}
	if got := binary.LittleEndian.Uint32(b[24:28]); got != SampleRate {
		t.Errorf("sample rate = %d, want %d", got, SampleRate)
	}
	if got := binary.LittleEndian.Uint16(b[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if string(b[36:40]) != "data" {
		t.Errorf("data magic = %q", b[36:40])
	}
	if got := binary.LittleEndian.Uint32(b[40:44]); got != wantData {
		t.Errorf("data chunk size = %d, want %d", got, wantData)
	}

	// First sample of the first block was mkBlock(-3000) → int16 LE = 0x88 0xF4.
	first := int16(uint16(b[44]) | uint16(b[45])<<8)
	if first != -3000 {
		t.Errorf("first sample = %d, want -3000", first)
	}
}

func TestWAVWriterCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAVWriter(filepath.Join(dir, "out.wav"))
	if err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v (want nil)", err)
	}
}

func TestWAVWriterWriteAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAVWriter(filepath.Join(dir, "out.wav"))
	if err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}
	_ = w.Close()
	if _, err := w.Write(make([]byte, 4)); err == nil {
		t.Error("Write after Close should error")
	}
}

func TestWAVWriterStopsAtSizeLimit(t *testing.T) {
	w, err := NewWAVWriter(filepath.Join(t.TempDir(), "full.wav"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.written = maxWAVData - 1000 // pretend ~4 GiB is already on disk
	if _, err := w.Write(make([]byte, 882)); err != nil {
		t.Fatalf("write under the limit: %v", err)
	}
	if _, err := w.Write(make([]byte, 882)); !errors.Is(err, ErrWAVFull) {
		t.Fatalf("write over the limit: err %v, want ErrWAVFull", err)
	}
	if w.written > maxWAVData {
		t.Fatalf("written %d exceeds the WAV limit", w.written)
	}
}
