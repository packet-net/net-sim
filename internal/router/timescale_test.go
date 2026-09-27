package router

import (
	"bytes"
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// TestScaledPeriods pins the arithmetic every paced loop relies on: the
// rxFeeder/composite block ticker, the txWatchdog tick, and the
// txSilenceWindow all divide by time_scale, and scale <= 1 (including
// the zero value of a hand-built config) is a no-op.
func TestScaledPeriods(t *testing.T) {
	cases := []struct {
		d     time.Duration
		scale float64
		want  time.Duration
	}{
		{blockPeriod, 1, 10 * time.Millisecond},
		{blockPeriod, 10, time.Millisecond},
		{blockPeriod, 0, 10 * time.Millisecond}, // unvalidated zero config = real time
		{txSilenceWindow, 1, 200 * time.Millisecond},
		{txSilenceWindow, 4, 50 * time.Millisecond},
		{txWatchdogTick, 5, 10 * time.Millisecond},
		{txWatchdogTick, 0.5, 50 * time.Millisecond}, // sub-1 scales are clamped to real time
	}
	for _, c := range cases {
		if got := scaled(c.d, c.scale); got != c.want {
			t.Errorf("scaled(%v, %g) = %v, want %v", c.d, c.scale, got, c.want)
		}
	}
}

// collectWriter accumulates rxFeeder output and closes done once target
// bytes have arrived, so the test can stop the feeder without sleeping.
type collectWriter struct {
	mu     sync.Mutex
	buf    []byte
	target int
	done   chan struct{}
	once   sync.Once
}

func (w *collectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	n := len(w.buf)
	w.mu.Unlock()
	if n >= w.target {
		w.once.Do(func() { close(w.done) })
	}
	return len(p), nil
}

// runRxFeederSequence pre-loads one link with nBlocks of a transmitter's
// carrier (a 1 kHz tone), runs the real rxFeeder against it at the given
// time_scale, and returns the first nBlocks of delivered audio. The
// receiver's noise is seeded, so the same carriers give the same audio.
func runRxFeederSequence(t *testing.T, timeScale float64, nBlocks int) []byte {
	t.Helper()
	cfg, err := config.ParseBytes([]byte(`
nodes:
  - { id: a, ports: [ { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8001 } ] }
  - { id: b, ports: [ { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8002 } ] }
links:
  - { from: a.vhf, to: b.vhf, path_loss_db: 140 }
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.TimeScale = timeScale
	src := config.PortRef{NodeID: "a", PortID: "vhf"}
	dst := config.PortRef{NodeID: "b", PortID: "vhf"}
	stations, factor := newStations(cfg)
	q := newLinkQueue(src, dst, newLinkRF(stations[src], stations[dst], 140, factor))

	r := &Router{
		cfg:      cfg,
		stations: stations,
		factor:   factor,
		logger:   quietLogger(),
		rxLinks:  map[config.PortRef][]*linkQueue{dst: {q}},
	}

	for i := 0; i < nBlocks; i++ {
		blk := make(audio.Block, audio.BlockBytes)
		for n := 0; n < audio.BlockSamples; n++ {
			v := int16(12000 * math.Sin(2*math.Pi*1000*float64(i*audio.BlockSamples+n)/audio.SampleRate))
			blk[2*n], blk[2*n+1] = byte(uint16(v)), byte(uint16(v)>>8)
		}
		q.push(queuedBlock{blk: blk, iq: stations[src].modulate(blk, blockPeriod)}, r.logger)
	}

	w := &collectWriter{target: nBlocks * audio.BlockBytes, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	feederDone := make(chan struct{})
	go func() {
		r.rxFeeder(ctx, dst, w)
		close(feederDone)
	}()
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		t.Fatal("rxFeeder never delivered the expected blocks")
	}
	cancel()
	<-feederDone
	return w.buf[:nBlocks*audio.BlockBytes]
}

// TestRxFeederSequenceIdenticalAcrossTimeScales: time_scale changes only
// the pacing of delivery, never its content: the audio a receiver hears
// must be byte-identical at scale 1 and at a large scale.
func TestRxFeederSequenceIdenticalAcrossTimeScales(t *testing.T) {
	const nBlocks = 8
	realTime := runRxFeederSequence(t, 1, nBlocks)
	accelerated := runRxFeederSequence(t, 20, nBlocks)
	if !bytes.Equal(realTime, accelerated) {
		t.Fatal("received audio differs between time_scale 1 and 20: scaling must affect timing only")
	}
	if audio.Block(realTime[len(realTime)-audio.BlockBytes:]).PeakAbs() < 1000 {
		t.Fatal("the receiver didn't demodulate the tone")
	}
}
