// Package tnc spawns and supervises one TNC child process per simulated
// port. Two backends are supported:
//
//   - "samoyed" (default): https://github.com/doismellburning/samoyed,
//     a Go port of Dire Wolf. Audio in via stdin, audio out via UDP
//     datagrams.
//   - "direwolf": stock Dire Wolf (apt install direwolf). Audio in via
//     stdin, audio out via an ALSA "file" plugin writing to a FIFO
//     because stock Dire Wolf 1.8 still doesn't support UDP audio out.
//   - "pdn": pdn-soundmodem (https://github.com/packet-net/pdn-soundmodem)
//     on its pipe: device, two FIFOs of 48 kHz float samples. Adds the
//     modes samoyed doesn't have (qpsk3600, c4fsk, OFDM-FM, ...).
//
// The router treats each child as a black box exposing:
//   - a stdin Writer where the router pushes RX audio (PCM samples)
//   - a TXAudio io.Reader where the child's TX PCM bytes appear
//   - a TCP KISS port (managed by the child itself)
package tnc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/packethacking/net-sim/internal/config"
)

// SampleRate is the audio sample rate both backends default to (and we
// follow suit). See NOTES-audio-io.md.
const SampleRate = 44100

// BytesPerSample for mono signed-16-bit PCM.
const BytesPerSample = 2

// Backend names a TNC implementation.
type Backend string

const (
	BackendSamoyed  Backend = "samoyed"
	BackendDirewolf Backend = "direwolf"
	BackendPdn      Backend = "pdn"
)

// Spec is everything a Child needs to launch.
type Spec struct {
	Backend        Backend // "samoyed" (default) or "direwolf"
	NodeID, PortID string
	Modem          config.Modem
	KissPort       int

	// RxAudioUDPPort is the local UDP port the *samoyed* backend sends
	// TX audio to (router listens here). Ignored by direwolf.
	RxAudioUDPPort int

	SamoyedBin  string // path to samoyed-direwolf
	DirewolfBin string // path to direwolf
	PdnBin      string // path to pdn-soundmodem
	WorkDir     string // scratch dir for per-port config files (and FIFOs, for direwolf)

	// StderrTap, if non-nil, also receives the TNC's stdout/stderr
	// (unprefixed). Used by router.Observer to scrape locally-
	// transmitted callsigns. The existing prefixed-to-real-stderr
	// stream remains so docker logs stay readable.
	StderrTap io.Writer
}

// Child is one running TNC process exposing stdin/TX-audio/KISS to the
// router. The internal layout differs per backend; the interface is
// uniform.
type Child struct {
	spec     Spec
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	txReader io.ReadCloser

	// cleanups run in Stop, in reverse order. Backends register their
	// own (close UDP listener, close + remove FIFO, etc.).
	cleanups []func() error

	mu     sync.Mutex
	exitC  chan error
	exited chan struct{} // closed once the process has exited
}

// Stdin returns the writer carrying RX audio into the TNC.
func (c *Child) Stdin() io.Writer { return c.stdin }

// TXAudio returns a reader producing the TX-side PCM stream from the
// TNC. samoyed datagrams are concatenated into a byte stream; direwolf
// bytes flow directly from the FIFO.
func (c *Child) TXAudio() io.Reader { return c.txReader }

// Spec returns the launch spec.
func (c *Child) Spec() Spec { return c.spec }

// Pid returns the child's OS process id, or 0 if it never started. Used
// by the router's -rt-priority plumbing to renice spawned TNCs.
func (c *Child) Pid() int {
	if c.cmd != nil && c.cmd.Process != nil {
		return c.cmd.Process.Pid
	}
	return 0
}

// Wait blocks until the child exits.
func (c *Child) Wait() error { return <-c.exitC }

// Done returns a channel that is closed when the child exits.
func (c *Child) Done() <-chan struct{} { return c.exited }

// Stop kills the child and runs registered cleanups. Idempotent.
func (c *Child) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.stdin.Close()
		_ = c.txReader.Close()
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, exec.ErrNotFound) {
			// keep going — cleanups still need to run
		}
	}
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		_ = c.cleanups[i]()
	}
	c.cleanups = nil
	return nil
}

// Start launches a child using the configured backend. Defaults to
// samoyed if Backend is empty.
func Start(ctx context.Context, s Spec) (*Child, error) {
	if err := SupportedMode(s.Backend, s.Modem); err != nil {
		return nil, err
	}
	switch s.Backend {
	case "", BackendSamoyed:
		return startSamoyed(ctx, s)
	case BackendDirewolf:
		return startDirewolf(ctx, s)
	case BackendPdn:
		return startPdn(ctx, s)
	default:
		return nil, fmt.Errorf("unknown tnc backend %q", s.Backend)
	}
}

// watch starts the goroutine that reaps the process and records its exit.
func (c *Child) watch() {
	c.exitC = make(chan error, 1)
	c.exited = make(chan struct{})
	go func() {
		err := c.cmd.Wait()
		c.exitC <- err
		close(c.exited)
	}()
}

// SupportedMode reports whether the modem mode can run on the TNC.
// samoyed and direwolf share the same modem flags, so the answer is the
// same for both. pdn-soundmodem modes are checked against the binary's own
// mode list when the port starts (see startPdn), since that list moves with
// pdn releases.
func SupportedMode(b Backend, m config.Modem) error {
	if b == BackendPdn {
		switch m.Mode {
		case config.ModeIL2P, config.ModeBPSK:
			return fmt.Errorf("%s is not a pdn-soundmodem mode name", m.Mode)
		}
		return nil
	}
	switch m.Mode {
	case config.ModeAFSK1200, config.ModeGFSK9600:
		return nil
	case config.ModeIL2P:
		switch m.Inner {
		case config.ModeAFSK1200, config.ModeGFSK9600:
			return nil
		default:
			return fmt.Errorf("il2p inner=%q not supported", m.Inner)
		}
	case config.ModeBPSK:
		return errors.New("bpsk not yet supported by either samoyed or direwolf")
	}
	return fmt.Errorf("unknown mode %q", m.Mode)
}

// modemConfLines translates a modem spec into TNC config-file directives.
// Both samoyed and direwolf accept the same MODEM/IL2PTX directives, so
// this is shared. Translation table also lives in NOTES-audio-io.md.
//
// `MODEM 9600` already implies G3RUH at that baud rate (see samoyed
// generic.conf comment "G3RUH style"); no `g3ruh` keyword needed.
//
// `IL2PTX <0|1>` enables IL2P transmit with the requested FEC strength
// (1 = max, 0 = weak). Polarity defaults to normal.
func modemConfLines(m config.Modem) []string {
	switch m.Mode {
	case config.ModeAFSK1200:
		return []string{"MODEM 1200"}
	case config.ModeGFSK9600:
		return []string{"MODEM 9600"}
	case config.ModeIL2P:
		var out []string
		switch m.Inner {
		case config.ModeGFSK9600:
			out = append(out, "MODEM 9600")
		default:
			out = append(out, "MODEM 1200")
		}
		fec := "0"
		if m.FEC == config.FECStrong {
			fec = "1"
		}
		out = append(out, "IL2PTX "+fec)
		return out
	}
	return nil
}
