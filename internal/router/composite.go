package router

import (
	"errors"
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

// compositeRecorder captures a chosen set of transmitters into a single,
// real-time-paced, sample-aligned multi-channel WAV — one transmitter per
// channel. For the canonical two-station case that is a stereo file with
// one station's transmitted audio in each ear.
//
// Why this is separate from recordSession's per-port .tx.wav files: those
// are written straight from txReader as samoyed *bursts* the audio (a
// 500 ms frame can land in a few wall-clock ms), so they faithfully carry
// the modulated waveform but are NOT a real-time timeline. The composite
// recorder instead drains each transmitter's blocks through its own
// real-time ticker — exactly the model rxFeeder uses to pace audio into
// samoyed — so the resulting file is an accurate timeline of what was on
// the air: overlapping transmissions overlap in time, inter-burst gaps are
// preserved as real silence, and because every channel is advanced one
// block per tick by a single goroutine they share one clock and one start
// time.
type compositeRecorder struct {
	path     string
	channels []config.PortRef
	logger   *slog.Logger
	started  time.Time

	// timeScale is the simulation's acceleration factor; the pacing
	// ticker runs at scaled(blockPeriod, timeScale) so the composite
	// stays sample-aligned with the (faster-than-wall-clock) sim
	// timeline. The WAV itself is still a SampleRate file — its time
	// axis is sim time, not wall-clock time.
	timeScale float64

	idx    map[config.PortRef]int // port → channel index
	queues []chan audio.Block     // one per channel; single producer each
	writer *audio.MultiWAVWriter

	frames atomic.Int64 // interleaved frames written (one per block/channel-group)

	failed     atomic.Bool // a write failed; feed stops queueing
	overflowed atomic.Bool // currently overflowing; warn once per episode

	done     chan struct{} // closed by stop() to request a graceful drain+close
	stopped  chan struct{} // closed by the run goroutine when it has fully exited
	stopOnce sync.Once

	mu  sync.Mutex
	err error // first write error, if any
}

// compositeQueueBlocks bounds each channel's pending-audio buffer. samoyed
// emits a whole transmission as a near-instant burst, so the buffer must
// hold the largest single keyup we expect to reconstruct in real time.
// 60 s is far longer than any AX.25 burst yet only ~5 MB per channel.
const compositeQueueBlocks = 60 * audio.SampleRate / audio.BlockSamples

// compositeResult is returned from StopCompositeRecording for status
// reporting and so callers (and external software) know what was written.
type compositeResult struct {
	Path     string
	Channels []config.PortRef
	Bytes    int64
	Duration time.Duration
}

// CompositeStatus is a snapshot of the composite recorder for the HTTP
// status endpoint.
type CompositeStatus struct {
	Active   bool
	Path     string
	Channels []config.PortRef
	Duration time.Duration
	Err      string
}

func newCompositeRecorder(base string, channels []config.PortRef, timeScale float64, logger *slog.Logger) (*compositeRecorder, error) {
	if base == "" {
		return nil, errors.New("composite: no record base dir configured")
	}
	if len(channels) == 0 {
		return nil, errors.New("composite: no transmitter ports selected")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("composite: mkdir %q: %w", base, err)
	}
	idx := make(map[config.PortRef]int, len(channels))
	for i, ref := range channels {
		if _, dup := idx[ref]; dup {
			return nil, fmt.Errorf("composite: port %s listed for more than one channel", ref)
		}
		idx[ref] = i
	}

	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	path := filepath.Join(base, "composite-"+stamp+".wav")
	w, err := audio.NewMultiWAVWriter(path, len(channels))
	if err != nil {
		return nil, fmt.Errorf("composite: open %s: %w", path, err)
	}

	cr := &compositeRecorder{
		path:      path,
		channels:  append([]config.PortRef(nil), channels...),
		logger:    logger,
		started:   time.Now(),
		timeScale: timeScale,
		idx:       idx,
		queues:    make([]chan audio.Block, len(channels)),
		writer:    w,
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	for i := range cr.queues {
		cr.queues[i] = make(chan audio.Block, compositeQueueBlocks)
	}
	go cr.run()
	logger.Info("composite recording started", "file", path, "channels", channelStrings(channels))
	return cr, nil
}

// feed routes one TX block to the channel ref maps to, if any. Called from
// each port's txReader hot path. A port not in this recording is a cheap
// map-miss. Each channel has a single producer (that port's txReader), so
// the only contention is with the draining run goroutine, handled by the
// buffered channel.
func (cr *compositeRecorder) feed(ref config.PortRef, blk audio.Block) {
	if cr.failed.Load() {
		return
	}
	i, ok := cr.idx[ref]
	if !ok {
		return
	}
	q := cr.queues[i]
	select {
	case q <- blk:
		cr.overflowed.Store(false)
		return
	default:
	}
	// Full: drop oldest to make room. Means a single keyup exceeded
	// compositeQueueBlocks of audio — rare, and worth a line.
	select {
	case <-q:
	default:
	}
	select {
	case q <- blk:
	default:
	}
	if !cr.overflowed.Swap(true) {
		cr.logger.Warn("composite: queue overflow, dropping oldest", "port", ref)
	}
}

// run is the real-time pacer. One block per channel per tick, interleaved
// into the WAV. Idle channels emit silence so the timeline keeps advancing
// and every channel stays sample-aligned.
func (cr *compositeRecorder) run() {
	defer close(cr.stopped)
	ticker := time.NewTicker(scaled(blockPeriod, cr.timeScale))
	defer ticker.Stop()
	for {
		select {
		case <-cr.done:
			cr.drainRemaining()
			cr.closeWriter()
			return
		case <-ticker.C:
			if !cr.writeFrame() {
				cr.closeWriter()
				return
			}
		}
	}
}

// collect pops at most one block from every channel queue, substituting
// silence for idle channels. any reports whether at least one channel had
// real audio this round (used by the stop-time drain to know when empty).
func (cr *compositeRecorder) collect() (blocks []audio.Block, any bool) {
	blocks = make([]audio.Block, len(cr.queues))
	for i, q := range cr.queues {
		select {
		case b := <-q:
			if len(b) == 0 {
				b = audio.Silence()
			}
			blocks[i] = b
			any = true
		default:
			blocks[i] = audio.Silence()
		}
	}
	return blocks, any
}

// writeFrame writes one interleaved frame group. Returns false on a write
// error (after recording it), which tells run to stop.
func (cr *compositeRecorder) writeFrame() bool {
	blocks, _ := cr.collect()
	if _, err := cr.writer.Write(audio.Interleave(blocks)); err != nil {
		cr.setErr(err)
		cr.failed.Store(true)
		cr.logger.Warn("composite: write failed, stopping", "file", cr.path, "err", err)
		return false
	}
	cr.frames.Add(1)
	return true
}

// drainRemaining flushes any blocks still queued at graceful stop, keeping
// channels aligned (idle ones padded with silence) so the tail of an
// in-flight transmission isn't lost.
func (cr *compositeRecorder) drainRemaining() {
	for {
		blocks, any := cr.collect()
		if !any {
			return
		}
		if _, err := cr.writer.Write(audio.Interleave(blocks)); err != nil {
			cr.setErr(err)
			return
		}
		cr.frames.Add(1)
	}
}

func (cr *compositeRecorder) closeWriter() {
	if err := cr.writer.Close(); err != nil {
		cr.setErr(err)
	}
}

func (cr *compositeRecorder) setErr(err error) {
	cr.mu.Lock()
	if cr.err == nil {
		cr.err = err
	}
	cr.mu.Unlock()
}

// stop requests a graceful drain + close and blocks until the run
// goroutine has finished writing the file (so the WAV header is patched
// and the file is safe to read / download). Idempotent.
func (cr *compositeRecorder) stop() compositeResult {
	cr.stopOnce.Do(func() { close(cr.done) })
	<-cr.stopped
	frames := cr.frames.Load()
	res := compositeResult{
		Path:     cr.path,
		Channels: cr.channels,
		Bytes:    frames * int64(len(cr.channels)) * 2,
		Duration: time.Duration(frames) * time.Second * audio.BlockSamples / audio.SampleRate,
	}
	cr.logger.Info("composite recording stopped", "file", cr.path,
		"duration", res.Duration.Round(time.Millisecond))
	return res
}

func (cr *compositeRecorder) status() CompositeStatus {
	frames := cr.frames.Load()
	cr.mu.Lock()
	errStr := ""
	if cr.err != nil {
		errStr = cr.err.Error()
	}
	cr.mu.Unlock()
	return CompositeStatus{
		Active:   true,
		Path:     cr.path,
		Channels: cr.channels,
		Duration: time.Duration(frames) * time.Second * audio.BlockSamples / audio.SampleRate,
		Err:      errStr,
	}
}

func channelStrings(refs []config.PortRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return out
}

// --- Router-level API ------------------------------------------------------

// StartCompositeRecording begins a composite recording of the given
// transmitter ports — one WAV channel per port, in the order given (channel
// 0 = first port = left ear in the stereo case). If ports is empty it
// defaults to the topology's first two ports, which is exactly the
// "two-station, one per ear" case. Returns the file path being written.
//
// It is an error to call this when a composite recording is already
// running, when no RecordDir is configured, or when a named port doesn't
// exist in the topology.
func (r *Router) StartCompositeRecording(ports []config.PortRef) (string, error) {
	r.compositeMu.Lock()
	defer r.compositeMu.Unlock()
	if r.opts.RecordDir == "" {
		return "", errors.New("composite: no RecordDir configured")
	}
	if r.compositeStopped {
		return "", errors.New("composite: the router has stopped")
	}
	if r.composite.Load() != nil {
		return "", errors.New("composite: already recording")
	}

	valid := r.portRefSet()
	if len(ports) == 0 {
		ports = r.defaultCompositePorts()
	}
	if len(ports) == 0 {
		return "", errors.New("composite: topology has no ports to record")
	}
	for _, p := range ports {
		if _, ok := valid[p]; !ok {
			return "", fmt.Errorf("composite: unknown port %s", p)
		}
	}

	cr, err := newCompositeRecorder(r.opts.RecordDir, ports, r.cfg.TimeScale, r.logger)
	if err != nil {
		return "", err
	}
	r.composite.Store(cr)
	return cr.path, nil
}

// StopCompositeRecording finalises the current composite recording (drains
// in-flight audio, patches the WAV header) and returns its result. No error
// and a zero result if nothing was recording.
func (r *Router) StopCompositeRecording() (compositeResult, error) {
	r.compositeMu.Lock()
	defer r.compositeMu.Unlock()
	cr := r.composite.Swap(nil)
	if cr == nil {
		return compositeResult{}, nil
	}
	return cr.stop(), nil
}

// CompositeActive reports whether a composite recording is in progress.
func (r *Router) CompositeActive() bool { return r.composite.Load() != nil }

// CompositeStatus returns a snapshot of the composite recorder, or a
// zero-value (Active=false) status when none is running.
func (r *Router) CompositeStatus() CompositeStatus {
	if cr := r.composite.Load(); cr != nil {
		return cr.status()
	}
	return CompositeStatus{}
}

// AllPortRefs returns every port in the topology in declaration order.
func (r *Router) AllPortRefs() []config.PortRef {
	var refs []config.PortRef
	for _, n := range r.cfg.Nodes {
		for _, p := range n.Ports {
			refs = append(refs, config.PortRef{NodeID: n.ID, PortID: p.ID})
		}
	}
	return refs
}

func (r *Router) portRefSet() map[config.PortRef]struct{} {
	set := map[config.PortRef]struct{}{}
	for _, ref := range r.AllPortRefs() {
		set[ref] = struct{}{}
	}
	return set
}

// defaultCompositePorts picks the first up-to-two ports for the no-argument
// "two-station, one per ear" default.
func (r *Router) defaultCompositePorts() []config.PortRef {
	all := r.AllPortRefs()
	if len(all) > 2 {
		all = all[:2]
	}
	return all
}
