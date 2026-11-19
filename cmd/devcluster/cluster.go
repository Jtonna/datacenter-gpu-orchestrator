package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/docs"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/audit"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/cfgstore"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/cfgsvc"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/mcpserver"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/fakefleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/ggufgen"
)

// cluster is the REAL server underneath the harness, wired as
// cmd/llm-scheduler's serve does: the configuration service on the database
// (seeded through the real import path), the audit log, the scheduler, the
// history store, the benchmark store and manager, the API and MCP at /mcp. Only
// the fleet (fakefleet, in process) and the llama processes (fakellama) are fakes.
type cluster struct {
	log   *slog.Logger
	tmp   string
	fake  string // fakellama binary
	tb    *tbShim
	fleet *fakefleet.Fleet
	data  string // data dir (scheduler.db)

	db       *bench.Store // the database: benchmarks, runs, configuration, audit
	cs       *cfgstore.Store
	svc      *cfgsvc.Service
	fc       *fleet.Client
	hist     *history.Store
	sched    *scheduler.Scheduler
	benchMgr *bench.Manager
	api      *api.Server
	mcp      *mcpserver.Server
	root     http.Handler // what the real binary serves: /mcp and the API
	srv      *http.Server
	ln       net.Listener
	url      string // http://127.0.0.1:port of the real server

	bgCtx     context.Context
	bgCancel  context.CancelFunc
	schedDone chan struct{}
	fleetDone chan struct{}
}

const devVersion = "0.1.0-devcluster"

// moduleRoot finds the directory holding go.mod: from the working directory
// upwards, else from this source file's location.
func moduleRoot() (string, error) {
	isRoot := func(dir string) bool {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		return err == nil && bytes.Contains(b, []byte("module github.com/Jtonna/ps5-ai-datacenter-load-balancer"))
	}
	if wd, err := os.Getwd(); err == nil {
		for d := wd; ; d = filepath.Dir(d) {
			if isRoot(d) {
				return d, nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		d := filepath.Join(filepath.Dir(file), "..", "..")
		if isRoot(d) {
			return filepath.Clean(d), nil
		}
	}
	return "", errors.New("cannot find the module root (go.mod); run devcluster from inside the repository")
}

func buildFakeLlama(root, dir string) (string, error) {
	name := "fakellama"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, "./internal/testutil/fakellama")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building fakellama: %v\n%s", err, b)
	}
	return out, nil
}

func fleetMembers() []fakefleet.Member {
	var ms []fakefleet.Member
	for _, n := range []nodeDef{ps5A, ps5B, ps5C} {
		// 16 GB of RAM with 2.6 used: 13.4 GiB available, the shared-memory free figure.
		ms = append(ms, fakefleet.PS5(n.ID, n.Host, n.IP, 13.4, 0, 16, 2.6))
	}
	ms = append(ms, fakefleet.Relay(alien.ID, alien.Host, alien.IP, 8, 0.4))
	return ms
}

// clusterOpts says how to start the real server.
type clusterOpts struct {
	// bare starts on an empty database (nothing imported: configured false)
	// with the history off.
	bare         bool
	statusSource func(ctx context.Context) (any, error)
	seedHistory  func(*history.Store)
}

// startCluster builds everything and starts the real server on its own
// loopback port. The benchmark manager is created but not started: the caller
// starts it (c.benchMgr.Start(c.bgCtx)) once the seed runs are written.
func startCluster(log *slog.Logger, o clusterOpts) (*cluster, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	sweepStale(log)
	tmp, err := os.MkdirTemp("", "devcluster-")
	if err != nil {
		return nil, err
	}
	os.WriteFile(filepath.Join(tmp, "devcluster.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644)
	c := &cluster{log: log, tmp: tmp, tb: &tbShim{log: log}, data: filepath.Join(tmp, "data")}
	ok := false
	defer func() {
		if !ok {
			c.close()
		}
	}()
	log.Info("building fakellama", "dir", tmp)
	if c.fake, err = buildFakeLlama(root, tmp); err != nil {
		return nil, err
	}
	models := filepath.Join(tmp, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		if seen[p.Model] {
			continue
		}
		seen[p.Model] = true
		if err := ggufgen.Write(filepath.Join(models, p.Model), ggufgen.Spec{Context: 32768}); err != nil {
			return nil, err
		}
	}

	c.fleet = fakefleet.New(c.tb)
	c.fleet.SetMembers(fleetMembers()...)

	c.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	c.url = "http://" + c.ln.Addr().String()

	boot := config.Bootstrap{Listen: c.ln.Addr().String(), ModelsDir: models, DataDir: c.data, ChildLogDir: filepath.Join(tmp, "child-logs")}
	if !o.bare {
		boot.HistoryDir = filepath.Join(tmp, "history")
	}

	// The database, as serve opens it.
	if c.db, err = bench.OpenStore(boot.DataDir); err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	if c.cs, err = cfgstore.Open(c.db); err != nil {
		return nil, fmt.Errorf("opening the configuration tables: %w", err)
	}
	// The seed configuration through the real import path (llm-scheduler import-config).
	if !o.bare {
		if err := importConfig(c, boot, clusterYAML(c.fleet.URL(), c.fake)); err != nil {
			return nil, err
		}
	}

	var started atomic.Bool
	c.svc = cfgsvc.New(context.Background(), cfgsvc.Options{Store: c.cs, Boot: boot, Logger: log,
		ProfileRefs: func(id string) []string { return benchmarksUsing(c.db, id) },
		Apply: func(cf *config.Config) {
			if started.Load() {
				c.fc.SetConfig(cf)
				c.sched.ApplyConfig(cf)
			}
		},
		OnChange: func(ev cfgsvc.ChangeEvent) {
			if started.Load() {
				c.api.PublishConfig(ev)
			}
		}})
	cfg := c.svc.Config()
	if e := c.svc.LoadError(); e != "" && !o.bare {
		return nil, fmt.Errorf("the configuration service: %s", e)
	}

	// Children (fakellama) load for a few seconds, print a load log and
	// stream slowly so a request stays in flight a while; short enough that
	// a benchmark run started from the UI takes a minute or two.
	env := append(os.Environ(), "FAKELLAMA_LOAD_DELAY=5s", "FAKELLAMA_LOAD_LOG=auto", "FAKELLAMA_STREAM_DELAY=250ms", "FAKELLAMA_STREAM_CHUNKS=8")

	bg, cancel := context.WithCancel(context.Background())
	c.bgCtx, c.bgCancel = bg, cancel
	fcfg := cfg
	if o.bare { // the built-in defaults name no fleet; the fake one stands in so the nodes show
		cp := *cfg
		cp.Fleet.URL, cp.Fleet.LocalNode = c.fleet.URL(), omen.Host
		fcfg = &cp
	}
	c.fc = fleet.New(fcfg, fleet.Options{Logger: log})
	c.fleetDone = make(chan struct{})
	go func() { c.fc.Run(bg); close(c.fleetDone) }()
	go c.pushLoop(bg)

	ports, err := instance.NewPortAllocator(6800, 6999)
	if err != nil {
		return nil, err
	}
	launcher := &instance.ProcessLauncher{Ports: ports, Env: env, LogDir: cfg.ChildLogDir, LogKeep: 50, Logger: log}
	if err := launcher.PrepareLogDir(); err != nil {
		launcher.LogDir = ""
	}

	c.hist = history.Open(history.Options{Dir: cfg.HistoryDir, Logger: log})
	if c.hist.On() && o.seedHistory != nil {
		o.seedHistory(c.hist)
	}
	c.hist.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TSchedulerStarted, Severity: history.Info,
		Data: map[string]any{"version": devVersion, "profiles": len(cfg.Profiles), "listen": cfg.Listen, "safety": safetySummary(cfg),
			"history": map[string]any{"max_file_mib": cfg.HistoryMaxFileMiB, "keep_files": cfg.HistoryKeepFiles, "memory_events": cfg.HistoryMemoryEvents}},
		Summary: fmt.Sprintf("llm-scheduler %s started with %d profiles.", devVersion, len(cfg.Profiles))})

	serverBin := cfg.Defaults.ServerBin
	if o.bare {
		serverBin = c.fake
	}
	so := scheduler.Options{View: c.fc, Prober: &instance.ExecProber{ServerBin: serverBin, Env: env}, Launcher: launcher, Logger: log}
	if c.hist.On() {
		so.History = c.hist
	}
	if c.sched, err = scheduler.New(cfg, so); err != nil {
		return nil, err
	}
	c.schedDone = make(chan struct{})
	go func() { c.sched.Run(bg); close(c.schedDone) }()

	// The benchmark manager, as openBench builds it; the runner talks to this
	// real server's own address.
	bo := bench.ManagerOptions{Store: c.db, API: &bench.HTTPAPI{Base: c.url}, Version: devVersion, Logger: log,
		Thresholds: bench.Thresholds{LoadPct: cfg.Bench.RegressLoadPct, DecodePct: cfg.Bench.RegressDecodePct},
		MaxRun:     time.Duration(cfg.Bench.MaxRunMinutes) * time.Minute,
		ProfileOK:  func(id string) bool { _, _, ok := c.svc.Config().Resolve(id); return ok }}
	if c.hist.On() {
		bo.Record = c.hist.Record
	}
	if c.benchMgr, err = bench.NewManager(bo); err != nil {
		return nil, fmt.Errorf("the benchmark manager: %w", err)
	}

	ao := api.Options{Logger: log, History: c.hist, Version: devVersion, WSMaxClients: cfg.WSMaxClients, Bench: c.benchMgr, BenchStore: c.db,
		Config: c.svc, Audit: c.cs,
		System: api.SystemInfo{Listen: cfg.Listen, Safety: safetySummary(cfg), HistoryDir: cfg.HistoryDir, DataDir: cfg.DataDir,
			ChildLogDir: cfg.ChildLogDir, ModelsDir: cfg.ModelsDir,
			Bench: map[string]any{"enabled": true, "regress_load_pct": cfg.Bench.RegressLoadPct, "regress_decode_pct": cfg.Bench.RegressDecodePct,
				"max_run_minutes": cfg.Bench.MaxRunMinutes}}}
	if !c.hist.On() {
		ao.History = nil
	}
	// DEV-ONLY: the live channel's status frames come from the scenario (devhooks.go).
	ao = ao.WithStatusSource(o.statusSource)
	c.api = api.New(cfg, c.sched, ao)
	started.Store(true)
	c.mcp = mcpserver.New(c.api, mcpserver.Options{Version: devVersion, Guide: docs.BenchmarkGuide,
		Bridge: func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }})
	c.root = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // as serve: not a ServeMux
		if r.URL.Path == "/mcp" {
			c.mcp.ServeHTTP(w, r)
			return
		}
		c.api.ServeHTTP(w, r)
	})
	c.srv = &http.Server{Handler: c.root, ReadHeaderTimeout: 10 * time.Second}
	go c.srv.Serve(c.ln)
	ok = true
	return c, nil
}

// importConfig is llm-scheduler import-config on the harness's YAML: parse,
// import as revision 1 with source import, seed the file's benchmarks.
func importConfig(c *cluster, boot config.Bootstrap, yaml string) error {
	path := filepath.Join(c.tmp, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		return err
	}
	imp, err := config.ParseYAMLImport([]byte(yaml), boot, config.Options{})
	if err != nil {
		return fmt.Errorf("the harness configuration is not valid: %w", err)
	}
	ctx := context.Background()
	o := cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceImport, Actor: "cli:alex", Comment: "import of the YAML configuration file"}}
	boot.ModelsDir = imp.Config.ModelsDir
	svc := cfgsvc.New(ctx, cfgsvc.Options{Store: c.cs, Boot: boot})
	if _, err := svc.Import(ctx, o, imp.Doc, false); err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	now := time.Now().UnixMilli()
	for _, b := range imp.Benchmarks {
		if err := c.db.SeedBenchmark(b, now); err != nil {
			return err
		}
		recordImport(ctx, c.cs, o.Origin, "benchmark", b.ID)
	}
	for _, su := range imp.Suites {
		if err := c.db.SeedSuite(su, now); err != nil {
			return err
		}
		recordImport(ctx, c.cs, o.Origin, "suite", su.ID)
	}
	return nil
}

func recordImport(ctx context.Context, cs *cfgstore.Store, o audit.Origin, kind, id string) {
	o.Comment = kind + " " + id + " from the imported file"
	e := audit.Entry{Action: audit.ActionImport, Section: audit.SectionBench, Target: kind + ":" + id, Before: []byte("null"), After: []byte("null")}
	o.Apply(&e)
	_, _ = cs.Record(ctx, e)
}

// benchmarksUsing lists the benchmarks that refer to a profile (cmd/llm-scheduler's).
func benchmarksUsing(st *bench.Store, profile string) []string {
	if st == nil {
		return nil
	}
	bms, err := st.ListBenchmarks()
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range bms {
		if b.Spec != nil && b.Spec.Profile == profile {
			out = append(out, b.Spec.ID)
		}
	}
	return out
}

// safetySummary mirrors cmd/llm-scheduler's summary for /api/system.
func safetySummary(cfg *config.Config) map[string]any {
	d := cfg.Defaults
	return map[string]any{
		"split_skew_margin": d.SplitSkewMargin, "estimate_margin": d.EstimateMargin,
		"load_memory_floor_mib": d.LoadMemoryFloorMiB, "forced_fit_check": d.ForcedFitCheck,
		"load_stall_timeout_s": d.LoadStallTimeout.Seconds(), "worker_silence_s": d.WorkerSilence.Seconds(),
		"worker_probe_interval_s": d.WorkerProbeInterval.Seconds(),
		"recovery_enabled":        cfg.Recovery.Enabled, "recovery_actions": cfg.Recovery.Actions,
		"exclude_nodes": len(cfg.ExcludeNodes),
	}
}

// pushLoop keeps the fake fleet's stream alive: the fake never pushes on its
// own, and a quiet stream would make the view stale after stale_after.
func (c *cluster) pushLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.fleet.Push()
			c.fleet.Ping()
		}
	}
}

// ready reports whether the real server answers and the fleet view is live.
func (c *cluster) ready(ctx context.Context) bool {
	if c == nil || c.sched == nil {
		return false
	}
	st, err := c.sched.Status(ctx)
	if err != nil {
		return false
	}
	return st.View.Connected && !st.View.Stale && len(st.View.Nodes) >= 4
}

// close stops the real server and its children and removes the temp dir.
func (c *cluster) close() {
	if c.benchMgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		c.benchMgr.Stop(ctx)
		cancel()
	}
	if c.api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := c.api.Shutdown(ctx); err != nil {
			c.log.Warn("scheduler shutdown", "err", err)
		}
		cancel()
	}
	if c.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if c.srv.Shutdown(ctx) != nil {
			c.srv.Close()
		}
		cancel()
	} else if c.ln != nil {
		c.ln.Close()
	}
	if c.bgCancel != nil {
		c.bgCancel()
	}
	if c.schedDone != nil {
		<-c.schedDone
	}
	if c.fleetDone != nil {
		<-c.fleetDone
	}
	if c.hist != nil {
		c.hist.Close()
	}
	if c.db != nil {
		c.db.Close()
	}
	c.tb.runCleanups()
	if c.tmp != "" {
		// Windows may hold a just-exited child's binary a moment longer.
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(c.tmp); err == nil || !strings.Contains(err.Error(), "used by another process") {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// clusterYAML is the YAML file the harness imports, in the format
// llm-scheduler import-config reads: settings, tiers, node overrides,
// exclude_nodes, recovery, fleet, bench settings and the profiles.
func clusterYAML(fleetURL, fake string) string {
	slash := filepath.ToSlash
	return fmt.Sprintf(`fleet: {url: %q, local_node: omen-runner, stale_after: 60s}
local_probe_cmd: "%s --nvidia-smi 8151 0"
defaults:
  ttl: 300s
  min_hold: 30s
  max_hold: 900s
  drain_after: 60s
  queue_timeout: 600s
  stall_timeout: 300s
  shutdown_grace: 10s
  server_bin: %q
  load_stall_timeout: 180s
  forced_fit_check: warn
  estimate_margin: 0.1
tiers:
  - {match: {hostname: "ps5-*"}, priority: 1, headroom_mib: 1024, shared_memory: true, link_mb_s: 100, settle: 3s}
  - {match: {hostname: "alienware*"}, priority: 2, headroom_mib: 256, link_mb_s: 60, settle: 8s}
  - {match: {local: true}, priority: 3, headroom_mib: 450, settle: 2s}
node_overrides:
  alienware-m15: {rpc: "10.0.0.3:50052"}
exclude_nodes: ["alienware-m15"]
recovery:
  enabled: true
  actions: [restart_rpc_server, restart_docker]
  min_interval: 600s
  step_wait: 90s
bench:
  regress_load_pct: 15
  regress_decode_pct: 7
  max_run_minutes: 120
profiles:
%s`, fleetURL, slash(fake), slash(fake), profilesYAML())
}
