package router

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/tnc"
)

// End-to-end tests: two real TNCs joined by the router, a KISS frame in at
// one end and out of the other. Each needs its binaries (PDN_BIN,
// SAMOYED_BIN, DIREWOLF_BIN, or $PATH) and skips without them.

func findBackend(t *testing.T, b tnc.Backend, env string) string {
	t.Helper()
	bin, err := tnc.ResolveBinary(b, os.Getenv(env))
	if err != nil {
		t.Skipf("%s not available (set %s): %v", tnc.BinaryName(b), env, err)
	}
	return bin
}

func TestPdnModesThroughRouter(t *testing.T) {
	pdn := findBackend(t, tnc.BackendPdn, "PDN_BIN")
	// One of each family that runs over FM: AFSK, G3RUH, the FM QPSK
	// mode, 4-level FSK and OFDM-FM.
	for _, mode := range []config.Mode{"afsk1200", config.ModeGFSK9600, "qpsk3600", "c4fsk9600", "ofdm-fm-8k"} {
		t.Run(string(mode), func(t *testing.T) {
			pair(t, Options{PdnBin: pdn},
				config.Port{TNC: config.TNCPdn, Modem: config.Modem{Mode: mode}},
				config.Port{TNC: config.TNCPdn, Modem: config.Modem{Mode: mode}})
		})
	}
}

func TestPdnDirewolfInterop(t *testing.T) {
	pdn := findBackend(t, tnc.BackendPdn, "PDN_BIN")
	dw := findBackend(t, tnc.BackendDirewolf, "DIREWOLF_BIN")
	for _, mode := range []config.Mode{config.ModeAFSK1200, config.ModeGFSK9600} {
		t.Run(string(mode), func(t *testing.T) {
			pair(t, Options{PdnBin: pdn, DirewolfBin: dw},
				config.Port{TNC: config.TNCPdn, Modem: config.Modem{Mode: mode}},
				config.Port{TNC: config.TNCDirewolf, Modem: config.Modem{Mode: mode}})
		})
	}
}

// pair runs a two-node topology, a <-> b, and checks a UI frame gets from
// a to b and another from b to a.
func pair(t *testing.T, opts Options, a, b config.Port) {
	t.Helper()
	port := func(p config.Port) string {
		tncName := p.TNC
		if tncName == "" {
			tncName = config.TNCSamoyed
		}
		return fmt.Sprintf("[{ id: p, tnc: %s, modem: { mode: %s }, kiss_port: %d }]", tncName, p.Modem.Mode, p.KissPort)
	}
	a.KissPort, b.KissPort = freeTCP(t), freeTCP(t)
	yml := fmt.Sprintf(`nodes:
  - { id: a, ports: %s }
  - { id: b, ports: %s }
links:
  - { from: a.p, to: b.p, loss_db: 0 }
  - { from: b.p, to: a.p, loss_db: 0 }
`, port(a), port(b))
	cfg, err := config.ParseBytes([]byte(yml))
	if err != nil {
		t.Fatalf("config:\n%s\n%v", yml, err)
	}

	opts.WorkDir = t.TempDir()
	opts.Logger = quietLogger()
	opts.StartingRxAudioPort = freeUDP(t)
	r, err := Start(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.Stop()

	ca := dialKISS(t, a.KissPort)
	cb := dialKISS(t, b.KissPort)
	send(t, ca, cb, "a to b")
	send(t, cb, ca, "b to a")
}

func send(t *testing.T, from, to net.Conn, text string) {
	t.Helper()
	info := strings.Repeat(text+" ", 8)
	if _, err := from.Write(kissFrame(uiFrame("TEST", "N0CALL", info))); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	var got []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(got, []byte(info)) {
		_ = to.SetReadDeadline(deadline)
		n, err := to.Read(buf)
		if err != nil {
			t.Fatalf("%s: frame not received: %v", text, err)
		}
		got = append(got, buf[:n]...)
	}
}

func dialKISS(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial kiss %d: %v", port, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func freeTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func freeUDP(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// kissFrame wraps an AX.25 frame as a KISS data frame on channel 0.
func kissFrame(ax25 []byte) []byte {
	out := []byte{0xC0, 0x00}
	for _, b := range ax25 {
		switch b {
		case 0xC0:
			out = append(out, 0xDB, 0xDC)
		case 0xDB:
			out = append(out, 0xDB, 0xDD)
		default:
			out = append(out, b)
		}
	}
	return append(out, 0xC0)
}

// uiFrame builds an AX.25 UI frame with no digipeaters.
func uiFrame(dest, src, info string) []byte {
	addr := func(call string, last bool) []byte {
		a := make([]byte, 7)
		padded := (call + "      ")[:6]
		for i := 0; i < 6; i++ {
			a[i] = padded[i] << 1
		}
		a[6] = 0x60
		if last {
			a[6] |= 1
		}
		return a
	}
	f := append(addr(dest, false), addr(src, true)...)
	f = append(f, 0x03, 0xF0)
	return append(f, info...)
}

func TestSamoyedInterop(t *testing.T) {
	sam := findBackend(t, tnc.BackendSamoyed, "SAMOYED_BIN")
	for _, mode := range []config.Mode{config.ModeAFSK1200, config.ModeGFSK9600} {
		t.Run(string(mode)+"/samoyed-samoyed", func(t *testing.T) {
			pair(t, Options{SamoyedBin: sam},
				config.Port{TNC: config.TNCSamoyed, Modem: config.Modem{Mode: mode}},
				config.Port{TNC: config.TNCSamoyed, Modem: config.Modem{Mode: mode}})
		})
		t.Run(string(mode)+"/samoyed-direwolf", func(t *testing.T) {
			dw := findBackend(t, tnc.BackendDirewolf, "DIREWOLF_BIN")
			pair(t, Options{SamoyedBin: sam, DirewolfBin: dw},
				config.Port{TNC: config.TNCSamoyed, Modem: config.Modem{Mode: mode}},
				config.Port{TNC: config.TNCDirewolf, Modem: config.Modem{Mode: mode}})
		})
		t.Run(string(mode)+"/samoyed-pdn", func(t *testing.T) {
			pdn := findBackend(t, tnc.BackendPdn, "PDN_BIN")
			pair(t, Options{SamoyedBin: sam, PdnBin: pdn},
				config.Port{TNC: config.TNCSamoyed, Modem: config.Modem{Mode: mode}},
				config.Port{TNC: config.TNCPdn, Modem: config.Modem{Mode: mode}})
		})
	}
}
