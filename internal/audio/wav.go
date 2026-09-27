package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// WAVWriter streams mono 16-bit-LE PCM at audio.SampleRate to a WAV file.
//
// The header is written up-front with placeholder sizes; on Close the two
// size fields (RIFF chunk size and data sub-chunk size) are patched with
// the real byte counts. If the process is killed without Close running,
// the file is still readable as raw int16 LE PCM but media players will
// not know how long the data section is — most will play the first few
// kilobytes and stop.
type WAVWriter struct {
	f       *os.File
	written uint32 // bytes of PCM written so far (data chunk size)
	closed  bool
}

const wavHeaderBytes = 44

// ErrWAVFull is returned once a WAV file reaches the format's 4 GiB limit
// (a 32-bit size field). The file is left valid up to that point; at
// 44.1 kHz mono that is about 13.5 hours, stereo about 6.8.
var ErrWAVFull = errors.New("wav: file reached the 4 GiB WAV size limit")

// maxWAVData is the largest data chunk whose RIFF size (36 + data) still
// fits in 32 bits.
const maxWAVData = math.MaxUint32 - 36

// capWrite writes b to f unless that would take written past the WAV
// limit, in which case it writes nothing and returns ErrWAVFull. frame is
// the byte size of one sample frame, so a file is never cut mid-frame.
func capWrite(f *os.File, written *uint32, b []byte, frame int) (int, error) {
	if uint64(*written)+uint64(len(b)) > maxWAVData/uint64(frame)*uint64(frame) {
		return 0, ErrWAVFull
	}
	n, err := f.Write(b)
	*written += uint32(n)
	return n, err
}

// NewWAVWriter creates path and writes the WAV header. The file is left
// open and ready to receive Block bytes via Write.
func NewWAVWriter(path string) (*WAVWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := writeWAVHeader(f, 0, 1); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write wav header: %w", err)
	}
	return &WAVWriter{f: f}, nil
}

// Write appends PCM bytes verbatim. The caller is responsible for handing
// in correctly-formatted little-endian int16 mono samples — Block already
// is.
func (w *WAVWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	return capWrite(w.f, &w.written, b, 2)
}

// Close patches the two size fields in the header and closes the file.
// Idempotent: calling twice is a no-op on the second call.
func (w *WAVWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	// Best-effort header patch even if a previous Write errored: a partial
	// file with the correct size in the header is still useful.
	if _, err := w.f.Seek(0, io.SeekStart); err == nil {
		_ = writeWAVHeader(w.f, w.written, 1)
	}
	return w.f.Close()
}

// MultiWAVWriter streams interleaved N-channel 16-bit-LE PCM at
// audio.SampleRate to a WAV file. It is the multi-track sibling of
// WAVWriter — same crash-safety contract (header patched on Close) — used
// by the composite recorder to write one transmitter per channel into a
// single sample-aligned file.
//
// Callers hand Write fully-interleaved frames (sample 0 of every channel,
// then sample 1 of every channel, …). Interleave builds those from a set
// of per-channel mono blocks.
type MultiWAVWriter struct {
	f        *os.File
	channels uint16
	written  uint32 // bytes of PCM written so far (data chunk size)
	closed   bool
}

// NewMultiWAVWriter creates path and writes an N-channel WAV header.
// channels must be >= 1.
func NewMultiWAVWriter(path string, channels int) (*MultiWAVWriter, error) {
	if channels < 1 {
		return nil, fmt.Errorf("wav: channels must be >= 1, got %d", channels)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := writeWAVHeader(f, 0, uint16(channels)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write wav header: %w", err)
	}
	return &MultiWAVWriter{f: f, channels: uint16(channels)}, nil
}

// Write appends interleaved PCM bytes verbatim. The caller is responsible
// for handing in correctly-formatted little-endian int16 frames matching
// the channel count — Interleave produces exactly that.
func (w *MultiWAVWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	return capWrite(w.f, &w.written, b, 2*int(w.channels))
}

// Close patches the two size fields and closes the file. Idempotent.
func (w *MultiWAVWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if _, err := w.f.Seek(0, io.SeekStart); err == nil {
		_ = writeWAVHeader(w.f, w.written, w.channels)
	}
	return w.f.Close()
}

// Interleave merges len(blocks) equal-length mono blocks into one
// interleaved frame group: sample 0 of channel 0, sample 0 of channel 1,
// …, sample 1 of channel 0, … Each input block is raw LE int16 mono PCM
// (typically a BlockBytes-sized audio.Block). A nil or short block is
// treated as silence so a channel with no audio this tick still advances
// in lock-step with the others — that lock-step is what keeps every
// channel sample-aligned to the same start time.
func Interleave(blocks []Block) []byte {
	n := len(blocks)
	if n == 0 {
		return nil
	}
	// Frame count is governed by the longest block; shorter/absent blocks
	// pad with silence. In normal operation every block is BlockBytes.
	maxBytes := 0
	for _, b := range blocks {
		if len(b) > maxBytes {
			maxBytes = len(b)
		}
	}
	samples := maxBytes / 2
	out := make([]byte, samples*n*2)
	for ch, b := range blocks {
		for s := 0; s < samples; s++ {
			lo, hi := byte(0), byte(0)
			if i := s * 2; i+1 < len(b) {
				lo, hi = b[i], b[i+1]
			}
			o := (s*n + ch) * 2
			out[o] = lo
			out[o+1] = hi
		}
	}
	return out
}

// writeWAVHeader writes the canonical 44-byte RIFF/WAVE/fmt /data header
// for numChannels-channel 16-bit LE PCM at SampleRate. dataSize is the
// byte length of the data chunk's PCM payload (0 at file creation;
// patched on Close).
func writeWAVHeader(w io.Writer, dataSize uint32, numChannels uint16) error {
	const (
		bitsPerSample = 16
		audioFormat   = 1 // PCM
	)
	byteRate := SampleRate * uint32(numChannels) * (bitsPerSample / 8)
	blockAlign := numChannels * (bitsPerSample / 8)
	hdr := make([]byte, 0, wavHeaderBytes)
	hdr = append(hdr, []byte("RIFF")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 36+dataSize)
	hdr = append(hdr, []byte("WAVE")...)
	hdr = append(hdr, []byte("fmt ")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 16) // fmt chunk size
	hdr = binary.LittleEndian.AppendUint16(hdr, audioFormat)
	hdr = binary.LittleEndian.AppendUint16(hdr, numChannels)
	hdr = binary.LittleEndian.AppendUint32(hdr, SampleRate)
	hdr = binary.LittleEndian.AppendUint32(hdr, byteRate)
	hdr = binary.LittleEndian.AppendUint16(hdr, blockAlign)
	hdr = binary.LittleEndian.AppendUint16(hdr, bitsPerSample)
	hdr = append(hdr, []byte("data")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, dataSize)
	_, err := w.Write(hdr)
	return err
}
