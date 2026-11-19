// Command llm-scheduler is the cluster scheduler (see docs/design.md).
//
// Usage:
//
//	llm-scheduler [--config <path>] [--check-config] [--version]
//
// It logs to stderr and exits 0 after a clean shutdown on SIGTERM or
// interrupt (design 8.6), and 1 when the shutdown overran its grace and
// children had to be killed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/docs"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/gguf"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/mcpserver"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

const (
	defaultConfig = "/etc/llm-scheduler/config.yaml"
	// Children listen on 127.0.0.1 in this range (design 7.4 example: 5801).
	childPortLo, childPortHi = 5800, 5999
	// apiGrace is how long the HTTP server may take to close its remaining
	// connections once the scheduler has finished shutting down.
	apiGrace = 5 * time.Second
	// shutdownSlack is added to shutdown_grace for the scheduler's own
	// SIGTERM-to-SIGKILL escalation before the shutdown context gives up.
	shutdownSlack = 30 * time.Second
)

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("llm-scheduler", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig, "path to the YAML config file")
	check := fs.Bool("check-config", false, "validate the config, print any errors, and exit 0 (valid) or 1")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "llm-scheduler", version)
		return 0
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		fmt.Fprintf(stdout, "config ok: %d profiles\n", len(cfg.Profiles))
		return 0
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	return serve(cfg, logger)
}

// warnSlidingWindow is the design section 6 startup warning: a kind: llama
// profile with no needs_mib whose model declares a sliding window gets an
// estimate that is several times too high.
func warnSlidingWindow(cfg *config.Config, log *slog.Logger) {
	for _, p := range cfg.ProfilesWithoutNeedsMiB() {
		path := filepath.Join(cfg.ModelsDir, p.Model)
		meta, err := gguf.ReadMeta(path)
		if err != nil {
			log.Warn("cannot read GGUF metadata; the memory estimate will fail for this profile", "profile", p.Name, "model", path, "err", err)
			continue
		}
		if meta.SlidingWindow {
			log.Warn("profile has no needs_mib and its model uses sliding-window attention; the memory estimate is several times too high, so measure the model once and set needs_mib (design 7.2, runbook)",
				"profile", p.Name, "model", path)
		}
	}
}

// checkFleet logs a clear error when the fleet console cannot be reached at
// startup. The scheduler keeps running: the local node still works and the
// fleet client keeps retrying.
func checkFleet(ctx context.Context, cfg *config.Config, log *slog.Logger) {
	if cfg.Fleet.URL == "" {
		log.Error("no fleet.url configured: only the local node can be used")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, cfg.Fleet.URL+"/api/fleet", nil)
	if err == nil {
		var resp *http.Response
		if resp, err = http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("HTTP %s", resp.Status)
		}
	}
	if ctx.Err() != nil {
		return
	}
	log.Error("cannot reach the fleet console; remote nodes are unavailable until it answers (check the host firewall rule for port 8090). Still running: the local node works and the connection is retried",
		"url", cfg.Fleet.URL, "err", err)
}

// exitCodeAfterShutdown keeps an earlier failure code; a shutdown that ran
// out of grace (so children had to be killed) is not a clean stop either:
// it is logged and makes the exit code 1.
func exitCodeAfterShutdown(code int, shutdownErr error, log *slog.Logger) int {
	if shutdownErr == nil {
		return code
	}
	log.Error("shutdown did not finish within its grace; children were killed", "err", shutdownErr)
	return 1
}

func serve(cfg *config.Config, log *slog.Logger) int {
	warnSlidingWindow(cfg, log)
	if len(cfg.ExcludeNodes) > 0 {
		log.Info("exclude_nodes configured: these nodes are never used", "entries", strings.Join(cfg.ExcludeNodes, ", "))
	}

	ports, err := instance.NewPortAllocator(childPortLo, childPortHi)
	if err != nil {
		log.Error("port allocator", "err", err)
		return 1
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.Listen, "err", err)
		return 1
	}

	bg, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	fc := fleet.New(cfg, fleet.Options{Logger: log})
	fleetDone := make(chan struct{})
	go func() { fc.Run(bg); close(fleetDone) }()
	go checkFleet(bg, cfg, log)

	launcher := &instance.ProcessLauncher{Ports: ports, LogDir: cfg.ChildLogDir, LogKeep: cfg.ChildLogKeep, Logger: log}
	if err := launcher.PrepareLogDir(); err != nil {
		log.Warn("child_log_dir is not usable; children run without full log files", "dir", cfg.ChildLogDir, "err", err)
		launcher.LogDir = ""
	} else if launcher.LogDir != "" {
		log.Info("keeping each child's full output", "dir", launcher.LogDir, "keep", cfg.ChildLogKeep)
	}

	// The event history: opened before the scheduler so that its first events are kept. It
	// never fails and never blocks scheduling (a bad directory only warns).
	hist := history.Open(history.Options{Dir: cfg.HistoryDir, MaxFileBytes: int64(cfg.HistoryMaxFileMiB) << 20,
		KeepFiles: cfg.HistoryKeepFiles, MemoryEvents: cfg.HistoryMemoryEvents, Logger: log})
	defer hist.Close()
	hist.Record(startedEvent(cfg, version, time.Now()))

	so := scheduler.Options{
		View:     fc,
		Prober:   &instance.ExecProber{ServerBin: cfg.Defaults.ServerBin},
		Launcher: launcher,
		Logger:   log,
	}
	if hist.On() {
		so.History = hist
	}
	sched, err := scheduler.New(cfg, so)
	if err != nil {
		log.Error("scheduler", "err", err)
		ln.Close()
		return 1
	}
	schedDone := make(chan struct{})
	go func() { sched.Run(bg); close(schedDone) }()

	benchStore, benchMgr := openBench(cfg, ln, hist, log)
	if benchStore != nil {
		defer benchStore.Close()
	}
	ao := api.Options{Logger: log, History: hist, Version: version, WSMaxClients: cfg.WSMaxClients, Bench: benchMgr, BenchStore: benchStore,
		System: api.SystemInfo{Listen: cfg.Listen, Safety: safetySummary(cfg), HistoryDir: cfg.HistoryDir, DataDir: cfg.DataDir,
			ChildLogDir: cfg.ChildLogDir, ModelsDir: cfg.ModelsDir, Bench: map[string]any{"enabled": benchMgr != nil, "regress_load_pct": cfg.Bench.RegressLoadPct,
				"regress_decode_pct": cfg.Bench.RegressDecodePct, "max_run_minutes": cfg.Bench.MaxRunMinutes}}}
	apiSrv := api.New(cfg, sched, ao)
	// MCP at /mcp on the same port; its tools run in-process through apiSrv (design 9.5).
	mcpSrv := mcpserver.New(apiSrv, mcpserver.Options{Version: version, Guide: docs.BenchmarkGuide})
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // not a ServeMux: it would clean paths the API serves verbatim
		if r.URL.Path == "/mcp" {
			mcpSrv.ServeHTTP(w, r)
			return
		}
		apiSrv.ServeHTTP(w, r)
	})
	httpSrv := &http.Server{Handler: root, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	if benchMgr != nil {
		benchMgr.Start(bg)
	}
	log.Info("llm-scheduler started", "version", version, "listen", ln.Addr().String(), "profiles", len(cfg.Profiles))

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	code := 0
	select {
	case s := <-sigs:
		log.Info("shutting down", "signal", s.String())
		hist.Record(stoppingEvent(s.String(), time.Now()))
	case err := <-serveErr:
		log.Error("HTTP server failed", "err", err)
		hist.Record(stoppingEvent("HTTP server failed: "+err.Error(), time.Now()))
		code = 1
	}

	// Design 8.6: stop accepting, then let the scheduler answer waiters,
	// wait for in-flight requests up to shutdown_grace, and stop children.
	// A second signal kills everything at once.
	ln.Close()
	grace := cfg.Defaults.ShutdownGrace + shutdownSlack
	sctx, scancel := context.WithTimeout(context.Background(), grace)
	defer scancel()
	go func() {
		select {
		case <-sigs:
			log.Warn("second signal: stopping everything at once")
			scancel()
		case <-sctx.Done():
		}
	}()
	if benchMgr != nil { // a running benchmark is cancelled and unloads what it loaded while the API still answers
		bmctx, bmcancel := context.WithTimeout(sctx, 60*time.Second)
		benchMgr.Stop(bmctx)
		bmcancel()
	}
	code = exitCodeAfterShutdown(code, apiSrv.Shutdown(sctx), log)
	hctx, hcancel := context.WithTimeout(context.Background(), apiGrace)
	defer hcancel()
	if err := httpSrv.Shutdown(hctx); err != nil {
		httpSrv.Close()
	}
	bgCancel()
	<-schedDone
	<-fleetDone
	log.Info("stopped")
	return code
}
