// Command devcluster is a dev harness for the LLM Scheduler web UI. It runs
// the REAL server, wired as cmd/llm-scheduler's serve: the configuration
// service on a real database (seeded through the real import path, then
// through real REST and MCP writes), the audit log, the scheduler, the
// history, the benchmark store and manager, the API and MCP at /mcp. Only the
// fleet (fakefleet, in process) and the llama processes (fakellama) are fakes.
// A front in front of it
//
//   - answers /api/status, /api/profiles, /api/plan and
//     /api/metrics/activity from a scripted scenario (all scenarios but live;
//     DEV-ONLY injection, built from the scheduler's own types, profile list
//     from the real configuration),
//
//   - in a frozen bench-* scenario, answers /api/bench/... from a snapshot
//     world run by a second real benchmark manager (bench_frozen.go),
//
//   - relays /ws to the real server,
//
//   - forwards everything else, /mcp and /ui/ included, to the real server.
//
//     go run ./cmd/devcluster --listen 127.0.0.1:8790 --scenario demo
//
// Control: GET /dev/scenarios, POST /dev/scenario?name=<name>, GET /dev/ready.
// It is a development tool only; nothing here is shipped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

type harness struct {
	c     *cluster
	log   *slog.Logger
	st    *scenarioState
	relay *relay
	bare  bool // started with --scenario bare
	real  bool // started with a real scenario: the recording is replayed (real.go)
	rp    *replay

	seeded  chan struct{} // closed when the startup seed is written
	seedErr atomic.Pointer[error]

	bmu    sync.Mutex // serializes bench world switches
	frozen atomic.Pointer[frozenWorld]
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8790", "address of the front")
	scen := flag.String("scenario", "demo", "scenario at startup (see GET /dev/scenarios)")
	verbose := flag.Bool("verbose", false, "log the real server at info level")
	uiDir := flag.String("ui-dir", "auto", `serve /ui/ from this directory (default: internal/api/ui under the module root when it exists; "" = the embedded files)`)
	capture := flag.String("capture", "", "record the GET answers of the scheduler at this base URL into cmd/devcluster/realdata (GET only), then exit")
	captureStatus := flag.String("capture-status", "", "with --capture: record only /api/status, under this file name")
	flag.Parse()
	if *capture != "" {
		if err := runCapture(*capture, *captureStatus); err != nil {
			fmt.Fprintf(os.Stderr, "devcluster: capture: %v\n", err)
			os.Exit(1)
		}
		return
	}

	sc, ok := scenarioByName(*scen)
	if !ok {
		fmt.Fprintf(os.Stderr, "devcluster: unknown scenario %q; one of:\n", *scen)
		for _, s := range scenarios {
			fmt.Fprintf(os.Stderr, "  %-20s %s\n", s.Name, s.Description)
		}
		os.Exit(2)
	}
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		os.Exit(1)
	}

	h := &harness{log: log, st: &scenarioState{}, bare: sc.Name == "bare", seeded: make(chan struct{})}
	h.st.set(sc.Name)
	if sc.Real {
		rp, err := loadReplay(time.Now())
		if err != nil {
			ln.Close()
			fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
			os.Exit(1)
		}
		h.real, h.rp, realReplay = true, rp, rp
	}
	fmt.Fprintf(os.Stderr, "devcluster: building fakellama and starting the real scheduler...\n")
	o := clusterOpts{bare: h.bare, statusSource: h.st.statusSource}
	if h.real {
		o.seedHistory, o.recorded = h.rp.seedHistory, true
	} else if !h.bare {
		o.seedHistory = func(s *history.Store) { seedHistory(s, time.Now()) }
	}
	h.c, err = startCluster(log, o)
	if err != nil {
		ln.Close()
		fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		os.Exit(1)
	}
	h.st.c = h.c
	liveProfiles = h.c.profileDefs
	h.st.configState = func() (int, bool) { return int(h.c.svc.Revision()), h.c.svc.Config().Configured }
	h.relay = newRelay(h.c.url)

	if h.real {
		close(h.seeded) // the recording has its own benchmarks and configuration: nothing is seeded
		h.relay.setFrozen(func() any { v, _ := h.rp.doc("/api/bench/state"); return v }, nil)
	} else if h.bare {
		close(h.seeded)
		h.c.benchMgr.Start(h.c.bgCtx)
	} else {
		fmt.Fprintf(os.Stderr, "devcluster: seeding the configuration history, benchmarks and suites...\n")
		if err := seedConfig(h.c, log); err != nil {
			fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		}
		if err := seedBenchDefs(h.c); err != nil {
			fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		}
		go h.seedRuns()
	}
	if sc.Bench != nil && !h.real {
		go h.applyBench(sc)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /dev/scenarios", h.listScenarios)
	mux.HandleFunc("POST /dev/scenario", h.setScenario)
	mux.HandleFunc("GET /dev/ready", h.ready)
	mux.HandleFunc("GET /dev/frame", devFrame)
	mux.HandleFunc("GET /ws", h.relay.serveWS)
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handleStatus(w, r) {
			h.c.root.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /api/metrics/activity", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handleActivity(w, r) {
			h.c.root.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /api/profiles", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handleProfiles(w, r) {
			h.c.root.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		if !h.handleModels(w, r) {
			h.c.root.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /api/models/{key...}", func(w http.ResponseWriter, r *http.Request) {
		if !h.handleModels(w, r) {
			h.c.root.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("POST /api/plan", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handlePlan(w, r) {
			h.c.root.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("/api/bench/", func(w http.ResponseWriter, r *http.Request) {
		if fw := h.frozen.Load(); fw != nil { // DEV-ONLY: the frozen scenario's snapshot world
			fw.ServeHTTP(w, r)
			return
		}
		h.c.root.ServeHTTP(w, r)
	})
	if *uiDir == "auto" {
		*uiDir = defaultUIDir()
	}
	uiMode := "embedded files of the real server"
	if *uiDir != "" {
		abs, _ := filepath.Abs(*uiDir)
		uiMode = "from disk: " + abs + " (edit and reload, no rebuild)"
		mux.HandleFunc("GET /ui/", uiDiskHandler(abs))
	}
	mux.Handle("/", h.c.root)

	var front http.Handler = mux
	if h.real { // DEV-ONLY: the recording answers before anything else
		front = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/dev/") || r.URL.Path == "/ws" || !h.serveReal(w, r) {
				mux.ServeHTTP(w, r)
			}
		})
	}
	srv := &http.Server{Handler: front, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		}
	}()

	url := "http://" + ln.Addr().String()
	fmt.Printf("\ndevcluster ready at %s/ui/  (real server underneath at %s, MCP at %s/mcp)\n", url, h.c.url, url)
	fmt.Printf("ui: %s\n", uiMode)
	fmt.Printf("scenario: %s\n\nscenarios (POST %s/dev/scenario?name=<name>):\n", sc.Name, url)
	for _, s := range scenarios {
		fmt.Printf("  %-20s %s\n", s.Name, s.Description)
	}
	fmt.Printf("\nGET %s/dev/ready answers 200 once the fake fleet is live and the seed is written. Ctrl+C stops.\n", url)
	go h.announceReady()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Fprintln(os.Stderr, "devcluster: stopping...")
	if fw := h.frozen.Swap(nil); fw != nil {
		fw.close()
	}
	h.relay.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
	cancel()
	h.c.close()
	fmt.Fprintln(os.Stderr, "devcluster: stopped, temp files removed.")
}

// seedRuns writes the past benchmark runs, gives the suites their real run
// options and then starts the real benchmark manager.
func (h *harness) seedRuns() {
	defer close(h.seeded)
	defer h.c.benchMgr.Start(h.c.bgCtx)
	start := time.Now()
	err := seedBenchRuns(h.c, h.log)
	if err == nil {
		err = seedBenchFinish(h.c)
	}
	if err != nil {
		h.seedErr.Store(&err)
		fmt.Fprintf(os.Stderr, "devcluster: benchmark seed: %v\n", err)
		return
	}
	fmt.Printf("devcluster: %d past benchmark runs written in %.0f s.\n", len(pastRuns()), time.Since(start).Seconds())
}

func (h *harness) isSeeded() bool {
	select {
	case <-h.seeded:
		return true
	default:
		return false
	}
}

// applyBench shows the bench side of a scenario: the real server's, or a
// frozen world. It waits for the seed (the snapshot needs it).
func (h *harness) applyBench(sc scenario) error {
	<-h.seeded
	h.bmu.Lock()
	defer h.bmu.Unlock()
	if cur, _ := h.st.get(); cur.Name != sc.Name { // another scenario was chosen meanwhile
		return nil
	}
	if old := h.frozen.Swap(nil); old != nil {
		old.close()
	}
	if sc.Bench == nil {
		h.relay.setFrozen(nil, func() any { return h.c.benchMgr.State() })
		return nil
	}
	placeholder := func() any { // drop the real bench frames while the snapshot world starts
		return map[string]any{"enabled": true, "run": nil, "current": nil, "queue": []any{}, "next_scheduled": []any{}}
	}
	h.relay.frozen.Store(&placeholder)
	fw, err := startFrozen(h.c, h.log, sc.Bench, h.relay.injectFrozen)
	if err != nil {
		h.log.Error("frozen bench scenario", "scenario", sc.Name, "err", err)
		h.relay.setFrozen(nil, func() any { return h.c.benchMgr.State() })
		return err
	}
	fw.settle(6 * time.Second)
	h.frozen.Store(fw)
	h.relay.setFrozen(func() any { return fw.mgr.State() }, nil)
	return nil
}

func (h *harness) announceReady() {
	for i := 0; i < 240; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ok := h.c.ready(ctx) && h.isSeeded()
		cancel()
		if ok {
			fmt.Println("devcluster: fleet live and seed written, /dev/ready is 200.")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (h *harness) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	sc, _ := h.st.get()
	doc := map[string]any{"scenario": sc.Name, "seeded": h.isSeeded(), "frozen_bench": h.frozen.Load() != nil}
	if e := h.seedErr.Load(); e != nil {
		doc["seed_error"] = (*e).Error()
	}
	live := h.c.ready(ctx)
	doc["fleet_live"] = live
	doc["ready"] = live && h.isSeeded() && (sc.Bench == nil || h.frozen.Load() != nil)
	if doc["ready"] != true {
		writeJSON(w, 503, doc)
		return
	}
	writeJSON(w, 200, doc)
}

func (h *harness) listScenarios(w http.ResponseWriter, r *http.Request) {
	cur, _ := h.st.get()
	list := []any{}
	for _, s := range scenarios {
		list = append(list, map[string]any{"name": s.Name, "description": s.Description, "startup_only": s.StartupOnly,
			"scripted": s.Status != nil, "frozen_bench": s.Bench != nil})
	}
	writeJSON(w, 200, map[string]any{"current": cur.Name, "started_bare": h.bare, "scenarios": list})
}

func (h *harness) setScenario(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	sc, ok := scenarioByName(name)
	if !ok {
		writeErr(w, 404, fmt.Sprintf("unknown scenario %q: see GET /dev/scenarios", name), "not found")
		return
	}
	if sc.Real != h.real {
		writeErr(w, 409, "the real scenarios replay a recording loaded at startup: restart with --scenario real to see them, and without it for the scripted ones", "conflict")
		return
	}
	if h.real {
		h.st.set(sc.Name)
		writeJSON(w, 200, map[string]any{"scenario": sc.Name, "description": sc.Description, "since": time.Now().UnixMilli()})
		return
	}
	if sc.StartupOnly && !h.bare {
		writeErr(w, 409, fmt.Sprintf("scenario %s only works at startup (it needs an empty database and history off): restart with --scenario %s", name, name), "conflict")
		return
	}
	if h.bare && sc.Bench != nil {
		writeErr(w, 409, fmt.Sprintf("scenario %s needs the seeded database; restart without --scenario bare", name), "conflict")
		return
	}
	h.st.set(sc.Name)
	now := time.Now()
	if sc.Name == "refused" && h.c.hist.On() {
		h.recordRefused(now)
	}
	doc := map[string]any{"scenario": sc.Name, "description": sc.Description, "since": now.UnixMilli()}
	if h.isSeeded() {
		if err := h.applyBench(sc); err != nil {
			doc["bench_error"] = err.Error()
		}
	} else {
		doc["bench"] = "the benchmark seed is still being written; the bench side follows when it is done"
		go h.applyBench(sc)
	}
	writeJSON(w, 200, doc)
}

// recordRefused writes the fresh history events the refused scenario promises.
func (h *harness) recordRefused(now time.Time) {
	fin := now.Add(-50 * time.Second)
	ps := []string{ps5A.Host, ps5B.Host, ps5C.Host}
	reason := "load stalled: no new bytes acknowledged for 180 s (8.5 of 25.5 GB sent); the child was stopped"
	h.c.hist.Record(history.Event{Ts: fin.UnixMilli(), Type: history.TLoadFinished, Severity: history.Error, Profile: "kimi-dev-72b-iq2s",
		ModelFile: modelFileOf("kimi-dev-72b-iq2s"), Nodes: ps, SessionID: sessionID(fin.Add(-71*time.Second), 12),
		Summary: "Loading kimi-dev-72b-iq2s failed after 71.4 s: " + reason + ".",
		Data:    map[string]any{"outcome": "failed", "reason": reason, "load_s": 71.4, "workers": addrsOf(ps)}})
	h.c.hist.Record(history.Event{Ts: fin.Add(400 * time.Millisecond).UnixMilli(), Type: history.TRequestRefused, Severity: history.Warn,
		Profile: "kimi-dev-72b-iq2s", Summary: "Request for kimi-dev-72b-iq2s refused before start (load failed): " + reason + ".",
		Data: map[string]any{"reason": "load failed", "status": 503, "message": reason, "waiters": 1}})
	h.c.hist.Record(history.Event{Ts: now.Add(-8 * time.Second).UnixMilli(), Type: history.TRequestRefused, Severity: history.Warn,
		Profile: "kimi-dev-72b-iq2s", Summary: "Request for kimi-dev-72b-iq2s refused before start (backed off): the profile is backed off for 2m10s after a failed load.",
		Data: map[string]any{"reason": "backed off", "status": 503, "message": "the profile is backed off after a failed load", "waiters": 1}})
}
