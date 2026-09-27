// sim-web is a tiny integrated web front-end for editing the simulator's
// network topology and controlling a sim-router instance.
//
//	sim-web -addr :8080 -config /opt/sim/network.yaml
//
// Bootstraps with a sensible default two-node network if -config doesn't
// exist yet. The YAML config is the source of truth; the web page just
// edits the file in place and offers Start / Stop / Restart for the
// embedded router.
//
// Stdlib + yaml.v3 only. No frameworks.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/events"
	"github.com/packethacking/net-sim/internal/router"
	"github.com/packethacking/net-sim/internal/tnc"
	"gopkg.in/yaml.v3"
)

//go:embed index.html map.html
var assets embed.FS

const defaultConfigYAML = `# Default two-node AFSK1200 network. Edit and click Apply.
mixer_mode: fm_capture
capture_db: 6.0
collision_mode: silence

nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8002

links:
  - { from: a.vhf, to: b.vhf, loss_db: 0 }
  - { from: b.vhf, to: a.vhf, loss_db: 0 }
`

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	cfgPath := flag.String("config", "network.yaml", "path to the network YAML")
	samoyed := flag.String("samoyed", "", "samoyed-direwolf binary (default: discover)")
	direwolf := flag.String("direwolf", "", "direwolf binary (default: discover)")
	pdn := flag.String("pdn", "", "pdn-soundmodem binary (default: discover)")
	workDir := flag.String("workdir", "", "scratch dir for per-port config files / FIFOs (default: temp)")
	autostart := flag.Bool("autostart", false, "start the router immediately on launch")
	recordDir := flag.String("record", "", "if set, enables the Record toggle and Composite recording panel in the UI; recordings land under this path")
	rtPriority := flag.Bool("rt-priority", false, "renice the router and its TNC children for jitter-free pacing (needs CAP_SYS_NICE)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := bootstrapConfig(*cfgPath); err != nil {
		logger.Error("bootstrap config", "err", err)
		os.Exit(1)
	}

	samoyedBin, _ := tnc.ResolveBinary(tnc.BackendSamoyed, *samoyed)
	direwolfBin, _ := tnc.ResolveBinary(tnc.BackendDirewolf, *direwolf)
	pdnBin, _ := tnc.ResolveBinary(tnc.BackendPdn, *pdn)
	if samoyedBin == "" {
		logger.Warn("samoyed-direwolf not found; ports with tnc=samoyed (default) won't start")
	}
	if direwolfBin == "" {
		logger.Warn("direwolf not found; ports with tnc=direwolf won't start")
	}
	if pdnBin == "" {
		logger.Info("pdn-soundmodem not found; ports with tnc=pdn won't start")
	}

	app := &app{
		cfgPath:     *cfgPath,
		samoyedBin:  samoyedBin,
		direwolfBin: direwolfBin,
		pdnBin:      pdnBin,
		workDir:     *workDir,
		recordBase:  *recordDir,
		rtPriority:  *rtPriority,
		logger:      logger,
		eventBus:    events.NewBus(),
		audioTap:    audio.NewTap(),
		observer:    router.NewObserver(),
	}

	tmpl, err := template.ParseFS(assets, "index.html")
	if err != nil {
		logger.Error("parse template", "err", err)
		os.Exit(1)
	}
	app.tmpl = tmpl

	mapTmpl, err := template.ParseFS(assets, "map.html")
	if err != nil {
		logger.Error("parse map template", "err", err)
		os.Exit(1)
	}
	app.mapTmpl = mapTmpl

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/map", app.handleMap)
	mux.HandleFunc("/api/events", app.handleEvents)
	mux.HandleFunc("/api/audio", app.handleAudio)
	mux.HandleFunc("/api/observed", app.handleObserved)
	mux.HandleFunc("/api/peer/", app.handlePeerProxy)
	mux.HandleFunc("/api/config", app.handleConfig)
	mux.HandleFunc("/api/topology", app.handleTopology)
	mux.HandleFunc("/api/stats", app.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/start", app.handleStart)
	mux.HandleFunc("/api/stop", app.handleStop)
	mux.HandleFunc("/api/restart", app.handleRestart)
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/record/start", app.handleRecordStart)
	mux.HandleFunc("/api/record/stop", app.handleRecordStop)
	mux.HandleFunc("/api/record/status", app.handleRecordStatus)
	mux.HandleFunc("/api/record/composite/start", app.handleCompositeStart)
	mux.HandleFunc("/api/record/composite/stop", app.handleCompositeStop)
	mux.HandleFunc("/api/record/composite/status", app.handleCompositeStatus)
	mux.HandleFunc("/api/record/composite/download", app.handleCompositeDownload)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if *autostart {
		if err := app.start(); err != nil {
			logger.Warn("autostart failed", "err", err)
		}
	}

	go func() {
		logger.Info("sim-web listening", "addr", *addr, "config", *cfgPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("signal received, shutting down")
	_ = app.stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

type app struct {
	cfgPath     string
	samoyedBin  string
	direwolfBin string
	pdnBin      string
	workDir     string
	recordBase  string // -record DIR; "" means feature disabled
	rtPriority  bool   // -rt-priority: renice router + TNC children at start
	logger      *slog.Logger
	tmpl        *template.Template
	mapTmpl     *template.Template

	eventBus *events.Bus
	audioTap *audio.Tap
	observer *router.Observer

	mu       sync.Mutex
	router   *router.Router
	cancel   context.CancelFunc
	lastErr  string
	lastTime time.Time

	// recordEnabled is the user's "record" toggle state. It persists
	// across router Start/Stop/Restart cycles, so a user who turned
	// recording on and then clicked Apply&Restart gets a fresh session
	// in the new run rather than having to re-toggle.
	recordEnabled bool

	// lastComposite is the path of the most recently *completed* composite
	// recording (set when StopCompositeRecording returns). The download
	// endpoint serves this file; the in-progress file isn't downloadable
	// until stopped (its WAV header isn't patched yet).
	lastComposite string
}

type pageData struct {
	ConfigPath string
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.Execute(w, pageData{ConfigPath: a.cfgPath}); err != nil {
		a.logger.Error("render", "err", err)
	}
}

// handleMap serves the live visualisation page. The page bootstraps from
// /api/topology for the static graph and then opens an EventSource on
// /api/events for the live activity overlay.
func (a *app) handleMap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.mapTmpl.Execute(w, pageData{ConfigPath: a.cfgPath}); err != nil {
		a.logger.Error("render map", "err", err)
	}
}

// handleObserved exposes what callsigns each port has been observed
// transmitting under, plus a per-station summary picking out the
// "routing call" (whichever call broadcasts NODES most often). The
// visualiser uses this to label stations with their real on-air
// callsign(s) instead of relying on a regex heuristic against the
// YAML node id.
func (a *app) handleObserved(w http.ResponseWriter, _ *http.Request) {
	resp := struct {
		Ports    map[string]router.PortObservations `json:"ports"`
		Stations map[string]router.StationSummary   `json:"stations"`
	}{
		Ports:    a.observer.Snapshot(),
		Stations: a.observer.StationSummaries(),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleStats is a cheap polling endpoint for the live-map HUD.
//
// Returns:
//   - sim_cpu_pct  — aggregate CPU% across all containers attached to
//     any of this container's Docker networks (sum, scaled by online
//     CPUs). NaN unless /var/run/docker.sock is mounted in.
//   - host_cpu_pct — host-wide CPU% from /proc/stat. NaN on non-Linux.
//
// The visualiser prefers sim_cpu_pct and falls back to host if the
// daemon socket isn't reachable.
func (a *app) handleStats(w http.ResponseWriter, _ *http.Request) {
	// JSON can't encode NaN — use *float64 with nil so the client sees
	// `"sim_cpu_pct": null` when the metric is unknown (e.g. docker
	// socket not mounted, or no /proc/stat).
	finite := func(f float64) *float64 {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return &f
	}
	resp := struct {
		SimCPUPct  *float64 `json:"sim_cpu_pct"`
		HostCPUPct *float64 `json:"host_cpu_pct"`
	}{
		SimCPUPct:  finite(currentSimCPUPct()),
		HostCPUPct: finite(currentCPUPct()),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleEvents streams the router's event bus to the client as
// Server-Sent Events. Each event is one JSON object on a single SSE
// "data:" line. The connection stays open until the client disconnects,
// the server shuts down, or the bus is closed.
//
// Heartbeat comments (": ping\n\n") fire every 15 s so intermediate
// proxies and idle-detection don't kill the stream during quiet periods.
func (a *app) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering when proxied
	w.WriteHeader(http.StatusOK)

	ch, cancel := a.eventBus.Subscribe()
	defer cancel()

	// Send an initial comment so EventSource's onopen fires immediately
	// even before any traffic flows.
	_, _ = w.Write([]byte(": hello\n\n"))
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			// enc.Encode already wrote a newline; SSE needs a *blank* line
			// as the message terminator.
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleConfig: GET returns current YAML (text/plain), PUT writes it after
// validation. We round-trip through config.Parse so a syntactically-bad
// or semantically-invalid file is rejected before it's saved.
func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(a.cfgPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(b)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := config.ParseBytes(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Atomic-write so a failed save doesn't leave a half-written file.
		tmp := a.cfgPath + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmp, a.cfgPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// handleTopology is the same content as handleConfig but expressed as
// JSON, for the form-based editor. GET parses the YAML on disk into a
// generic map so the client doesn't need to know our struct layout. PUT
// re-marshals to YAML, runs the strict validator, and saves.
//
// The two endpoints share a file (a.cfgPath) — Raw YAML edits and Form
// edits both end up in the same place. Comments are preserved when
// editing via /api/config (text in, text out) but lost when editing
// via /api/topology (round-trips through map → YAML).
func (a *app) handleTopology(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(a.cfgPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var raw any
		if err := yaml.Unmarshal(b, &raw); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(raw)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var raw any
		if err := json.Unmarshal(body, &raw); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		yamlBytes, err := yaml.Marshal(raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := config.ParseBytes(yamlBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tmp := a.cfgPath + ".tmp"
		if err := os.WriteFile(tmp, yamlBytes, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmp, a.cfgPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if err := a.start(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeStatus(w)
}

func (a *app) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if err := a.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeStatus(w)
}

func (a *app) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	_ = a.stop()
	if err := a.start(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeStatus(w)
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.writeStatus(w)
}

type statusResponse struct {
	Running   bool             `json:"running"`
	Ports     []portStatusItem `json:"ports"`
	LastError string           `json:"last_error,omitempty"`
	LastEvent string           `json:"last_event,omitempty"`
	Record    recordStatus     `json:"record"`
	Composite compositeStatus  `json:"composite"`
}

// compositeStatus is the JSON view of the composite (multi-channel TX
// timeline) recorder. "channels" is the ordered port list — index 0 is the
// left ear in the stereo case. It is also returned standalone from
// /api/record/composite/{start,stop,status}.
type compositeStatus struct {
	Available   bool     `json:"available"`              // -record DIR was set
	Active      bool     `json:"active"`                 // a recording is in progress
	Channels    []string `json:"channels,omitempty"`     // ordered port refs, one per WAV channel
	Path        string   `json:"path,omitempty"`         // file currently being written
	DurationS   float64  `json:"duration_s,omitempty"`   // seconds captured so far
	Error       string   `json:"error,omitempty"`        // write error, if the stream faulted
	LastPath    string   `json:"last_path,omitempty"`    // most recent completed file
	DownloadURL string   `json:"download_url,omitempty"` // present when a completed file exists
}

type recordStatus struct {
	Available bool   `json:"available"`      // -record DIR was set on the CLI
	Enabled   bool   `json:"enabled"`        // user has toggled recording on
	Active    bool   `json:"active"`         // a session is currently capturing
	Base      string `json:"base,omitempty"` // configured base dir
	Dir       string `json:"dir,omitempty"`  // current session dir, when active
}

type portStatusItem struct {
	Node     string `json:"node"`
	Port     string `json:"port"`
	Mode     string `json:"mode"`
	KissPort int    `json:"kiss_port"`
}

func (a *app) writeStatus(w http.ResponseWriter) {
	a.mu.Lock()
	defer a.mu.Unlock()

	resp := statusResponse{
		Running:   a.router != nil,
		LastError: a.lastErr,
	}
	if !a.lastTime.IsZero() {
		resp.LastEvent = a.lastTime.Format(time.RFC3339)
	}

	if a.router != nil {
		// Re-read config so the port list reflects what's currently running.
		if cfg, err := config.Load(a.cfgPath); err == nil {
			for _, n := range cfg.Nodes {
				for _, p := range n.Ports {
					resp.Ports = append(resp.Ports, portStatusItem{
						Node:     n.ID,
						Port:     p.ID,
						Mode:     string(p.Modem.Mode),
						KissPort: p.KissPort,
					})
				}
			}
		}
	}

	resp.Record = recordStatus{
		Available: a.recordBase != "",
		Enabled:   a.recordEnabled,
		Base:      a.recordBase,
	}
	if a.router != nil {
		resp.Record.Active = a.router.RecordingActive()
		resp.Record.Dir = a.router.RecordingDir()
	}

	resp.Composite = a.compositeStatusLocked()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// compositeStatusLocked builds the composite-recorder status. The caller
// must hold a.mu.
func (a *app) compositeStatusLocked() compositeStatus {
	cs := compositeStatus{Available: a.recordBase != ""}
	if a.router != nil {
		rs := a.router.CompositeStatus()
		cs.Active = rs.Active
		cs.Path = rs.Path
		cs.Error = rs.Err
		cs.DurationS = rs.Duration.Seconds()
		for _, ref := range rs.Channels {
			cs.Channels = append(cs.Channels, ref.String())
		}
	}
	if a.lastComposite != "" {
		cs.LastPath = a.lastComposite
		cs.DownloadURL = "/api/record/composite/download"
	}
	return cs
}

func (a *app) handleRecordStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	if a.recordBase == "" {
		a.mu.Unlock()
		http.Error(w, "recording disabled: sim-web was started without -record DIR", http.StatusBadRequest)
		return
	}
	a.recordEnabled = true
	rt := a.router
	a.mu.Unlock()
	if rt != nil {
		if _, err := rt.StartRecording(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	a.writeStatus(w)
}

func (a *app) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	a.recordEnabled = false
	rt := a.router
	a.mu.Unlock()
	if rt != nil {
		if err := rt.StopRecording(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	a.writeStatus(w)
}

func (a *app) handleRecordStatus(w http.ResponseWriter, _ *http.Request) {
	a.writeStatus(w)
}

// compositeStartReq is the optional JSON body for /api/record/composite/start.
// ports is an ordered list of "<node>.<port>" refs — one WAV channel each,
// index 0 = left ear. Omit it (or send an empty body) to default to the
// topology's first two ports, i.e. a stereo "one station per ear" file.
type compositeStartReq struct {
	Ports []string `json:"ports"`
}

// handleCompositeStart begins a composite (multi-channel TX timeline)
// recording. POST, optional JSON body (see compositeStartReq).
func (a *app) handleCompositeStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req compositeStartReq
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16)); err == nil && len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	refs, err := parsePortRefs(req.Ports)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	if a.recordBase == "" {
		a.mu.Unlock()
		http.Error(w, "recording disabled: sim-web was started without -record DIR", http.StatusBadRequest)
		return
	}
	rt := a.router
	a.mu.Unlock()
	if rt == nil {
		http.Error(w, "router not running: Start the simulator first", http.StatusBadRequest)
		return
	}
	if _, err := rt.StartCompositeRecording(refs); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeCompositeStatus(w)
}

// handleCompositeStop finalises the current composite recording and reports
// the resulting file (now downloadable). POST.
func (a *app) handleCompositeStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	rt := a.router
	a.mu.Unlock()
	if rt != nil {
		res, err := rt.StopCompositeRecording()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if res.Path != "" {
			a.mu.Lock()
			a.lastComposite = res.Path
			a.mu.Unlock()
		}
	}
	a.writeCompositeStatus(w)
}

func (a *app) handleCompositeStatus(w http.ResponseWriter, _ *http.Request) {
	a.writeCompositeStatus(w)
}

func (a *app) writeCompositeStatus(w http.ResponseWriter) {
	a.mu.Lock()
	cs := a.compositeStatusLocked()
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(cs)
}

// handleCompositeDownload serves the most recently completed composite WAV
// as a file attachment. The in-progress file isn't offered because its WAV
// header isn't patched until the recording is stopped.
func (a *app) handleCompositeDownload(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	path := a.lastComposite
	a.mu.Unlock()
	if path == "" {
		http.Error(w, "no completed composite recording yet — start one, transmit, then stop it", http.StatusNotFound)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "composite file missing: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

// parsePortRefs turns "<node>.<port>" strings into PortRefs. An empty list
// is allowed (the router defaults it to the first two ports).
func parsePortRefs(ss []string) ([]config.PortRef, error) {
	var refs []config.PortRef
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		dot := strings.IndexByte(s, '.')
		if dot <= 0 || dot >= len(s)-1 {
			return nil, fmt.Errorf("invalid port ref %q (want <node>.<port>)", s)
		}
		refs = append(refs, config.PortRef{NodeID: s[:dot], PortID: s[dot+1:]})
	}
	return refs, nil
}

func (a *app) start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.router != nil {
		return errors.New("already running")
	}
	for b, bin := range map[tnc.Backend]*string{
		tnc.BackendSamoyed:  &a.samoyedBin,
		tnc.BackendDirewolf: &a.direwolfBin,
		tnc.BackendPdn:      &a.pdnBin,
	} {
		if *bin == "" {
			*bin, _ = tnc.ResolveBinary(b, "")
		}
	}
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		a.lastErr = err.Error()
		a.lastTime = time.Now()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, err := router.Start(ctx, cfg, router.Options{
		SamoyedBin:    a.samoyedBin,
		DirewolfBin:   a.direwolfBin,
		PdnBin:        a.pdnBin,
		WorkDir:       a.workDir,
		Logger:        a.logger,
		RecordDir:     a.recordBase,
		RecordOnStart: a.recordEnabled && a.recordBase != "",
		EventBus:      a.eventBus,
		AudioTap:      a.audioTap,
		Observer:      a.observer,
		RTPriority:    a.rtPriority,
	})
	if err != nil {
		cancel()
		a.lastErr = err.Error()
		a.lastTime = time.Now()
		return err
	}
	a.router = r
	a.cancel = cancel
	a.lastErr = ""
	a.lastTime = time.Now()
	go a.watchRouter(r)
	return nil
}

// watchRouter notices a router that stopped by itself (a TNC child
// exited) and clears it, so /api/status stops reporting running:true.
func (a *app) watchRouter(r *router.Router) {
	<-r.Done()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.router != r {
		return // stopped deliberately via stop()
	}
	_ = r.Stop()
	a.router = nil
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.lastErr = "simulator stopped: a TNC process exited (see the log)"
	a.lastTime = time.Now()
}

func (a *app) stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.router == nil {
		return nil
	}
	err := a.router.Stop()
	a.router = nil
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.lastTime = time.Now()
	return err
}

func bootstrapConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(defaultConfigYAML), 0o644)
}
