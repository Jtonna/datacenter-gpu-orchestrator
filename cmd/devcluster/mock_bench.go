// DEV-ONLY MOCK of the /api/bench/* surface of feat/bench-phases
// (internal/api/bench.go, internal/bench, internal/benchspec). The JSON shapes
// follow the real types; the work is simulated in memory in cmd/devcluster.
// This file: the run, step and phase types, phase expansion and validation.

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
)

// ---- result types (internal/bench/types.go of feat/bench-phases) ----

type mTurn struct {
	Turn         int      `json:"turn"`
	Name         string   `json:"name"`
	Status       int      `json:"status"`
	PromptTokens int      `json:"prompt_tokens"`
	PromptN      int      `json:"prompt_n"`
	CacheN       int      `json:"cache_n"`
	PrefillTPS   float64  `json:"prefill_tps"`
	DecodeTPS    float64  `json:"decode_tps"`
	TTFTMS       *float64 `json:"ttft_ms,omitempty"`
	WallMS       float64  `json:"wall_ms"`
	PredictedN   int      `json:"predicted_n"`
}

type mConcItem struct {
	Index      int      `json:"index"`
	Status     int      `json:"status"`
	WaitMS     *float64 `json:"wait_ms,omitempty"`
	WallMS     float64  `json:"wall_ms"`
	DecodeTPS  float64  `json:"decode_tps,omitempty"`
	PrefillTPS float64  `json:"prefill_tps,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type mConcurrent struct {
	N      int         `json:"n"`
	WallMS float64     `json:"wall_ms"`
	Errors int         `json:"errors"`
	Items  []mConcItem `json:"items"`
}

type mUnload struct {
	Loaded  bool    `json:"was_loaded"`
	Gone    bool    `json:"gone"`
	UnloadS float64 `json:"unload_s"`
}

// mPhase is bench.PhaseResult.
type mPhase struct {
	Idx             int               `json:"idx"`
	Type            string            `json:"type"`
	Label           string            `json:"label,omitempty"`
	State           string            `json:"state"` // running, done, failed, cancelled, skipped
	Outcome         string            `json:"outcome,omitempty"`
	Error           string            `json:"error,omitempty"`
	StartedAt       int64             `json:"started_at,omitempty"`
	FinishedAt      int64             `json:"finished_at,omitempty"`
	DurationS       float64           `json:"duration_s"`
	Load            *bench.LoadResult `json:"load,omitempty"`
	Stats           *bench.Stats      `json:"stats,omitempty"`
	Turns           []mTurn           `json:"turns,omitempty"`
	Concurrent      *mConcurrent      `json:"concurrent,omitempty"`
	HeldS           *float64          `json:"held_s,omitempty"`
	LoadedAfterHold *bool             `json:"loaded_after_hold,omitempty"`
	Unload          *mUnload          `json:"unload,omitempty"`
}

// mRequest is bench.RequestResult with the phase fields of feat/bench-phases.
type mRequest struct {
	bench.RequestResult
	PhaseIdx int      `json:"phase_idx"`
	Turn     int      `json:"turn,omitempty"`
	WaitMS   *float64 `json:"wait_ms,omitempty"`
}

// mStep is bench.Step. desc and nodes are kept for the progress document only.
type mStep struct {
	ID          int64               `json:"id"`
	Idx         int                 `json:"idx"`
	Group       string              `json:"group,omitempty"`
	BenchmarkID string              `json:"benchmark_id"`
	Name        string              `json:"name"`
	Profile     string              `json:"profile"`
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
	ModelFile   string              `json:"model_file,omitempty"`
	NodeCount   int                 `json:"node_count,omitempty"`
	Phases      []mPhase            `json:"phases,omitempty"`
	Requests    []mRequest          `json:"requests,omitempty"`

	desc  string
	nodes []string
}

type mWorkerEnv struct {
	Node             string `json:"node"`
	RPCServerVersion string `json:"rpc_server_version,omitempty"`
}

// mEnv is bench.Environment.
type mEnv struct {
	SchedulerVersion string       `json:"scheduler_version,omitempty"`
	ConfigRevision   string       `json:"config_revision,omitempty"`
	LlamaBinary      string       `json:"llama_binary,omitempty"`
	Workers          []mWorkerEnv `json:"workers,omitempty"`
}

func (e *mEnv) key() string {
	if e == nil {
		return ""
	}
	c := *e
	c.Workers = append([]mWorkerEnv(nil), e.Workers...)
	sort.Slice(c.Workers, func(i, j int) bool { return c.Workers[i].Node < c.Workers[j].Node })
	b, _ := json.Marshal(c)
	return string(b)
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
	Env           *mEnv             `json:"env,omitempty"`
	Summary       *bench.RunSummary `json:"summary,omitempty"`
	Steps         []*mStep          `json:"steps,omitempty"`
}

// short is the run without steps and suite (the list shape).
func (r *mRun) short() mRun {
	c := *r
	c.Steps, c.Suite = nil, nil
	return c
}

// brief is Manager.brief: the run in state documents and run frames.
func (r *mRun) brief() map[string]any {
	b := map[string]any{"id": r.ID, "suite_id": r.SuiteID, "suite_name": r.SuiteName, "state": r.State, "trigger": r.Trigger,
		"queued_at": r.QueuedAt, "started_at": r.StartedAt, "finished_at": r.FinishedAt, "note": r.Note,
		"baseline_run_id": r.BaselineRunID, "error": r.Error}
	if r.Summary != nil {
		b["summary"] = r.Summary
	}
	return b
}

// stepBrief is the step in step_started and step_finished frames (steprun.go stepBrief).
func stepBrief(st *mStep) map[string]any {
	b := map[string]any{"idx": st.Idx, "group": st.Group, "benchmark_id": st.BenchmarkID, "name": st.Name, "profile": st.Profile,
		"state": st.State, "outcome": st.Outcome, "reason": st.Reason, "http_status": st.HTTPStatus, "started_at": st.StartedAt, "finished_at": st.FinishedAt}
	if st.Load != nil {
		b["load"] = st.Load
	}
	if st.Stats != nil {
		b["stats"] = st.Stats
	}
	if st.Expect != nil {
		b["expect"] = st.Expect
	}
	if st.Comparison != nil {
		b["comparison"] = st.Comparison
	}
	return b
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

// ---- phases of a benchmark (benchspec.Expand) ----

type phaseSpec struct {
	Type  string
	Label string
	Opts  map[string]any
}

// title is Phase.Title: the label, else the type.
func (p phaseSpec) title() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Type
}

func boolOpt(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func intOpt(m map[string]any, k string, def int) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func strOpt(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func isParallel(bm map[string]any) bool { return len(asList(bm["parallel"])) > 0 }

func benchTitle(bm map[string]any) string {
	if n := strOpt(bm, "name"); n != "" {
		return n
	}
	return strOpt(bm, "id")
}

// expandPhases is Benchmark.Expand: the phases, or for the simple form load
// (cold, default true), warmup (warmup_requests, default 1), requests and
// unload (unload_after, default true).
func expandPhases(bm map[string]any) []phaseSpec {
	if isParallel(bm) {
		return nil
	}
	if l := asList(bm["phases"]); len(l) > 0 {
		var out []phaseSpec
		for _, p := range l {
			m := asMap(p)
			out = append(out, phaseSpec{Type: strOpt(m, "type"), Label: strOpt(m, "label"), Opts: m})
		}
		return out
	}
	var out []phaseSpec
	if boolOpt(bm, "cold", true) {
		out = append(out, phaseSpec{Type: "load", Opts: map[string]any{"type": "load"}})
	}
	reqs := asList(bm["requests"])
	if n := intOpt(bm, "warmup_requests", 1); n > 0 && len(reqs) > 0 {
		out = append(out, phaseSpec{Type: "warmup", Opts: map[string]any{"type": "warmup", "count": float64(n), "request": reqs[0]}})
	}
	out = append(out, phaseSpec{Type: "requests", Opts: map[string]any{"type": "requests", "requests": bm["requests"], "repeats": float64(intOpt(bm, "repeats", 1))}})
	if boolOpt(bm, "unload_after", true) {
		out = append(out, phaseSpec{Type: "unload", Opts: map[string]any{"type": "unload"}})
	}
	return out
}

// reqPlan is one chat request a phase sends. kind is RequestResult.phase:
// warmup, measured, conversation, hold or concurrent.
type reqPlan struct {
	kind   string
	name   string
	prompt string
	max    int
	turn   int
	stream bool
}

func reqOf(m map[string]any, i int, phase map[string]any) reqPlan {
	name := strOpt(m, "name")
	if name == "" {
		name = fmt.Sprintf("request-%d", i+1)
	}
	prompt := strOpt(m, "prompt")
	if prompt == "" {
		for _, x := range asList(m["messages"]) {
			prompt = strOpt(asMap(x), "content")
		}
	}
	mx := intOpt(m, "max_tokens", intOpt(phase, "max_tokens", 128))
	return reqPlan{name: name, prompt: prompt, max: mx, stream: boolOpt(m, "stream", boolOpt(phase, "stream", false))}
}

// defaultRequest is Benchmark.DefaultRequest: the first request of the first requests phase.
func defaultRequest(phases []phaseSpec) reqPlan {
	for _, p := range phases {
		if p.Type == "requests" {
			if l := asList(p.Opts["requests"]); len(l) > 0 {
				return reqOf(asMap(l[0]), 0, p.Opts)
			}
		}
	}
	return reqPlan{name: "request-1", prompt: "Say hello.", max: 128}
}

// phaseRequests lists the chat requests one phase sends, in order.
func phaseRequests(phases []phaseSpec, i int) []reqPlan {
	p := phases[i]
	one := func() reqPlan {
		if r, ok := p.Opts["request"].(map[string]any); ok {
			return reqOf(r, 0, p.Opts)
		}
		return defaultRequest(phases)
	}
	var out []reqPlan
	switch p.Type {
	case "warmup":
		r := one()
		r.kind = "warmup"
		for k := 0; k < intOpt(p.Opts, "count", 1); k++ {
			out = append(out, r)
		}
	case "requests":
		l := asList(p.Opts["requests"])
		for k := 0; k < max(intOpt(p.Opts, "repeats", 1), 1); k++ {
			for j, x := range l {
				r := reqOf(asMap(x), j, p.Opts)
				r.kind = "measured"
				out = append(out, r)
			}
		}
	case "conversation":
		for j, x := range asList(p.Opts["turns"]) {
			t := asMap(x)
			name := strOpt(t, "name")
			if name == "" {
				name = fmt.Sprintf("turn-%d", j+1)
			}
			out = append(out, reqPlan{kind: "conversation", name: name, prompt: strOpt(t, "content"),
				max: intOpt(t, "max_tokens", intOpt(p.Opts, "max_tokens", 128)), turn: j + 1, stream: boolOpt(p.Opts, "stream", false)})
		}
	case "hold":
		r := one()
		r.kind = "hold"
		out = append(out, r)
	case "concurrent_requests":
		r := one()
		r.kind = "concurrent"
		for k := 0; k < intOpt(p.Opts, "n", 2); k++ {
			c := r
			c.turn = k + 1
			out = append(out, c)
		}
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

// ---- validation (a reduced benchspec.Validate driven by the schema) ----

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var simpleKeys = []string{"cold", "requests", "repeats", "warmup_requests", "unload_after"}

// validateBenchmark checks a benchmark body and, like benchspec.Validate,
// reports every problem it finds, one per line, prefixed with where it is
// (for example "phases[1] (conversation): turns[0].content is empty").
func validateBenchmark(bm map[string]any, saved map[string]map[string]any) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	done := func() error {
		if len(errs) == 0 {
			return nil
		}
		return bad("%s", strings.Join(errs, "\n"))
	}
	id := strOpt(bm, "id")
	if !slugRe.MatchString(id) {
		add("id %q must be 1 to 64 characters: lower-case letters, digits, '.', '-' or '_', starting with a letter or digit", id)
	}
	if isParallel(bm) {
		for _, k := range sortedKeys(bm) {
			if !contains([]string{"id", "name", "description", "tags", "parallel"}, k) {
				add("a parallel step names the benchmarks to start together and nothing else (no profile, phases, requests, cold, repeats, warmup_requests, timeout_s, expect or unload_after)")
				break
			}
		}
		l := asList(bm["parallel"])
		if len(l) < 2 || len(l) > 8 {
			add("parallel needs 2 to 8 benchmark ids")
		}
		seen := map[string]bool{}
		for _, x := range l {
			mid := fmt.Sprint(x)
			m, ok := saved[mid]
			switch {
			case mid == id:
				add("parallel: a step cannot contain itself")
			case seen[mid]:
				add("parallel: %q is listed twice", mid)
			case !ok:
				add("parallel member %q is not a saved benchmark", mid)
			case isParallel(m):
				add("parallel member %q is itself a parallel step", mid)
			}
			seen[mid] = true
		}
		return done()
	}
	for _, k := range sortedKeys(bm) {
		if _, ok := optionIn(benchSchema.BenchmarkOptions, k); !ok && !contains(simpleKeys, k) && k != "expect" {
			add("unknown field %q", k)
		}
	}
	prof := strOpt(bm, "profile")
	if prof == "" {
		add("profile is required")
	} else if _, ok := profileByID(prof); !ok {
		add("profile %q is not a profile or alias of this scheduler", prof)
	}
	if t, ok := bm["timeout_s"]; ok {
		if x, ok := t.(float64); !ok || x < 1 || x > 86400 || x != float64(int64(x)) {
			add("timeout_s must be 1 to 86400, got %s", show(t))
		}
	}
	if ph, ok := bm["phases"]; ok {
		for _, k := range simpleKeys {
			if _, ok := bm[k]; ok {
				add("phases replaces the simple form: remove %s", k)
			}
		}
		l, ok := ph.([]any)
		if !ok || len(l) == 0 || len(l) > 32 {
			add("phases: between 1 and 32 are required")
		}
		for i, p := range l {
			m, ok := p.(map[string]any)
			if !ok {
				add("phases[%d]: must be an object", i)
				continue
			}
			t := strOpt(m, "type")
			def, ok := phaseTypeDef(t)
			if !ok {
				add("phases[%d]: type %q is not one of load, warmup, requests, conversation, hold, concurrent_requests, unload", i, t)
				continue
			}
			w := func(f string, a ...any) { add("phases[%d] (%s): %s", i, t, fmt.Sprintf(f, a...)) }
			for _, o := range def.Options {
				if _, ok := m[o.Name]; o.Required && !ok {
					w("%s is required", o.Name)
				}
			}
			for _, k := range sortedKeys(m) {
				if k == "type" {
					continue
				}
				o, ok := optionIn(def.Options, k)
				if !ok {
					var names []string
					for _, x := range def.Options {
						names = append(names, x.Name)
					}
					w("option %q does not belong to this phase type (it takes: %s)", k, strings.Join(names, ", "))
					continue
				}
				switch {
				case o.Type == "request":
					checkRequest(m[k], 0, fmt.Sprintf("phases[%d].request", i), add)
				case o.Type == "list" && o.ItemType == "request":
					rl, ok := m[k].([]any)
					if !ok || len(rl) == 0 || len(rl) > 64 {
						w("requests: between 1 and 64 are required")
					}
					for j, r := range rl {
						checkRequest(r, j, fmt.Sprintf("phases[%d].requests[%d]", i, j), add)
					}
				default:
					checkOption(o, k, m[k], w)
				}
			}
		}
	} else {
		l, ok := bm["requests"].([]any)
		if !ok || len(l) == 0 || len(l) > 64 {
			add("requests: between 1 and 64 are required (or give phases)")
		}
		for i, r := range l {
			checkRequest(r, i, fmt.Sprintf("requests[%d]", i), add)
		}
	}
	if e, ok := bm["expect"]; ok {
		m, ok := e.(map[string]any)
		if !ok {
			add("expect must be an object")
		} else if o, ok := m["outcome"]; ok && !contains([]string{"ready", "refused", "failed"}, fmt.Sprint(o)) {
			add("expect.outcome must be ready, refused or failed, got %s", show(o))
		}
	}
	return done()
}

// checkRequest checks one request object; its messages read "<where> (<name>): ...".
func checkRequest(v any, i int, where string, add func(string, ...any)) {
	m, ok := v.(map[string]any)
	if !ok {
		add("%s: must be a request object {name, prompt or messages, max_tokens, ...}", where)
		return
	}
	name := strOpt(m, "name")
	if name == "" {
		name = fmt.Sprintf("request-%d", i+1)
	}
	w := func(f string, a ...any) { add("%s (%s): %s", where, name, fmt.Sprintf(f, a...)) }
	for _, k := range sortedKeys(m) {
		ro, ok := optionIn(benchSchema.RequestOptions, k)
		if !ok {
			w("unknown field %q", k)
			continue
		}
		if ro.Type == "list" { // messages
			if _, ok := m[k].([]any); !ok {
				w("messages must be a list of {role, content}")
			}
			continue
		}
		checkOption(ro, k, m[k], w)
	}
	switch {
	case strOpt(m, "prompt") != "" && len(asList(m["messages"])) > 0:
		w("give either messages or prompt, not both")
	case strOpt(m, "prompt") == "" && len(asList(m["messages"])) == 0:
		w("messages or prompt is required")
	}
}

// checkOption checks one value against a schema option; w adds a message.
func checkOption(o benchOption, name string, v any, w func(string, ...any)) {
	rangeText := ""
	switch {
	case o.Min != nil && o.Max != nil:
		rangeText = fmt.Sprintf(" from %v to %v", *o.Min, *o.Max)
	case o.Min != nil:
		rangeText = fmt.Sprintf(" of at least %v", *o.Min)
	case o.Max != nil:
		rangeText = fmt.Sprintf(" of at most %v", *o.Max)
	}
	switch o.Type {
	case "integer", "number":
		x, ok := v.(float64)
		kind := map[string]string{"integer": "a whole number", "number": "a number"}[o.Type]
		if !ok || o.Type == "integer" && x != float64(int64(x)) || o.Min != nil && x < *o.Min || o.Max != nil && x > *o.Max {
			w("%s must be %s%s, got %s", name, kind, rangeText, show(v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			w("%s must be true or false", name)
		}
	case "string", "text":
		if _, ok := v.(string); !ok {
			w("%s must be a string", name)
		}
	case "list":
		l, ok := v.([]any)
		if !ok {
			w("%s must be a list", name)
			return
		}
		if o.Min != nil && float64(len(l)) < *o.Min || o.Max != nil && float64(len(l)) > *o.Max {
			w("%s: between %v and %v are required", name, show(o.Min), show(o.Max))
		}
		for j, x := range l {
			switch o.ItemType {
			case "turn":
				m, ok := x.(map[string]any)
				if !ok {
					w("%s[%d] must be a turn object {name, content, max_tokens}", name, j)
					continue
				}
				for _, k := range sortedKeys(m) {
					to, ok := optionIn(benchSchema.TurnOptions, k)
					if !ok {
						w("%s[%d]: unknown field %q", name, j, k)
						continue
					}
					checkOption(to, fmt.Sprintf("%s[%d].%s", name, j, k), m[k], w)
				}
				if strings.TrimSpace(strOpt(m, "content")) == "" {
					w("%s[%d].content is empty", name, j)
				}
			case "string":
				if _, ok := x.(string); !ok {
					w("%s[%d] must be a string", name, j)
				}
			}
		}
	}
}

func validateSuite(su map[string]any, benchmarks map[string]map[string]any) error {
	id := strOpt(su, "id")
	if !slugRe.MatchString(id) {
		return bad("suite %q: id must be lower-case letters, digits, '.', '-' and '_', up to 64 characters", id)
	}
	l, ok := su["benchmarks"].([]any)
	if !ok || len(l) == 0 || len(l) > 100 {
		return bad("suite %s: benchmarks must list 1 to 100 benchmarks", id)
	}
	for i, b := range l {
		s, _ := b.(string)
		if _, ok := benchmarks[s]; !ok {
			return bad("suite %s item %d: %s is not a saved benchmark", id, i+1, show(b))
		}
	}
	if sc, ok := su["schedule"].(map[string]any); ok {
		if at, ok := sc["at"].(string); ok && !regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`).MatchString(at) {
			return bad("suite %s: schedule.at must be HH:MM, got %q", id, at)
		}
		if ev, ok := sc["every"].(string); ok {
			if d, err := time.ParseDuration(ev); err != nil || d < 10*time.Minute {
				return bad("suite %s: schedule.every must be a duration of at least 10m, got %q", id, ev)
			}
		}
	}
	return nil
}
