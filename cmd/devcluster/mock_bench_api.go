// DEV-ONLY MOCK of the /api/bench/* handlers of feat/bench-phases
// (internal/api/bench.go): benchmarks, suites, runs, exports, trends, state.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

type benchAPI struct {
	store  *benchStore
	engine *benchEngine
	cfg    *configStore
}

func (a *benchAPI) routes(m *http.ServeMux) {
	m.HandleFunc("GET /api/bench/schema", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(benchSchemaJSON)
	})
	m.HandleFunc("GET /api/bench/benchmarks", a.listBenchmarks)
	m.HandleFunc("POST /api/bench/benchmarks", a.createBenchmark)
	m.HandleFunc("GET /api/bench/benchmarks/{id}", a.getBenchmark)
	m.HandleFunc("PUT /api/bench/benchmarks/{id}", a.putBenchmark)
	m.HandleFunc("DELETE /api/bench/benchmarks/{id}", a.deleteBenchmark)
	m.HandleFunc("GET /api/bench/suites", a.listSuites)
	m.HandleFunc("POST /api/bench/suites", a.createSuite)
	m.HandleFunc("GET /api/bench/suites/{id}", a.getSuite)
	m.HandleFunc("PUT /api/bench/suites/{id}", a.putSuite)
	m.HandleFunc("DELETE /api/bench/suites/{id}", a.deleteSuite)
	m.HandleFunc("POST /api/bench/suites/{id}/run", a.runSuite)
	m.HandleFunc("GET /api/bench/runs", a.listRuns)
	m.HandleFunc("GET /api/bench/runs/{id}", a.getRun)
	m.HandleFunc("POST /api/bench/runs/{id}/cancel", a.cancelRun)
	m.HandleFunc("GET /api/bench/runs/{id}/export", a.exportRun)
	m.HandleFunc("GET /api/bench/trends", a.trends)
	m.HandleFunc("GET /api/bench/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.engine.stateDoc()) })
	m.HandleFunc("GET /api/bench/aggregates", a.aggregates)
	m.HandleFunc("GET /api/bench/profiles", a.benchProfiles)
	m.HandleFunc("/api/bench/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, "no such path: "+r.URL.Path, "not found")
	})
}

func apiErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeErr(w, status, fmt.Sprintf(format, args...), http.StatusText(status))
}

func (a *benchAPI) listBenchmarks(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	ids := sortedKeysOf(a.store.benchmarks)
	out := []any{}
	for _, id := range ids {
		out = append(out, clone(a.store.benchmarks[id]))
	}
	a.store.mu.Unlock()
	writeJSON(w, 200, map[string]any{"benchmarks": out})
}

func sortedKeysOf(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (a *benchAPI) getBenchmark(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	b, ok := a.store.benchmarks[r.PathValue("id")]
	var out any
	if ok {
		out = clone(b)
	}
	a.store.mu.Unlock()
	if !ok {
		apiErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, out)
}

func (a *benchAPI) parse(w http.ResponseWriter, r *http.Request, wantID string) (map[string]any, bool) {
	var b map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&b); err != nil || b == nil {
		apiErr(w, 400, "the body is not a valid JSON object: %v", err)
		return nil, false
	}
	for _, k := range []string{"source", "created_at", "updated_at", "last_run"} {
		delete(b, k)
	}
	if wantID != "" {
		if id, _ := b["id"].(string); id == "" {
			b["id"] = wantID
		} else if id != wantID {
			apiErr(w, 400, "the id in the body (%q) differs from the id in the path (%q)", id, wantID)
			return nil, false
		}
	}
	return b, true
}

func (a *benchAPI) createBenchmark(w http.ResponseWriter, r *http.Request) {
	b, ok := a.parse(w, r, "")
	if !ok {
		return
	}
	a.store.mu.Lock()
	err := validateBenchmark(b, a.store.benchmarks)
	a.store.mu.Unlock()
	if err != nil {
		apiErr(w, 422, "%v", err)
		return
	}
	id := b["id"].(string)
	now := time.Now().UnixMilli()
	a.store.mu.Lock()
	if _, ok := a.store.benchmarks[id]; ok {
		a.store.mu.Unlock()
		apiErr(w, 409, "benchmark %q already exists: use PUT /api/bench/benchmarks/%s to replace it", id, id)
		return
	}
	b["source"], b["created_at"], b["updated_at"] = "api", now, now
	a.store.benchmarks[id] = b
	out := clone(b)
	a.store.mu.Unlock()
	a.cfg.auditBench("benchmarks", "create", id, nil, out, originOf(r, ""))
	w.Header().Set("Location", "/api/bench/benchmarks/"+id)
	writeJSON(w, 201, out)
}

func (a *benchAPI) putBenchmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := a.parse(w, r, id)
	if !ok {
		return
	}
	a.store.mu.Lock()
	old, exists := a.store.benchmarks[id]
	err := validateBenchmark(b, a.store.benchmarks)
	a.store.mu.Unlock()
	if !exists {
		apiErr(w, 404, "not found")
		return
	}
	if err != nil {
		apiErr(w, 422, "%v", err)
		return
	}
	now := time.Now().UnixMilli()
	a.store.mu.Lock()
	b["source"], b["created_at"], b["updated_at"] = "api", old["created_at"], now
	a.store.benchmarks[id] = b
	out := clone(b)
	a.store.mu.Unlock()
	a.cfg.auditBench("benchmarks", "update", id, old, out, originOf(r, ""))
	writeJSON(w, 200, out)
}

func (a *benchAPI) deleteBenchmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.store.mu.Lock()
	old, ok := a.store.benchmarks[id]
	if !ok {
		a.store.mu.Unlock()
		apiErr(w, 404, "not found")
		return
	}
	using, steps := []string{}, []string{}
	for sid, su := range a.store.suites {
		for _, b := range asList(su["benchmarks"]) {
			if b == id {
				using = append(using, sid)
			}
		}
	}
	for bid, bm := range a.store.benchmarks {
		for _, m := range asList(bm["parallel"]) {
			if m == id {
				steps = append(steps, bid)
			}
		}
	}
	if len(using)+len(steps) > 0 {
		a.store.mu.Unlock()
		sort.Strings(using)
		sort.Strings(steps)
		apiErr(w, 409, "benchmark %q is used by suites %v and parallel steps %v: remove it from them first", id, using, steps)
		return
	}
	delete(a.store.benchmarks, id)
	a.store.mu.Unlock()
	a.cfg.auditBench("benchmarks", "delete", id, old, nil, originOf(r, ""))
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (a *benchAPI) suiteJSON(su map[string]any) map[string]any {
	m := cloneMap(su)
	for i := len(a.store.runs) - 1; i >= 0; i-- {
		if a.store.runs[i].SuiteID == su["id"] {
			m["last_run"] = a.store.runs[i].short()
			break
		}
	}
	return m
}

func (a *benchAPI) listSuites(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	out := []any{}
	for _, id := range sortedKeysOf(a.store.suites) {
		out = append(out, a.suiteJSON(a.store.suites[id]))
	}
	a.store.mu.Unlock()
	writeJSON(w, 200, map[string]any{"suites": out})
}

func (a *benchAPI) getSuite(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	su, ok := a.store.suites[r.PathValue("id")]
	var out any
	if ok {
		out = cloneMap(su)
	}
	a.store.mu.Unlock()
	if !ok {
		apiErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, out)
}

func (a *benchAPI) saveSuite(w http.ResponseWriter, r *http.Request, id string, create bool) {
	su, ok := a.parse(w, r, id)
	if !ok {
		return
	}
	a.store.mu.Lock()
	err := validateSuite(su, a.store.benchmarks)
	sid, _ := su["id"].(string)
	old, exists := a.store.suites[sid]
	a.store.mu.Unlock()
	switch {
	case !create && !exists:
		apiErr(w, 404, "not found")
		return
	case err != nil:
		apiErr(w, 422, "%v", err)
		return
	case create && exists:
		apiErr(w, 409, "suite %q already exists: use PUT /api/bench/suites/%s to replace it", sid, sid)
		return
	}
	now := time.Now().UnixMilli()
	created := any(now)
	action := "create"
	if exists {
		created, action = old["created_at"], "update"
	}
	su["source"], su["created_at"], su["updated_at"] = "api", created, now
	a.store.mu.Lock()
	a.store.suites[sid] = su
	out := cloneMap(su)
	a.store.mu.Unlock()
	a.cfg.auditBench("suites", action, sid, old, out, originOf(r, ""))
	status := 200
	if create {
		status = 201
		w.Header().Set("Location", "/api/bench/suites/"+sid)
	}
	writeJSON(w, status, out)
}

func (a *benchAPI) createSuite(w http.ResponseWriter, r *http.Request) { a.saveSuite(w, r, "", true) }
func (a *benchAPI) putSuite(w http.ResponseWriter, r *http.Request) {
	a.saveSuite(w, r, r.PathValue("id"), false)
}

func (a *benchAPI) deleteSuite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.store.mu.Lock()
	old, ok := a.store.suites[id]
	delete(a.store.suites, id)
	a.store.mu.Unlock()
	if !ok {
		apiErr(w, 404, "not found")
		return
	}
	a.cfg.auditBench("suites", "delete", id, old, nil, originOf(r, ""))
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (a *benchAPI) runSuite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Baseline string `json:"baseline"`
		Note     string `json:"note"`
	}
	if r.ContentLength != 0 {
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err == nil {
			req.Baseline, _ = raw["baseline"].(string)
			req.Note, _ = raw["note"].(string)
		}
	}
	run, status, msg := a.engine.enqueue(r.PathValue("id"), req.Baseline, req.Note, "manual")
	if status != 0 {
		apiErr(w, status, "%s", msg)
		return
	}
	w.Header().Set("Location", "/api/bench/runs/"+run.ID)
	writeJSON(w, 202, run)
}

func (a *benchAPI) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	suite, state := q.Get("suite"), q.Get("state")
	switch state {
	case "", "queued", "running", "paused", "completed", "failed", "cancelled", "skipped":
	default:
		writeErr(w, 400, fmt.Sprintf("state must be queued, running, paused, completed, failed, cancelled or skipped, got %q", state), "bad request")
		return
	}
	limit := qInt(r, "limit", 100)
	a.store.mu.Lock()
	out := []mRun{}
	for i := len(a.store.runs) - 1; i >= 0 && len(out) < limit; i-- {
		run := a.store.runs[i]
		if (suite == "" || run.SuiteID == suite) && (state == "" || run.State == state) {
			out = append(out, run.short())
		}
	}
	a.store.mu.Unlock()
	writeJSON(w, 200, map[string]any{"runs": out})
}

func (a *benchAPI) getRun(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	run := a.store.runByID(r.PathValue("id"))
	var out any
	if run != nil {
		out = clone(run)
	}
	a.store.mu.Unlock()
	if run == nil {
		apiErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, out)
}

func (a *benchAPI) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, msg := a.engine.cancel(id)
	if status != 0 {
		apiErr(w, status, "%s", msg)
		return
	}
	writeJSON(w, 202, map[string]any{"cancelling": id, "note": "a running run stops after its current request and unloads what it loaded"})
}

func (a *benchAPI) exportRun(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	run := a.store.runByID(r.PathValue("id"))
	var cp mRun
	if run != nil {
		cp = *run
	}
	a.store.mu.Unlock()
	if run == nil {
		apiErr(w, 404, "not found")
		return
	}
	h := w.Header()
	switch f := r.URL.Query().Get("format"); f {
	case "", "json":
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, cp.ID))
		writeJSON(w, 200, cp)
	case "csv":
		h.Set("Content-Type", "text/csv; charset=utf-8")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, cp.ID))
		var b strings.Builder
		b.WriteString("idx,benchmark_id,profile,nodes,outcome,load_s,load_mb_s,prefill_tps,decode_tps,decode_stdev,memory_mib,load_pct,decode_pct,regression\n")
		for _, s := range cp.Steps {
			lo, mbs, mem := "", "", ""
			if s.Load != nil {
				lo, mbs, mem = fmt.Sprintf("%.1f", s.Load.LoadS), fmt.Sprintf("%.1f", s.Load.AvgMBs), fmt.Sprintf("%.0f", s.Load.MemoryMiB)
			}
			pf, dc, sd := "", "", ""
			if s.Stats != nil && s.Stats.Decode != nil {
				dc, sd = fmt.Sprintf("%.2f", s.Stats.Decode.Mean), fmt.Sprintf("%.2f", s.Stats.Decode.Stdev)
			}
			if s.Stats != nil && s.Stats.Prefill != nil {
				pf = fmt.Sprintf("%.1f", s.Stats.Prefill.Mean)
			}
			lp, dp, reg := "", "", "false"
			if c := s.Comparison; c != nil {
				if c.LoadPct != nil {
					lp = fmt.Sprintf("%.1f", *c.LoadPct)
				}
				if c.DecodePct != nil {
					dp = fmt.Sprintf("%.1f", *c.DecodePct)
				}
				reg = fmt.Sprint(c.Regression)
			}
			fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n", s.Idx, s.BenchmarkID, s.Profile, strings.Join(s.nodes, ";"), s.Outcome,
				lo, mbs, pf, dc, sd, mem, lp, dp, reg)
		}
		w.Write([]byte(b.String()))
	case "md":
		h.Set("Content-Type", "text/markdown; charset=utf-8")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, cp.ID))
		var b strings.Builder
		fmt.Fprintf(&b, "# Benchmark run %s\n\nSuite %s (%s), state %s.\n\n", cp.ID, cp.SuiteName, cp.SuiteID, cp.State)
		b.WriteString("| Benchmark | Profile | Nodes | Load s | MB/s | Prefill tok/s | Decode tok/s | Decode sd | Memory MiB | Outcome | vs baseline |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, s := range cp.Steps {
			lo, mbs, mem, pf, dc, sd, cmp := "-", "-", "-", "-", "-", "-", "-"
			if s.Load != nil && s.Load.LoadS > 0 {
				lo, mbs, mem = fmt.Sprintf("%.1f", s.Load.LoadS), fmt.Sprintf("%.1f", s.Load.AvgMBs), fmt.Sprintf("%.0f", s.Load.MemoryMiB)
			}
			if s.Stats != nil && s.Stats.Decode != nil {
				dc, sd = fmt.Sprintf("%.2f", s.Stats.Decode.Mean), fmt.Sprintf("%.2f", s.Stats.Decode.Stdev)
			}
			if s.Stats != nil && s.Stats.Prefill != nil {
				pf = fmt.Sprintf("%.1f", s.Stats.Prefill.Mean)
			}
			if c := s.Comparison; c != nil && c.LoadPct != nil {
				cmp = fmt.Sprintf("load %+.1f%%", *c.LoadPct)
				if c.Regression {
					cmp += " REGRESSION"
				}
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.BenchmarkID, s.Profile, strings.Join(s.nodes, ", "),
				lo, mbs, pf, dc, sd, mem, s.Outcome, cmp)
		}
		w.Write([]byte(b.String()))
	default:
		writeErr(w, 400, fmt.Sprintf("format must be json, csv or md, got %q", f), "bad request")
	}
}

func (a *benchAPI) trends(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, metric := q.Get("benchmark"), q.Get("metric")
	if id == "" {
		writeErr(w, 400, "benchmark is required", "bad request")
		return
	}
	if metric == "" {
		metric = "load_s"
	}
	known := false
	for _, d := range metricDefs {
		known = known || d.name == metric
	}
	if !known {
		writeErr(w, 400, fmt.Sprintf("metric must be one of load_s, load_mb_s, decode_tps, prefill_tps, ttft_ms, memory_mib, unload_s, concurrent_wall_s, got %q", metric), "bad request")
		return
	}
	limit := qInt(r, "limit", 50)
	a.store.mu.Lock()
	pts := []map[string]any{}
	for _, run := range a.store.runs {
		if run.State != bench.RunCompleted && run.State != bench.RunFailed {
			continue
		}
		for _, s := range run.Steps {
			if s.BenchmarkID == id {
				if v, ok := stepValues(s)[metric]; ok {
					pts = append(pts, map[string]any{"run_id": run.ID, "started_at": run.StartedAt, "value": v})
				}
			}
		}
	}
	a.store.mu.Unlock()
	if len(pts) > limit {
		pts = pts[len(pts)-limit:]
	}
	writeJSON(w, 200, map[string]any{"benchmark": id, "metric": metric, "points": pts})
}

func (a *benchAPI) benchProfiles(w http.ResponseWriter, r *http.Request) {
	type pr struct {
		ID      string   `json:"id"`
		Kind    string   `json:"kind"`
		Aliases []string `json:"aliases,omitempty"`
	}
	out := []pr{}
	for _, p := range profiles {
		out = append(out, pr{ID: p.ID, Kind: "llama", Aliases: p.Aliases})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"profiles": out})
}

// profileRefs lists the saved benchmarks that use a profile (the config
// mock refuses to delete it without ?force=1).
func (s *benchStore) profileRefs(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, bid := range sortedKeysOf(s.benchmarks) {
		if p, _ := s.benchmarks[bid]["profile"].(string); p == id {
			out = append(out, bid)
		}
	}
	return out
}
