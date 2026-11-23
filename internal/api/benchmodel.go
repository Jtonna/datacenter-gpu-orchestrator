package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// The benchmark data model (potluck-benchmark-data-model.md): runs of one
// benchmark, the measurement record and its history.

func (s *Server) modelRoutes(m *http.ServeMux) {
	g := s.benchGuard
	m.HandleFunc("POST /api/bench/benchmarks/{id}/run", g(s.runBenchmark))
	m.HandleFunc("GET /api/bench/runs/{id}/measurements", g(s.runMeasurements))
	m.HandleFunc("GET /api/bench/measurements", g(s.queryMeasurements))
	m.HandleFunc("GET /api/bench/metrics", g(s.listMetrics))
}

// runBenchmark is POST /api/bench/benchmarks/{id}/run: one benchmark on its
// own, as a run of kind "benchmark".
func (s *Server) runBenchmark(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown(w) {
		return
	}
	var req struct {
		Baseline     string `json:"baseline"`
		Note         string `json:"note"`
		EvictOthers  *bool  `json:"evict_others"`
		OnlyWhenIdle *bool  `json:"only_when_idle"`
		IdleWaitS    *int   `json:"idle_wait_s"`
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := benchspec.DecodeStrict(body, &req); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be empty or {\"note\": \"...\", \"baseline\": \"<run id>\", \"only_when_idle\": true, \"idle_wait_s\": 600, \"evict_others\": false}: %v", err)
			return
		}
	}
	id := r.PathValue("id")
	run, err := s.opts.Bench.EnqueueBenchmark(id, bench.BenchRunOptions{Baseline: req.Baseline, Note: req.Note, EvictOthers: req.EvictOthers,
		OnlyWhenIdle: req.OnlyWhenIdle, IdleWaitS: req.IdleWaitS})
	if err != nil {
		s.storeErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/bench/runs/"+run.ID)
	s.audit(r, "run_start", "benchmark:"+id, nil, map[string]any{"run_id": run.ID, "baseline": req.Baseline, "note": req.Note})
	writeJSON(w, http.StatusAccepted, run)
}

// metricsParam reads every ?metric= (also comma separated) and checks the names.
func metricsParam(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	known := bench.MetricNames()
	var out []string
	for _, v := range r.URL.Query()["metric"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			if !known[name] {
				names := make([]string, 0, len(known))
				for k := range known {
					names = append(names, k)
				}
				sort.Strings(names)
				badParam(w, "unknown metric %q: one of %s (GET /api/bench/metrics)", name, strings.Join(names, ", "))
				return nil, false
			}
			out = append(out, name)
		}
	}
	return out, true
}

// runMeasurements is GET /api/bench/runs/{id}/measurements (?step=, ?metric=).
func (s *Server) runMeasurements(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := bench.MeasureQuery{}
	if v := r.URL.Query().Get("step"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badParam(w, "step must be a step index (0, 1, ...), got %q", v)
			return
		}
		q.Step = &n
	}
	var ok bool
	if q.Metrics, ok = metricsParam(w, r); !ok {
		return
	}
	if !s.runExists(w, id) {
		return
	}
	ms, err := s.opts.BenchStore.RunMeasurements(id, q)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "measurements": ms})
}

// queryMeasurements is GET /api/bench/measurements: the history of the
// record across runs, by benchmark, profile, model, node, metric and time.
func (s *Server) queryMeasurements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := bench.MeasureFilter{Run: q.Get("run"), Benchmark: q.Get("benchmark"), Profile: q.Get("profile"), Model: q.Get("model"), Node: q.Get("node"), Kind: q.Get("kind")}
	switch f.Kind {
	case "", bench.RunKindSuite, bench.RunKindBenchmark:
	default:
		badParam(w, "kind must be suite or benchmark, got %q", f.Kind)
		return
	}
	var ok bool
	if f.Metrics, ok = metricsParam(w, r); !ok {
		return
	}
	now := s.opts.Now()
	var err error
	if f.Since, err = bench.ParseAggTime(q.Get("since"), now); err != nil {
		badParam(w, "since: %v", err)
		return
	}
	if f.Until, err = bench.ParseAggTime(q.Get("until"), now); err != nil {
		badParam(w, "until: %v", err)
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > bench.MaxMeasureRows {
			badParam(w, "limit must be 1 to %d, got %q", bench.MaxMeasureRows, v)
			return
		}
		f.Limit = n
	}
	rows, err := s.opts.BenchStore.QueryMeasurements(f)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"measurements": rows})
}

func (s *Server) listMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"metrics": bench.Metrics})
}
