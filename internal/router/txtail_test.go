package router

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// A burst that doesn't end on a block boundary must still reach the link
// queue once the source goes quiet, zero-padded, rather than waiting for
// the next burst.
func TestTxReaderFlushesBurstTail(t *testing.T) {
	src := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}
	q := newLinkQueue(src, dst, linkRF{})
	r := &Router{
		cfg:        &config.Config{TimeScale: 1},
		logger:     quietLogger(),
		linkQueues: map[config.PortRef][]*linkQueue{src: {q}},
	}

	pr, pw, err := os.Pipe() // supports read deadlines, like a FIFO
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.txReader(ctx, src, pr); close(done) }()
	defer func() { cancel(); pr.Close(); pw.Close(); <-done }()

	burst := make([]byte, audio.BlockBytes+100)
	for i := range burst {
		burst[i] = 0x11
	}
	if _, err := pw.Write(burst); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		n := len(q.buf)
		q.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) != 2 {
		t.Fatalf("queue holds %d blocks, want the full block plus the flushed tail", len(q.buf))
	}
	tail := q.buf[1]
	if tail.blk[99] != 0x11 || tail.blk[100] != 0 || tail.blk[len(tail.blk)-1] != 0 {
		t.Error("tail block is not the last 100 bytes followed by zero padding")
	}
}
