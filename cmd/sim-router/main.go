// sim-router is the entrypoint for the AX.25 packet network simulator.
//
//	sim-router -config configs/hidden-node.yaml [-v] [-samoyed PATH]
//
// Reads a YAML topology, spawns one samoyed-direwolf child per port,
// routes audio between ports per the topology with FM capture-effect
// mixing, and exposes each port's KISS interface on the configured TCP
// port.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/router"
	"github.com/packethacking/net-sim/internal/tnc"
)

func main() {
	cfgPath := flag.String("config", "", "path to YAML topology file (required)")
	verbose := flag.Bool("v", false, "verbose / debug logging")
	samoyedPath := flag.String("samoyed", "", "path to samoyed-direwolf (default: search $PATH and common install paths)")
	direwolfPath := flag.String("direwolf", "", "path to direwolf (default: search $PATH)")
	pdnPath := flag.String("pdn", "", "path to pdn-soundmodem (default: search $PATH and /usr/bin)")
	workDir := flag.String("workdir", "", "scratch dir for per-port config files / FIFOs (default: a unique subdir of $TMPDIR)")
	recordDir := flag.String("record", "", "if set, record all per-port TX and RX audio to a timestamped subdirectory of this path")
	composite := flag.String("composite", "", "comma-separated transmitter ports (e.g. a.vhf,b.vhf) to composite into one real-time, sample-aligned WAV (one TX per channel — stereo for two). Requires -record for the output dir")
	timeScale := flag.Float64("time-scale", 0, "run the simulation N x faster than wall clock (>= 1.0; overrides the config's time_scale; see README for the fidelity caveat)")
	rtPriority := flag.Bool("rt-priority", false, "renice sim-router and every TNC child to -10 for smoother audio pacing under host load (best-effort; needs CAP_SYS_NICE)")
	flag.Parse()

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "sim-router: -config is required")
		flag.Usage()
		os.Exit(2)
	}

	if *composite != "" && *recordDir == "" {
		fmt.Fprintln(os.Stderr, "sim-router: -composite requires -record DIR (for the output directory)")
		os.Exit(2)
	}
	compositePorts, err := parseCompositePorts(*composite)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sim-router:", err)
		os.Exit(2)
	}

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}
	if *timeScale != 0 {
		if *timeScale < 1 {
			logger.Error("-time-scale must be >= 1.0 (slower-than-real-time is not supported)", "got", *timeScale)
			os.Exit(2)
		}
		cfg.TimeScale = *timeScale
		// Re-validate: some backends (tnc: pdn) can't run scaled.
		if err := cfg.Validate(); err != nil {
			logger.Error("-time-scale", "err", err)
			os.Exit(2)
		}
	}
	if cfg.TimeScale > 1 {
		// Accelerated-testing mode, not a calibrated CSMA simulation: the
		// TNC children's wall-clock waits (persist/slottime, timeouts) do
		// NOT scale. See README "time_scale".
		logger.Warn("time_scale active — TNC CSMA timing does not scale; hosts must scale their own protocol timers", "time_scale", cfg.TimeScale)
	}

	// Each backend is only required if at least one port uses it; defer
	// the strict check to router.Start, but warn now about any backend
	// this topology uses whose binary can't be found.
	used := map[tnc.Backend]bool{}
	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			b := tnc.Backend(p.TNC)
			if b == "" {
				b = tnc.BackendSamoyed
			}
			used[b] = true
		}
	}
	bins := map[tnc.Backend]string{}
	for b, explicit := range map[tnc.Backend]string{
		tnc.BackendSamoyed:  *samoyedPath,
		tnc.BackendDirewolf: *direwolfPath,
		tnc.BackendPdn:      *pdnPath,
	} {
		bin, err := tnc.ResolveBinary(b, explicit)
		if err != nil && used[b] {
			logger.Warn(tnc.BinaryName(b)+" not found; ports with tnc="+string(b)+" won't start", "err", err)
		}
		bins[b] = bin
	}
	samoyedBin, direwolfBin, pdnBin := bins[tnc.BackendSamoyed], bins[tnc.BackendDirewolf], bins[tnc.BackendPdn]

	wd, err := resolveWorkDir(*workDir)
	if err != nil {
		logger.Error("workdir", "err", err)
		os.Exit(1)
	}

	logger.Info("starting", "config", *cfgPath, "samoyed", samoyedBin, "direwolf", direwolfBin, "pdn", pdnBin, "workdir", wd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := router.Start(ctx, cfg, router.Options{
		SamoyedBin:    samoyedBin,
		DirewolfBin:   direwolfBin,
		PdnBin:        pdnBin,
		WorkDir:       wd,
		Verbose:       *verbose,
		Logger:        logger,
		RecordDir:     *recordDir,
		RecordOnStart: *recordDir != "",
		RTPriority:    *rtPriority,
	})
	if err != nil {
		logger.Error("start router", "err", err)
		os.Exit(1)
	}

	if len(compositePorts) > 0 {
		path, err := r.StartCompositeRecording(compositePorts)
		if err != nil {
			logger.Error("start composite recording", "err", err)
			_ = r.Stop()
			os.Exit(1)
		}
		logger.Info("composite recording", "file", path, "channels", *composite)
	}

	// Run until SIGINT/SIGTERM or a child dies (router cancels its own ctx).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		logger.Info("signal received, shutting down")
		cancel()
	}()

	// Wait for the router to stop, not for its goroutines: they only
	// finish once Stop closes the children's audio sockets and FIFOs.
	<-r.Done()
	if err := r.Stop(); err != nil {
		logger.Error("stop", "err", err)
	}
}

// parseCompositePorts turns a comma-separated "a.vhf,b.vhf" list into
// PortRefs. Empty input yields no refs (composite disabled). Order is
// preserved: the first port is channel 0 (left ear in the stereo case).
func parseCompositePorts(s string) ([]config.PortRef, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var refs []config.PortRef
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dot := strings.IndexByte(part, '.')
		if dot <= 0 || dot >= len(part)-1 {
			return nil, fmt.Errorf("invalid -composite port %q (want <node>.<port>)", part)
		}
		refs = append(refs, config.PortRef{NodeID: part[:dot], PortID: part[dot+1:]})
	}
	return refs, nil
}

func resolveWorkDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, os.MkdirAll(explicit, 0o755)
	}
	wd, err := os.MkdirTemp("", "sim-router-")
	if err != nil {
		return "", err
	}
	return wd, nil
}
