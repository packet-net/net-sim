package router

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRecordSessionWritesPerPortFiles(t *testing.T) {
	base := t.TempDir()
	refs := []config.PortRef{
		{NodeID: "a", PortID: "vhf"},
		{NodeID: "b", PortID: "vhf"},
	}
	s, err := newRecordSession(base, refs, quietLogger())
	if err != nil {
		t.Fatalf("newRecordSession: %v", err)
	}

	blk := audio.Silence()
	for i := 0; i < 5; i++ {
		s.WriteTX(refs[0], blk)
		s.WriteRX(refs[1], blk)
	}
	s.Close()

	for _, name := range []string{"a.vhf.tx.wav", "a.vhf.rx.wav", "b.vhf.tx.wav", "b.vhf.rx.wav"} {
		fi, err := os.Stat(filepath.Join(s.Dir(), name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if fi.Size() < 44 {
			t.Errorf("%s size = %d, want >= 44 (header)", name, fi.Size())
		}
	}

	// a.vhf got 5 blocks of TX, no RX → tx file should have data, rx file
	// should be header-only.
	txInfo, _ := os.Stat(filepath.Join(s.Dir(), "a.vhf.tx.wav"))
	if want := int64(44 + 5*audio.BlockBytes); txInfo.Size() != want {
		t.Errorf("a.vhf.tx.wav size = %d, want %d", txInfo.Size(), want)
	}
	rxInfoA, _ := os.Stat(filepath.Join(s.Dir(), "a.vhf.rx.wav"))
	if rxInfoA.Size() != 44 {
		t.Errorf("a.vhf.rx.wav size = %d, want 44 (header only)", rxInfoA.Size())
	}
}

func TestRecordSessionCloseIsIdempotent(t *testing.T) {
	s, err := newRecordSession(t.TempDir(), []config.PortRef{{NodeID: "a", PortID: "x"}}, quietLogger())
	if err != nil {
		t.Fatalf("newRecordSession: %v", err)
	}
	s.Close()
	s.Close() // must not panic / error
}

func TestRecordSessionDirIsTimestamped(t *testing.T) {
	base := t.TempDir()
	s, err := newRecordSession(base, nil, quietLogger())
	if err != nil {
		t.Fatalf("newRecordSession: %v", err)
	}
	defer s.Close()
	rel, err := filepath.Rel(base, s.Dir())
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	// e.g. "20260506T120000.123Z" — 20 chars, ends with Z, contains 'T' separator
	if len(rel) != 20 || !strings.HasSuffix(rel, "Z") || !strings.Contains(rel, "T") {
		t.Errorf("session dir name = %q, want compact UTC stamp like 20060102T150405.000Z", rel)
	}
}

func TestRecordSessionNilSafe(t *testing.T) {
	var s *recordSession
	s.WriteTX(config.PortRef{NodeID: "a", PortID: "x"}, audio.Silence())
	s.WriteRX(config.PortRef{NodeID: "a", PortID: "x"}, audio.Silence())
	s.Close()
}

// TestRouterHotToggle exercises StartRecording / StopRecording on a
// Router that has not had its children spawned. We construct the struct
// directly because router.Start would shell out to samoyed.
func TestRouterHotToggle(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "a", Ports: []config.Port{{ID: "vhf"}}},
			{ID: "b", Ports: []config.Port{{ID: "vhf"}}},
		},
	}
	r := &Router{
		opts:   Options{RecordDir: t.TempDir()},
		cfg:    cfg,
		logger: quietLogger(),
	}

	if r.RecordingActive() {
		t.Fatal("fresh router should not be recording")
	}

	dir1, err := r.StartRecording()
	if err != nil {
		t.Fatalf("first StartRecording: %v", err)
	}
	if !r.RecordingActive() {
		t.Error("RecordingActive should be true after StartRecording")
	}
	if r.RecordingDir() != dir1 {
		t.Errorf("RecordingDir = %q, want %q", r.RecordingDir(), dir1)
	}

	if _, err := r.StartRecording(); err == nil {
		t.Error("second StartRecording should fail (already recording)")
	}

	if err := r.StopRecording(); err != nil {
		t.Fatalf("StopRecording: %v", err)
	}
	if r.RecordingActive() {
		t.Error("RecordingActive should be false after StopRecording")
	}

	if _, err := r.StartRecording(); err != nil {
		t.Fatalf("second cycle StartRecording: %v", err)
	}

	if err := r.StopRecording(); err != nil {
		t.Fatalf("second StopRecording: %v", err)
	}
	if err := r.StopRecording(); err != nil {
		t.Errorf("idempotent StopRecording: %v (want nil)", err)
	}
}

func TestRouterStartRecordingRejectsNoBaseDir(t *testing.T) {
	r := &Router{
		opts:   Options{},
		cfg:    &config.Config{},
		logger: quietLogger(),
	}
	if _, err := r.StartRecording(); err == nil {
		t.Error("StartRecording with empty RecordDir should error")
	}
}

// A stream whose disk can't keep up is stopped, never blocks the caller.
func TestStreamWriterNeverBlocks(t *testing.T) {
	sw := &streamWriter{name: "slow.wav", ch: make(chan audio.Block, 1), done: make(chan struct{}), logger: quietLogger()}
	// No run goroutine: nothing drains the queue, like a stalled disk.
	finished := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			sw.write(audio.Silence())
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("write blocked on a full queue")
	}
	if !sw.dead.Load() {
		t.Error("stream not stopped after its queue overflowed")
	}
}
