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
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/fakefleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/ggufgen"
)

// cluster is the REAL server underneath the harness: the real scheduler, API
// and history store, running against the in-process fake fleet and the
// fakellama binary.
type cluster struct {
	log     *slog.Logger
	tmp     string
	fake    string // fakellama binary
	tb      *tbShim
	fleet   *fakefleet.Fleet
	cfg     *config.Config
	cfgPath string
	hist    *history.Store
	sched   *scheduler.Scheduler
	api     *api.Server
	srv     *http.Server
	ln      net.Listener
	url     string // http://127.0.0.1:port of the real server

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

// startCluster builds everything and starts the real server on its own
// loopback port. historyOn false starts it with history off (scenario bare).
func startCluster(log *slog.Logger, historyOn bool, statusSource func(ctx context.Context) (any, error), seed func(*history.Store)) (*cluster, error) {
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
	c := &cluster{log: log, tmp: tmp, tb: &tbShim{log: log}}
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

	histDir := ""
	if historyOn {
		histDir = filepath.Join(tmp, "history")
	}
	yaml := clusterYAML(c.ln.Addr().String(), c.fleet.URL(), models, c.fake, histDir, filepath.Join(tmp, "child-logs"))
	c.cfgPath = filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(c.cfgPath, []byte(yaml), 0o644); err != nil {
		return nil, err
	}
	if c.cfg, err = config.Load(c.cfgPath); err != nil {
		return nil, err
	}

	// Children (fakellama) load for a few seconds, print a load log and
	// stream slowly so a request stays in flight a while.
	env := append(os.Environ(), "FAKELLAMA_LOAD_DELAY=6s", "FAKELLAMA_LOAD_LOG=auto", "FAKELLAMA_STREAM_DELAY=600ms", "FAKELLAMA_STREAM_CHUNKS=8")

	bg, cancel := context.WithCancel(context.Background())
	c.bgCancel = cancel
	fc := fleet.New(c.cfg, fleet.Options{Logger: log})
	c.fleetDone = make(chan struct{})
	go func() { fc.Run(bg); close(c.fleetDone) }()
	go c.pushLoop(bg)

	ports, err := instance.NewPortAllocator(6800, 6999)
	if err != nil {
		return nil, err
	}
	launcher := &instance.ProcessLauncher{Ports: ports, Env: env, LogDir: c.cfg.ChildLogDir, LogKeep: 50, Logger: log}
	if err := launcher.PrepareLogDir(); err != nil {
		launcher.LogDir = ""
	}

	c.hist = history.Open(history.Options{Dir: c.cfg.HistoryDir, Logger: log})
	if c.hist.On() && seed != nil {
		seed(c.hist)
	}
	c.hist.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TSchedulerStarted, Severity: history.Info,
		Data: map[string]any{"version": devVersion, "profiles": len(c.cfg.Profiles), "listen": c.cfg.Listen, "safety": safetySummary(c.cfg),
			"history": map[string]any{"max_file_mib": c.cfg.HistoryMaxFileMiB, "keep_files": c.cfg.HistoryKeepFiles, "memory_events": c.cfg.HistoryMemoryEvents}},
		Summary: fmt.Sprintf("llm-scheduler %s started with %d profiles.", devVersion, len(c.cfg.Profiles))})

	so := scheduler.Options{View: fc, Prober: &instance.ExecProber{ServerBin: c.cfg.Defaults.ServerBin, Env: env}, Launcher: launcher, Logger: log}
	if c.hist.On() {
		so.History = c.hist
	}
	if c.sched, err = scheduler.New(c.cfg, so); err != nil {
		return nil, err
	}
	c.schedDone = make(chan struct{})
	go func() { c.sched.Run(bg); close(c.schedDone) }()

	ao := api.Options{Logger: log, History: c.hist, Version: devVersion, WSMaxClients: c.cfg.WSMaxClients,
		System: api.SystemInfo{Listen: c.cfg.Listen, Safety: safetySummary(c.cfg), HistoryDir: c.cfg.HistoryDir, DataDir: filepath.Join(tmp, "data"),
			ChildLogDir: c.cfg.ChildLogDir, ModelsDir: c.cfg.ModelsDir,
			Bench: map[string]any{"enabled": true, "regress_load_pct": c.cfg.Bench.RegressLoadPct, "regress_decode_pct": c.cfg.Bench.RegressDecodePct,
				"max_run_minutes": c.cfg.Bench.MaxRunMinutes}}}
	if !c.hist.On() {
		ao.History = nil
	}
	ao = ao.WithStatusSource(statusSource)
	c.api = api.New(c.cfg, c.sched, ao)
	c.srv = &http.Server{Handler: c.api, ReadHeaderTimeout: 10 * time.Second}
	go c.srv.Serve(c.ln)
	ok = true
	return c, nil
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

// clusterYAML is the config file of the real server underneath.
func clusterYAML(listen, fleetURL, models, fake, histDir, childLogs string) string {
	slash := filepath.ToSlash
	return fmt.Sprintf(`listen: %q
fleet: {url: %q, local_node: omen-runner, stale_after: 60s}
models_dir: %q
local_probe_cmd: "%s --nvidia-smi 8151 0"
history_dir: %q
child_log_dir: %q
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
tiers:
  - {match: {hostname: "ps5-*"}, priority: 1, headroom_mib: 1024, shared_memory: true, link_mb_s: 100, settle: 3s}
  - {match: {hostname: "alienware*"}, priority: 2, headroom_mib: 256, link_mb_s: 60, settle: 8s}
  - {match: {local: true}, priority: 3, headroom_mib: 450, settle: 2s}
node_overrides:
  alienware-m15: {rpc: "10.0.0.3:50052"}
exclude_nodes: ["alienware-m15"]
profiles:
%s`, listen, fleetURL, slash(models), slash(fake), slash(histDir), slash(childLogs), slash(fake), profilesYAML())
}
