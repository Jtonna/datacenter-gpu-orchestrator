package benchspec

import (
	"fmt"
	"sort"
	"strings"
)

// Phase types.
const (
	PhaseLoad         = "load"
	PhaseWarmup       = "warmup"
	PhaseRequests     = "requests"
	PhaseConversation = "conversation"
	PhaseContextFill  = "context_fill"
	PhaseHold         = "hold"
	PhaseConcurrent   = "concurrent_requests"
	PhaseUnload       = "unload"
)

// PhaseTypes lists the phase types in the order a UI should offer them.
var PhaseTypes = []string{PhaseLoad, PhaseWarmup, PhaseRequests, PhaseConversation, PhaseContextFill, PhaseHold, PhaseConcurrent, PhaseTasks, PhaseUnload}

// Limits and defaults of phases.
const (
	MaxPhases         = 32
	MaxTurns          = 64
	MaxConcurrent     = 16
	MaxHoldSeconds    = 3600
	DefaultHoldS      = 60
	DefaultWarmups    = 1
	DefaultConcurrent = 2
)

// Turn is one user message of a conversation phase.
type Turn struct {
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
	Content   string `json:"content" yaml:"content"`
	MaxTokens int    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// Phase is one step of a benchmark. Which options apply depends on Type (see
// Schema); an option that does not belong to the type is rejected.
type Phase struct {
	Type  string `json:"type" yaml:"type"`
	Label string `json:"label,omitempty" yaml:"label,omitempty"`

	// warmup: Count requests; warmup, hold and concurrent_requests send Request
	// (default: the first request of the benchmark's requests phase).
	Count   *int     `json:"count,omitempty" yaml:"count,omitempty"`
	Request *Request `json:"request,omitempty" yaml:"request,omitempty"`

	// requests: the measured requests, each run Repeats times.
	Requests []Request `json:"requests,omitempty" yaml:"requests,omitempty"`
	Repeats  int       `json:"repeats,omitempty" yaml:"repeats,omitempty"`

	// requests and conversation: defaults for what a request leaves unset.
	MaxTokens   int      `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	Seed        *int64   `json:"seed,omitempty" yaml:"seed,omitempty"`
	CachePrompt *bool    `json:"cache_prompt,omitempty" yaml:"cache_prompt,omitempty"`
	Stream      *bool    `json:"stream,omitempty" yaml:"stream,omitempty"`

	// conversation
	System string `json:"system,omitempty" yaml:"system,omitempty"`
	Turns  []Turn `json:"turns,omitempty" yaml:"turns,omitempty"`

	// context_fill (contextfill.go): the share of the context to fill, the
	// marks where a turn ends and a reading is taken, the facts asked back at
	// the end, the memory stop line, and whether a shape shared with a
	// profile a person wrote refuses the step. seed and max_tokens apply too.
	FillPct           *int  `json:"fill_pct,omitempty" yaml:"fill_pct,omitempty"`
	MarksPct          []int `json:"marks_pct,omitempty" yaml:"marks_pct,omitempty"`
	Recall            *int  `json:"recall,omitempty" yaml:"recall,omitempty"`
	StopAvailableMiB  *int  `json:"stop_available_mib,omitempty" yaml:"stop_available_mib,omitempty"`
	RefuseSharedShape *bool `json:"refuse_shared_shape,omitempty" yaml:"refuse_shared_shape,omitempty"`

	// hold
	Seconds *int `json:"seconds,omitempty" yaml:"seconds,omitempty"`

	// concurrent_requests
	N int `json:"n,omitempty" yaml:"n,omitempty"`

	// load: drop the model file from the file caches before the load.
	Cold *bool `json:"cold,omitempty" yaml:"cold,omitempty"`

	// tasks: a stored task set (TaskSet, and TaskSetVersion, 0 for the
	// newest) or an inline one (Tasks), and the phase's own time limit.
	TaskSet        string   `json:"task_set,omitempty" yaml:"task_set,omitempty"`
	TaskSetVersion int      `json:"task_set_version,omitempty" yaml:"task_set_version,omitempty"`
	Tasks          *TaskSet `json:"tasks,omitempty" yaml:"tasks,omitempty"`
	TimeoutS       *int     `json:"timeout_s,omitempty" yaml:"timeout_s,omitempty"`

	// soft marks the unload that the simple form's unload_after expands to: it
	// only unloads what the run itself loaded.
	soft bool
}

// OnlyIfLoadedByRun is set on the unload of the simple form (unload_after): it
// leaves a model alone that somebody else had loaded before the run.
func (p *Phase) OnlyIfLoadedByRun() bool { return p.soft }

// EvictsCache is a load phase's cold option (default false): drop the model
// file from the file cache of the nodes and of the scheduler's machine first.
func (p *Phase) EvictsCache() bool { return p.Type == PhaseLoad && p.Cold != nil && *p.Cold }

// Title is the label, else the type.
func (p *Phase) Title() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Type
}

// WarmupCount is count (default 1).
func (p *Phase) WarmupCount() int {
	if p.Count == nil {
		return DefaultWarmups
	}
	return *p.Count
}

// HoldS is seconds (default 60).
func (p *Phase) HoldS() int {
	if p.Seconds == nil {
		return DefaultHoldS
	}
	return *p.Seconds
}

// Conc is n (default 2).
func (p *Phase) Conc() int {
	if p.N <= 0 {
		return DefaultConcurrent
	}
	return p.N
}

// RepeatCount is repeats (default 1).
func (p *Phase) RepeatCount() int {
	if p.Repeats <= 0 {
		return 1
	}
	return p.Repeats
}

// CachesConversation is cache_prompt of a conversation (default true: the
// point of a conversation is that the server reuses the earlier turns).
func (p *Phase) CachesConversation() bool { return p.CachePrompt == nil || *p.CachePrompt }

// withDefaults fills what r leaves unset from the phase's defaults.
func (p *Phase) withDefaults(r Request) Request {
	if r.MaxTokens == 0 {
		r.MaxTokens = p.MaxTokens
	}
	if r.Temperature == nil {
		r.Temperature = p.Temperature
	}
	if r.Seed == nil {
		r.Seed = p.Seed
	}
	if r.CachePrompt == nil {
		r.CachePrompt = p.CachePrompt
	}
	if r.Stream == nil {
		r.Stream = p.Stream
	}
	return r
}

// PhaseRequests are the requests of a requests phase with the phase's defaults applied.
func (p *Phase) PhaseRequests() []Request {
	out := make([]Request, len(p.Requests))
	for i, r := range p.Requests {
		out[i] = p.withDefaults(r)
	}
	return out
}

// ConversationRequest is the sampling of a conversation turn (the user's
// content and the history are added by the runner).
func (p *Phase) ConversationRequest(t Turn) Request {
	r := Request{Name: t.Name, MaxTokens: t.MaxTokens, Temperature: p.Temperature, Seed: p.Seed, Stream: p.Stream}
	if r.MaxTokens == 0 {
		r.MaxTokens = p.MaxTokens
	}
	cp := p.CachesConversation()
	r.CachePrompt = &cp
	return r
}

// Expand returns the phases the benchmark runs: its phases, or the simple form
// turned into load (when cold), warmup, requests and unload (when unload_after).
// A parallel step has none.
func (b *Benchmark) Expand() []Phase {
	if b.IsParallel() {
		return nil
	}
	if len(b.Phases) > 0 {
		return b.Phases
	}
	var out []Phase
	if b.IsCold() {
		out = append(out, Phase{Type: PhaseLoad})
	}
	if n := b.Warmups(); n > 0 && len(b.Requests) > 0 {
		w := n
		first := b.Requests[0]
		out = append(out, Phase{Type: PhaseWarmup, Count: &w, Request: &first})
	}
	out = append(out, Phase{Type: PhaseRequests, Requests: b.Requests, Repeats: b.RepeatCount()})
	if b.UnloadsAfter() {
		out = append(out, Phase{Type: PhaseUnload, soft: true})
	}
	return out
}

// DefaultRequest is the request that warmup, hold and concurrent_requests send
// when they name none: the first request of the first requests phase.
func (b *Benchmark) DefaultRequest() (Request, bool) {
	phases := b.Expand()
	for i := range phases {
		p := &phases[i]
		if p.Type == PhaseRequests && len(p.Requests) > 0 {
			return p.withDefaults(p.Requests[0]), true
		}
	}
	return Request{}, false
}

// RequestOf is the request a warmup, hold or concurrent_requests phase sends.
func (b *Benchmark) RequestOf(p *Phase) (Request, bool) {
	if p.Request != nil {
		return *p.Request, true
	}
	return b.DefaultRequest()
}

// StreamsAnywhere says whether any phase streams its answer.
func (b *Benchmark) StreamsAnywhere() bool {
	for _, p := range b.Expand() {
		if p.Stream != nil && *p.Stream {
			return true
		}
		if p.Request != nil && p.Request.Streams() {
			return true
		}
		for _, r := range p.Requests {
			if r.Streams() {
				return true
			}
		}
	}
	return false
}

func (b *Benchmark) hasPhase(t string) bool {
	for _, p := range b.Expand() {
		if p.Type == t {
			return true
		}
	}
	return false
}

// allowed lists the options (besides type and label) each phase type takes.
var allowed = map[string][]string{
	PhaseLoad:         {"cold"},
	PhaseWarmup:       {"count", "request"},
	PhaseRequests:     {"requests", "repeats", "max_tokens", "temperature", "seed", "cache_prompt", "stream"},
	PhaseConversation: {"system", "turns", "max_tokens", "temperature", "seed", "cache_prompt", "stream"},
	PhaseContextFill:  {"fill_pct", "marks_pct", "recall", "seed", "max_tokens", "stop_available_mib", "refuse_shared_shape"},
	PhaseHold:         {"seconds", "request"},
	PhaseConcurrent:   {"n", "request"},
	PhaseTasks:        {"task_set", "task_set_version", "tasks", "timeout_s"},
	PhaseUnload:       {},
}

// setOptions names the options of p that are set.
func (p *Phase) setOptions() []string {
	var s []string
	add := func(on bool, n string) {
		if on {
			s = append(s, n)
		}
	}
	add(p.Count != nil, "count")
	add(p.Request != nil, "request")
	add(len(p.Requests) > 0, "requests")
	add(p.Repeats != 0, "repeats")
	add(p.MaxTokens != 0, "max_tokens")
	add(p.Temperature != nil, "temperature")
	add(p.Seed != nil, "seed")
	add(p.CachePrompt != nil, "cache_prompt")
	add(p.Stream != nil, "stream")
	add(p.System != "", "system")
	add(len(p.Turns) > 0, "turns")
	add(p.Seconds != nil, "seconds")
	add(p.N != 0, "n")
	add(p.Cold != nil, "cold")
	add(p.TaskSet != "", "task_set")
	add(p.TaskSetVersion != 0, "task_set_version")
	add(p.Tasks != nil, "tasks")
	add(p.TimeoutS != nil, "timeout_s")
	add(p.FillPct != nil, "fill_pct")
	add(len(p.MarksPct) > 0, "marks_pct")
	add(p.Recall != nil, "recall")
	add(p.StopAvailableMiB != nil, "stop_available_mib")
	add(p.RefuseSharedShape != nil, "refuse_shared_shape")
	return s
}

func validateRequest(r *Request, i int, where string, add func(string, ...any)) {
	if r.Name == "" {
		r.Name = fmt.Sprintf("request-%d", i+1)
	}
	w := func(f string, a ...any) { add("%s (%s): %s", where, r.Name, fmt.Sprintf(f, a...)) }
	if len(r.Name) > 80 {
		w("name is longer than 80 characters")
	}
	switch {
	case r.Prompt != "" && len(r.Messages) > 0:
		w("give either messages or prompt, not both")
	case r.Prompt == "" && len(r.Messages) == 0:
		w("messages or prompt is required")
	}
	if len(r.Prompt) > 1<<20 {
		w("prompt is larger than 1 MiB")
	}
	for j, m := range r.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			w("messages[%d].role must be system, user or assistant, got %q", j, m.Role)
		}
		if m.Content == "" {
			w("messages[%d].content is empty", j)
		}
	}
	if r.MaxTokens < 0 || r.MaxTokens > MaxTokensLimit {
		w("max_tokens must be 1 to %d (default %d)", MaxTokensLimit, DefaultMaxTokens)
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		w("temperature must be 0 to 2")
	}
}

func (b *Benchmark) validatePhases() []error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if len(b.Phases) > MaxPhases {
		add("phases: at most %d", MaxPhases)
	}
	holds := 0
	for _, p := range b.Phases {
		if p.Type == PhaseHold {
			holds += p.HoldS()
		}
	}
	if limit := int(b.Timeout().Seconds()); holds > 0 && holds+30 > limit {
		add("the holds add up to %d s, which does not fit in timeout_s (%d s): raise timeout_s to at least %d", holds, limit, holds+30)
	}
	for i := range b.Phases {
		p := &b.Phases[i]
		w := func(f string, a ...any) { add("phases[%d] (%s): %s", i, p.Type, fmt.Sprintf(f, a...)) }
		ok := false
		for _, t := range PhaseTypes {
			if t == p.Type {
				ok = true
			}
		}
		if !ok {
			add("phases[%d]: type %q is not one of %s", i, p.Type, strings.Join(PhaseTypes, ", "))
			continue
		}
		if len(p.Label) > 120 {
			w("label is longer than 120 characters")
		}
		okSet := map[string]bool{}
		for _, n := range allowed[p.Type] {
			okSet[n] = true
		}
		for _, n := range p.setOptions() {
			if !okSet[n] {
				w("option %q does not belong to this phase type (it takes: %s)", n, optionList(allowed[p.Type]))
			}
		}
		common := func() {
			if p.MaxTokens < 0 || p.MaxTokens > MaxTokensLimit {
				w("max_tokens must be 1 to %d", MaxTokensLimit)
			}
			if p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2) {
				w("temperature must be 0 to 2")
			}
		}
		reqNeeded := func() {
			if p.Request != nil {
				validateRequest(p.Request, 0, fmt.Sprintf("phases[%d].request", i), add)
				return
			}
			if _, has := b.DefaultRequest(); !has {
				w("request is required here, because the benchmark has no requests phase to borrow one from")
			}
		}
		switch p.Type {
		case PhaseWarmup:
			if p.Count != nil && (*p.Count < 0 || *p.Count > MaxWarmups) {
				w("count must be 0 to %d", MaxWarmups)
			}
			reqNeeded()
		case PhaseRequests:
			common()
			if p.Repeats < 0 || p.Repeats > MaxRepeats {
				w("repeats must be 0 to %d", MaxRepeats)
			}
			if len(p.Requests) == 0 || len(p.Requests) > MaxRequests {
				w("requests: between 1 and %d are required", MaxRequests)
			}
			for j := range p.Requests {
				validateRequest(&p.Requests[j], j, fmt.Sprintf("phases[%d].requests[%d]", i, j), add)
			}
		case PhaseConversation:
			common()
			if len(p.Turns) == 0 || len(p.Turns) > MaxTurns {
				w("turns: between 1 and %d are required", MaxTurns)
			}
			if len(p.System) > 1<<20 {
				w("system is larger than 1 MiB")
			}
			for j := range p.Turns {
				t := &p.Turns[j]
				if t.Name == "" {
					t.Name = fmt.Sprintf("turn-%d", j+1)
				}
				if strings.TrimSpace(t.Content) == "" {
					w("turns[%d].content is empty", j)
				}
				if len(t.Content) > 1<<20 {
					w("turns[%d].content is larger than 1 MiB", j)
				}
				if t.MaxTokens < 0 || t.MaxTokens > MaxTokensLimit {
					w("turns[%d].max_tokens must be 1 to %d", j, MaxTokensLimit)
				}
				if len(t.Name) > 80 {
					w("turns[%d].name is longer than 80 characters", j)
				}
			}
		case PhaseHold:
			if p.Seconds != nil && (*p.Seconds < 1 || *p.Seconds > MaxHoldSeconds) {
				w("seconds must be 1 to %d", MaxHoldSeconds)
			}
			reqNeeded()
		case PhaseConcurrent:
			if p.N < 0 || p.N == 1 || p.N > MaxConcurrent {
				w("n must be 2 to %d (default %d)", MaxConcurrent, DefaultConcurrent)
			}
			reqNeeded()
		case PhaseTasks:
			b.validateTasksPhase(p, w)
		case PhaseContextFill:
			validateContextFill(p, w)
		}
	}
	return errs
}

func optionList(names []string) string {
	if len(names) == 0 {
		return "no options"
	}
	return strings.Join(names, ", ")
}

func (b *Benchmark) validateExpect(add func(string, ...any)) {
	e := b.Expect
	if e == nil {
		return
	}
	switch e.Outcome {
	case "", OutcomeReady, OutcomeRefused, OutcomeFailed:
	default:
		add("expect.outcome must be ready, refused or failed, got %q", e.Outcome)
	}
	if keys := setExpectKeys(e); (e.Outcome == OutcomeRefused || e.Outcome == OutcomeFailed) && len(keys) > 0 {
		add("expect.%s only make sense when the outcome is ready", strings.Join(keys, ", expect."))
	}
	min0 := func(name string, v *float64) {
		if v != nil && *v < 0 {
			add("expect.%s must not be negative", name)
		}
	}
	pos := func(name string, v *float64) {
		if v != nil && *v <= 0 {
			add("expect.%s must be positive", name)
		}
	}
	min0("min_decode_tps", e.MinDecodeTPS)
	min0("min_prefill_tps", e.MinPrefillTPS)
	pos("max_load_s", e.MaxLoadS)
	pos("max_ttft_ms", e.MaxTTFTMS)
	pos("max_unload_s", e.MaxUnloadS)
	pos("max_concurrent_wall_s", e.MaxConcurrentWall)
	if e.MinReusedTokens != nil && *e.MinReusedTokens < 0 {
		add("expect.min_reused_tokens must not be negative")
	}
	if e.MaxErrors != nil && *e.MaxErrors < 0 {
		add("expect.max_errors must not be negative")
	}
	if e.OutputSHA256 != "" && !sha256Re.MatchString(e.OutputSHA256) {
		add("expect.output_sha256 must be 64 lower-case hex characters")
	}
	needs := func(set bool, key, what string, ok bool) {
		if set && !ok {
			add("expect.%s needs %s", key, what)
		}
	}
	measures := b.hasPhase(PhaseRequests) || b.hasPhase(PhaseConversation) || b.hasPhase(PhaseHold) || b.hasPhase(PhaseConcurrent)
	needs(e.MinDecodeTPS != nil, "min_decode_tps", "a requests, conversation, hold or concurrent_requests phase", measures)
	needs(e.MinPrefillTPS != nil, "min_prefill_tps", "a requests, conversation, hold or concurrent_requests phase", measures)
	needs(e.MaxTTFTMS != nil, "max_ttft_ms", "a phase that sets stream to true (time to first token is only measured on a streamed answer)", b.StreamsAnywhere())
	needs(e.MaxUnloadS != nil, "max_unload_s", "an unload phase", b.hasPhase(PhaseUnload))
	needs(e.MaxConcurrentWall != nil, "max_concurrent_wall_s", "a concurrent_requests phase", b.hasPhase(PhaseConcurrent))
	needs(e.MaxErrors != nil, "max_errors", "a concurrent_requests phase", b.hasPhase(PhaseConcurrent))
	needs(e.MinReusedTokens != nil, "min_reused_tokens", "a conversation phase", b.hasPhase(PhaseConversation))
	needs(e.OutputSHA256 != "", "output_sha256", "a requests phase", b.hasPhase(PhaseRequests))
}

func setExpectKeys(e *Expect) []string {
	var k []string
	add := func(on bool, n string) {
		if on {
			k = append(k, n)
		}
	}
	add(e.MinDecodeTPS != nil, "min_decode_tps")
	add(e.OutputSHA256 != "", "output_sha256")
	add(e.MinPrefillTPS != nil, "min_prefill_tps")
	add(e.MaxTTFTMS != nil, "max_ttft_ms")
	add(e.MaxUnloadS != nil, "max_unload_s")
	add(e.MaxConcurrentWall != nil, "max_concurrent_wall_s")
	add(e.MinReusedTokens != nil, "min_reused_tokens")
	add(e.MaxErrors != nil, "max_errors")
	sort.Strings(k)
	return k
}
