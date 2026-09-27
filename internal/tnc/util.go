package tnc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// deriveCallsign turns a node id into a synthetic AX.25 callsign for
// MYCALL. Direwolf/samoyed want something there but we never use it on
// the TX path (KISS frames carry their own source), so this is purely
// for log readability — `[a.vhf] N0A-1>APDW01:...` is easier to scan
// with multiple nodes than the same line saying `N0CALL`.
//
// Pads "N0" then takes the alphanumeric prefix of the node id, capping
// at 6 chars total per the AX.25 v2 callsign limit.
func deriveCallsign(nodeID string) string {
	var sb strings.Builder
	sb.WriteString("N0")
	for _, r := range strings.ToUpper(nodeID) {
		if sb.Len() >= 6 {
			break
		}
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 2 {
		return "N0SIM"
	}
	return sb.String()
}

// waitListening blocks until a TCP listener is accepting on 127.0.0.1:port,
// or until ctx is cancelled, the process exits (exited closes), or timeout
// elapses. Without the exit check a TNC that refuses its config costs the
// whole timeout and then reports "not listening" instead of "it exited".
func waitListening(ctx context.Context, port int, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d not listening after %s", port, timeout)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return errors.New("the TNC exited during start-up (its log lines above say why)")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// portFree reports an error if a TCP listener already holds port on any
// local address.
func portFree(port int) error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("kiss_port %d is already in use by another process", port)
	}
	return l.Close()
}

// prefixWriter writes each line with a "[label] " prefix, so a single
// stderr stream from N TNCs is still readable.
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
	mu     sync.Mutex
}

func newPrefixWriter(w io.Writer, prefix string) io.Writer {
	return &prefixWriter{w: w, prefix: prefix}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := indexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		if _, err := p.w.Write([]byte(p.prefix)); err != nil {
			return 0, err
		}
		if _, err := p.w.Write(p.buf[:i+1]); err != nil {
			return 0, err
		}
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
