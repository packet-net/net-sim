package router

import (
	"strings"
	"testing"

	"github.com/packethacking/net-sim/internal/config"
)

func TestObserverParsesLocalTX(t *testing.T) {
	o := NewObserver()
	ref := config.PortRef{NodeID: "QA0HUB", PortID: "vhf"}
	w := o.WriterFor(ref)
	// Feed three local-TX lines (typical samoyed-direwolf output) and
	// one remote-RX line (should be ignored).
	in := strings.Join([]string{
		"[0L] QA0HUB-7>NODES:(UI cmd, p=0)<0xfffd>ABEHUB",
		"[0.6] QA0ABN-7>NODES:(UI cmd, p=0)<0xfffd>ABENOR",
		"[0L] QA0HUB-7>ID:",
		"[0L] QA0HUB-1>BBS:(UI cmd, p=0)<0xfffd>",
		"",
	}, "\n")
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap := o.Snapshot()
	po, ok := snap["QA0HUB.vhf"]
	if !ok {
		t.Fatalf("no observations for QA0HUB.vhf")
	}
	if len(po.Calls) != 2 {
		t.Fatalf("want 2 distinct src calls, got %d (%v)", len(po.Calls), po.Calls)
	}
	calls := map[string]ObservedCall{}
	for _, c := range po.Calls {
		calls[c.Call] = c
	}
	hub, ok := calls["QA0HUB-7"]
	if !ok || hub.Frames != 2 {
		t.Errorf("QA0HUB-7 frames = %d, want 2", hub.Frames)
	}
	if hub.Roles["NODES"] != 1 || hub.Roles["ID"] != 1 {
		t.Errorf("QA0HUB-7 roles = %v, want NODES:1 ID:1", hub.Roles)
	}
	bbs, ok := calls["QA0HUB-1"]
	if !ok || bbs.Frames != 1 {
		t.Errorf("QA0HUB-1 frames = %d, want 1", bbs.Frames)
	}
}

func TestObserverStationSummaryPicksRoutingCall(t *testing.T) {
	o := NewObserver()
	// QA0HUB-7 broadcasts NODES many times; QA0HUB-1 sends BBS a few
	// times. Summary should mark -7 as the routing call and list both.
	w := o.WriterFor(config.PortRef{NodeID: "QA0HUB", PortID: "vhf"})
	lines := []string{}
	for i := 0; i < 12; i++ {
		lines = append(lines, "[0L] QA0HUB-7>NODES:...")
	}
	for i := 0; i < 3; i++ {
		lines = append(lines, "[0L] QA0HUB-1>BBS:...")
	}
	w.Write([]byte(strings.Join(lines, "\n") + "\n"))

	sums := o.StationSummaries()
	st, ok := sums["QA0HUB"]
	if !ok {
		t.Fatalf("no summary for QA0HUB")
	}
	if st.RoutingCall != "QA0HUB-7" {
		t.Errorf("routing call = %q, want QA0HUB-7", st.RoutingCall)
	}
	if len(st.AllCalls) != 2 {
		t.Errorf("all calls = %v, want 2 entries", st.AllCalls)
	}
}

func TestObserverPartialLines(t *testing.T) {
	// Stderr arrives in chunks that may split mid-line. The buffer
	// should glue partial reads back together before scanning.
	o := NewObserver()
	w := o.WriterFor(config.PortRef{NodeID: "X", PortID: "vhf"})
	w.Write([]byte("[0L] QA0HUB-7>"))
	w.Write([]byte("NODES:rest of line\n"))
	if got := o.Snapshot()["X.vhf"].Calls; len(got) != 1 || got[0].Call != "QA0HUB-7" {
		t.Errorf("partial-line glue failed: %v", got)
	}
}

func TestObserverParsesPdnSoundmodemTX(t *testing.T) {
	o := NewObserver()
	ref := config.PortRef{NodeID: "a", PortID: "fm"}
	w := o.WriterFor(ref)
	in := strings.Join([]string{
		"tx[0] qpsk3600 QA0HUB-7>NODES 136 bytes",
		"rx[0] qpsk3600-il2pc QA0ABN-7>NODES 136 bytes  crc ok  fec 0  snr 69.6 dB  -4 Hz",
		"tx[0] afsk1200 QA0HUB-1>BBS 40 bytes",
		"",
	}, "\n")
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}
	po := o.Snapshot()["a.fm"]
	got := map[string]int{}
	for _, c := range po.Calls {
		got[c.Call] = c.Frames
	}
	if len(got) != 2 || got["QA0HUB-7"] != 1 || got["QA0HUB-1"] != 1 {
		t.Fatalf("want QA0HUB-7 and QA0HUB-1 once each (and no rx lines), got %v", got)
	}
}
