package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// The tasks phase (release 0.1.8): task sets, the answers a run stored, and
// the scores a scorer outside Potluck posts back. Changes are audited as
// section "bench", targets "task_set:<id>@<version>" and
// "scores:<run>/<scorer>".

func (s *Server) taskRoutes(m *http.ServeMux) {
	g := s.benchGuard
	m.HandleFunc("GET /api/bench/task-sets", g(s.listTaskSets))
	m.HandleFunc("POST /api/bench/task-sets", g(s.createTaskSet))
	m.HandleFunc("GET /api/bench/task-sets/{id}", g(s.getTaskSet))
	m.HandleFunc("DELETE /api/bench/task-sets/{id}", g(s.deleteTaskSet))
	m.HandleFunc("GET /api/bench/runs/{id}/tasks", g(s.runTasks))
	m.HandleFunc("GET /api/bench/runs/{id}/tasks/{result}", g(s.runTask))
	m.HandleFunc("GET /api/bench/runs/{id}/scores", g(s.runScores))
	m.HandleFunc("PUT /api/bench/runs/{id}/scores/{scorer}", g(s.putScores))
	m.HandleFunc("DELETE /api/bench/runs/{id}/scores/{scorer}", g(s.deleteScores))
}

// taskSetJSON is a stored set with its hash and when it was stored.
func taskSetJSON(t *bench.StoredTaskSet) map[string]any {
	b, _ := json.Marshal(t.Set)
	m := map[string]any{}
	json.Unmarshal(b, &m)
	m["sha256"], m["created_at"] = t.SHA256, t.CreatedAt
	return m
}

func taskSetTarget(id string, version int) string { return fmt.Sprintf("task_set:%s@%d", id, version) }

// versionParam reads ?version= (0 when absent).
func versionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("version")
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		badParam(w, "version must be a whole number from 1, got %q", v)
		return 0, false
	}
	return n, true
}

func (s *Server) listTaskSets(w http.ResponseWriter, r *http.Request) {
	all, err := s.opts.BenchStore.ListTaskSets()
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_sets": all})
}

func (s *Server) getTaskSet(w http.ResponseWriter, r *http.Request) {
	v, ok := versionParam(w, r)
	if !ok {
		return
	}
	t, err := s.opts.BenchStore.GetTaskSet(r.PathValue("id"), v)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskSetJSON(t))
}

// createTaskSet stores a task set version: 201 when new, 200 when the same
// content is already stored, 409 when the version is stored with other prompts.
func (s *Server) createTaskSet(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	ts, err := benchspec.ParseTaskSetJSON(body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "the body is not a valid task set: %v", err)
		return
	}
	if err := ts.Validate(); err != nil {
		apiErr(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	created, err := s.opts.BenchStore.SaveTaskSet(ts, s.opts.Now().UnixMilli())
	if err != nil {
		s.storeErr(w, err)
		return
	}
	got, err := s.opts.BenchStore.GetTaskSet(ts.ID, ts.Version)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	out := taskSetJSON(got)
	out["created"] = created
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", fmt.Sprintf("/api/bench/task-sets/%s?version=%d", ts.ID, ts.Version))
		s.audit(r, "create", taskSetTarget(ts.ID, ts.Version), nil, map[string]any{"id": ts.ID, "version": ts.Version, "sha256": got.SHA256, "title": ts.Title, "tasks": len(ts.Tasks)})
	}
	writeJSON(w, status, out)
}

func (s *Server) deleteTaskSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := versionParam(w, r)
	if !ok {
		return
	}
	if v == 0 {
		badParam(w, "name the version to delete: ?version=N")
		return
	}
	old, err := s.opts.BenchStore.GetTaskSet(id, v)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	if err := s.opts.BenchStore.DeleteTaskSet(id, v); err != nil {
		s.storeErr(w, err)
		return
	}
	s.audit(r, "delete", taskSetTarget(id, v), map[string]any{"id": id, "version": v, "sha256": old.SHA256, "title": old.Set.Title, "tasks": len(old.Set.Tasks)}, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "version": v})
}

// checkTaskSets answers 422 when a tasks phase names a task set that is not stored.
func (s *Server) checkTaskSets(w http.ResponseWriter, b *benchspec.Benchmark, where string) bool {
	for _, ref := range b.TaskSetRefs() {
		if _, err := s.opts.BenchStore.GetTaskSet(ref.ID, ref.Version); err != nil {
			switch {
			case !errors.Is(err, bench.ErrNotFound):
				s.storeErr(w, err)
			case ref.Version != 0:
				apiErr(w, http.StatusUnprocessableEntity, "%stask set %q version %d is not stored", where, ref.ID, ref.Version)
			default:
				apiErr(w, http.StatusUnprocessableEntity, "%stask set %q is not stored: POST it to /api/bench/task-sets first", where, ref.ID)
			}
			return false
		}
	}
	return true
}

// ---- answers ----

func (s *Server) runExists(w http.ResponseWriter, id string) bool {
	if _, err := s.opts.BenchStore.GetRun(id); err != nil {
		s.storeErr(w, err)
		return false
	}
	return true
}

// runTasks is GET /api/bench/runs/{id}/tasks: every task result of the run
// with its scores and each scorer's total; ?step= one step, ?answers=1 with
// the answer and reasoning texts.
func (s *Server) runTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := bench.TaskQuery{}
	if v := r.URL.Query().Get("step"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badParam(w, "step must be a step index (0, 1, ...), got %q", v)
			return
		}
		q.Step = &n
	}
	switch v := r.URL.Query().Get("answers"); v {
	case "", "0", "false":
	case "1", "true":
		q.Answers = true
	default:
		badParam(w, "answers must be 1 or 0, got %q", v)
		return
	}
	if !s.runExists(w, id) {
		return
	}
	list, err := s.opts.BenchStore.RunTasks(id, q)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	totals, err := s.opts.BenchStore.ScoreTotals(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "tasks": list, "scores": totals})
}

// runTask is GET /api/bench/runs/{id}/tasks/{result}: one answer in full.
// ?format=text answers the answer text alone, ?format=reasoning the
// reasoning text alone, as text/plain.
func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	rid, err := strconv.ParseInt(r.PathValue("result"), 10, 64)
	if err != nil || rid < 1 {
		badParam(w, "the result id must be a number, got %q", r.PathValue("result"))
		return
	}
	format := r.URL.Query().Get("format")
	switch format {
	case "", "json", "text", "reasoning":
	default:
		badParam(w, "format must be json, text or reasoning, got %q", format)
		return
	}
	t, err := s.opts.BenchStore.TaskResult(r.PathValue("id"), rid)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	switch format {
	case "text", "reasoning":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		text := t.Answer
		if format == "reasoning" {
			text = t.Reasoning
		}
		io.WriteString(w, text)
	default:
		writeJSON(w, http.StatusOK, t)
	}
}

// ---- scores ----

func (s *Server) runScores(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.runExists(w, id) {
		return
	}
	totals, err := s.opts.BenchStore.ScoreTotals(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "scores": totals})
}

func scorerTotal(all []bench.ScorerTotal, scorer string) any {
	for i := range all {
		if all[i].Scorer == scorer {
			return all[i]
		}
	}
	return nil
}

// putScores is PUT /api/bench/runs/{id}/scores/{scorer}: the scorer's scores
// for the run's tasks, replacing whatever it posted before.
func (s *Server) putScores(w http.ResponseWriter, r *http.Request) {
	id, scorer := r.PathValue("id"), r.PathValue("scorer")
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var doc bench.ScoresDoc
	if err := benchspec.DecodeStrict(body, &doc); err != nil {
		apiErr(w, http.StatusBadRequest, "the body must be {\"scorer_version\": \"...\", \"scores\": [{\"step\": 0, \"task\": \"t1\", \"points\": 17, \"max_points\": 17}]}: %v", err)
		return
	}
	before, _ := s.opts.BenchStore.ScoreTotals(id)
	tot, err := s.opts.BenchStore.PutScores(id, scorer, doc, s.opts.Now().UnixMilli())
	if err != nil {
		s.storeErr(w, err)
		return
	}
	s.audit(r, "update", "scores:"+id+"/"+scorer, scorerTotal(before, scorer), tot)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "scorer": scorer, "total": tot})
}

func (s *Server) deleteScores(w http.ResponseWriter, r *http.Request) {
	id, scorer := r.PathValue("id"), r.PathValue("scorer")
	before, _ := s.opts.BenchStore.ScoreTotals(id)
	if err := s.opts.BenchStore.DeleteScores(id, scorer); err != nil {
		s.storeErr(w, err)
		return
	}
	s.audit(r, "delete", "scores:"+id+"/"+scorer, scorerTotal(before, scorer), nil)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "deleted": strings.TrimSpace(scorer)})
}
