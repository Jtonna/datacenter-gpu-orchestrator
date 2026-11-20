package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// The benchmark REST endpoints (design 9.3). They are pull and push only: the
// live progress of a run goes over /ws. Every one of them answers 503 when the
// benchmark feature is off (no data_dir, or the database could not be opened).

// SystemInfo is what GET /api/system reports about the configuration; the
// sizes are read when asked.
type SystemInfo struct {
	Listen      string
	Safety      map[string]any
	HistoryDir  string
	DataDir     string
	ChildLogDir string
	ModelsDir   string
	Bench       map[string]any
}

// AuditHook records who changed what in the benchmark section. The audit log
// is its own package (built on another branch): its recorder implements this
// and is plugged in with SetAuditHook. Record is called after the change is
// stored, with the request (for the caller and its address), an action
// (create, update, delete, run_start, run_cancel), the section (always
// "bench"), the target ("benchmark:<id>", "suite:<id>", "run:<id>") and the
// state before and after (nil when there is none).
type AuditHook interface {
	Record(r *http.Request, action, section, target string, before, after any)
}

// NoAudit is the default hook: it records nothing.
type NoAudit struct{}

// Record implements AuditHook.
func (NoAudit) Record(*http.Request, string, string, string, any, any) {}

var auditHooks sync.Map // *Server -> AuditHook

// SetAuditHook plugs the audit recorder into the benchmark endpoints (nil
// restores the no-op default). The hook is kept beside the server rather than
// in its struct, so the benchmark code stays in its own files.
func (s *Server) SetAuditHook(h AuditHook) {
	if h == nil {
		auditHooks.Delete(s)
		return
	}
	auditHooks.Store(s, h)
}

func (s *Server) audit(r *http.Request, action, target string, before, after any) {
	h, ok := auditHooks.Load(s)
	if !ok {
		return
	}
	h.(AuditHook).Record(r, action, "bench", target, before, after)
}

func (s *Server) benchOn() bool { return s.opts.Bench != nil && s.opts.BenchStore != nil }

const benchOffMessage = "the benchmark feature is off: set data_dir in the config to a writable directory (compose mounts ./data at /var/lib/llm-scheduler/data) and keep bench.enabled on; the scheduler log says why the database could not be opened"

// benchGuard answers 503 when the feature is off.
func (s *Server) benchGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.benchOn() {
			w.Header().Set("Retry-After", "0")
			proxy.WriteError(w, http.StatusServiceUnavailable, benchOffMessage, "server_error", "benchmarks off", nil)
			return
		}
		h(w, r)
	}
}

func (s *Server) benchRoutes(m *http.ServeMux) {
	g := s.benchGuard
	if s.benchOn() { // the progress shows a profile's model file before its instance exists
		s.opts.Bench.SetModelFile(func(profile string) string {
			if p, _, ok := s.cfg.Resolve(profile); ok {
				return p.Model
			}
			return ""
		})
	}
	m.HandleFunc("GET /api/bench/benchmarks", g(s.listBenchmarks))
	m.HandleFunc("POST /api/bench/benchmarks", g(s.createBenchmark))
	m.HandleFunc("GET /api/bench/benchmarks/{id}", g(s.getBenchmark))
	m.HandleFunc("GET /api/bench/benchmarks/{id}/results", g(s.benchmarkResults))
	m.HandleFunc("PUT /api/bench/benchmarks/{id}", g(s.putBenchmark))
	m.HandleFunc("DELETE /api/bench/benchmarks/{id}", g(s.deleteBenchmark))
	m.HandleFunc("GET /api/bench/suites", g(s.listSuites))
	m.HandleFunc("POST /api/bench/suites", g(s.createSuite))
	m.HandleFunc("GET /api/bench/suites/{id}", g(s.getSuite))
	m.HandleFunc("PUT /api/bench/suites/{id}", g(s.putSuite))
	m.HandleFunc("DELETE /api/bench/suites/{id}", g(s.deleteSuite))
	m.HandleFunc("POST /api/bench/suites/{id}/run", g(s.runSuite))
	m.HandleFunc("GET /api/bench/runs", g(s.listRuns))
	m.HandleFunc("GET /api/bench/runs/{id}", g(s.getRun))
	m.HandleFunc("POST /api/bench/runs/{id}/cancel", g(s.cancelRun))
	m.HandleFunc("GET /api/bench/runs/{id}/export", g(s.exportRun))
	m.HandleFunc("GET /api/bench/trends", g(s.trends))
	m.HandleFunc("GET /api/bench/aggregates", g(s.aggregates))
	m.HandleFunc("GET /api/bench/schema", g(s.benchSchema))
	m.HandleFunc("GET /api/bench/state", g(s.benchState))
	m.HandleFunc("GET /api/bench/profiles", g(s.benchProfiles))
	m.HandleFunc("GET /api/system", s.handleSystem)
}

func apiErr(w http.ResponseWriter, status int, format string, args ...any) {
	proxy.WriteError(w, status, fmt.Sprintf(format, args...), errType(status), http.StatusText(status), nil)
}

// readBody reads a JSON body of at most 4 MiB.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		apiErr(w, http.StatusRequestEntityTooLarge, "the body is larger than 4 MiB or unreadable: %v", err)
		return nil, false
	}
	return b, true
}

func (s *Server) profileOK(id string) bool {
	_, _, ok := s.conf().Resolve(id)
	return ok
}

func (s *Server) lookup(id string) (*benchspec.Benchmark, bool) {
	b, err := s.opts.BenchStore.GetBenchmark(id)
	if err != nil {
		return nil, false
	}
	return b.Spec, true
}

func flatten(spec any, source string, created, updated int64) map[string]any {
	b, _ := json.Marshal(spec)
	m := map[string]any{}
	json.Unmarshal(b, &m)
	m["source"], m["created_at"], m["updated_at"] = source, created, updated
	return m
}

func benchmarkJSON(b *bench.StoredBenchmark) map[string]any {
	return flatten(b.Spec, b.Source, b.CreatedAt, b.UpdatedAt)
}

func suiteJSON(su *bench.StoredSuite) map[string]any {
	return flatten(su.Spec, su.Source, su.CreatedAt, su.UpdatedAt)
}

func (s *Server) storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bench.ErrNotFound):
		apiErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, bench.ErrInvalid):
		apiErr(w, http.StatusUnprocessableEntity, "%v", strings.TrimPrefix(err.Error(), "invalid: "))
	case errors.Is(err, bench.ErrQueueFull):
		w.Header().Set("Retry-After", "60")
		apiErr(w, http.StatusTooManyRequests, "%v: wait for a run to finish or cancel one", err)
	default:
		s.log.Warn("benchmark database error", "err", err)
		apiErr(w, http.StatusInternalServerError, "database error: %v", err)
	}
}

// ---- benchmarks ----

func (s *Server) listBenchmarks(w http.ResponseWriter, r *http.Request) {
	all, err := s.opts.BenchStore.ListBenchmarks()
	if err != nil {
		s.storeErr(w, err)
		return
	}
	out := make([]any, 0, len(all))
	for i := range all {
		out = append(out, benchmarkJSON(&all[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmarks": out})
}

func (s *Server) getBenchmark(w http.ResponseWriter, r *http.Request) {
	b, err := s.opts.BenchStore.GetBenchmark(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, benchmarkJSON(b))
}

// benchmarkResults is GET /api/bench/benchmarks/{id}/results: what the
// benchmark did in each stored run, oldest first (?limit= runs, default 200).
func (s *Server) benchmarkResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > bench.MaxAggRuns {
			badParam(w, "limit must be 1 to %d, got %q", bench.MaxAggRuns, v)
			return
		}
		limit = n
	}
	res, err := s.opts.BenchStore.BenchmarkResults(id, limit)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmark": id, "results": res})
}

// parseBenchmark decodes and validates a benchmark body; wantID, when not
// empty, is the id the path names.
func (s *Server) parseBenchmark(w http.ResponseWriter, r *http.Request, wantID string) (*benchspec.Benchmark, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	b, err := benchspec.ParseBenchmarkJSON(body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "the body is not a valid benchmark: %v", err)
		return nil, false
	}
	if wantID != "" {
		if b.ID == "" {
			b.ID = wantID
		} else if b.ID != wantID {
			apiErr(w, http.StatusBadRequest, "the id in the body (%q) differs from the id in the path (%q)", b.ID, wantID)
			return nil, false
		}
	}
	if err := b.Validate(s.profileOK); err != nil {
		apiErr(w, http.StatusUnprocessableEntity, "%v", err)
		return nil, false
	}
	for _, id := range b.Parallel {
		m, ok := s.lookup(id)
		switch {
		case !ok:
			apiErr(w, http.StatusUnprocessableEntity, "parallel member %q is not a saved benchmark", id)
			return nil, false
		case m.IsParallel():
			apiErr(w, http.StatusUnprocessableEntity, "parallel member %q is itself a parallel step", id)
			return nil, false
		}
	}
	return b, true
}

func (s *Server) createBenchmark(w http.ResponseWriter, r *http.Request) {
	b, ok := s.parseBenchmark(w, r, "")
	if !ok {
		return
	}
	if _, err := s.opts.BenchStore.GetBenchmark(b.ID); err == nil {
		apiErr(w, http.StatusConflict, "benchmark %q already exists: use PUT /api/bench/benchmarks/%s to replace it", b.ID, b.ID)
		return
	}
	now := s.opts.Now().UnixMilli()
	if err := s.opts.BenchStore.SaveBenchmark(b, bench.SourceAPI, now); err != nil {
		s.storeErr(w, err)
		return
	}
	got, _ := s.opts.BenchStore.GetBenchmark(b.ID)
	w.Header().Set("Location", "/api/bench/benchmarks/"+b.ID)
	s.audit(r, "create", "benchmark:"+b.ID, nil, benchmarkJSON(got))
	writeJSON(w, http.StatusCreated, benchmarkJSON(got))
}

func (s *Server) putBenchmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.opts.BenchStore.GetBenchmark(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	b, ok := s.parseBenchmark(w, r, id)
	if !ok {
		return
	}
	// Editing an item the config seeded makes it an api item: config seeding never overwrites it again.
	if err := s.opts.BenchStore.SaveBenchmark(b, bench.SourceAPI, s.opts.Now().UnixMilli()); err != nil {
		s.storeErr(w, err)
		return
	}
	got, _ := s.opts.BenchStore.GetBenchmark(id)
	s.audit(r, "update", "benchmark:"+id, benchmarkJSON(old), benchmarkJSON(got))
	writeJSON(w, http.StatusOK, benchmarkJSON(got))
}

func (s *Server) deleteBenchmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.opts.BenchStore.GetBenchmark(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	suites, _ := s.opts.BenchStore.SuitesUsing(id)
	steps, _ := s.opts.BenchStore.BenchmarksUsing(id)
	if len(suites)+len(steps) > 0 {
		apiErr(w, http.StatusConflict, "benchmark %q is used by suites %v and parallel steps %v: remove it from them first", id, suites, steps)
		return
	}
	if err := s.opts.BenchStore.DeleteBenchmark(id); err != nil {
		s.storeErr(w, err)
		return
	}
	s.audit(r, "delete", "benchmark:"+id, benchmarkJSON(old), nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---- suites ----

func (s *Server) listSuites(w http.ResponseWriter, r *http.Request) {
	all, err := s.opts.BenchStore.ListSuites()
	if err != nil {
		s.storeErr(w, err)
		return
	}
	out := make([]any, 0, len(all))
	for i := range all {
		m := suiteJSON(&all[i])
		if runs, err := s.opts.BenchStore.ListRuns(bench.RunFilter{Suite: all[i].Spec.ID, Limit: 1}); err == nil && len(runs) > 0 {
			m["last_run"] = runs[0]
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"suites": out})
}

func (s *Server) getSuite(w http.ResponseWriter, r *http.Request) {
	su, err := s.opts.BenchStore.GetSuite(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, suiteJSON(su))
}

func (s *Server) parseSuite(w http.ResponseWriter, r *http.Request, wantID string) (*benchspec.Suite, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	su, err := benchspec.ParseSuiteJSON(body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "the body is not a valid suite: %v", err)
		return nil, false
	}
	if wantID != "" {
		if su.ID == "" {
			su.ID = wantID
		} else if su.ID != wantID {
			apiErr(w, http.StatusBadRequest, "the id in the body (%q) differs from the id in the path (%q)", su.ID, wantID)
			return nil, false
		}
	}
	if err := su.Validate(s.profileOK, s.lookup); err != nil {
		apiErr(w, http.StatusUnprocessableEntity, "%v", err)
		return nil, false
	}
	return su, true
}

func (s *Server) createSuite(w http.ResponseWriter, r *http.Request) {
	su, ok := s.parseSuite(w, r, "")
	if !ok {
		return
	}
	if _, err := s.opts.BenchStore.GetSuite(su.ID); err == nil {
		apiErr(w, http.StatusConflict, "suite %q already exists: use PUT /api/bench/suites/%s to replace it", su.ID, su.ID)
		return
	}
	if err := s.opts.BenchStore.SaveSuite(su, bench.SourceAPI, s.opts.Now().UnixMilli()); err != nil {
		s.storeErr(w, err)
		return
	}
	got, _ := s.opts.BenchStore.GetSuite(su.ID)
	w.Header().Set("Location", "/api/bench/suites/"+su.ID)
	s.audit(r, "create", "suite:"+su.ID, nil, suiteJSON(got))
	writeJSON(w, http.StatusCreated, suiteJSON(got))
}

func (s *Server) putSuite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.opts.BenchStore.GetSuite(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	su, ok := s.parseSuite(w, r, id)
	if !ok {
		return
	}
	if err := s.opts.BenchStore.SaveSuite(su, bench.SourceAPI, s.opts.Now().UnixMilli()); err != nil {
		s.storeErr(w, err)
		return
	}
	got, _ := s.opts.BenchStore.GetSuite(id)
	s.audit(r, "update", "suite:"+id, suiteJSON(old), suiteJSON(got))
	writeJSON(w, http.StatusOK, suiteJSON(got))
}

func (s *Server) deleteSuite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, _ := s.opts.BenchStore.GetSuite(id)
	if err := s.opts.BenchStore.DeleteSuite(id); err != nil {
		s.storeErr(w, err)
		return
	}
	if old != nil {
		s.audit(r, "delete", "suite:"+id, suiteJSON(old), nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) runSuite(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown(w) {
		return
	}
	var req struct {
		Baseline string `json:"baseline"`
		Note     string `json:"note"`
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := benchspec.DecodeStrict(body, &req); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be empty or {\"baseline\": \"<run id>\", \"note\": \"...\"}: %v", err)
			return
		}
	}
	run, err := s.opts.Bench.Enqueue(r.PathValue("id"), bench.EnqueueOptions{Baseline: req.Baseline, Note: req.Note})
	if err != nil {
		s.storeErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/bench/runs/"+run.ID)
	s.audit(r, "run_start", "suite:"+r.PathValue("id"), nil, map[string]any{"run_id": run.ID, "baseline": req.Baseline, "note": req.Note})
	writeJSON(w, http.StatusAccepted, run)
}

// ---- runs ----

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := bench.RunFilter{Suite: q.Get("suite"), State: q.Get("state")}
	switch f.State {
	case "", bench.RunQueued, bench.RunRunning, bench.RunPaused, bench.RunCompleted, bench.RunFailed, bench.RunCancelled, bench.RunSkipped:
	default:
		badParam(w, "state must be queued, running, paused, completed, failed, cancelled or skipped, got %q", f.State)
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			badParam(w, "limit must be 1 to 1000, got %q", v)
			return
		}
		f.Limit = n
	}
	runs, err := s.opts.BenchStore.ListRuns(f)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	if runs == nil {
		runs = []bench.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.opts.BenchStore.GetRun(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var before any
	if run, err := s.opts.BenchStore.GetRun(id); err == nil {
		before = map[string]any{"run_id": id, "suite_id": run.SuiteID, "state": run.State}
	}
	if err := s.opts.Bench.Cancel(id); err != nil {
		if errors.Is(err, bench.ErrInvalid) {
			apiErr(w, http.StatusConflict, "%v", strings.TrimPrefix(err.Error(), "invalid: "))
			return
		}
		s.storeErr(w, err)
		return
	}
	s.audit(r, "run_cancel", "run:"+id, before, map[string]any{"cancelling": id})
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelling": id, "note": "a running run stops after its current request and unloads what it loaded"})
}

func (s *Server) exportRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.opts.BenchStore.GetRun(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err)
		return
	}
	h := w.Header()
	switch f := r.URL.Query().Get("format"); f {
	case "", bench.FormatJSON:
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, run.ID))
		writeJSON(w, http.StatusOK, run)
	case bench.FormatCSV:
		var out string
		var err error
		switch d := r.URL.Query().Get("detail"); d {
		case "":
			out, err = bench.ExportCSV(run)
		case "phases":
			out, err = bench.ExportCSVPhases(run)
		case "turns":
			out, err = bench.ExportCSVTurns(run)
		default:
			badParam(w, "detail must be phases or turns (or empty for one row per benchmark), got %q", d)
			return
		}
		if err != nil {
			s.storeErr(w, err)
			return
		}
		h.Set("Content-Type", "text/csv; charset=utf-8")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, run.ID))
		io.WriteString(w, out)
	case bench.FormatMD:
		h.Set("Content-Type", "text/markdown; charset=utf-8")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, run.ID))
		io.WriteString(w, bench.ExportMarkdown(run))
	default:
		badParam(w, "format must be json, csv or md, got %q", f)
	}
}

func (s *Server) trends(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, metric := q.Get("benchmark"), q.Get("metric")
	if id == "" {
		badParam(w, "benchmark is required")
		return
	}
	if metric == "" {
		metric = "load_s"
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			badParam(w, "limit must be 1 to 1000, got %q", v)
			return
		}
		limit = n
	}
	pts, err := s.opts.BenchStore.Trend(id, metric, limit)
	if err != nil {
		badParam(w, "%v", err)
		return
	}
	if pts == nil {
		pts = []bench.TrendPoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmark": id, "metric": metric, "points": pts})
}

// aggregates is GET /api/bench/aggregates: what the stored runs add up to.
func (s *Server) aggregates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.opts.Now()
	f := bench.AggFilter{Suite: q.Get("suite"), Benchmark: q.Get("benchmark"), Profile: q.Get("profile")}
	var err error
	if f.Since, err = bench.ParseAggTime(q.Get("since"), now); err != nil {
		badParam(w, "since: %v", err)
		return
	}
	if f.Until, err = bench.ParseAggTime(q.Get("until"), now); err != nil {
		badParam(w, "until: %v", err)
		return
	}
	if v := q.Get("limit_runs"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > bench.MaxAggRuns {
			badParam(w, "limit_runs must be 1 to %d, got %q", bench.MaxAggRuns, v)
			return
		}
		f.LimitRuns = n
	}
	switch v := q.Get("env"); v {
	case "":
	case "same":
		f.EnvSame = true
	default:
		badParam(w, "env must be same (or left out), got %q", v)
		return
	}
	doc, err := s.opts.Bench.Aggregates(f)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// benchSchema is GET /api/bench/schema: the phase types, their options, the
// expect keys and some ready-made benchmarks, so that an editor can be drawn
// without knowing any of them.
func (s *Server) benchSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, benchspec.Schema())
}

func (s *Server) benchState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opts.Bench.State())
}

// benchProfiles lists the profile ids a benchmark may name (for editors).
func (s *Server) benchProfiles(w http.ResponseWriter, r *http.Request) {
	type pr struct {
		ID      string   `json:"id"`
		Kind    string   `json:"kind"`
		Aliases []string `json:"aliases,omitempty"`
	}
	var out []pr
	for name, p := range s.conf().Profiles {
		out = append(out, pr{ID: name, Kind: string(p.Kind), Aliases: p.Aliases})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// ---- system ----

func dirBytes(dir string) (int64, int) {
	if dir == "" {
		return 0, 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var total int64
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
			n++
		}
	}
	return total, n
}

// handleSystem is what the About view shows: the version, the safety settings
// and where the data lives with how large it is. Read-only.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	now := s.opts.Now()
	sys := s.opts.System
	storage := map[string]any{}
	if sys.HistoryDir != "" {
		b, n := dirBytes(sys.HistoryDir)
		storage["history"] = map[string]any{"dir": sys.HistoryDir, "bytes": b, "files": n, "on": s.hist.On()}
	} else {
		storage["history"] = map[string]any{"dir": "", "on": false}
	}
	db := map[string]any{"dir": sys.DataDir, "on": s.benchOn()}
	if s.benchOn() {
		db["path"] = s.opts.BenchStore.Path()
		if n, err := s.opts.BenchStore.SizeBytes(); err == nil {
			db["bytes"] = n
		}
		if v, err := s.opts.BenchStore.SchemaVersion(); err == nil {
			db["schema_version"] = v
		}
	}
	storage["database"] = db
	if sys.ChildLogDir != "" {
		b, n := dirBytes(sys.ChildLogDir)
		storage["child_logs"] = map[string]any{"dir": sys.ChildLogDir, "bytes": b, "files": n}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.opts.Version, "started_at": s.start.UnixMilli(), "now": now.UnixMilli(), "uptime_s": int64(now.Sub(s.start) / time.Second),
		"listen": sys.Listen, "models_dir": sys.ModelsDir, "safety": sys.Safety, "bench": sys.Bench, "storage": storage,
		"live_clients": s.ws.clientCount(), "live_clients_max": s.ws.max, "profiles": len(s.conf().Profiles),
		"origin_policy": "same origin, or an origin on a loopback or private-network address (the live channel has no authentication)",
	})
}
