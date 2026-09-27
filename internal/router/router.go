// Package router orchestrates the simulator: spawns the TNC children, owns
// the topology, gives each port an FM radio and carries the carriers
// between them (see channel.go).
//
// Audio flow per port P:
//
//	TNC TX audio --txReader--> P's transmitter (FM modulation)
//	    --> for each link P->D, the carrier is queued for D
//	D's rxFeeder, every 10 ms: sum of the carriers reaching D at their
//	    received levels, plus D's noise, through D's receiver --> D's TNC
//
// Each TNC gets continuous receive audio (hiss, with the squelch open)
// so its demodulator never starves.
//
// TNCs only produce TX audio while keying, and produce it as fast as they
// can compute it (no pacing): a 500 ms frame can arrive in a few wall-clock
// ms. We therefore queue per-link blocks and each receiver's rxFeeder
// drains them at exactly SampleRate/BlockSamples Hz, so the channel runs in
// real time.
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

// rxPrimeMS is how much of each receiver's idle output is put in front of
// its TNC's audio before the TNC starts. See Start.
const rxPrimeMS = 150

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
	opts Options
	cfg  *config.Config

	// stations are the ports' radios; factor is the IF oversampling they
	// share. See channel.go.
	stations map[config.PortRef]*station
	factor   int
	logger   *slog.Logger

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

// queuedBlock is one FIFO entry: a block of the transmitter's audio (as
// its TNC produced it) and the same block as modulated carrier at the IF
// rate, which is what receivers hear. The carrier is shared, read-only, by
// every link the transmitter fans out to.
type queuedBlock struct {
	blk audio.Block
	iq  []complex128
}

// linkQueue is one source->destination link: a non-dropping FIFO of the
// carrier blocks the source has transmitted, drained by the destination's
// rxFeeder in real time, plus the link's radio path (received level,
// frequency offset).
//
// A TNC bursts a whole keyup's worth of audio with no real-time pacing on its
// end (see txReader), so the buffer must hold an entire transmission and the
// rxFeeder plays it out at real time. It is non-dropping and grows as needed:
// dropping mid-transmission would drop the carrier mid-keyup, collapse the
// far end's DCD and make it key up on top of an in-progress transmission.
// Memory is bounded in practice (a keyup is finite; the backing array is
// released once drained); only a pathological runaway past maxQueueBlocks
// ever drops.
type linkQueue struct {
	src, dst config.PortRef
	rf       linkRF // rot is touched only by the destination's rxFeeder

	mu  sync.Mutex
	buf []queuedBlock // FIFO; index 0 = oldest
}

func newLinkQueue(src, dst config.PortRef, rf linkRF) *linkQueue {
	return &linkQueue{src: src, dst: dst, rf: rf}
}

// push appends a block to the link's buffer. It never blocks and never drops
// within a transmission, no matter how far the source TNC has run ahead of
// real time. The only drop is the maxQueueBlocks safety valve, which signals
// a stalled rxFeeder rather than normal operation, so it is logged.
func (q *linkQueue) push(qb queuedBlock, logger *slog.Logger) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) >= maxQueueBlocks {
		q.buf[0] = queuedBlock{}
		q.buf = q.buf[1:]
		logger.Warn("audio queue safety cap reached (rxFeeder stalled?)", "from", q.src, "to", q.dst)
	}
	q.buf = append(q.buf, qb)
}

// pop returns one block if immediately available (FIFO, oldest first).
func (q *linkQueue) pop() (queuedBlock, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) == 0 {
		return queuedBlock{}, false
	}
	qb := q.buf[0]
	q.buf[0] = queuedBlock{} // release the reference
	q.buf = q.buf[1:]
	if len(q.buf) == 0 {
		q.buf = nil // release the backing array once drained
	}
	return qb, true
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
	stations, factor := newStations(cfg)
	r := &Router{
		opts:       opts,
		cfg:        cfg,
		stations:   stations,
		factor:     factor,
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
		rf := newLinkRF(stations[fr], stations[to], *l.PathLossDB, factor)
		q := newLinkQueue(fr, to, rf)
		r.logger.Debug("link", "from", fr, "to", to, "rx_dbm", math.Round(rf.rxDBm*10)/10,
			"cnr_db", math.Round((rf.rxDBm-stations[to].floorDBm)*10)/10)
		r.linkQueues[fr] = append(r.linkQueues[fr], q)
		r.rxLinks[to] = append(r.rxLinks[to], q)
	}

	if opts.RTPriority {
		// pid 0 = this process (the router and all its pacing tickers).
		applyRTPriority(r.logger, "sim-router", 0)
	}

	// One tx tracker per port; pointers are stable across the run.
	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			r.txTrackers[config.PortRef{NodeID: n.ID, PortID: p.ID}] = &txTracker{}
		}
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
			// A real station hears its receiver from the first sample: an
			// open squelch is hiss from the start, never silence. Give the
			// TNC's audio input a moment of exactly that before it starts,
			// or an energy-based carrier sense sees silence turn into hiss
			// and calls the channel busy.
			st := r.stations[ref]
			st.iq = make([]complex128, audio.BlockSamples*factor)
			spec.RxPrime = st.idle(msBlocks(rxPrimeMS))
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
			r.runPort(rctx, cancel, ref, child)
		}
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

// runPort starts a port's audio as soon as its TNC is up, rather than
// after every port has started, so its receive audio never stalls.
func (r *Router) runPort(rctx context.Context, cancel context.CancelFunc, ref config.PortRef, child *tnc.Child) {
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		r.txReader(rctx, ref, child.TXAudio())
	}()
	go func() {
		defer r.wg.Done()
		r.rxFeeder(rctx, ref, child.Stdin())
	}()
	if r.opts.EventBus != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.txWatchdog(rctx, ref)
		}()
	}
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
		// Modulate once; every link shares the carrier. Even a radio with
		// no links is on the air (and so deaf) while it transmits.
		qb := queuedBlock{blk: blk}
		if st := r.stations[ref]; st != nil {
			qb.iq = st.modulate(blk, scaled(blockPeriod, r.cfg.TimeScale))
		}
		for _, q := range outgoing {
			q.push(qb, r.logger)
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
// to this port's TNC: whatever its radio's receiver makes of the carriers
// reaching it through the topology this block, and its own noise. A port
// never hears its own transmission (config validation rejects self-loops,
// and a transmitting radio's receiver is muted).
func (r *Router) rxFeeder(ctx context.Context, dst config.PortRef, stdin io.Writer) {
	ticker := time.NewTicker(scaled(blockPeriod, r.cfg.TimeScale))
	defer ticker.Stop()

	links := r.rxLinks[dst] // links *into* this destination
	st := r.stations[dst]
	openBlocks := msBlocks(st.radio.Squelch.OpenMS)
	closeBlocks := msBlocks(st.radio.Squelch.CloseMS)
	if len(st.iq) != audio.BlockSamples*r.factor {
		st.iq = make([]complex128, audio.BlockSamples*r.factor)
	}

	// Per-destination RX event state. We emit rx_decision only when the
	// (decision, source-set) tuple changes since the last block - busy
	// channels would otherwise generate ~100 events/s/port.
	var (
		lastDecision = "none"
		lastSources  string
		sourcePorts  []*linkQueue // scratch, reused
		carriers     []heard
		levels       []float64
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		carriers, sourcePorts, levels = carriers[:0], sourcePorts[:0], levels[:0]
		for _, q := range links {
			qb, ok := q.pop()
			if !ok || qb.iq == nil {
				continue
			}
			carriers = append(carriers, heard{rf: &q.rf, iq: qb.iq})
			sourcePorts = append(sourcePorts, q)
			levels = append(levels, q.rf.rxDBm)
		}

		blk, open := st.receive(carriers, openBlocks, closeBlocks)
		dec := decisionFor(levels)

		if r.opts.EventBus != nil {
			srcKey, srcList := summariseSources(sourcePorts)
			if dec != lastDecision || srcKey != lastSources {
				// Only emit when something interesting is happening or
				// when the channel just went quiet; silent-to-silent
				// transitions (the common case) are skipped.
				if dec != "silence" || lastDecision != "none" {
					r.opts.EventBus.Publish(events.Event{
						T:        time.Now(),
						Type:     events.RXDecision,
						Port:     dst.String(),
						Decision: dec,
						Sources:  srcList,
					})
				}
				lastDecision = dec
				lastSources = srcKey
			}
		}

		if r.opts.Verbose && dec != "silence" {
			r.logger.Debug("rx", "port", dst, "decision", dec, "carriers", len(carriers),
				"squelch_open", open, "peak", blk.PeakAbs())
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
