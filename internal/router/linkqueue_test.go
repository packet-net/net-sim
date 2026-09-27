package router

import (
	"log/slog"
	"testing"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// A transmission far longer than the old fixed 3 s buffer must be delivered
// gap-free: net-sim models a continuous carrier and must never drop audio
// within a transmission. Dropping mid-transmission gaps the receiver's audio,
// collapses its carrier sense, and makes the far end key up on top of the
// in-progress transmission (a collision), the bug this non-dropping FIFO
// fixes. Regression for the 3 s drop-on-overflow `linkQueue`.
func TestLinkQueueDeliversLongBurstGapFree(t *testing.T) {
	q := newLinkQueue(
		config.PortRef{NodeID: "a", PortID: "vhf"},
		config.PortRef{NodeID: "b", PortID: "vhf"},
		linkRF{},
	)

	// ~10 s of audio — well past the old 3 s cap that used to drop.
	n := 10 * audio.SampleRate / audio.BlockSamples
	for i := 0; i < n; i++ {
		blk := make(audio.Block, audio.BlockBytes)
		blk[0] = byte(i)
		blk[1] = byte(i >> 8)
		q.push(queuedBlock{blk: blk}, slog.Default())
	}

	for i := 0; i < n; i++ {
		qb, ok := q.pop()
		blk := qb.blk
		if !ok {
			t.Fatalf("block %d of %d was dropped — the queue must never drop within a transmission", i, n)
		}
		if got := int(blk[0]) | int(blk[1])<<8; got != i {
			t.Fatalf("FIFO order broken at block %d: got marker %d", i, got)
		}
	}
	if _, ok := q.pop(); ok {
		t.Fatal("queue should be empty after draining the whole transmission")
	}
}
