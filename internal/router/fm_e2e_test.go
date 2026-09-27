package router

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/tnc"
)

// End-to-end behaviour of the FM channel with a real modem (direwolf,
// afsk1200, default radios: 25 W, narrow data taps, residential site noise,
// a floor of about -120.8 dBm).

// startTopology runs a YAML topology of direwolf ports with the KISS ports
// filled in (KISS_<node>) and returns a KISS connection per node.
func startTopology(t *testing.T, yml string, nodes ...string) map[string]net.Conn {
	t.Helper()
	dw := findBackend(t, tnc.BackendDirewolf, "DIREWOLF_BIN")
	ports := map[string]int{}
	for _, n := range nodes {
		ports[n] = freeTCP(t)
		yml = strings.ReplaceAll(yml, "KISS_"+n, fmt.Sprint(ports[n]))
	}
	cfg, err := config.ParseBytes([]byte(yml))
	if err != nil {
		t.Fatalf("config:\n%s\n%v", yml, err)
	}
	r, err := Start(context.Background(), cfg, Options{
		DirewolfBin: dw, WorkDir: t.TempDir(), Logger: quietLogger(), StartingRxAudioPort: freeUDP(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop() })
	conns := map[string]net.Conn{}
	for _, n := range nodes {
		c := dialKISS(t, ports[n])
		// Persistence 255 and slot time 0: transmit as soon as the channel
		// is clear, so two hidden stations really do key up together.
		_, _ = c.Write([]byte{0xC0, 0x02, 0xFF, 0xC0, 0xC0, 0x03, 0x00, 0xC0})
		conns[n] = c
	}
	return conns
}

// collector reads a KISS connection in the background and records which of
// the expected payloads arrived.
type collector struct {
	mu  sync.Mutex
	buf []byte
}

func collect(c net.Conn) *collector {
	col := &collector{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := c.Read(b)
			if err != nil {
				return
			}
			col.mu.Lock()
			col.buf = append(col.buf, b[:n]...)
			col.mu.Unlock()
		}
	}()
	return col
}

func (col *collector) has(s string) bool {
	col.mu.Lock()
	defer col.mu.Unlock()
	return bytes.Contains(col.buf, []byte(s))
}

func linkYAML(from, to string, pathLoss float64) string {
	return fmt.Sprintf("  - { from: %s.p, to: %s.p, path_loss_db: %g }\n", from, to, pathLoss)
}

func node(name string) string {
	return fmt.Sprintf("  - { id: %s, ports: [ { id: p, tnc: direwolf, modem: { mode: afsk1200 }, kiss_port: KISS_%s } ] }\n", name, name)
}

// A strong link delivers everything; one below the FM threshold delivers
// nothing, because the discriminator's output collapses into clicks.
func TestFMThresholdWithDirewolf(t *testing.T) {
	if testing.Short() {
		t.Skip("sends 24 frames in real time")
	}
	for _, c := range []struct {
		pathLoss float64
		min, max int
	}{
		{135, 7, 8}, // CNR about 30 dB: everything
		{168, 0, 1}, // CNR about -3 dB: nothing
	} {
		conns := startTopology(t, "nodes:\n"+node("a")+node("b")+"links:\n"+linkYAML("a", "b", c.pathLoss), "a", "b")
		rx := collect(conns["b"])
		var msgs []string
		for i := 0; i < 8; i++ {
			msg := fmt.Sprintf("threshold %g frame %d ", c.pathLoss, i)
			msgs = append(msgs, msg)
			_, _ = conns["a"].Write(kissFrame(uiFrame("TEST", "N0CALL", strings.Repeat(msg, 3))))
			time.Sleep(1500 * time.Millisecond)
		}
		time.Sleep(2 * time.Second)
		got := 0
		for _, msg := range msgs {
			if rx.has(msg) {
				got++
			}
		}
		t.Logf("path loss %g dB: %d of 8 frames", c.pathLoss, got)
		if got < c.min || got > c.max {
			t.Errorf("path loss %g dB: %d of 8 frames, want %d to %d", c.pathLoss, got, c.min, c.max)
		}
	}
}

// Two stations that can't hear each other transmit at once into a third.
// With one 20 dB stronger the receiver captures it and copies its frame;
// the weaker one is lost.
func TestFMCaptureWithDirewolf(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time collisions")
	}
	yml := "nodes:\n" + node("a") + node("b") + node("c") + "links:\n" +
		linkYAML("a", "c", 130) + linkYAML("b", "c", 150)
	conns := startTopology(t, yml, "a", "b", "c")
	rx := collect(conns["c"])
	strong, weak := 0, 0
	for i := 0; i < 5; i++ {
		ma := fmt.Sprintf("strong station frame %d ", i)
		mb := fmt.Sprintf("weak station frame %d ", i)
		fa := kissFrame(uiFrame("TEST", "N0AAA", strings.Repeat(ma, 4)))
		fb := kissFrame(uiFrame("TEST", "N0BBB", strings.Repeat(mb, 4)))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = conns["a"].Write(fa) }()
		go func() { defer wg.Done(); _, _ = conns["b"].Write(fb) }()
		wg.Wait()
		time.Sleep(2500 * time.Millisecond)
		if rx.has(ma) {
			strong++
		}
		if rx.has(mb) {
			weak++
		}
	}
	t.Logf("capture: strong station %d of 5, weak %d of 5", strong, weak)
	if strong < 4 || weak > 1 {
		t.Errorf("strong station copied %d of 5 and weak %d of 5; want the strong one to capture", strong, weak)
	}
}
