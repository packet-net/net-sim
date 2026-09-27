package router

// Observer learns each port's AX.25 source callsigns by reading
// samoyed/direwolf's stderr stream. Every locally-transmitted frame
// is logged by samoyed in a recognisable shape:
//
//	[0L] QA0HUB-7>NODES:(UI cmd, p=0)...
//	[0L] QA0HUB-1>BBS:...
//
// The "[0L]" prefix marks a LOCAL transmission (channel 0, originating
// here), distinguishing it from "[0.X]" which marks a remote frame
// decoded at this port. We care only about local TX — the calls a
// station uses on the air at this port.
//
// One port can legitimately send under several SSIDs in the same
// session: NODECALL-7 for routing, BBSCALL-1 for the BBS app,
// CHATCALL-2 for chat, user logins masquerading as their own
// callsign. We track them all together, with frame counts and a
// per-destination histogram so consumers can heuristic "this is
// the routing call" (high NODES count) vs. "this is the BBS app"
// (high BBS count).

import (
	"bytes"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/packethacking/net-sim/internal/config"
)

// ObservedCall is one source callsign seen leaving a port.
type ObservedCall struct {
	Call      string         `json:"call"`
	FirstSeen time.Time      `json:"first_seen"`
	LastSeen  time.Time      `json:"last_seen"`
	Frames    int            `json:"frames"`
	// Roles maps destination call/alias seen in outgoing frames to a
	// count. E.g. {"NODES": 14, "ID": 3} suggests this is a routing
	// call; {"BBS": 1, "RMS": 1} suggests an application call.
	Roles map[string]int `json:"roles"`
}

// PortObservations groups every call seen leaving one port.
type PortObservations struct {
	Calls []ObservedCall `json:"calls"`
}

// StationSummary is the per-station aggregate consumers usually want.
// RoutingCall is the call that broadcasts NODES (NETROM L3 routing)
// most often — typically NODECALL-7. AllCalls is every distinct
// observed source call across the station's ports, deduplicated.
type StationSummary struct {
	RoutingCall string   `json:"routing_call,omitempty"`
	AllCalls    []string `json:"all_calls,omitempty"`
}

// Observer accumulates call observations from samoyed/direwolf stderr.
// One Observer per running simulator (created by router.Start when
// observation is wanted).
type Observer struct {
	mu      sync.RWMutex
	perPort map[config.PortRef]map[string]*ObservedCall
}

// NewObserver returns an empty Observer.
func NewObserver() *Observer {
	return &Observer{perPort: map[config.PortRef]map[string]*ObservedCall{}}
}

// WriterFor returns an io.Writer to feed one port's stderr stream
// into. The writer is line-buffered and thread-safe.
func (o *Observer) WriterFor(ref config.PortRef) io.Writer {
	return &observerWriter{obs: o, ref: ref}
}

// Snapshot returns a deep-copied view of the current per-port state.
// Safe to JSON-encode without further locking.
func (o *Observer) Snapshot() map[string]PortObservations {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]PortObservations, len(o.perPort))
	for ref, calls := range o.perPort {
		list := make([]ObservedCall, 0, len(calls))
		for _, c := range calls {
			cc := *c
			cc.Roles = make(map[string]int, len(c.Roles))
			for k, v := range c.Roles {
				cc.Roles[k] = v
			}
			list = append(list, cc)
		}
		out[ref.String()] = PortObservations{Calls: list}
	}
	return out
}

// StationSummaries collapses the per-port observations down to one
// entry per station. RoutingCall is the call that has sent the most
// NODES frames across the station (ties broken by overall frame
// count); AllCalls is the de-duplicated set of all observed calls.
func (o *Observer) StationSummaries() map[string]StationSummary {
	o.mu.RLock()
	defer o.mu.RUnlock()

	type score struct {
		nodes  int
		frames int
	}
	type stationData struct {
		calls  map[string]score
		seen   map[string]struct{} // ordered with all-calls list
		order  []string
	}
	stations := map[string]*stationData{}

	for ref, calls := range o.perPort {
		st, ok := stations[ref.NodeID]
		if !ok {
			st = &stationData{calls: map[string]score{}, seen: map[string]struct{}{}}
			stations[ref.NodeID] = st
		}
		for call, oc := range calls {
			s := st.calls[call]
			s.frames += oc.Frames
			s.nodes += oc.Roles["NODES"]
			st.calls[call] = s
			if _, ok := st.seen[call]; !ok {
				st.seen[call] = struct{}{}
				st.order = append(st.order, call)
			}
		}
	}

	out := make(map[string]StationSummary, len(stations))
	for node, st := range stations {
		var bestCall string
		var bestNodes, bestFrames int
		for call, s := range st.calls {
			better := s.nodes > bestNodes ||
				(s.nodes == bestNodes && s.frames > bestFrames)
			if better {
				bestCall, bestNodes, bestFrames = call, s.nodes, s.frames
			}
		}
		out[node] = StationSummary{
			RoutingCall: bestCall,
			AllCalls:    append([]string{}, st.order...),
		}
	}
	return out
}

// observerWriter is the per-port stream sink installed in tnc.Spec.
// It buffers partial lines, scans completed lines for the local-TX
// pattern, and updates the parent Observer's state.
type observerWriter struct {
	obs *Observer
	ref config.PortRef
	mu  sync.Mutex
	buf []byte
}

// frameLine matches samoyed/direwolf's local-TX summary lines.
// Example: "[0L] QA0HUB-7>NODES:(UI cmd, p=0)..."
//
// Group 1: src call (with optional SSID)
// Group 2: dest (call or alias)
//
// pdn-soundmodem logs the same thing as "tx[0] qpsk3600 QA0HUB-7>NODES 136 bytes".
var frameLine = regexp.MustCompile(`(?:\[0L\]|\btx\[\d+\] [a-z0-9-]+)\s+([A-Z0-9][A-Z0-9-]{1,9})>([A-Z0-9][A-Z0-9-]{0,9})`)

func (w *observerWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		w.scan(line)
	}
	return len(b), nil
}

func (w *observerWriter) scan(line []byte) {
	m := frameLine.FindSubmatch(line)
	if m == nil {
		return
	}
	src := string(m[1])
	dest := string(m[2])
	w.obs.note(w.ref, src, dest)
}

// note records one observation. Called under w.mu but takes the
// Observer's own lock for the update.
func (o *Observer) note(ref config.PortRef, src, dest string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	calls, ok := o.perPort[ref]
	if !ok {
		calls = map[string]*ObservedCall{}
		o.perPort[ref] = calls
	}
	oc, ok := calls[src]
	now := time.Now()
	if !ok {
		oc = &ObservedCall{
			Call:      src,
			FirstSeen: now,
			Roles:     map[string]int{},
		}
		calls[src] = oc
	}
	oc.LastSeen = now
	oc.Frames++
	oc.Roles[dest]++
}
