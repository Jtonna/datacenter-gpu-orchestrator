// Command llm-scheduler is the cluster scheduler (see docs/architecture.md).
//
// Usage:
//
//	llm-scheduler [--listen ADDR] [--data-dir DIR] [--models-dir DIR] [--history-dir DIR] [--child-log-dir DIR] [--programs-dir DIR] [--version]
//	llm-scheduler import-config --data-dir DIR [--replace] [--check] file.yaml
//
// Everything except those deployment paths is configuration kept in the
// database in data-dir and changed through the REST API, MCP or the web UI.
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/docs"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.10"

// potluckRelease is the program release Potluck serves to agents and
// requires of them (agent contract 5.1), separate from version. Set it at
// build time with -ldflags "-X main.potluckRelease=...".
var potluckRelease = agentproto.Release

// Test-only overrides of the agent contract's fixed ports, so an end-to-end
// test can run several fake agents on one machine. Never set in production.
const (
	envTestAgentPort  = "LLM_SCHEDULER_TEST_AGENT_PORT"
	envTestWorkerPort = "LLM_SCHEDULER_TEST_WORKER_PORT"
)

const (
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
	if len(args) > 0 && args[0] == "import-config" {
		return importConfig(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("llm-scheduler", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bf := bindBootstrap(fs)
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
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	return serve(bf.value(), logger)
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
// fleet client keeps retrying. LEGACY: fleet membership mode only, kept
// until phase 7.
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

func serve(boot config.Bootstrap, log *slog.Logger) int {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	return serveUntil(boot, log, sigs)
}

// serveUntil runs the scheduler until the first value on sigs (a second one
// stops everything at once).
// shutdownStep, when set (tests), is told each step of the shutdown that
// matters for the database, in order.
var shutdownStep func(step string, st *bench.Store)

func stepDone(step string, st *bench.Store) {
	if f := shutdownStep; f != nil {
		f(step, st)
	}
}

func serveUntil(boot config.Bootstrap, log *slog.Logger, sigs <-chan os.Signal) int {
	fatal := newFatalPath()
	defer fatal.disarm() // the first defer, so the last to run: main exits right after
	// CR-10: Potluck's own lines also go to the log store (source scheduler),
	// through a RAM ring and one background sender; the printed log stays as
	// it is. The lines from here on are kept until the sender starts and
	// learns whether logging is on (selfLog, logs.go).
	var relayAt atomic.Pointer[logrelay.Relay]
	var cfgAt atomic.Pointer[config.Config]
	self := newSelfLog(relayAt.Load, cfgAt.Load)
	log = slog.New(self.Handler(log.Handler()))
	// Background-task isolation: a recovered failure is logged (and so
	// shipped with Potluck's own lines), recorded in the history and counted;
	// one that must not continue ends the process through the fatal path.
	var histAt atomic.Pointer[history.Store]
	supervise.Configure(supervise.Options{Logger: log, Fatal: fatal.trigger,
		Record: func(f supervise.Failure) {
			if h := histAt.Load(); h != nil {
				h.Record(failureEvent(f))
			}
		}})
	// The database holds the configuration. A first start creates it. One
	// that exists but cannot be used (a newer version migrated it, it is
	// damaged) never runs as built-in defaults with no profiles, healthy and
	// empty: the scheduler stays up unhealthy with the reason (unhealthy.go).
	var dbStore *bench.Store
	var cs *cfgstore.Store
	st, err := bench.OpenStore(boot.DataDir)
	if err != nil {
		return serveUnhealthy(boot, log, sigs, storeFailure(err))
	}
	dbStore = st
	defer func() { dbStore.Close(); stepDone("store closed", dbStore) }() // the last step: every user of the store has stopped
	if cs, err = cfgstore.Open(st); err != nil {
		return serveUnhealthy(boot, log, sigs, "the configuration tables cannot be opened: "+err.Error())
	}
	var (
		sched   *scheduler.Scheduler
		fc      *fleet.Client
		pl      *pool.Pool
		apiSrv  *api.Server
		started atomic.Bool // set once the pieces above exist; guards the service callbacks
		// What the log routes read without waiting for anything (CR-10): the
		// configuration in force and the pool, each behind an atomic pointer.
		curCfg atomic.Pointer[config.Config]
		curPl  atomic.Pointer[pool.Pool]
	)
	so0 := cfgsvc.Options{Store: cs, Boot: boot, Logger: log,
		ProfileRefs: func(id string) []string { return benchmarksUsing(dbStore, id) },
		Apply: func(c *config.Config) {
			curCfg.Store(c)
			cfgAt.Store(c)
			if !started.Load() {
				return
			}
			if fc != nil {
				fc.SetConfig(c)
			}
			if pl != nil {
				pl.Refresh()
			}
			sched.ApplyConfig(c)
		},
		OnChange: func(ev cfgsvc.ChangeEvent) {
			if started.Load() {
				apiSrv.PublishConfig(ev)
			}
		},
		// Instances left running on nodes a write excludes (reported, not drained).
		LoadedOnExcluded: func(c *config.Config) []config.LoadedOnExcluded {
			if !started.Load() {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return sched.LoadedOnExcluded(ctx, c)
		}}
	svc := cfgsvc.New(context.Background(), so0)
	if e := svc.LoadError(); e != "" {
		// the stored configuration exists but cannot be used: not empty and healthy either
		return serveUnhealthy(boot, log, sigs, e)
	}
	cfg := svc.Config()
	curCfg.CompareAndSwap(nil, cfg)
	cfgAt.Store(cfg)
	logRelay := newLogRelay(curCfg.Load, curPl.Load, log)
	relayAt.Store(logRelay)
	if cs != nil {
		if n, err := cs.PruneAudit(cfg.AuditRetentionDays); err != nil {
			log.Warn("audit retention", "err", err)
		} else if n > 0 {
			log.Info("audit entries older than audit_retention_days were removed", "removed", n)
		}
	}
	if !cfg.Configured {
		log.Warn("no configuration yet: no profiles are defined and no node matches a tier, so nothing can be loaded; import one with `llm-scheduler import-config` or the REST API (PUT /api/config/...)", "data_dir", boot.DataDir, "error", svc.LoadError())
	}
	warnSlidingWindow(cfg, log)
	if len(cfg.ExcludeNodes) > 0 {
		log.Info("exclude_nodes configured: these nodes are never used", "entries", strings.Join(cfg.ExcludeNodes, ", "))
	}
	if len(cfg.NoAutoNodes) > 0 {
		log.Info("no_auto_nodes configured: only profiles that force these nodes use them", "entries", strings.Join(cfg.NoAutoNodes, ", "))
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
	poolDone := make(chan struct{}) // closed when the pool's loop has returned (agents membership)
	defer bgCancel()
	self.SetLogger(log)
	go supervise.Once("selflog", func() { self.Run(bg) }, nil) // its loops are supervised inside
	if logRelay != nil {                                       // warns when the store's retention or disk cap differ from the settings
		supervise.Go(bg, supervise.Spec{Name: "logrelay.watch_store", Policy: supervise.Restart}, logRelay.WatchStore)
	}
	launcher := &instance.ProcessLauncher{Ports: ports, LogDir: cfg.ChildLogDir, LogKeep: cfg.ChildLogKeep, Logger: log,
		Tap: func(profile string, id uint64) instance.ChildTap { return self.ChildTap(profile, id) }}
	if err := launcher.PrepareLogDir(); err != nil {
		log.Warn("child_log_dir is not usable; children run without full log files", "dir", cfg.ChildLogDir, "err", err)
		launcher.LogDir = ""
	} else if launcher.LogDir != "" {
		log.Info("keeping each child's full output", "dir", launcher.LogDir, "keep", cfg.ChildLogKeep)
	}

	// The node view: the agents' registrations (membership agents), or the
	// fleet feed. With agents nothing connects to a fleet console and no log
	// line names one.
	var view fleet.View
	fleetDone := make(chan struct{})
	if cfg.AgentsMode() {
		close(fleetDone)
		pl, err = newPool(bg, svc, cs, dbStore, launcher.LogDir, logRelay, log)
		if err != nil {
			log.Error("agent membership", "err", err)
			ln.Close()
			return 1
		}
		curPl.Store(pl)
		view = pl
		// The pool's loop runs CheckLost, which is NOT restarted after a
		// failure: it changes the agents and launches tables that the
		// scheduler core's node claims mirror, and a half-applied pass
		// leaves the two disagreeing about which node holds what, with
		// nothing in the process to bring them back into agreement. Policy
		// Exit (the fatal path). The local GPU probe it also runs is guarded.
		supervise.Go(bg, supervise.Spec{Name: "pool.check_lost", Policy: supervise.Exit, Done: func() { close(poolDone) }}, pl.Run)
	} else {
		fc = fleet.New(cfg, fleet.Options{Logger: log})
		go supervise.Once("fleet.run", func() { defer close(fleetDone); fc.Run(bg) }, nil) // its loops are supervised inside
		go supervise.Once("fleet.check", func() { checkFleet(bg, cfg, log) }, nil)
		view = fc
	}

	// The event history: opened before the scheduler so that its first events are kept. It
	// never fails and never blocks scheduling (a bad directory only warns).
	hist := history.Open(history.Options{Dir: cfg.HistoryDir, MaxFileBytes: int64(cfg.HistoryMaxFileMiB) << 20,
		KeepFiles: cfg.HistoryKeepFiles, MemoryEvents: cfg.HistoryMemoryEvents, Logger: log})
	defer hist.Close()
	histAt.Store(hist)
	hist.Record(startedEvent(cfg, version, time.Now()))

	so := scheduler.Options{
		View:     view,
		Prober:   &instance.ExecProber{ServerBin: cfg.Defaults.ServerBin},
		Launcher: launcher,
		Logger:   log,
		Release:  potluckRelease,
	}
	// The llama build of the host program: stored with each learned need. A
	// stored need is trusted only when its build is this one and the one the
	// served release's release.json names (the workers'); otherwise, or when
	// either is not known, it only raises the estimate.
	if b, err := release.HostBuild(bg, cfg.Defaults.ServerBin); err == nil {
		so.LlamaBuild = b
	} else {
		log.Warn("the llama build of the host program is not known; stored memory needs only raise the estimates", "err", err)
	}
	// The llama build a release shipped, from its release.json in the
	// programs folder: the served release's is what the workers run, and a
	// stored need measured before builds were recorded is kept trusted
	// across a release bump that kept the build.
	so.ReleaseBuild = func(rel string) string {
		b, _ := release.LlamaBuildOf(cfg.ProgramsDir, rel)
		return b
	}
	// When a build is not known yet, the scheduler reads both again in the
	// background until they are (a slow share at boot).
	so.HostBuild = func(ctx context.Context) (string, error) { return release.HostBuild(ctx, cfg.Defaults.ServerBin) }
	if hist.On() {
		so.History = hist
	}
	// Learned memory needs live in the scheduler database, beside the bench
	// and configuration tables (design 7.2a); without it they last for this
	// process only.
	if dbStore != nil {
		if ns, err := needstore.Open(dbStore); err != nil {
			log.Warn("learned memory needs are kept in memory only", "err", err)
		} else {
			so.NeedStore = ns
		}
	}
	if pl != nil {
		so.Workers, so.LoadGate = pl, pl.LoadGate
		if hist.On() {
			pl.SetHistory(hist)
		}
	}
	sched, err = scheduler.New(cfg, so)
	if err != nil {
		log.Error("scheduler", "err", err)
		ln.Close()
		return 1
	}
	schedDone := make(chan struct{})
	// The scheduler's single event loop is NOT restarted after a failure: it
	// owns the instances, waiters, leases, node claims and memory
	// reservations, mutated in place with no store to rebuild them from, so a
	// panic half way leaves them inconsistent (over-committed node memory, a
	// waiter never answered). Policy Exit: the fatal path ends the process and
	// the container starts it again from the database. Only the foreign code
	// the loop calls (history and bench subscribers) is guarded.
	supervise.Go(bg, supervise.Spec{Name: "scheduler.loop", Policy: supervise.Exit, Done: func() { close(schedDone) }},
		func(ctx context.Context) { sched.Run(ctx) })
	if pl != nil {
		pl.SetForceUnload(sched.ForceUnloadNode)
	}

	benchStore, benchMgr := openBench(svc, dbStore, ln, hist, log)
	ao := api.Options{Logger: log, History: hist, Version: version, WSMaxClients: cfg.WSMaxClients, Bench: benchMgr, BenchStore: benchStore, Config: svc,
		System: api.SystemInfo{Listen: cfg.Listen, Safety: safetySummary(cfg), HistoryDir: cfg.HistoryDir, DataDir: cfg.DataDir,
			ChildLogDir: cfg.ChildLogDir, ModelsDir: cfg.ModelsDir, Bench: map[string]any{"enabled": benchMgr != nil, "regress_load_pct": cfg.Bench.RegressLoadPct,
				"regress_decode_pct": cfg.Bench.RegressDecodePct, "max_run_minutes": cfg.Bench.MaxRunMinutes}}}
	if cs != nil {
		ao.Audit = cs
	}
	if pl != nil {
		ao.Pool = pl
		ao.System.Programs = pl.ReleaseStatus
	}
	ao.Logs = logRelay
	apiSrv = api.New(cfg, sched, ao)
	started.Store(true)
	// MCP at /mcp on the same port; its tools run in-process through apiSrv (design 9.5).
	mcpSrv := mcpserver.New(apiSrv, mcpserver.Options{Version: version, Guide: docs.BenchmarkGuide,
		Bridge:   func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) },
		Identify: apiSrv.IdentifyAgent}) // an agent key names the agent in the audit log (it is not access control)
	apiSrv.SetMCPTools(mcpSrv.ToolInfo)
	apiSrv.Warm()                                                           // the model catalog is built in the background, before anyone asks
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // not a ServeMux: it would clean paths the API serves verbatim
		// A handler panic is logged with its stack and counted as task http
		// (never escalated); net/http then recovers it as before.
		defer supervise.ReportRepanic("http", func(p any) bool { return p == http.ErrAbortHandler })
		if r.URL.Path == "/mcp" {
			mcpSrv.ServeHTTP(w, r)
			return
		}
		apiSrv.ServeHTTP(w, r)
	})
	httpSrv := &http.Server{Handler: root, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go supervise.Once("http.serve", func() { serveErr <- httpSrv.Serve(ln) }, // a failure here stops Potluck as a Serve error does
		func(p any) { serveErr <- fmt.Errorf("internal error: %v", p) })
	if benchMgr != nil {
		benchMgr.Start(bg)
	}
	log.Info("llm-scheduler started", "version", version, "listen", ln.Addr().String(), "profiles", len(cfg.Profiles))

	code := 0
	select {
	case s := <-sigs:
		log.Info("shutting down", "signal", s.String())
		hist.Record(stoppingEvent(s.String(), time.Now()))
	case err := <-serveErr:
		log.Error("HTTP server failed", "err", err)
		hist.Record(stoppingEvent("HTTP server failed: "+err.Error(), time.Now()))
		code = 1
	case reason := <-fatal.ch:
		return fatalStop(reason, log, hist, httpSrv, launcher, bgCancel)
	}

	// Design 8.6: stop accepting, then let the scheduler answer waiters,
	// wait for in-flight requests up to shutdown_grace, and stop children.
	// A second signal kills everything at once.
	ln.Close()
	grace := svc.Config().Defaults.ShutdownGrace + shutdownSlack
	// A task failure from here on does not shorten the grace (review IS2);
	// the bound covers the benchmark stop, the servers and the waits below.
	fatal.beginShutdown(grace + apiGrace + 10*time.Second + fatalWatchdog)
	sctx, scancel := context.WithTimeout(context.Background(), grace)
	defer scancel()
	go supervise.Once("main.second_signal", func() {
		select {
		case <-sigs:
			log.Warn("second signal: stopping everything at once")
			scancel()
		case <-sctx.Done():
		}
	}, nil)
	if benchMgr != nil { // a running benchmark is cancelled and unloads what it loaded while the API still answers
		bmctx, bmcancel := context.WithTimeout(sctx, 60*time.Second)
		benchMgr.Stop(bmctx)
		bmcancel()
	}
	if logRelay != nil {
		logRelay.StopReads() // a reader of /logs/ that streams must not hold the shutdown up
	}
	code = exitCodeAfterShutdown(code, apiSrv.Shutdown(sctx), log)
	hctx, hcancel := context.WithTimeout(context.Background(), apiGrace)
	defer hcancel()
	if err := httpSrv.Shutdown(hctx); err != nil {
		httpSrv.Close()
	}
	if pl != nil {
		pl.WaitStops(10 * time.Second) // the worker launches of the last instances
	}
	bgCancel()
	<-schedDone // the scheduler and its learned-need writer have written what was queued
	<-fleetDone
	if pl != nil {
		<-poolDone
	}
	// The API server's background work (the catalog build, the audit queue,
	// the agents table) is stopped and flushed before the database is closed
	// on return: nothing writes to it once "stopped" is logged.
	apiSrv.Close()
	stepDone("api closed", nil)
	if fatal.fired.Load() { // a task failed for good during the shutdown
		code = supervise.ExitCode
	}
	log.Info("stopped")
	return code
}
