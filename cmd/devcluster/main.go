// Command devcluster is a dev harness for the LLM Scheduler web UI. It runs
// the REAL api.Server, scheduler and history store against an in-process fake
// fleet and the fakellama binary, and puts a front in front of it that
//
//   - answers /api/status, /api/profiles and /api/plan from a scripted
//     scenario (all scenarios but live),
//
//   - mocks the config, audit and benchmark API that the server does not have
//     yet (the mock_*.go files),
//
//   - relays /ws to the real server and adds the mock frames,
//
//   - forwards everything else, /ui/ included, to the real server.
//
//     go run ./cmd/devcluster --listen 127.0.0.1:8790 --scenario demo
//
// Control: GET /dev/scenarios, POST /dev/scenario?name=<name>[&reset=1],
// GET /dev/ready. It is a development tool only; nothing here is shipped.
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
	"syscall"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

type harness struct {
	c      *cluster
	st     *scenarioState
	cfg    *configStore
	audit  *auditLog
	engine *benchEngine
	relay  *relay
	bare   bool // started with --scenario bare
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8790", "address of the front")
	scen := flag.String("scenario", "demo", "scenario at startup (see GET /dev/scenarios)")
	verbose := flag.Bool("verbose", false, "log the real server at info level")
	uiDir := flag.String("ui-dir", "auto", `serve /ui/ from this directory (default: internal/api/ui under the module root when it exists; "" = the embedded files)`)
	flag.Parse()

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

	h := &harness{st: &scenarioState{}, bare: sc.Name == "bare"}
	h.st.set(sc.Name)
	fmt.Fprintf(os.Stderr, "devcluster: building fakellama and starting the real scheduler...\n")
	h.c, err = startCluster(log, !h.bare, h.st.statusSource, func(s *history.Store) { seedHistory(s, time.Now()) })
	if err != nil {
		ln.Close()
		fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		os.Exit(1)
	}
	h.st.c = h.c

	h.audit = &auditLog{}
	h.cfg = newConfigStore(h.audit)
	h.cfg.reset(h.bare)
	h.st.configState = func() (int, bool) { r := h.cfg.current(); return r, r > 0 }
	store := newBenchStore()
	h.engine = newBenchEngine(store)
	h.relay = newRelay(h.c.url, !h.bare, h.cfg.current, h.engine.stateDoc)
	h.cfg.notify = func(d map[string]any) { h.relay.inject("config", d) }
	h.engine.emit = func(ev map[string]any) { h.relay.inject("bench", ev) }
	if h.c.hist.On() {
		h.engine.record = h.c.hist.Record
	}
	h.engine.reset(h.bare, sc.Bench, time.Now())
	ba := &benchAPI{store: store, engine: h.engine, cfg: h.cfg}
	h.cfg.profileRefs = store.profileRefs

	mux := http.NewServeMux()
	mux.HandleFunc("GET /dev/scenarios", h.listScenarios)
	mux.HandleFunc("POST /dev/scenario", h.setScenario)
	mux.HandleFunc("GET /dev/ready", h.ready)
	mux.HandleFunc("GET /dev/frame", devFrame)
	h.cfg.routes(mux)
	mux.HandleFunc("GET /api/audit", h.audit.handle)
	mux.HandleFunc("GET /api/audit/{id}", h.audit.get)
	ba.routes(mux)
	mux.HandleFunc("GET /ws", h.relay.serveWS)
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handleStatus(w, r) {
			h.c.api.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /api/metrics/activity", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handleActivity(w, r) {
			h.c.api.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("GET /api/profiles", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handleProfiles(w, r) {
			h.c.api.ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("POST /api/plan", func(w http.ResponseWriter, r *http.Request) {
		if !h.st.handlePlan(w, r) {
			h.c.api.ServeHTTP(w, r)
		}
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
	mux.Handle("/", h.c.api)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "devcluster: %v\n", err)
		}
	}()

	url := "http://" + ln.Addr().String()
	fmt.Printf("\ndevcluster ready at %s/ui/  (real server underneath at %s)\n", url, h.c.url)
	fmt.Printf("ui: %s\n", uiMode)
	fmt.Printf("scenario: %s\n\nscenarios (POST %s/dev/scenario?name=<name>[&reset=1]):\n", sc.Name, url)
	for _, s := range scenarios {
		fmt.Printf("  %-20s %s\n", s.Name, s.Description)
	}
	fmt.Printf("\nGET %s/dev/ready answers 200 once the fake fleet is live. Ctrl+C stops.\n", url)
	go h.announceReady()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Fprintln(os.Stderr, "devcluster: stopping...")
	h.engine.reset(true, "", time.Now())
	h.relay.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
	cancel()
	h.c.close()
	fmt.Fprintln(os.Stderr, "devcluster: stopped, temp files removed.")
}

func (h *harness) announceReady() {
	for i := 0; i < 120; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ok := h.c.ready(ctx)
		cancel()
		if ok {
			fmt.Println("devcluster: fleet live, /dev/ready is 200.")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (h *harness) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	sc, _ := h.st.get()
	if !h.c.ready(ctx) {
		writeJSON(w, 503, map[string]any{"ready": false, "scenario": sc.Name})
		return
	}
	writeJSON(w, 200, map[string]any{"ready": true, "scenario": sc.Name})
}

func (h *harness) listScenarios(w http.ResponseWriter, r *http.Request) {
	cur, _ := h.st.get()
	list := []any{}
	for _, s := range scenarios {
		list = append(list, map[string]any{"name": s.Name, "description": s.Description, "startup_only": s.StartupOnly,
			"scripted": s.Status != nil, "bench": s.Bench})
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
	if sc.StartupOnly && !h.bare {
		writeErr(w, 409, fmt.Sprintf("scenario %s only works at startup (history cannot be switched off at runtime): restart with --scenario %s", name, name), "conflict")
		return
	}
	reset := r.URL.Query().Get("reset") == "1" || r.URL.Query().Get("reset") == "true"
	h.st.set(sc.Name)
	now := time.Now()
	h.engine.reset(h.bare, sc.Bench, now)
	if reset {
		h.cfg.reset(h.bare)
		h.relay.inject("config", map[string]any{"revision": h.cfg.current(), "audit_entry_ids": []int64{}, "section": "multiple", "target": "",
			"source": "system", "actor": "devcluster"})
	}
	if sc.Name == "refused" && h.c.hist.On() {
		h.recordRefused(now)
	}
	writeJSON(w, 200, map[string]any{"scenario": sc.Name, "description": sc.Description, "since": now.UnixMilli(), "reset": reset})
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
