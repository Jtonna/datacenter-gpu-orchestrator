package api

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// Groups are suites (decision D24: old suites read as groups); the same
	// routes under the new name.
	m.HandleFunc("GET /api/bench/groups", g(s.listSuites))
	m.HandleFunc("POST /api/bench/groups", g(s.createSuite))
	m.HandleFunc("GET /api/bench/groups/{id}", g(s.getSuite))
	m.HandleFunc("PUT /api/bench/groups/{id}", g(s.putSuite))
	m.HandleFunc("DELETE /api/bench/groups/{id}", g(s.deleteSuite))
	m.HandleFunc("POST /api/bench/groups/{id}/run", g(s.runSuite))
	m.HandleFunc("GET /api/bench/groups/{id}/expand", g(s.expandGroup))
	m.HandleFunc("GET /api/bench/suites/{id}/expand", g(s.expandGroup))
	// Transcripts (data model step 4).
	m.HandleFunc("GET /api/bench/runs/{id}/transcripts", g(s.listTranscripts))
	m.HandleFunc("GET /api/bench/runs/{id}/steps/{step}/transcript", g(s.getTranscript))
	// Reports, generated when opened.
	m.HandleFunc("GET /api/bench/runs/{id}/report", g(s.getReport))
}

// getReport is GET /api/bench/runs/{id}/report: the run's own report (the
// summary of a group run, the benchmark report of a direct run), ?step=N for
// the benchmark report of a step, &phase=P for one section; ?format=json
// (default), md or html (a page with a print layout).
func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query()
	var o bench.ReportOptions
	for _, p := range []struct {
		name string
		dst  **int
	}{{"step", &o.Step}, {"phase", &o.Phase}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				badParam(w, "%s must be an index (0, 1, ...), got %q", p.name, v)
				return
			}
			*p.dst = &n
		}
	}
	if o.Phase != nil && o.Step == nil {
		badParam(w, "phase needs step")
		return
	}
	format := q.Get("format")
	switch format {
	case "", "json", "md", "markdown", "html":
	default:
		badParam(w, "format must be json, md or html, got %q", format)
		return
	}
	run, err := s.opts.BenchStore.GetRun(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	ms, err := s.opts.BenchStore.RunMeasurements(id, bench.MeasureQuery{})
	if err != nil {
		s.storeErr(w, err)
		return
	}
	list, err := s.opts.BenchStore.Transcripts(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	o.Transcripts = map[int]bool{}
	for _, t := range list {
		o.Transcripts[t.Step] = true
	}
	rep, err := bench.BuildReport(run, ms, o)
	if errors.Is(err, bench.ErrNotFound) {
		apiErr(w, http.StatusNotFound, "%v", strings.TrimPrefix(err.Error(), bench.ErrNotFound.Error()+": "))
		return
	}
	if err != nil {
		s.storeErr(w, err)
		return
	}
	name := id
	if o.Step != nil {
		name += fmt.Sprintf("-step-%d", *o.Step+1)
	}
	if o.Phase != nil {
		name += fmt.Sprintf("-phase-%d", *o.Phase+1)
	}
	switch format {
	case "md", "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", name+"-report.md"))
		w.Write([]byte(bench.ReportMarkdown(rep)))
	case "html":
		mq := r.URL.Query()
		mq.Set("format", "md")
		page, err := bench.ReportHTML(rep, r.URL.Path+"?"+mq.Encode())
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "the report cannot be drawn: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.Write(page)
	default:
		writeJSON(w, http.StatusOK, rep)
	}
}

// listTranscripts is GET /api/bench/runs/{id}/transcripts: the steps that
// saved a transcript, with its size.
func (s *Server) listTranscripts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.runExists(w, id) {
		return
	}
	list, err := s.opts.BenchStore.Transcripts(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "transcripts": list})
}

// getTranscript is GET /api/bench/runs/{id}/steps/{step}/transcript
// (?format=json, the stored file, or md, a readable chat).
func (s *Server) getTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	step, err := strconv.Atoi(r.PathValue("step"))
	if err != nil || step < 0 {
		badParam(w, "step must be a step index (0, 1, ...), got %q", r.PathValue("step"))
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "md" && format != "markdown" {
		badParam(w, "format must be json or md, got %q", format)
		return
	}
	if !s.runExists(w, id) {
		return
	}
	raw, err := s.opts.BenchStore.ReadTranscript(id, step)
	if errors.Is(err, bench.ErrNotFound) {
		apiErr(w, http.StatusNotFound, "%v", strings.TrimPrefix(err.Error(), bench.ErrNotFound.Error()+": "))
		return
	}
	if err != nil {
		s.storeErr(w, err)
		return
	}
	if format == "md" || format == "markdown" {
		var t bench.Transcript
		if err := json.Unmarshal(raw, &t); err != nil {
			apiErr(w, http.StatusInternalServerError, "the transcript cannot be read: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", fmt.Sprintf("%s-step-%d-transcript.md", id, step+1)))
		w.Write([]byte(bench.TranscriptMarkdown(&t)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

func (s *Server) groupLookup(id string) (*benchspec.Suite, bool) {
	su, err := s.opts.BenchStore.GetSuite(id)
	if err != nil {
		return nil, false
	}
	return su.Spec, true
}

// expandGroup is GET /api/bench/groups/{id}/expand: the flat list a run of
// the group would execute now (nested groups spliced in, one entry per use
// of a recipe), without running anything.
func (s *Server) expandGroup(w http.ResponseWriter, r *http.Request) {
	su, err := s.opts.BenchStore.GetSuite(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err)
		return
	}
	flat, err := benchspec.ExpandGroup(su.Spec, s.lookup, s.groupLookup)
	if err != nil {
		apiErr(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	entries := make([]map[string]any, 0, len(flat.Benchmarks))
	for _, it := range flat.Benchmarks {
		b := it.Inline
		if b == nil {
			b, _ = s.lookup(it.Ref)
		}
		e := map[string]any{"benchmark_id": it.Ref}
		if b != nil {
			e["benchmark_id"], e["name"], e["profile"] = b.ID, b.Title(), b.Profile
			if b.Recipe != "" {
				e["recipe"], e["inputs"] = b.Recipe, b.Applied
			}
			if b.IsParallel() {
				e["parallel"] = b.Parallel
			}
		}
		entries = append(entries, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": su.Spec.ID, "count": len(entries), "entries": entries})
}

// runBenchmark is POST /api/bench/benchmarks/{id}/run: one benchmark on its
// own, as a run of kind "benchmark".
func (s *Server) runBenchmark(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown(w) {
		return
	}
	var req struct {
		Baseline     string            `json:"baseline"`
		Note         string            `json:"note"`
		EvictOthers  *bool             `json:"evict_others"`
		OnlyWhenIdle *bool             `json:"only_when_idle"`
		IdleWaitS    *int              `json:"idle_wait_s"`
		Inputs       *benchspec.Inputs `json:"inputs"`
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := benchspec.DecodeStrict(body, &req); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be empty or {\"note\": \"...\", \"baseline\": \"<run id>\", \"only_when_idle\": true, \"idle_wait_s\": 600, \"evict_others\": false, \"inputs\": {\"model\": \"...\", \"ctx\": 8192, \"placement\": \"any:2\"}}: %v", err)
			return
		}
	}
	if req.Inputs != nil {
		// The inputs alone, checked as a recipe's would be.
		probe := benchspec.Benchmark{ID: "inputs", Inputs: req.Inputs, Phases: []benchspec.Phase{{Type: "load"}}}
		if err := probe.Validate(nil); err != nil {
			apiErr(w, http.StatusUnprocessableEntity, "%v", err)
			return
		}
	}
	id := r.PathValue("id")
	run, err := s.opts.Bench.EnqueueBenchmark(id, bench.BenchRunOptions{Baseline: req.Baseline, Note: req.Note, EvictOthers: req.EvictOthers,
		OnlyWhenIdle: req.OnlyWhenIdle, IdleWaitS: req.IdleWaitS, Inputs: req.Inputs})
	if err != nil {
		s.storeErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/bench/runs/"+run.ID)
	s.audit(r, "run_start", "benchmark:"+id, nil, map[string]any{"run_id": run.ID, "baseline": req.Baseline, "note": req.Note, "inputs": req.Inputs})
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
	f := bench.MeasureFilter{Run: q.Get("run"), Benchmark: q.Get("benchmark"), Profile: q.Get("profile"), Model: q.Get("model"), Node: q.Get("node"), Kind: q.Get("kind"),
		Variant: q.Get("variant"), Placement: q.Get("placement")}
	if v := q.Get("ctx"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badParam(w, "ctx must be a context size in tokens, got %q", v)
			return
		}
		f.Ctx = n
	}
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
