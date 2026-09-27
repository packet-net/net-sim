package router

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// recordSession owns one timestamped directory of WAV files - two files
// (.tx.wav, .rx.wav) per port - and is the tap point for the router's
// audio path. The router holds zero or one of these via an atomic
// pointer, so hot toggle is just "swap the pointer".
//
// Disk I/O never happens on the audio path: each stream has its own
// buffered queue and writer goroutine, so a slow disk can't stall a
// txReader or rxFeeder. A stream whose queue overflows or whose write
// fails is stopped (with a warning) rather than left with silent gaps, so
// every file that is still growing stays sample-aligned with the others.
type recordSession struct {
	dir    string
	logger *slog.Logger

	mu      sync.RWMutex // write-locked only by Close
	writers map[config.PortRef]*portWriters
	closed  bool
}

type portWriters struct {
	tx *streamWriter
	rx *streamWriter
}

// recordQueueBlocks is each stream's buffer: 10 s of audio.
const recordQueueBlocks = 10 * audio.SampleRate / audio.BlockSamples

// streamWriter is one WAV file fed through a queue by its own goroutine.
type streamWriter struct {
	name   string
	w      *audio.WAVWriter
	ch     chan audio.Block
	done   chan struct{}
	dead   atomic.Bool
	logger *slog.Logger
}

func newStreamWriter(path string, logger *slog.Logger) (*streamWriter, error) {
	w, err := audio.NewWAVWriter(path)
	if err != nil {
		return nil, err
	}
	sw := &streamWriter{
		name:   filepath.Base(path),
		w:      w,
		ch:     make(chan audio.Block, recordQueueBlocks),
		done:   make(chan struct{}),
		logger: logger,
	}
	go sw.run()
	return sw, nil
}

func (sw *streamWriter) run() {
	defer close(sw.done)
	for blk := range sw.ch {
		if sw.dead.Load() {
			continue
		}
		if _, err := sw.w.Write(blk); err != nil {
			sw.stop("write failed", err)
		}
	}
	_ = sw.w.Close()
}

// write queues a copy of blk without ever blocking.
func (sw *streamWriter) write(blk audio.Block) {
	if sw.dead.Load() {
		return
	}
	select {
	case sw.ch <- append(audio.Block(nil), blk...):
	default:
		sw.stop("disk not keeping up (queue full)", nil)
	}
}

func (sw *streamWriter) stop(why string, err error) {
	if sw.dead.CompareAndSwap(false, true) && sw.logger != nil {
		sw.logger.Warn("recorder: stopping stream: "+why, "file", sw.name, "err", err)
	}
}

// close ends the queue; wait blocks until the file is finalised.
func (sw *streamWriter) close() { close(sw.ch) }
func (sw *streamWriter) wait()  { <-sw.done }

// newRecordSession creates a fresh subdirectory under base named with the
// current UTC timestamp, then opens one .tx.wav + .rx.wav per port. The
// final directory path is reported via Dir() and logged for the
// operator.
func newRecordSession(base string, refs []config.PortRef, logger *slog.Logger) (*recordSession, error) {
	if base == "" {
		return nil, fmt.Errorf("recorder: base dir is empty")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: mkdir %q: %w", base, err)
	}
	// Compact RFC3339-ish for filenames (no colons — easier to copy-paste).
	// Millisecond precision so a quick Stop → Start cycle in the UI
	// can't overwrite the previous session.
	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	dir := filepath.Join(base, stamp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: mkdir session dir %q: %w", dir, err)
	}

	s := &recordSession{
		dir:     dir,
		logger:  logger,
		writers: map[config.PortRef]*portWriters{},
	}

	for _, ref := range refs {
		txPath := filepath.Join(dir, fmt.Sprintf("%s.%s.tx.wav", ref.NodeID, ref.PortID))
		rxPath := filepath.Join(dir, fmt.Sprintf("%s.%s.rx.wav", ref.NodeID, ref.PortID))
		tx, err := newStreamWriter(txPath, logger)
		if err != nil {
			s.closeAll()
			return nil, fmt.Errorf("recorder: open %s: %w", txPath, err)
		}
		rx, err := newStreamWriter(rxPath, logger)
		if err != nil {
			tx.close()
			tx.wait()
			s.closeAll()
			return nil, fmt.Errorf("recorder: open %s: %w", rxPath, err)
		}
		s.writers[ref] = &portWriters{tx: tx, rx: rx}
	}

	logger.Info("recording started", "dir", dir, "ports", len(refs))
	return s, nil
}

// Dir is the absolute path of the per-session subdirectory. Useful for
// status reporting.
func (s *recordSession) Dir() string { return s.dir }

// WriteTX queues one block for ref's TX wav. Never blocks.
func (s *recordSession) WriteTX(ref config.PortRef, blk audio.Block) {
	if pw := s.port(ref); pw != nil {
		pw.tx.write(blk)
	}
	s.done()
}

// WriteRX queues one block for ref's RX wav. Never blocks.
func (s *recordSession) WriteRX(ref config.PortRef, blk audio.Block) {
	if pw := s.port(ref); pw != nil {
		pw.rx.write(blk)
	}
	s.done()
}

// port read-locks the session and returns ref's writers, or nil if the
// session is closed or has no such port. Callers must call done after.
func (s *recordSession) port(ref config.PortRef) *portWriters {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	if s.closed {
		return nil
	}
	return s.writers[ref]
}

func (s *recordSession) done() {
	if s != nil {
		s.mu.RUnlock()
	}
}

// Close drains every queue, patches the WAV headers and closes the files.
// Idempotent.
func (s *recordSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.closeAll()
	if s.logger != nil {
		s.logger.Info("recording stopped", "dir", s.dir)
	}
}

// closeAll ends every stream and waits for its file to be finalised. Only
// called once no writer can reach the streams (closed is set, or the
// session was never published).
func (s *recordSession) closeAll() {
	for _, pw := range s.writers {
		pw.tx.close()
		pw.rx.close()
	}
	for _, pw := range s.writers {
		pw.tx.wait()
		pw.rx.wait()
	}
}
