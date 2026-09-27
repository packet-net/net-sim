// Package router orchestrates the simulator: spawns samoyed children, owns
// the topology, and routes audio between ports per the link table and
// receiver-side mixer.
//
// Audio flow per port P:
//
//	     ┌─ KISS TCP ───────────────────────┐
//	     │ (samoyed exposes; router doesn't │
//	     │  touch KISS frames)              │
//	     │                                  │
//	stdin: ◄── rxFeeder ── mixer ── all S where link S→P
//	     │                                  │
//	     │                  fresh blocks ──►│
//	     │                                  │
//	udp out: ── txReader ── for each link P→D, push to D's bus
//
// Each samoyed gets continuous PCM on stdin (silence-filled when no peer
// is transmitting) so its demodulator never starves.
//
// samoyed only writes to its UDP output when actually keying. Importantly,
// samoyed produces TX audio as fast as it can compute it (no pacing): a
// 500 ms frame can land on UDP in a few wall-clock ms. We therefore queue
// per-link blocks and the receiver-side rxFeeder drains them at exactly
// SampleRate/BlockSamples Hz, so the audio reaches each samoyed at the
// rate it expects to consume it.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/events"
	"github.com/packethacking/net-sim/internal/tnc"
)

// txActiveThreshold is the audio peak (0–32767) above which a port's
// transmitted block counts as "keyed." Tuned by eye against samoyed's
// preamble levels — comfortably above the residual non-zero noise the
// encoder leaves behind, well below modulator full-scale.
const txActiveThreshold = 1500

// txSilenceWindow is the wall-clock idle gap after which a tx_end event
// fires. We use a wall-clock watchdog rather than counting silent
// blocks because samoyed stops sending UDP datagrams entirely at burst
// end (rather than streaming silence), so block-count silence detection
// would never advance after the last audible block. 200 ms is long
// enough to bridge the inter-frame gaps inside a multi-frame burst
// without falsely closing keying, short enough to mark "key up" cleanly.
//
// At time_scale > 1 the audio (and hence the inter-frame gaps) runs
// proportionally faster, so the watchdog applies scaled(txSilenceWindow)
// — an unscaled window would bridge real inter-transmission gaps and a
// scaled audio stream would otherwise fire tx_end mid-transmission.
const txSilenceWindow = 200 * time.Millisecond

// txWatchdogTick is how often the watchdog re-checks staleness at
// time_scale 1. Scaled down with time_scale so end-of-keying detection
// keeps the same sim-time resolution.
const txWatchdogTick = 50 * time.Millisecond

// pdnWarmup is how long Start waits, feeding audio, before returning when
// any port runs pdn-soundmodem. See Start.
const pdnWarmup = time.Second

// blockPeriod is the wall-clock duration of one audio block at
// time_scale 1: BlockSamples / SampleRate = 10 ms. The rxFeeder and the
// composite recorder pace themselves at scaled(blockPeriod).
const blockPeriod = time.Duration(audio.BlockSamples) * time.Second / audio.SampleRate

// scaled divides a real-time pacing interval by the simulation's
// time_scale factor: at scale N the simulation runs N× faster than wall
// clock, so every wall-clock interval the router waits on shrinks by N.
// Scales <= 1 (including the zero value of an unvalidated config) leave
// the duration untouched.
func scaled(d time.Duration, timeScale float64) time.Duration {
	if timeScale <= 1 {
		return d
	}
	return time.Duration(float64(d) / timeScale)
}

// Options configures a Router. All paths default to "look up on $PATH".
type Options struct {
	SamoyedBin          string // path to samoyed-direwolf
	DirewolfBin         string // path to direwolf
	PdnBin              string // path to pdn-soundmodem
	WorkDir             string // where temporary config files / FIFOs go
	Verbose             bool   // log every routing decision
	Logger              *slog.Logger
	StartingRxAudioPort int // first ephemeral UDP port for samoyed-→-router audio

	// RecordDir, if non-empty, is the base directory under which WAV
	// recordings are written. Each call to StartRecording (or the
	// auto-start path triggered by RecordOnStart) creates a fresh
	// timestamped subdirectory holding two files per port — <node>.<port>.tx.wav
	// and <node>.<port>.rx.wav.
	RecordDir string

	// RecordOnStart causes Router.Start to begin recording immediately
	// after children come up. Ignored if RecordDir is empty.
	RecordOnStart bool

	// EventBus, if non-nil, receives per-port activity events from the
	// audio path (tx_start / tx_end / rx_decision). Publishing is
	// non-blocking, so leaving the bus attached but unsubscribed is
	// cheap. See the events package.
	EventBus *events.Bus

	// AudioTap, if non-nil, receives raw PCM blocks from the TX and RX
	// audio paths, keyed by "<node>.<port>|tx" or "<node>.<port>|rx".
	// Per-block Publish is a no-op when nobody's subscribed to a key,
	// so leaving the tap attached is cheap.
	AudioTap *audio.Tap

	// Observer, if non-nil, scrapes locally-transmitted callsigns
	// from each TNC's stderr stream. Cheap; the parser only matches
	// lines containing "[0L]" so the overhead on busy logs is low.
	Observer *Observer

	// RTPriority, if set, renices the router's own process and every
	// spawned TNC child to rtNice (-10) so the 10 ms pacing tickers and
	// the children's demodulators don't glitch under shared host load.
	// Best-effort: needs CAP_SYS_NICE, otherwise a one-line warning is
	// logged and the simulation runs at normal priority. See priority.go.
	RTPriority bool
}

// Router is the running simulator.
type Router struct {
	opts   Options
	cfg    *config.Config
	mixer  *audio.Mixer
	logger *slog.Logger

	mu       sync.RWMutex
	children map[config.PortRef]*tnc.Child

	// linkQueues hold per-link audio: when source S transmits, blocks are
	// pushed into linkQueues[S][index] for every link S→D. The destination
	// D's rxFeeder drains the corresponding queue at sample rate.
	//
	// A separate queue per link (rather than one per source) means each
	// destination gets its own back-pressure / drop behaviour and we don't
	// need to track per-destination read positions on a shared queue.
	linkQueues map[config.PortRef][]*linkQueue // keyed by source
	rxLinks    map[config.PortRef][]*linkQueue // keyed by destination

	// session is the active recording session, or nil. Read on the audio
	// hot path (txReader, rxFeeder) so it lives in an atomic pointer.
	// recordMu serialises Start/StopRecording against each other and
	// against shutdown.
	session       atomic.Pointer[recordSession]
	recordMu      sync.Mutex
	recordStopped bool // set by shutdown under recordMu; refuses new sessions

	// composite is the active composite (multi-channel TX timeline)
	// recorder, or nil. Read on the txReader hot path, so atomic.
	// compositeMu serialises Start/StopCompositeRecording and shutdown.
	composite        atomic.Pointer[compositeRecorder]
	compositeMu      sync.Mutex
	compositeStopped bool   // set by shutdown under compositeMu
	lastComposite    string // file finalised by shutdown, if one was running

	// txTrackers — one per port — record the wall-clock time of each
	// port's last above-threshold TX block. The txWatchdog goroutine
	// fires tx_end when a tracker hasn't been bumped within
	// txSilenceWindow. Populated at Start; pointers are stable.
	txTrackers map[config.PortRef]*txTracker

	cancel context.CancelFunc
	done   <-chan struct{} // closed when the router's context ends
	wg     sync.WaitGroup
}

// txTracker carries the wall-clock TX-active state for one port,
// shared by the port's txReader (writes lastBusyNanos / sets active)
// and the txWatchdog (clears active on staleness).
type txTracker struct {
	active        atomic.Bool
	lastBusyNanos atomic.Int64
}

// maxQueueBlocks is a safety cap on a single link's audio backlog (~60 s).
// The channel is modelled as a CONTINUOUS CARRIER: blocks are never dropped
// within a transmission (see linkQueue.push). 60 s only triggers on a genuine
// runaway (a wedged rxFeeder), where dropping the head is the lesser evil — a
// real keyup is at most a few seconds.
const maxQueueBlocks = 60 * audio.SampleRate / audio.BlockSamples

// queuedBlock is one FIFO entry: the PCM block plus a start-of-transmission
// marker set by push when the block began a new keyup (see push for how the
// boundary is detected). Carrying the boundary on the block itself keeps the
// pop side timing-independent: the squelch mute is counted in blocks of the
// transmission, not in wall clock.
type queuedBlock struct {
	blk audio.Block
	sot bool // start of transmission
}

// linkQueue is one source→destination link's audio buffer: a non-dropping FIFO
// of PCM blocks the source has transmitted, drained by the destination's
// rxFeeder at the channel sample rate (so the receiver hears the carrier in
// real time).
//
// A TNC bursts a whole keyup's worth of audio with no real-time pacing on its
// end (see txReader), so the buffer must hold an entire transmission and the
// rxFeeder plays it out at real time. It is non-dropping and grows as needed:
// dropping mid-transmission would gap the receiver's audio, collapse its DCD /
// carrier sense, and make the far end key up on top of an in-progress
// transmission — a collision that cannot happen on a real continuous-carrier
// channel. Memory is bounded in practice (a keyup is finite; the backing array
// is released once drained); only a pathological runaway past maxQueueBlocks
// ever drops.
//
// The linkQueue is also where the squelch_open_ms model lives — it is the
// per-directed-link seam the squelch belongs to (squelch is a property of the
// destination receiver hearing this particular carrier): pop substitutes
// silence for the first squelchBlocks blocks of every transmission.
type linkQueue struct {
	src, dst config.PortRef
	loss     float64
	noise    float64

	// squelchBlocks is the number of leading blocks of every transmission
	// delivered as silence (squelch_open_ms rounded up to whole blocks).
	// Counted in blocks — i.e. in sim-time audio — so the muted span is
	// the same amount of *audio* regardless of time_scale. 0 = squelch
	// opens instantly (default; pop is then byte-transparent).
	squelchBlocks int

	// silenceWindow is the push-side wall-clock idle gap that separates
	// two transmissions. It mirrors the txWatchdog's rule exactly (the
	// time-scaled txSilenceWindow): the source TNC bursts a keyup faster
	// than real time but pauses between *frames inside one keyup* show up
	// as short gaps on its TX stream, and the watchdog already answers
	// "is this still the same transmission?" with this window. Reusing it
	// here — rather than, say, a pop-side "queue drained" test, which
	// would misfire whenever the rxFeeder catches up with a still-keyed
	// source — keeps the two transmission-boundary definitions identical.
	silenceWindow time.Duration
	now           func() time.Time // stubbed by tests

	mu            sync.Mutex
	buf           []queuedBlock // FIFO; index 0 = oldest
	lastPush      time.Time
	muteRemaining int // blocks of the current transmission still to mute
}

func newLinkQueue(src, dst config.PortRef, loss, noise float64, squelchBlocks int, silenceWindow time.Duration) *linkQueue {
	return &linkQueue{
		src: src, dst: dst, loss: loss, noise: noise,
		squelchBlocks: squelchBlocks,
		silenceWindow: silenceWindow,
		now:           time.Now,
	}
}

// push appends a block to the link's buffer. It never blocks and — modelling a
// continuous carrier — never drops within a transmission, no matter how far the
// source TNC has run ahead of real time. The only drop is the maxQueueBlocks
// safety valve, which signals a stalled rxFeeder rather than normal operation,
// so it is logged.
//
// The block is tagged start-of-transmission when it is the first push after a
// silenceWindow-sized idle gap (the same idle→active transition the txTracker
// uses) — that tag is what re-arms the squelch mute on the pop side.
func (q *linkQueue) push(blk audio.Block, logger *slog.Logger) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	sot := q.lastPush.IsZero() || now.Sub(q.lastPush) >= q.silenceWindow
	q.lastPush = now
	if len(q.buf) >= maxQueueBlocks {
		q.buf[0] = queuedBlock{}
		q.buf = q.buf[1:]
		logger.Warn("audio queue safety cap reached (rxFeeder stalled?)", "from", q.src, "to", q.dst)
	}
	q.buf = append(q.buf, queuedBlock{blk: blk, sot: sot})
}

// pushTail appends the zero-padded last block of a transmission that has
// already gone quiet. It belongs to that transmission: it is never tagged
// start-of-transmission and doesn't count as fresh activity for the next
// transmission's boundary test.
func (q *linkQueue) pushTail(blk audio.Block) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.buf = append(q.buf, queuedBlock{blk: blk})
}

// pop returns one block if immediately available (FIFO, oldest first).
//
// While the squelch is opening (the first squelchBlocks blocks of each
// transmission), pop delivers silence INSTEAD OF the block — not nothing:
// the carrier is on the air and the mixer must still see it for capture /
// collision decisions; it is only the receiver's audio that stays muted
// until the squelch opens. The original block is never modified (it is
// shared with every other link the source fans out to).
func (q *linkQueue) pop() (audio.Block, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) == 0 {
		return nil, false
	}
	qb := q.buf[0]
	q.buf[0] = queuedBlock{} // release the reference
	q.buf = q.buf[1:]
	if len(q.buf) == 0 {
		q.buf = nil // release the backing array once drained
	}
	if qb.sot {
		q.muteRemaining = q.squelchBlocks
	}
	if q.muteRemaining > 0 {
		q.muteRemaining--
		return audio.Silence(), true
	}
	return qb.blk, true
}

// squelchBlocksFor converts a link's squelch_open_ms into a whole number of
// audio blocks, rounding up so a configured delay is never under-delivered.
func squelchBlocksFor(ms float64) int {
	if ms <= 0 {
		return 0
	}
	const blockMS = float64(audio.BlockSamples) * 1000 / audio.SampleRate
	return int(math.Ceil(ms / blockMS))
}

// Start spawns all samoyed children and begins routing audio.
func Start(ctx context.Context, cfg *config.Config, opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.StartingRxAudioPort == 0 {
		opts.StartingRxAudioPort = 17000
	}

	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			if err := tnc.SupportedMode(tnc.Backend(p.TNC), p.Modem); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", n.ID, p.ID, err)
			}
		}
	}

	rctx, cancel := context.WithCancel(ctx)
	r := &Router{
		opts:       opts,
		cfg:        cfg,
		mixer:      audio.NewMixer(cfg.CaptureDB, cfg.MixerMode == config.MixerLinearSum, string(cfg.CollisionMode)),
		logger:     opts.Logger,
		children:   map[config.PortRef]*tnc.Child{},
		linkQueues: map[config.PortRef][]*linkQueue{},
		rxLinks:    map[config.PortRef][]*linkQueue{},
		txTrackers: map[config.PortRef]*txTracker{},
		cancel:     cancel,
		done:       rctx.Done(),
	}

	for _, l := range cfg.Links {
		fr, _ := parsePortRef(l.From)
		to, _ := parsePortRef(l.To)
		// The transmission-boundary window scales with time_scale just
		// like the txWatchdog's: at scale N the audio (and its gaps)
		// arrives N× faster.
		q := newLinkQueue(fr, to, l.LossDB, l.NoiseDB,
			squelchBlocksFor(l.SquelchOpenMS), scaled(txSilenceWindow, cfg.TimeScale))
		r.linkQueues[fr] = append(r.linkQueues[fr], q)
		r.rxLinks[to] = append(r.rxLinks[to], q)
	}

	if opts.RTPriority {
		// pid 0 = this process (the router and all its pacing tickers).
		applyRTPriority(r.logger, "sim-router", 0)
	}

	udpPort := opts.StartingRxAudioPort
	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			ref := config.PortRef{NodeID: n.ID, PortID: p.ID}
			backend := tnc.Backend(p.TNC)
			if backend == "" {
				backend = tnc.BackendSamoyed
			}
			spec := tnc.Spec{
				Backend:        backend,
				NodeID:         n.ID,
				PortID:         p.ID,
				Modem:          p.Modem,
				KissPort:       p.KissPort,
				RxAudioUDPPort: udpPort,
				SamoyedBin:     opts.SamoyedBin,
				DirewolfBin:    opts.DirewolfBin,
				PdnBin:         opts.PdnBin,
				WorkDir:        opts.WorkDir,
			}
			if opts.Observer != nil {
				spec.StderrTap = opts.Observer.WriterFor(ref)
			}
			// direwolf and pdn don't use the per-port UDP port. Reserve
			// it anyway so subsequent samoyed ports get the next one
			// regardless of preceding direwolf/pdn ports; keeps the UDP
			// port assignment stable as the topology is edited.
			udpPort++

			child, err := tnc.Start(rctx, spec)
			if err != nil {
				cancel()
				_ = r.shutdown()
				return nil, fmt.Errorf("start %s: %w", ref, err)
			}
			r.children[ref] = child
			if opts.RTPriority {
				applyRTPriority(r.logger, ref.String(), child.Pid())
			}

			fields := []any{
				"node", n.ID, "port", p.ID, "tnc", string(backend),
				"mode", p.Modem.Mode, "kiss_tcp", p.KissPort,
			}
			if backend == tnc.BackendSamoyed {
				fields = append(fields, "rx_audio_udp", spec.RxAudioUDPPort)
			}
			r.logger.Info("port up", fields...)
		}
	}

	// One tx tracker per port — pointers are stable across the run.
	for ref := range r.children {
		r.txTrackers[ref] = &txTracker{}
	}

	for ref, child := range r.children {
		ref := ref
		child := child
		r.wg.Add(2)
		go func() {
			defer r.wg.Done()
			r.txReader(rctx, ref, child.TXAudio())
		}()
		go func() {
			defer r.wg.Done()
			r.rxFeeder(rctx, ref, child.Stdin())
		}()
		if opts.EventBus != nil {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.txWatchdog(rctx, ref)
			}()
		}
	}

	for ref, child := range r.children {
		ref := ref
		child := child
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			err := child.Wait()
			if rctx.Err() == nil {
				r.logger.Error("TNC child exited unexpectedly", "port", ref, "err", err)
				cancel()
			}
		}()
	}

	// pdn-soundmodem's receivers need a moment of audio after start-up
	// before they decode reliably: a c4fsk9600 frame arriving 60 ms into
	// the stream was missed, one arriving 160 ms in was not. Hold Start's
	// return (and so "running") until the feeders have been going a while.
	for _, child := range r.children {
		if child.Spec().Backend == tnc.BackendPdn {
			select {
			case <-time.After(pdnWarmup):
			case <-rctx.Done():
			}
			break
		}
	}

	if opts.RecordDir != "" && opts.RecordOnStart {
		if _, err := r.StartRecording(); err != nil {
			r.logger.Warn("auto-start recording failed", "err", err)
		}
	}

	return r, nil
}

// Done is closed when the router stops running: its parent context was
// cancelled, Stop was called, or a TNC child exited. Callers should then
// call Stop, which closes the children's audio I/O so the routing
// goroutines can finish; Wait alone would block on their reads.
func (r *Router) Done() <-chan struct{} { return r.done }

// Stop terminates all children and waits for routing goroutines to exit.
func (r *Router) Stop() error {
	r.cancel()
	return r.shutdown()
}

func (r *Router) shutdown() error {
	for _, c := range r.children {
		_ = c.Stop()
	}
	r.wg.Wait()
	// Close recordings under their locks, with the stopped flags set, so
	// a StartRecording racing with shutdown can't leave a session (or a
	// composite writing silence forever) on a dead router.
	r.recordMu.Lock()
	r.recordStopped = true
	s := r.session.Swap(nil)
	r.recordMu.Unlock()
	if s != nil {
		s.Close()
	}
	r.compositeMu.Lock()
	r.compositeStopped = true
	if cr := r.composite.Swap(nil); cr != nil {
		r.lastComposite = cr.stop().Path
	}
	r.compositeMu.Unlock()
	return nil
}

// LastCompositePath is the composite recording that shutdown finalised,
// if one was still running when the router stopped.
func (r *Router) LastCompositePath() string {
	r.compositeMu.Lock()
	defer r.compositeMu.Unlock()
	return r.lastComposite
}

// StartRecording opens a fresh timestamped subdirectory under
// Options.RecordDir, opens a TX + RX wav per port, and installs the
// session as the live recorder. It is an error to call this when a
// session is already active or when RecordDir is empty. Returns the
// absolute directory path being recorded into.
func (r *Router) StartRecording() (string, error) {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if r.opts.RecordDir == "" {
		return "", errors.New("recorder: no RecordDir configured")
	}
	if r.recordStopped {
		return "", errors.New("recorder: the router has stopped")
	}
	if r.session.Load() != nil {
		return "", errors.New("recorder: already recording")
	}
	var refs []config.PortRef
	for _, n := range r.cfg.Nodes {
		for _, p := range n.Ports {
			refs = append(refs, config.PortRef{NodeID: n.ID, PortID: p.ID})
		}
	}
	s, err := newRecordSession(r.opts.RecordDir, refs, r.logger)
	if err != nil {
		return "", err
	}
	r.session.Store(s)
	return s.Dir(), nil
}

// StopRecording closes the current session (flushes WAV headers) and
// disarms the audio-path tap. No error if there is no active session.
func (r *Router) StopRecording() error {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	s := r.session.Swap(nil)
	if s == nil {
		return nil
	}
	s.Close()
	return nil
}

// RecordingActive reports whether a session is currently capturing.
func (r *Router) RecordingActive() bool { return r.session.Load() != nil }

// RecordingDir returns the directory of the current session, or "" if
// none.
func (r *Router) RecordingDir() string {
	if s := r.session.Load(); s != nil {
		return s.Dir()
	}
	return ""
}

// RecordingBase returns the configured base directory (Options.RecordDir),
// or "" if the router was started without one.
func (r *Router) RecordingBase() string { return r.opts.RecordDir }

// Wait blocks until the router stops (e.g. ctx cancelled or child died).
func (r *Router) Wait() {
	r.wg.Wait()
}

// txReader pulls TX-side PCM bytes from this port's TNC and fans
// each BlockBytes-sized chunk out to every linked destination's queue.
//
// TNCs burst TX audio (no real-time pacing on their end), so we must
// preserve the order and quantity of blocks rather than collapsing to
// "latest block" — otherwise the receiver hears only a fragment of the
// transmission. Source is io.Reader regardless of backend (samoyed
// UDP datagrams stitched into a byte stream, or direwolf FIFO bytes).
//
// The loop exits when src.Read returns an error — typically because
// Stop() closed the underlying socket / file at shutdown.
func (r *Router) txReader(ctx context.Context, ref config.PortRef, src io.Reader) {
	buf := make([]byte, 4096)
	pending := make([]byte, 0, audio.BlockBytes*4)
	outgoing := r.linkQueues[ref] // empty slice if this port has no outgoing links
	tt := r.txTrackers[ref]       // may be nil only if Start partially failed

	// A TNC's burst rarely ends on a block boundary, and the remainder
	// would otherwise sit in pending until the next transmission, then go
	// out in front of it. Where the source supports read deadlines
	// (samoyed's UDP, direwolf's FIFO), a quiet spell as long as the
	// transmission-boundary window flushes it as a zero-padded block
	// belonging to the transmission that just ended. The pdn backend pads
	// its own bursts, so it needs none of this.
	dl, _ := src.(interface{ SetReadDeadline(time.Time) error })
	tailGap := scaled(txSilenceWindow, r.cfg.TimeScale)
	deadlineSet := false

	emit := func(blk audio.Block, tail bool) {
		peak := blk.PeakAbs()
		if r.opts.Verbose {
			r.logger.Debug("tx block", "port", ref, "peak", peak, "outgoing", len(outgoing), "tail", tail)
		}
		// Bump the TX tracker on every active block; the watchdog
		// goroutine emits tx_start (first bump) and tx_end (after
		// txSilenceWindow with no further bumps). A flushed tail is the
		// end of a transmission, not activity.
		if !tail && r.opts.EventBus != nil && tt != nil && peak >= txActiveThreshold {
			tt.lastBusyNanos.Store(time.Now().UnixNano())
			if !tt.active.Swap(true) {
				r.opts.EventBus.Publish(events.Event{
					T:    time.Now(),
					Type: events.TXStart,
					Port: ref.String(),
					Peak: peak,
				})
			}
		}
		if s := r.session.Load(); s != nil {
			s.WriteTX(ref, blk)
		}
		if cr := r.composite.Load(); cr != nil {
			cr.feed(ref, blk)
		}
		if r.opts.AudioTap != nil {
			key := ref.String() + "|tx"
			if r.opts.AudioTap.HasSubscribers(key) {
				r.opts.AudioTap.Publish(key, blk)
			}
		}
		for _, q := range outgoing {
			if tail {
				q.pushTail(blk)
			} else {
				q.push(blk, r.logger)
			}
		}
	}

	for {
		if dl != nil && (len(pending) > 0) != deadlineSet {
			deadlineSet = len(pending) > 0
			when := time.Time{}
			if deadlineSet {
				when = time.Now().Add(tailGap)
			}
			_ = dl.SetReadDeadline(when)
		}
		n, err := src.Read(buf)
		if err != nil {
			if deadlineSet && errors.Is(err, os.ErrDeadlineExceeded) {
				blk := make(audio.Block, audio.BlockBytes)
				copy(blk, pending)
				pending = pending[:0]
				emit(blk, true)
				continue
			}
			// EOF / closed-during-shutdown is the normal exit path.
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			r.logger.Debug("tx audio read error", "port", ref, "err", err)
			return
		}
		if n == 0 {
			continue
		}
		if deadlineSet {
			// Still transmitting: push the deadline out again.
			_ = dl.SetReadDeadline(time.Now().Add(tailGap))
		}
		pending = append(pending, buf[:n]...)
		for len(pending) >= audio.BlockBytes {
			blk := make(audio.Block, audio.BlockBytes)
			copy(blk, pending[:audio.BlockBytes])
			pending = pending[audio.BlockBytes:]
			emit(blk, false)
		}
	}
}

// txWatchdog fires tx_end after a port has been idle for txSilenceWindow.
// One goroutine per port; cheap because it sleeps almost all the time.
// The wall-clock timer is required because samoyed's TX UDP stream
// stops at end of burst — the txReader's Read blocks indefinitely with
// no further blocks to evaluate, so we can't drive silence detection
// from the audio path alone.
func (r *Router) txWatchdog(ctx context.Context, ref config.PortRef) {
	tt := r.txTrackers[ref]
	if tt == nil {
		return
	}
	tick := time.NewTicker(scaled(txWatchdogTick, r.cfg.TimeScale))
	defer tick.Stop()
	window := scaled(txSilenceWindow, r.cfg.TimeScale)
	emitEnd := func() {
		// Also fires once during shutdown for any port that was keyed at
		// the moment ctx cancelled — visualiser then doesn't end with
		// stale "TX active" state.
		if tt.active.CompareAndSwap(true, false) && r.opts.EventBus != nil {
			r.opts.EventBus.Publish(events.Event{
				T:    time.Now(),
				Type: events.TXEnd,
				Port: ref.String(),
			})
		}
	}
	for {
		select {
		case <-ctx.Done():
			emitEnd()
			return
		case <-tick.C:
		}
		if !tt.active.Load() {
			continue
		}
		last := tt.lastBusyNanos.Load()
		if time.Since(time.Unix(0, last)) >= window {
			emitEnd()
		}
	}
}

// rxFeeder writes one audio block per blockPeriod (divided by time_scale)
// to this port's samoyed stdin. The block is the mixer's verdict on every
// active TX reaching this port through the topology.
//
// "Active" simply means a block is available on that link's queue. Per
// PLAN Phase 3 self-mute, a port never hears its own TX — that's enforced
// by config validation rejecting self-loops, so it's a no-op here.
func (r *Router) rxFeeder(ctx context.Context, dst config.PortRef, stdin io.Writer) {
	ticker := time.NewTicker(scaled(blockPeriod, r.cfg.TimeScale))
	defer ticker.Stop()

	links := r.rxLinks[dst] // links *into* this destination

	// Per-port noise floor (the FM band-noise hiss). Looked up once
	// here because the port + global config don't change at runtime.
	// Resolution order: Port.NoiseDB → Config.DefaultNoiseDB → 0.
	portNoise := r.portNoiseFloor(dst)

	// Per-destination RX event state. We emit rx_decision only when the
	// (decision, source-set) tuple changes since the last block — busy
	// channels would otherwise generate ~100 events/s/port. lastSources is
	// kept sorted so equality comparison is straightforward.
	var (
		lastDecision audio.MixDecision = -1
		lastSources  string
		sourcePorts  []*linkQueue // scratch, reused
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		var active []audio.ActiveTX
		sourcePorts = sourcePorts[:0]
		for _, q := range links {
			b, ok := q.pop()
			if !ok {
				continue
			}
			active = append(active, audio.ActiveTX{
				Block:   b,
				LossDB:  q.loss,
				NoiseDB: q.noise,
			})
			sourcePorts = append(sourcePorts, q)
		}

		blk, dec := r.mixer.Mix(active)

		if r.opts.EventBus != nil {
			srcKey, srcList := summariseSources(sourcePorts)
			if dec != lastDecision || srcKey != lastSources {
				// Only emit when something interesting is happening
				// or when the channel just went quiet — both inform
				// the visualiser, but silent-→-silent transitions
				// (the common case) are skipped.
				if dec != audio.MixSilence || lastDecision != -1 {
					r.opts.EventBus.Publish(events.Event{
						T:        time.Now(),
						Type:     events.RXDecision,
						Port:     dst.String(),
						Decision: decisionName(dec),
						Sources:  srcList,
					})
				}
				lastDecision = dec
				lastSources = srcKey
			}
		}

		// Noise floor — always at least the receiver's own band-noise
		// (FM band hiss, modelled per-port or via the global default).
		// Per-link noise still considered so a noisy-medium link can
		// add more than the receiver's own floor (rare, but supported).
		// Applied *after* the capture decision so noise doesn't tilt
		// the capture margin.
		maxNoise := portNoise
		for _, tx := range active {
			if tx.NoiseDB > maxNoise {
				maxNoise = tx.NoiseDB
			}
		}
		if len(active) == 0 {
			for _, q := range links {
				if q.noise > maxNoise {
					maxNoise = q.noise
				}
			}
		}
		if maxNoise > 0 {
			// AddNoiseQuieted models the FM threshold effect: full
			// hiss when the channel is idle, signal "captures" the
			// limiter and drops the audible noise floor when a
			// strong TX is present. See audio.AddNoiseQuieted.
			r.mixer.AddNoiseQuieted(blk, maxNoise)
		}

		if r.opts.Verbose && dec != audio.MixSilence {
			r.logger.Debug("rx mix",
				"port", dst, "decision", decisionName(dec),
				"sources", len(active),
				"peak", blk.PeakAbs())
		}

		if s := r.session.Load(); s != nil {
			s.WriteRX(dst, blk)
		}
		if r.opts.AudioTap != nil {
			key := dst.String() + "|rx"
			if r.opts.AudioTap.HasSubscribers(key) {
				r.opts.AudioTap.Publish(key, blk)
			}
		}

		if _, err := stdin.Write(blk); err != nil {
			if ctx.Err() == nil {
				r.logger.Debug("stdin write failed", "port", dst, "err", err)
			}
			return
		}
	}
}

func parsePortRef(s string) (config.PortRef, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return config.PortRef{NodeID: s[:i], PortID: s[i+1:]}, nil
		}
	}
	return config.PortRef{}, fmt.Errorf("invalid port ref %q", s)
}

// summariseSources returns a stable key for the set of source ports
// feeding a destination this tick, plus the sorted list of "node.port"
// strings. The key is the join of the sorted list; cheap to compare for
// equality across ticks.
func summariseSources(qs []*linkQueue) (string, []string) {
	if len(qs) == 0 {
		return "", nil
	}
	list := make([]string, len(qs))
	for i, q := range qs {
		list[i] = q.src.String()
	}
	// Bubble sort is fine — typically 0–5 entries.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
	// Join without allocating a separator string per call.
	key := list[0]
	for i := 1; i < len(list); i++ {
		key += "|" + list[i]
	}
	return key, list
}

// portNoiseFloor returns the receiver-side band-noise floor for a
// port, in dB below full-scale. Port-level NoiseDB overrides the
// global DefaultNoiseDB; both default to 0 (no global floor — old
// per-link-only behaviour preserved when neither is set).
func (r *Router) portNoiseFloor(ref config.PortRef) float64 {
	for _, n := range r.cfg.Nodes {
		if n.ID != ref.NodeID {
			continue
		}
		for _, p := range n.Ports {
			if p.ID != ref.PortID {
				continue
			}
			if p.NoiseDB > 0 {
				return p.NoiseDB
			}
			return r.cfg.DefaultNoiseDB
		}
	}
	return r.cfg.DefaultNoiseDB
}

func decisionName(d audio.MixDecision) string {
	switch d {
	case audio.MixSilence:
		return "silence"
	case audio.MixSingle:
		return "single"
	case audio.MixCapture:
		return "capture"
	case audio.MixCollision:
		return "collision"
	case audio.MixSum:
		return "sum"
	}
	return "?"
}
