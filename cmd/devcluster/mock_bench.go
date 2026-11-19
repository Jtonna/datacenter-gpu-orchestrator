// DEV-ONLY MOCK of a server contract that is not built yet: the whole
// /api/bench/* surface with benchmark phases and the new progress shape. The
// real bench package knows neither, so none of it is used here; everything
// lives in memory in cmd/devcluster.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

type mPhase struct {
	Index      int     `json:"index"`
	Type       string  `json:"type"`
	Label      string  `json:"label"`
	State      string  `json:"state"` // pending, running, done, failed, skipped, cancelled
	StartedAt  int64   `json:"started_at,omitempty"`
	FinishedAt int64   `json:"finished_at,omitempty"`
	WallMS     float64 `json:"wall_ms"`
}

type mRequest struct {
	bench.RequestResult
	PhaseIndex int `json:"phase_index"`
	Turn       int `json:"turn,omitempty"`
}

type mStep struct {
	ID          int64               `json:"id"`
	Idx         int                 `json:"idx"`
	BenchmarkID string              `json:"benchmark_id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Profile     string              `json:"profile"`
	ModelFile   string              `json:"model_file"`
	Nodes       []string            `json:"nodes"`
	State       string              `json:"state"`
	Outcome     string              `json:"outcome,omitempty"`
	Reason      string              `json:"reason,omitempty"`
	HTTPStatus  int                 `json:"http_status,omitempty"`
	LogFile     string              `json:"log_file,omitempty"`
	StartedAt   int64               `json:"started_at,omitempty"`
	FinishedAt  int64               `json:"finished_at,omitempty"`
	Spec        map[string]any      `json:"spec,omitempty"`
	Plan        json.RawMessage     `json:"plan,omitempty"`
	Load        *bench.LoadResult   `json:"load,omitempty"`
	Stats       *bench.Stats        `json:"stats,omitempty"`
	Expect      *bench.ExpectResult `json:"expect,omitempty"`
	Comparison  *bench.Comparison   `json:"comparison,omitempty"`
	Error       string              `json:"error,omitempty"`
	Phases      []mPhase            `json:"phases"`
	Requests    []mRequest          `json:"requests,omitempty"`
}

type mRun struct {
	ID            string            `json:"id"`
	SuiteID       string            `json:"suite_id"`
	SuiteName     string            `json:"suite_name"`
	State         string            `json:"state"`
	Note          string            `json:"note,omitempty"`
	Trigger       string            `json:"trigger"`
	BaselineRunID string            `json:"baseline_run_id,omitempty"`
	QueuedAt      int64             `json:"queued_at"`
	StartedAt     int64             `json:"started_at,omitempty"`
	FinishedAt    int64             `json:"finished_at,omitempty"`
	Error         string            `json:"error,omitempty"`
	Version       string            `json:"version,omitempty"`
	Suite         map[string]any    `json:"suite,omitempty"`
	Summary       *bench.RunSummary `json:"summary,omitempty"`
	Steps         []*mStep          `json:"steps,omitempty"`
}

// short is the run without steps and suite (the list shape).
func (r *mRun) short() mRun {
	c := *r
	c.Steps, c.Suite = nil, nil
	return c
}

func (r *mRun) brief() map[string]any {
	b := map[string]any{"id": r.ID, "suite_id": r.SuiteID, "suite_name": r.SuiteName, "state": r.State, "trigger": r.Trigger,
		"queued_at": r.QueuedAt, "started_at": r.StartedAt, "finished_at": r.FinishedAt, "note": r.Note,
		"baseline_run_id": r.BaselineRunID, "error": r.Error}
	if r.Summary != nil {
		b["summary"] = r.Summary
	}
	return b
}

// stepBrief is a step without its requests, as the bench frames send it.
func stepBrief(s *mStep) mStep {
	c := *s
	c.Requests = nil
	return c
}

type benchStore struct {
	mu         sync.Mutex
	benchmarks map[string]map[string]any
	suites     map[string]map[string]any
	runs       []*mRun // oldest first
	stepSeq    int64
	runSeq     int
	reqSeq     int64
}

func newBenchStore() *benchStore {
	return &benchStore{benchmarks: map[string]map[string]any{}, suites: map[string]map[string]any{}}
}

func (s *benchStore) runByID(id string) *mRun {
	for _, r := range s.runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// lastCompleted is the newest completed run of a suite before t (0: any).
func (s *benchStore) lastCompleted(suite string, before int64) *mRun {
	for i := len(s.runs) - 1; i >= 0; i-- {
		r := s.runs[i]
		if r.SuiteID == suite && r.State == bench.RunCompleted && (before == 0 || r.StartedAt < before) {
			return r
		}
	}
	return nil
}

// ---- phases of a benchmark ----

type phaseSpec struct {
	Type string
	Opts map[string]any
}

var phaseLabels = map[string]string{"load": "Load", "warmup": "Warmup", "requests": "Requests", "conversation": "Conversation",
	"hold": "Hold", "concurrent_requests": "Simultaneous requests", "unload": "Unload", "measured": "Measured requests"}

func boolOpt(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func intOpt(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	if v, ok := m[k].(int); ok {
		return v
	}
	return def
}

// phasesOf is the benchmark's phases; a simple-form benchmark (no phases) is
// load, warmup, measured requests and unload as design 9.3 runs it.
func phasesOf(bm map[string]any) ([]phaseSpec, bool) {
	if l, ok := bm["phases"].([]any); ok && len(l) > 0 {
		var out []phaseSpec
		for _, p := range l {
			m := asMap(p)
			t, _ := m["type"].(string)
			out = append(out, phaseSpec{Type: t, Opts: m})
		}
		return out, false
	}
	var out []phaseSpec
	if boolOpt(bm, "cold", true) {
		out = append(out, phaseSpec{Type: "load", Opts: map[string]any{"cold": true}})
	}
	if n := intOpt(bm, "warmup_requests", 1); n > 0 {
		out = append(out, phaseSpec{Type: "warmup", Opts: map[string]any{"requests": float64(n)}})
	}
	out = append(out, phaseSpec{Type: "measured", Opts: map[string]any{"requests": bm["requests"], "repeats": bm["repeats"]}})
	if boolOpt(bm, "unload_after", true) {
		out = append(out, phaseSpec{Type: "unload", Opts: map[string]any{}})
	}
	return out, true
}

func phaseLabel(p phaseSpec) string {
	if p.Type == "load" && boolOpt(p.Opts, "cold", true) {
		return "Cold load"
	}
	if l, ok := phaseLabels[p.Type]; ok {
		return l
	}
	return p.Type
}

type reqPlan struct {
	name   string
	prompt string
	max    int
	turn   int
}

// requestsOf lists the chat requests one phase sends.
func requestsOf(bm map[string]any, p phaseSpec, simple bool) []reqPlan {
	var out []reqPlan
	reqList := func(v any) []reqPlan {
		var l []reqPlan
		for i, r := range asList(v) {
			m := asMap(r)
			name, _ := m["name"].(string)
			if name == "" {
				name = fmt.Sprintf("request-%d", i+1)
			}
			prompt, _ := m["prompt"].(string)
			l = append(l, reqPlan{name: name, prompt: prompt, max: intOpt(m, "max_tokens", 128)})
		}
		return l
	}
	switch p.Type {
	case "load":
		if simple {
			l := reqList(bm["requests"])
			if len(l) > 0 {
				out = append(out, l[0])
			}
		}
	case "warmup":
		for i := 0; i < intOpt(p.Opts, "requests", 1); i++ {
			out = append(out, reqPlan{name: fmt.Sprintf("warmup-%d", i+1), prompt: "Say hello.", max: intOpt(p.Opts, "max_tokens", 32)})
		}
	case "requests", "measured":
		l := reqList(p.Opts["requests"])
		for k := 0; k < max(intOpt(p.Opts, "repeats", 1), 1); k++ {
			out = append(out, l...)
		}
	case "conversation":
		for i, t := range asList(p.Opts["turns"]) {
			s, _ := t.(string)
			out = append(out, reqPlan{name: fmt.Sprintf("turn-%d", i+1), prompt: s, max: intOpt(p.Opts, "max_tokens", 160), turn: i + 1})
		}
	case "hold":
		out = append(out, reqPlan{name: "after-hold", prompt: "Are you still there?", max: intOpt(p.Opts, "max_tokens", 32)})
	case "concurrent_requests":
		for i := 0; i < intOpt(p.Opts, "concurrency", 2); i++ {
			out = append(out, reqPlan{name: fmt.Sprintf("concurrent-%d", i+1), max: intOpt(p.Opts, "max_tokens", 96)})
		}
	}
	return out
}

func phaseTypes(bm map[string]any) []string {
	ps, simple := phasesOf(bm)
	out := []string{}
	for _, p := range ps {
		t := p.Type
		if simple && t == "measured" {
			t = "requests"
		}
		out = append(out, t)
	}
	return out
}

// hostsOfProfile is where the scripted cluster puts a profile.
func hostsOfProfile(profile string) []string {
	switch profile {
	case "bench-gemma-12b-1w":
		return []string{ps5A.Host}
	case "bench-gemma-12b-2w":
		return []string{ps5A.Host, ps5B.Host}
	case "bench-gemma-12b-3w", "bench-kimi-72b-3w", "kimi-dev-72b-iq2s", "qwen3.8-27b":
		return []string{ps5A.Host, ps5B.Host, ps5C.Host}
	case "bench-devstral-24b":
		return []string{ps5B.Host}
	case "llama33-70b-q2":
		return []string{}
	case "qwen3.5-4b":
		return []string{ps5C.Host}
	}
	return []string{ps5A.Host}
}

// ---- validation ----

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func validateBenchmark(bm map[string]any) error {
	id, _ := bm["id"].(string)
	if !slugRe.MatchString(id) {
		return bad("id: %q is not a slug (letters, digits, . _ -, at most 64)", id)
	}
	prof, _ := bm["profile"].(string)
	if prof == "" {
		return bad("profile: is required")
	}
	if _, ok := profileByID(prof); !ok {
		return bad("profile: %q is not a profile or alias of the configuration", prof)
	}
	if ph, ok := bm["phases"]; ok {
		l, ok := ph.([]any)
		if !ok || len(l) == 0 {
			return bad("phases: must be a non-empty list")
		}
		for i, p := range l {
			m, ok := p.(map[string]any)
			if !ok {
				return bad("phases[%d]: must be an object", i)
			}
			t, _ := m["type"].(string)
			def, ok := phaseTypeDef(t)
			if !ok {
				return bad("phases[%d].type: %q is not one of load, warmup, requests, conversation, hold, concurrent_requests, unload", i, t)
			}
			for k, v := range m {
				if k == "type" {
					continue
				}
				o, ok := optionOf(def, k)
				if !ok {
					return bad("phases[%d].%s: unknown option of a %s phase", i, k, t)
				}
				if err := validateOption(o, fmt.Sprintf("phases[%d].%s", i, k), v); err != nil {
					return err
				}
			}
			if t == "requests" && len(asList(m["requests"])) == 0 {
				return bad("phases[%d].requests: a requests phase needs at least one request", i)
			}
			if t == "conversation" && len(asList(m["turns"])) == 0 {
				return bad("phases[%d].turns: a conversation needs at least one turn", i)
			}
		}
	} else if len(asList(bm["requests"])) == 0 {
		return bad("requests: a benchmark without phases needs 1 to 64 requests")
	}
	if e, ok := bm["expect"].(map[string]any); ok {
		if o, ok := e["outcome"]; ok && !contains([]string{"ready", "refused", "failed"}, fmt.Sprint(o)) {
			return bad("expect.outcome: must be ready, refused or failed, got %s", show(o))
		}
	}
	if t, ok := bm["timeout_s"]; ok {
		if x, ok := t.(float64); !ok || x < 1 || x > 86400 {
			return bad("timeout_s: must be 1 to 86400, got %s", show(t))
		}
	}
	return nil
}

func validateOption(o optionField, path string, v any) error {
	switch o.Type {
	case "int", "float":
		x, ok := v.(float64)
		if !ok || (o.Type == "int" && x != float64(int64(x))) {
			return bad("%s: must be a number, got %s", path, show(v))
		}
		if o.Min != nil && x < *o.Min || o.Max != nil && x > *o.Max {
			return bad("%s: %v is out of range", path, x)
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return bad("%s: must be true or false", path)
		}
	case "string", "text":
		if _, ok := v.(string); !ok {
			return bad("%s: must be a string", path)
		}
	case "requests":
		l, ok := v.([]any)
		if !ok {
			return bad("%s: must be a list of {name, prompt, max_tokens, stream}", path)
		}
		for i, r := range l {
			if _, ok := r.(map[string]any); !ok {
				return bad("%s[%d]: must be an object", path, i)
			}
		}
	case "turns":
		l, ok := v.([]any)
		if !ok {
			return bad("%s: must be a list of user messages", path)
		}
		for i, t := range l {
			if _, ok := t.(string); !ok {
				return bad("%s[%d]: must be a string", path, i)
			}
		}
	}
	return nil
}

func validateSuite(su map[string]any, benchmarks map[string]map[string]any) error {
	id, _ := su["id"].(string)
	if !slugRe.MatchString(id) {
		return bad("id: %q is not a slug", id)
	}
	l, ok := su["benchmarks"].([]any)
	if !ok || len(l) == 0 {
		return bad("benchmarks: must list 1 to 100 benchmark ids")
	}
	for i, b := range l {
		s, _ := b.(string)
		if _, ok := benchmarks[s]; !ok {
			return bad("benchmarks[%d]: %s is not a saved benchmark", i, show(b))
		}
	}
	if sc, ok := su["schedule"].(map[string]any); ok {
		if at, ok := sc["at"].(string); ok && !regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`).MatchString(at) {
			return bad("schedule.at: must be HH:MM, got %q", at)
		}
		if ev, ok := sc["every"].(string); ok {
			if d, err := time.ParseDuration(ev); err != nil || d < 10*time.Minute {
				return bad("schedule.every: must be a duration of at least 10m, got %q", ev)
			}
		}
	}
	return nil
}

// ---- handlers ----

type benchAPI struct {
	store  *benchStore
	engine *benchEngine
	cfg    *configStore
}

func (a *benchAPI) routes(m *http.ServeMux) {
	m.HandleFunc("GET /api/bench/schema", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, benchSchemaDoc()) })
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
	if err := validateBenchmark(b); err != nil {
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
	a.store.mu.Unlock()
	if !exists {
		apiErr(w, 404, "not found")
		return
	}
	if err := validateBenchmark(b); err != nil {
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
	var using []string
	for sid, su := range a.store.suites {
		for _, b := range asList(su["benchmarks"]) {
			if b == id {
				using = append(using, sid)
			}
		}
	}
	if len(using) > 0 {
		a.store.mu.Unlock()
		sort.Strings(using)
		apiErr(w, 409, "benchmark %q is used by suites %v and parallel steps []: remove it from them first", id, using)
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
			fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n", s.Idx, s.BenchmarkID, s.Profile, strings.Join(s.Nodes, " "), s.Outcome,
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
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.BenchmarkID, s.Profile, strings.Join(s.Nodes, ", "),
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
	if _, ok := metricFuncs[metric]; !ok {
		writeErr(w, 400, fmt.Sprintf("metric must be load_s, decode_tps or prefill_tps, got %q", metric), "bad request")
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
			if s.BenchmarkID == id && s.Outcome == bench.OutcomeReady {
				if v, ok := metricFuncs[metric](s); ok {
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
