package benchspec

import "encoding/json"

// Option describes one option of a phase, a benchmark or an expect block, so
// that a UI can render an editor without knowing the phase types.
type Option struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string, text, integer, number, boolean, enum, list
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	Min         any    `json:"min,omitempty"`
	Max         any    `json:"max,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Values      []any  `json:"values,omitempty"`    // enum
	ItemType    string `json:"item_type,omitempty"` // list: request, turn
	Description string `json:"description"`
}

// PhaseSchema describes a phase type.
type PhaseSchema struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Records     []string `json:"records"` // the metrics the phase produces
	Options     []Option `json:"options"`
}

// ExpectKey describes one key of the expect block.
type ExpectKey struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Unit        string `json:"unit,omitempty"`
	Needs       string `json:"needs,omitempty"` // the phase type the check needs
	Description string `json:"description"`
}

// Template is a ready-made benchmark to start an editor from. Its profile is a
// placeholder to replace.
type Template struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Benchmark   json.RawMessage `json:"benchmark"`
}

// SchemaDoc is GET /api/bench/schema.
type SchemaDoc struct {
	PhaseTypes []PhaseSchema  `json:"phase_types"`
	Request    []Option       `json:"request_options"`
	Turn       []Option       `json:"turn_options"`
	Benchmark  []Option       `json:"benchmark_options"`
	Expect     []ExpectKey    `json:"expect_keys"`
	Templates  []Template     `json:"templates"`
	Limits     map[string]int `json:"limits"`
}

var samplingOptions = []Option{
	{Name: "max_tokens", Type: "integer", Default: DefaultMaxTokens, Min: 1, Max: MaxTokensLimit, Unit: "tokens", Description: "The most tokens one answer may have."},
	{Name: "temperature", Type: "number", Default: 0, Min: 0, Max: 2, Description: "Sampling temperature; 0 makes the output repeatable, so its hash can be compared between runs."},
	{Name: "seed", Type: "integer", Default: DefaultSeed, Description: "Sampling seed."},
	{Name: "stream", Type: "boolean", Default: false, Description: "Stream the answer, which also measures the time to first token."},
}

// Schema describes every phase type, option and expect key, with a few templates.
func Schema() *SchemaDoc {
	d := &SchemaDoc{}
	d.Request = append([]Option{
		{Name: "name", Type: "string", Description: "A short label for the request (default request-N)."},
		{Name: "prompt", Type: "text", Description: "The prompt, sent as one user message. Give this or messages."},
		{Name: "messages", Type: "list", ItemType: "message", Description: "A full chat: a list of {role, content}. Give this or prompt."},
		{Name: "cache_prompt", Type: "boolean", Default: false, Description: "Let the server reuse the cached prompt; off means every request does its full prefill."},
	}, samplingOptions...)
	d.Turn = []Option{
		{Name: "name", Type: "string", Description: "A short label for the turn (default turn-N)."},
		{Name: "content", Type: "text", Required: true, Description: "What the user says in this turn."},
		{Name: "max_tokens", Type: "integer", Min: 1, Max: MaxTokensLimit, Unit: "tokens", Description: "Overrides the phase's max_tokens for this turn."},
	}
	d.Benchmark = []Option{
		{Name: "id", Type: "string", Required: true, Description: "Lower-case letters, digits, '.', '-' and '_'; up to 64 characters."},
		{Name: "name", Type: "string", Description: "A readable name (default the id)."},
		{Name: "description", Type: "text", Description: "What this benchmark does and measures, shown while it runs."},
		{Name: "profile", Type: "string", Required: true, Description: "The profile (or alias) of this scheduler to benchmark."},
		{Name: "phases", Type: "list", ItemType: "phase", Description: "What to do, in order. Without phases the simple form (cold, warmup_requests, requests, repeats, unload_after) is used."},
		{Name: "timeout_s", Type: "integer", Default: DefaultTimeoutS, Min: 1, Max: MaxTimeoutS, Unit: "s", Description: "Limit for the whole benchmark, load and holds included."},
		{Name: "tags", Type: "list", ItemType: "string", Description: "Free tags for filtering."},
	}
	d.PhaseTypes = []PhaseSchema{
		{Type: PhaseLoad, Title: "Load", Description: "Cold load only: unload the profile if it is loaded, then load it without sending any chat request and wait until it is ready.",
			Records: []string{"load_s", "load_bytes", "load_mb_s", "node_bytes", "memory_mib", "plan"}, Options: []Option{}},
		{Type: PhaseWarmup, Title: "Warm-up", Description: "Send requests that are recorded but left out of the statistics, so a cold cache does not skew the measured ones.",
			Records: []string{"decode_tps", "prefill_tps"}, Options: []Option{
				{Name: "count", Type: "integer", Default: DefaultWarmups, Min: 0, Max: MaxWarmups, Description: "How many warm-up requests to send."},
				{Name: "request", Type: "request", Description: "The request to send (default: the first request of the requests phase)."},
			}},
		{Type: PhaseRequests, Title: "Requests", Description: "The measured requests: each prompt is sent `repeats` times and statistics are computed over them.",
			Records: []string{"decode_tps", "prefill_tps", "ttft_ms", "wall_ms", "output_sha256"}, Options: append([]Option{
				{Name: "requests", Type: "list", ItemType: "request", Required: true, Min: 1, Max: MaxRequests, Description: "The prompts or chats to send."},
				{Name: "repeats", Type: "integer", Default: 1, Min: 1, Max: MaxRepeats, Description: "How many times each request is sent."},
				{Name: "cache_prompt", Type: "boolean", Default: false, Description: "Default for requests that do not set it: reuse the cached prompt."},
			}, samplingOptions...)},
		{Type: PhaseConversation, Title: "Conversation", Description: "A multi-turn exchange: each turn is sent with all earlier turns and the model's replies, which measures how much of the prompt the server reuses.",
			Records: []string{"decode_tps", "prefill_tps", "ttft_ms", "prompt_tokens", "reused_tokens", "wall_ms"}, Options: append([]Option{
				{Name: "system", Type: "text", Description: "An optional system message that starts the conversation."},
				{Name: "turns", Type: "list", ItemType: "turn", Required: true, Min: 1, Max: MaxTurns, Description: "The user's messages, in order."},
				{Name: "cache_prompt", Type: "boolean", Default: true, Description: "Let the server reuse the cached earlier turns."},
			}, samplingOptions...)},
		{Type: PhaseHold, Title: "Hold", Description: "Keep the model loaded and idle for a while, then send one request to see how it behaves after the idle time.",
			Records: []string{"held_s", "decode_tps", "prefill_tps", "ttft_ms"}, Options: []Option{
				{Name: "seconds", Type: "integer", Default: DefaultHoldS, Min: 1, Max: MaxHoldSeconds, Unit: "s", Description: "How long to stay idle."},
				{Name: "request", Type: "request", Description: "The request sent afterwards (default: the first request of the requests phase)."},
			}},
		{Type: PhaseConcurrent, Title: "Simultaneous requests", Description: "Send n requests to the same profile at the same moment and record each one's wait and decode speed, the total time and the errors.",
			Records: []string{"wall_s", "wait_ms", "decode_tps", "errors"}, Options: []Option{
				{Name: "n", Type: "integer", Default: DefaultConcurrent, Min: 2, Max: MaxConcurrent, Description: "How many requests to send together."},
				{Name: "request", Type: "request", Description: "The request every one of them sends (default: the first request of the requests phase)."},
			}},
		{Type: PhaseUnload, Title: "Unload", Description: "Unload the profile and record how long it takes until the instance is gone.",
			Records: []string{"unload_s"}, Options: []Option{}},
	}
	for k := range d.PhaseTypes {
		d.PhaseTypes[k].Options = append([]Option{{Name: "label", Type: "string", Description: "A name for this phase, shown in the progress view."}}, d.PhaseTypes[k].Options...)
	}
	d.Expect = []ExpectKey{
		{Name: "outcome", Type: "enum", Description: "What the scheduler should do: ready (default), refused (it declines before loading) or failed (the load fails)."},
		{Name: "max_load_s", Type: "number", Unit: "s", Description: "The load must take at most this long."},
		{Name: "min_decode_tps", Type: "number", Unit: "tok/s", Needs: "requests", Description: "Mean decode speed must be at least this."},
		{Name: "min_prefill_tps", Type: "number", Unit: "tok/s", Needs: "requests", Description: "Mean prompt-processing speed must be at least this."},
		{Name: "max_ttft_ms", Type: "number", Unit: "ms", Needs: "requests", Description: "Mean time to first token must be at most this (needs a streamed phase)."},
		{Name: "output_sha256", Type: "string", Needs: "requests", Description: "The first request's output must hash to this (needs temperature 0)."},
		{Name: "min_reused_tokens", Type: "integer", Unit: "tokens", Needs: "conversation", Description: "Every turn after the first must reuse at least this many prompt tokens from the cache."},
		{Name: "max_concurrent_wall_s", Type: "number", Unit: "s", Needs: "concurrent_requests", Description: "All simultaneous requests together must finish within this time."},
		{Name: "max_errors", Type: "integer", Needs: "concurrent_requests", Description: "At most this many simultaneous requests may fail (default: not checked)."},
		{Name: "max_unload_s", Type: "number", Unit: "s", Needs: "unload", Description: "The instance must be gone within this time of the unload."},
	}
	d.Limits = map[string]int{"phases": MaxPhases, "requests": MaxRequests, "repeats": MaxRepeats, "warmups": MaxWarmups, "turns": MaxTurns,
		"concurrent": MaxConcurrent, "hold_seconds": MaxHoldSeconds, "max_tokens": MaxTokensLimit, "timeout_s": MaxTimeoutS, "suite_items": MaxSuiteItems}
	d.Templates = []Template{
		tpl("load-only", "Load only", "Measures just the cold load of a profile: how long it takes, how many bytes go to which worker and how much memory it uses. No chat request is sent.",
			`{"id":"load-only-example","name":"Load only","description":"Cold load, then unload. Measures load time and upload rate only.","profile":"your-profile","phases":[{"type":"load"},{"type":"unload"}]}`),
		tpl("load-and-requests", "Load and requests", "A cold load, one warm-up, then measured requests repeated several times, then unload.",
			`{"id":"load-and-requests-example","name":"Load and requests","description":"Cold load, a warm-up and 3 measured requests.","profile":"your-profile","phases":[{"type":"load"},{"type":"warmup","count":1,"request":{"name":"hello","prompt":"Say hello in five words.","max_tokens":16}},{"type":"requests","repeats":3,"max_tokens":128,"requests":[{"name":"explain","prompt":"Explain in two sentences what a GPU does."}]},{"type":"unload"}]}`),
		tpl("load-and-conversation", "Load and conversation", "A cold load followed by a multi-turn conversation, to see prefill, decode and how much of the earlier turns the server reuses.",
			`{"id":"load-and-conversation-example","name":"Load and conversation","description":"Cold load, then a 3-turn conversation with prompt caching.","profile":"your-profile","phases":[{"type":"load"},{"type":"conversation","system":"You are a concise assistant.","max_tokens":96,"stream":true,"turns":[{"content":"Name three uses of a hash table."},{"content":"Give a one-line example of the second one."},{"content":"Now summarise our conversation in one sentence."}]},{"type":"unload"}]}`),
		tpl("requests-on-loaded", "Requests on a loaded model", "No load and no unload: sends requests to whatever is loaded (the model is loaded first if it is not).",
			`{"id":"requests-on-loaded-example","name":"Requests on a loaded model","description":"Measured requests only; leaves the model loaded.","profile":"your-profile","phases":[{"type":"requests","repeats":5,"max_tokens":128,"requests":[{"name":"explain","prompt":"Explain in two sentences what a GPU does."}]}]}`),
		tpl("simultaneous-requests", "Simultaneous requests", "Loads the model, then fires several identical requests at once to see how the scheduler and the model share the time.",
			`{"id":"simultaneous-requests-example","name":"Simultaneous requests","description":"Cold load, then 4 requests at the same moment.","profile":"your-profile","phases":[{"type":"load"},{"type":"concurrent_requests","n":4,"request":{"name":"hello","prompt":"Say hello in ten words.","max_tokens":32}},{"type":"unload"}]}`),
	}
	return d
}

func tpl(id, name, desc, js string) Template {
	return Template{ID: id, Name: name, Description: desc, Benchmark: json.RawMessage(js)}
}
