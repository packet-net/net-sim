// Package events provides a tiny pub/sub bus for visualising live
// simulator activity. The router publishes per-port TX and per-port RX
// events; sim-web subscribes and forwards them to browsers via SSE.
//
// Events are intentionally lossy: publishing is non-blocking and full
// subscriber channels lose the oldest pending event rather than stalling
// the audio path. The bus is therefore safe to call from hot per-block
// goroutines.
package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// Type discriminates the Event variants.
type Type string

const (
	// TXStart fires once per port when its audio peak crosses the
	// active-threshold from below. The port is "keyed."
	TXStart Type = "tx_start"

	// TXEnd fires once per port after a debounced run of silent blocks.
	// The port is "unkeyed."
	TXEnd Type = "tx_end"

	// RXDecision fires per receiving port when what is on its channel
	// changes: the decision flipped (silence, single, capture or
	// collision: a label for the RF situation, see router.decisionFor) or
	// the set of transmitters reaching it changed.
	RXDecision Type = "rx_decision"
)

// Event is a single point of activity on a port.
//
// Field semantics depend on Type:
//
//   - tx_start / tx_end:     Port set. Peak gives the block peak (0–32767)
//     at the moment of transition. Decision and Sources are empty.
//   - rx_decision:           Port is the destination. Decision is one of
//     "silence", "single", "capture", "collision", "sum". Sources are the
//     source ports currently delivering audio to this destination.
type Event struct {
	T        time.Time `json:"t"`
	Type     Type      `json:"type"`
	Port     string    `json:"port"`
	Peak     int       `json:"peak,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Sources  []string  `json:"sources,omitempty"`
}

// Bus is a fan-out broadcaster. Publishers never block; slow subscribers
// drop events.
type Bus struct {
	mu      sync.RWMutex
	subs    map[*subscription]struct{}
	stopped atomic.Bool
}

type subscription struct {
	ch chan Event
}

// NewBus returns an empty bus ready to publish/subscribe.
func NewBus() *Bus {
	return &Bus{subs: map[*subscription]struct{}{}}
}

// Publish delivers e to all current subscribers. Subscribers whose
// buffer is full lose the oldest pending event (the new one wins);
// publishing never blocks. Safe to call from any goroutine.
func (b *Bus) Publish(e Event) {
	if b == nil || b.stopped.Load() {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			// Buffer full — drop oldest, push newest. Best-effort:
			// if the slow consumer is *really* stuck we just keep
			// dropping the oldest.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- e:
			default:
			}
		}
	}
}

// Subscribe returns a buffered channel of Events and a cancel function
// that unsubscribes and closes the channel. The default buffer is sized
// to absorb a couple of seconds of full-load events from a 30-port sim
// without dropping.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	s := &subscription{ch: make(chan Event, 1024)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if _, ok := b.subs[s]; ok {
			delete(b.subs, s)
			close(s.ch)
		}
		b.mu.Unlock()
	}
	return s.ch, cancel
}

// Close drops all subscribers and discards subsequent Publish calls.
// Safe to call multiple times.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	if !b.stopped.CompareAndSwap(false, true) {
		return
	}
	b.mu.Lock()
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
	b.mu.Unlock()
}
