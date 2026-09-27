package tnc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// pdn-soundmodem backend.
//
// pdn-soundmodem has a "pipe:" audio device made for exactly this: two
// FIFOs standing in for a sound card, raw 32-bit float samples, read and
// written by the modem itself. We create both FIFOs, hold them open
// read-write (so neither side blocks on the other being there), and put an
// adapter on each so the router still sees int16 like every other backend.
// Both sides run at the router's 48 kHz, so the adapters only convert
// sample format:
//
//	router rxFeeder --s16--> pdnRxWriter --f32--> rx.fifo --> pdn
//	pdn --> tx.fifo --f32--> pdnTxReader --s16--> router txReader
//
// Two behaviours of the pipe device shape the adapters:
//
//   - The capture side is paced to wall clock and fills any shortfall with
//     silence, like a real card on a quiet band. So pdn never blocks the
//     router, but a block that arrives late is heard as a gap. We keep a
//     small cushion of audio queued in the FIFO so ordinary ticker jitter
//     never reaches the demodulator, and cap how much can pile up (for
//     instance before pdn has opened its input) so latency stays bounded.
//     This is also why tnc: pdn refuses time_scale > 1.
//
//   - The transmit side writes each keyup as fast as the FIFO takes it and
//     then nothing, exactly like samoyed's UDP. The router already handles
//     bursts; the adapter only has to notice the end of one so it can pad
//     to a whole router block, rather than leave the last few milliseconds
//     stuck until the next transmission.

// pdnPipeRate is the rate we run pdn's pipe device at: the router's, which
// is a multiple of every pdn DSP rate (12 kHz and 48 kHz), as pdn's
// start-up check requires.
const pdnPipeRate = audio.SampleRate

// pdnRxCushion is how much audio we keep queued ahead of pdn's reader, and
// pdnRxMaxBacklog the most we let pile up before dropping blocks.
const (
	pdnRxCushion    = 40 * time.Millisecond
	pdnRxMaxBacklog = 200 * time.Millisecond
)

// pdnTxBurstGap is how long the transmit FIFO has to be quiet before we
// call the keyup over. pdn writes a keyup far faster than real time, so a
// gap this long inside one only happens if pdn itself has stalled.
const pdnTxBurstGap = 50 * time.Millisecond

// pdnPipeBytes is the FIFO capacity we ask for (Linux F_SETPIPE_SZ). The
// default 64 KiB holds only 340 ms of 48 kHz float audio.
const pdnPipeBytes = 1 << 20

// pdnStartTimeout bounds the wait for pdn's KISS listener.
const pdnStartTimeout = 15 * time.Second

// pdnModeName maps a net-sim modem to the pdn-soundmodem mode name.
// gfsk9600 is net-sim's name for G3RUH 9600, which pdn calls fsk9600;
// every other mode is already a pdn name (validated in config).
func pdnModeName(m config.Modem) string {
	if m.Mode == config.ModeGFSK9600 {
		return "fsk9600"
	}
	return string(m.Mode)
}

func pdnArgs(s Spec, rxPath, txPath string) []string {
	return []string{
		"--device", fmt.Sprintf("pipe:%s,%s,%d", rxPath, txPath, pdnPipeRate),
		"--kiss", strconv.Itoa(s.KissPort),
		"--bind", "127.0.0.1",
		"--modem", "0:" + pdnModeName(s.Modem),
	}
}

var pdnModeCache sync.Map // binary path -> []string

// pdnModes returns the modes the pdn-soundmodem binary supports, read from
// the list at the end of its --help. Cached per binary path.
func pdnModes(ctx context.Context, bin string) ([]string, error) {
	if v, ok := pdnModeCache.Load(bin); ok {
		return v.([]string), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--help").Output()
	if err != nil {
		return nil, fmt.Errorf("run %s --help: %w", bin, err)
	}
	modes := parsePdnModes(out)
	if len(modes) == 0 {
		return nil, fmt.Errorf("%s --help lists no modes; is it pdn-soundmodem?", bin)
	}
	pdnModeCache.Store(bin, modes)
	return modes, nil
}

// parsePdnModes pulls the comma-separated list that follows the
// "Modes for --modem N:MODE:" line of pdn-soundmodem's usage text.
func parsePdnModes(help []byte) []string {
	var modes []string
	sc := bufio.NewScanner(bytes.NewReader(help))
	in := false
	for sc.Scan() {
		line := sc.Text()
		if !in {
			in = strings.HasPrefix(line, "Modes for --modem")
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" || !strings.HasPrefix(line, " ") || strings.HasPrefix(t, "plus ") {
			break
		}
		for _, m := range strings.Split(t, ",") {
			if m = strings.TrimSpace(m); m != "" {
				modes = append(modes, m)
			}
		}
	}
	return modes
}

func startPdn(ctx context.Context, s Spec) (*Child, error) {
	if s.PdnBin == "" {
		s.PdnBin = "pdn-soundmodem"
	}
	if s.WorkDir == "" {
		s.WorkDir = os.TempDir()
	}

	modes, err := pdnModes(ctx, s.PdnBin)
	if err != nil {
		return nil, err
	}
	if name := pdnModeName(s.Modem); !slices.Contains(modes, name) {
		return nil, fmt.Errorf("pdn-soundmodem has no mode %q; this build has: %s", name, strings.Join(modes, ", "))
	}

	portDir := filepath.Join(s.WorkDir, fmt.Sprintf("pdn-%s-%s", s.NodeID, s.PortID))
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}
	rxPath := filepath.Join(portDir, "rx.fifo") // router -> pdn
	txPath := filepath.Join(portDir, "tx.fifo") // pdn -> router

	var cleanups []func() error
	fail := func(err error) (*Child, error) {
		for i := len(cleanups) - 1; i >= 0; i-- {
			_ = cleanups[i]()
		}
		return nil, err
	}

	openFifo := func(path string) (*os.File, error) {
		_ = os.Remove(path)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			return nil, fmt.Errorf("mkfifo %s: %w", path, err)
		}
		cleanups = append(cleanups, func() error { return os.Remove(path) })
		// Read-write, for the same reason pdn opens its ends that way:
		// it never blocks waiting for the far end and never sees EOF.
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("open fifo %s: %w", path, err)
		}
		cleanups = append(cleanups, func() error { _ = f.Close(); return nil })
		setPipeSize(f, pdnPipeBytes) // best effort
		return f, nil
	}
	rxFifo, err := openFifo(rxPath)
	if err != nil {
		return fail(err)
	}
	txFifo, err := openFifo(txPath)
	if err != nil {
		return fail(err)
	}

	rx := newPdnRxWriter(rxFifo, io.Discard)
	if len(s.RxPrime) > 0 {
		// Written before pdn starts, in place of the silence cushion.
		rx.primed = true
		if _, err := rx.Write(s.RxPrime); err != nil {
			return fail(fmt.Errorf("prime rx fifo: %w", err))
		}
	}

	cmd := exec.CommandContext(ctx, s.PdnBin, pdnArgs(s, rxPath, txPath)...)
	cmd.Dir = portDir
	// pdn keeps state files beside its config or in $STATE_DIRECTORY; we
	// give it no config, so point the state directory at the port's dir
	// rather than let it reach for /var/lib/pdn-soundmodem.
	cmd.Env = append(os.Environ(), "STATE_DIRECTORY="+portDir)
	prefixed := newPrefixWriter(os.Stderr, "["+s.NodeID+"."+s.PortID+"] ")
	if s.StderrTap != nil {
		cmd.Stdout = io.MultiWriter(prefixed, s.StderrTap)
	} else {
		cmd.Stdout = prefixed
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start pdn-soundmodem: %w", err))
	}

	c := &Child{
		spec:     s,
		cmd:      cmd,
		stdin:    rx,
		txReader: newPdnTxReader(txFifo),
		cleanups: cleanups,
	}
	rx.log = prefixed
	c.watch()

	if err := waitListening(ctx, s.KissPort, pdnStartTimeout, c.exited); err != nil {
		_ = c.Stop()
		return nil, fmt.Errorf("wait for kiss tcp %d: %w", s.KissPort, err)
	}
	return c, nil
}

// pdnRxWriter takes the router's int16 blocks and writes them to pdn's
// capture FIFO as float32, keeping the FIFO's backlog between pdnRxCushion
// and pdnRxMaxBacklog.
type pdnRxWriter struct {
	f   *os.File
	log io.Writer

	in      []float32
	buf     []byte
	primed  bool
	dropped int // blocks dropped in the current overflow, 0 when flowing
}

func newPdnRxWriter(f *os.File, log io.Writer) *pdnRxWriter {
	return &pdnRxWriter{f: f, log: log}
}

func (w *pdnRxWriter) Write(p []byte) (int, error) {
	w.in = w.in[:0]
	for i := 0; i+1 < len(p); i += 2 {
		w.in = append(w.in, float32(int16(binary.LittleEndian.Uint16(p[i:])))/32768)
	}

	if !w.primed {
		// Start with a cushion of silence so a late tick is absorbed by
		// the FIFO instead of reaching the demodulator as a gap.
		w.primed = true
		cushion := make([]float32, int(pdnRxCushion.Seconds()*pdnPipeRate))
		if err := w.writeFloats(cushion); err != nil {
			return 0, err
		}
	}

	maxBytes := int(pdnRxMaxBacklog.Seconds()*pdnPipeRate) * 4
	if depth, err := fifoDepth(w.f); err == nil && depth > maxBytes {
		// pdn isn't reading (still starting, or stalled). Drop rather
		// than let latency grow without bound.
		if w.dropped == 0 {
			fmt.Fprintf(w.log, "net-sim: pdn-soundmodem is not keeping up with its audio input; dropping receive audio\n")
		}
		w.dropped++
		return len(p), nil
	}
	if w.dropped > 0 {
		fmt.Fprintf(w.log, "net-sim: pdn-soundmodem audio input flowing again after %d dropped blocks\n", w.dropped)
		w.dropped = 0
	}
	if err := w.writeFloats(w.in); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *pdnRxWriter) writeFloats(v []float32) error {
	w.buf = w.buf[:0]
	for _, x := range v {
		w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(x))
	}
	_, err := w.f.Write(w.buf)
	return err
}

func (w *pdnRxWriter) Close() error { return w.f.Close() }

// pdnTxReader turns pdn's float32 transmit FIFO into the int16 byte stream
// the router's txReader expects.
type pdnTxReader struct {
	f *os.File

	raw     []byte
	carry   []byte // a partial float left over from the last read
	flt     []float32
	out     []byte
	emitted int // bytes emitted in the current burst
	inBurst bool
}

func newPdnTxReader(f *os.File) *pdnTxReader {
	return &pdnTxReader{f: f, raw: make([]byte, 32*1024)}
}

func (r *pdnTxReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		r.out = r.out[:0]
		deadline := time.Time{}
		if r.inBurst {
			deadline = time.Now().Add(pdnTxBurstGap)
		}
		_ = r.f.SetReadDeadline(deadline)
		n, err := r.f.Read(r.raw)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			r.endBurst()
			continue
		}
		if err != nil {
			return 0, err
		}
		r.inBurst = true
		data := append(r.carry, r.raw[:n]...)
		whole := len(data) / 4 * 4
		r.flt = r.flt[:0]
		for i := 0; i < whole; i += 4 {
			r.flt = append(r.flt, math.Float32frombits(binary.LittleEndian.Uint32(data[i:])))
		}
		r.carry = append(r.carry[:0], data[whole:]...)
		r.appendS16(r.flt)
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// endBurst pads the burst to a whole router block, so none of it waits
// for the next transmission.
func (r *pdnTxReader) endBurst() {
	r.inBurst = false
	r.carry = r.carry[:0]
	if rem := r.emitted % audio.BlockBytes; rem != 0 {
		r.out = append(r.out, make([]byte, audio.BlockBytes-rem)...)
	}
	r.emitted = 0
}

func (r *pdnTxReader) appendS16(v []float32) {
	for _, x := range v {
		s := math.Round(float64(x) * 32767)
		s = math.Max(math.MinInt16, math.Min(math.MaxInt16, s))
		r.out = binary.LittleEndian.AppendUint16(r.out, uint16(int16(s)))
	}
	r.emitted += 2 * len(v)
}

func (r *pdnTxReader) Close() error { return r.f.Close() }

// fionread is Linux's FIONREAD ioctl number.
const fionread = 0x541B

// fifoDepth returns how many bytes are queued in a pipe (FIONREAD, which
// Linux answers for either end).
func fifoDepth(f *os.File) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int32
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, fionread, uintptr(unsafe.Pointer(&n)))
	}); err != nil {
		return 0, err
	}
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// setPipeSize asks for a larger pipe buffer (F_SETPIPE_SZ). Best effort:
// an unprivileged process is capped at /proc/sys/fs/pipe-max-size, 1 MiB
// by default, and a failure only means less headroom.
func setPipeSize(f *os.File, size int) {
	const fSetPipeSz = 1031
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, fSetPipeSz, uintptr(size))
	})
}
