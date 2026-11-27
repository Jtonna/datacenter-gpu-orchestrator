package benchspec

import (
	"encoding/json"
	"strconv"
)

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
	PhaseTypes []PhaseSchema `json:"phase_types"`
	Request    []Option      `json:"request_options"`
	Turn       []Option      `json:"turn_options"`
	Benchmark  []Option      `json:"benchmark_options"`
	Suite      []Option      `json:"suite_options"`
	// Inputs are a recipe's inputs; Entry the object form of a group entry;
	// Placements the written forms of a placement (data model step 2).
	Inputs     []Option       `json:"input_options"`
	Entry      []Option       `json:"entry_options"`
	Placements []string       `json:"placement_forms"`
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
		{Name: "profile", Type: "string", Description: "The profile (or alias) of this scheduler to benchmark. Give this (a fixed profile) or inputs (a recipe)."},
		{Name: "inputs", Type: "object", Description: "Makes the benchmark a recipe: the defaults of its inputs (see input_options). The profile is made from the inputs when the recipe is used in a group or run directly; a group entry or the run fills in what is missing (model and ctx are needed)."},
		{Name: "phases", Type: "list", ItemType: "phase", Description: "What to do, in order. Without phases the simple form (cold, warmup_requests, requests, repeats, unload_after) is used."},
		{Name: "timeout_s", Type: "integer", Default: DefaultTimeoutS, Min: 1, Max: MaxTimeoutS, Unit: "s", Description: "Limit for the whole benchmark, load and holds included."},
		{Name: "tags", Type: "list", ItemType: "string", Description: "Free tags for filtering."},
	}
	d.Suite = []Option{
		{Name: "stop_on_failure", Type: "boolean", Default: false, Description: "Skip the rest after a benchmark fails."},
		{Name: "cooldown_s", Type: "integer", Default: DefaultCooldownS, Min: 0, Max: 3600, Unit: "s", Description: "Pause between benchmarks."},
		{Name: "only_when_idle", Type: "boolean", Default: true, Description: "Wait while models are loaded, requests are running or the queue is not empty; skip the run when it stays busy longer than idle_wait_s."},
		{Name: "idle_wait_s", Type: "integer", Default: DefaultIdleWaitS, Min: 0, Max: 86400, Unit: "s", Description: "How long to wait for the scheduler to be idle."},
		{Name: "evict_others", Type: "boolean", Default: false, Description: "Do not wait for other users' idle loaded models: the run's loads may make the scheduler unload them to make room, as any load does. Requests in flight and queued requests are still waited for."},
	}
	d.Inputs = []Option{
		{Name: "model", Type: "string", Description: "The model file under the models directory."},
		{Name: "ctx", Type: "integer", Min: MinInputCtx, Max: MaxInputCtx, Unit: "tokens", Description: "Context size per slot."},
		{Name: "placement", Type: "string", Default: PlaceAuto, Description: "Where the model goes, agent zones only (not the scheduler's machine): auto (Potluck chooses), all (every zone that can take a load), any:N (N zones, those with the most available memory), nodes:a,b (these zones). A placement that cannot be met when the step starts is a refused step."},
		{Name: "thinking", Type: "boolean", Description: "false turns the model's reasoning off (--reasoning-budget 0); leave it out to keep the model's default."},
		{Name: "kv_quant", Type: "enum", Values: []any{"f16", "q8_0", "q4_0"}, Default: "f16", Description: "Key-value cache precision; q8_0 and q4_0 need flash_attn."},
		{Name: "flash_attn", Type: "boolean", Default: false, Description: "Turn flash attention on."},
	}
	d.Entry = []Option{
		{Name: "benchmark", Type: "string", Description: "A stored benchmark. With inputs or matrix it must be a recipe."},
		{Name: "inputs", Type: "object", Description: "Inputs for this use of the recipe; they override its defaults."},
		{Name: "matrix", Type: "object", Description: "Lists of model, placement, ctx and thinking: the entry stands for every combination (model outermost), at most " + itoa(MaxMatrixCombos) + "."},
		{Name: "group", Type: "string", Description: "Another group (suite), run where it stands with its own entries; its run options are not used. Give this or benchmark."},
	}
	d.Placements = []string{"auto", "all", "any:N", "nodes:a,b"}
	d.PhaseTypes = []PhaseSchema{
		{Type: PhaseLoad, Title: "Load", Description: "Cold load only: unload the profile if it is loaded, then load it without sending any chat request and wait until it is ready.",
			Records: []string{"load_s", "load_bytes", "load_mb_s", "node_bytes", "memory_mib", "plan", "cold"}, Options: []Option{
				{Name: "cold", Type: "boolean", Default: false, Description: "Drop the model file from the file cache of every node the load will use and of the scheduler's machine first, so the load reads the file from disk or the share. The result says whether that was verified (under 1 % of the file left cached everywhere)."},
			}},
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
		{Type: PhaseContextFill, Title: "Context fill", Description: "A conversation that fills the model's context to a share of its size with generated filler (the same seed gives the same text), counted with the model's own tokenizer. Each mark is one turn; after it the tokens held and every machine's available memory are recorded, and the growth per token is worked out. The last turn asks back the facts planted in the filler. The step stops, and the model is unloaded, when a machine's available memory would fall below the stop line. By default it refuses to run on a shape (model, context and options) that a profile a person wrote shares, because the step's memory peak becomes that shape's stored peak.",
			Records: []string{"fill_tokens", "fill_growth_mib", "fill_kib_per_token", "recall_correct", "prefill_tps", "ttft_ms", "wall_ms"}, Options: []Option{
				{Name: "fill_pct", Type: "integer", Default: DefaultFillPct, Min: 1, Max: MaxFillPct, Unit: "%", Description: "The share of the context to fill."},
				{Name: "marks_pct", Type: "list", ItemType: "integer", Min: 1, Max: MaxTurns, Unit: "%", Description: "Where a turn ends and a reading is taken, rising, ending at fill_pct (default 25, 50, 75 below it, then fill_pct)."},
				{Name: "recall", Type: "integer", Default: DefaultRecall, Min: 0, Max: MaxRecall, Description: "How many facts to plant in the filler and ask back at the end (0: none, the last turn only asks for OK)."},
				{Name: "seed", Type: "integer", Default: DefaultFillSeed, Description: "Seed of the filler and the facts; also the sampling seed."},
				{Name: "max_tokens", Type: "integer", Default: DefaultFillAnswerTok, Min: 1, Max: MaxFillAnswerTok, Unit: "tokens", Description: "The answer budget of the last turn (earlier turns answer in at most 8 tokens)."},
				{Name: "stop_available_mib", Type: "integer", Default: DefaultStopAvailMiB, Min: MinStopAvailMiB, Max: MaxStopAvailMiB, Unit: "MiB", Description: "Stop the step when any machine's available memory falls, or would fall with the next turn, below this."},
				{Name: "refuse_shared_shape", Type: "boolean", Default: true, Description: "Refuse the step when a profile a person wrote has the same shape (model, context, parallel and options), so the fill's memory peak cannot move that profile's plan. Whatever it says, a step whose own profile a person wrote is always refused: use the recipe."},
				{Name: "settle_s", Type: "integer", Min: 0, Max: MaxSettleS, Unit: "s", Description: "The hold after the load before the first turn, so the scheduler takes the cycle's settle use and the growth reading can be worked out. Default: the scheduler's free_average_window (30 s unless configured); 0: no hold."},
			}},
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
		{Type: PhaseTasks, Title: "Tasks", Description: "Send each task of a task set once, in order, with the profile loaded (temperature 0, the set's seed, streamed), and keep every answer with its tokens and speed so it can be scored later. Potluck never runs the answers and does not score them: a scorer posts its points to the run afterwards. A task that fails or runs out of time is recorded and the next one is sent.",
			Records: []string{"answer", "finish_reason", "prompt_tokens", "completion_tokens", "ttft_ms", "decode_tps", "wall_ms", "error"}, Options: []Option{
				{Name: "task_set", Type: "string", Description: "The id of a task set stored with POST /api/bench/task-sets. Give this or tasks."},
				{Name: "task_set_version", Type: "integer", Min: 1, Description: "The version of the stored task set (default: its newest version when the phase starts)."},
				{Name: "tasks", Type: "object", Description: "A task set given inline, in the same format as a stored one. Give this or task_set."},
				{Name: "timeout_s", Type: "integer", Min: 1, Max: MaxTimeoutS, Unit: "s", Description: "Limit for the whole phase; tasks not sent by then are recorded as not sent. Default: what the benchmark's timeout_s leaves, minus 30 s for the unload."},
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
		"concurrent": MaxConcurrent, "hold_seconds": MaxHoldSeconds, "max_tokens": MaxTokensLimit, "timeout_s": MaxTimeoutS, "suite_items": MaxSuiteItems, "tasks": MaxTasks,
		"matrix_values": MaxMatrixValues, "matrix_combos": MaxMatrixCombos, "expanded_items": MaxExpandedItems, "group_depth": MaxGroupDepth, "placement_nodes": MaxPlacementNodes}
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
		tpl(TemplateColdReload, "Cold, reload, loaded", "A load from an empty file cache, the first answer, the loaded speed, then an unload and a reload from the file cache with its first answer: cold load and reload times, time to first token after each, and the steady speed.",
			`{"id":"cold-reload-loaded-example","name":"Cold, reload, loaded","description":"Cold load (file caches dropped first), first answer, loaded speed; then unload, reload from the file cache and its first answer.","profile":"your-profile","timeout_s":1800,"phases":[{"type":"load","label":"cold load","cold":true},{"type":"warmup","label":"first generation","count":1,"request":{"name":"first","prompt":"Say hello in five words.","max_tokens":16,"stream":true}},{"type":"requests","label":"loaded","repeats":3,"max_tokens":128,"stream":true,"requests":[{"name":"explain","prompt":"Explain in two sentences what a GPU does."}]},{"type":"unload"},{"type":"load","label":"reload"},{"type":"requests","label":"after the reload","repeats":3,"max_tokens":128,"stream":true,"requests":[{"name":"explain","prompt":"Explain in two sentences what a GPU does."}]},{"type":"unload"}]}`),
		tpl(TemplateTasks, "Tasks", "A cold load, then every task of a task set sent once (temperature 0, streamed), then unload. The answers are kept for a scorer to post points; replace the inline set with task_set: \"<id>\" to use a stored one.",
			`{"id":"tasks-example","name":"Tasks","description":"Cold load, the tasks of a task set, unload.","profile":"your-profile","timeout_s":3600,"phases":[{"type":"load","cold":true},{"type":"tasks","label":"tasks","tasks":{"id":"example-tasks","version":1,"title":"Example tasks","max_tokens":1024,"tasks":[{"id":"fib","prompt":"Write a Python function fib(n) that returns the n-th Fibonacci number."},{"id":"sql-top3","prompt":"Write a SQL query that returns the three highest salaries per department from employees(name, department, salary)."}]}},{"type":"unload"}]}`),
		tpl(TemplateRecipe, "Cold load recipe", "A recipe: no fixed profile, it takes its model, context and placement as inputs, so one definition serves every model. Use it in a group with inputs or a matrix, or run it with inputs.",
			`{"id":"cold-load-recipe-example","name":"Cold load","description":"Cold load, first answer, unload; for any model.","inputs":{"ctx":8192,"placement":"any:2"},"phases":[{"type":"load","cold":true},{"type":"warmup","label":"first generation","count":1,"request":{"name":"first","prompt":"Say hello in five words.","max_tokens":16,"stream":true}},{"type":"unload"}]}`),
		tpl(TemplateContextFill, "Context fill recipe", "A recipe that fills the context of any model to 90 % with generated filler, reading every machine's memory at 25, 50, 75 and 90 %, then asks back the facts planted in the filler. Pick a context no profile you wrote uses: by default the step refuses a shape a written profile shares, because its memory peak becomes that shape's stored peak.",
			`{"id":"context-fill-recipe-example","name":"Context fill","description":"Load, fill the context to 90 % in four turns, recall, unload; for any model.","inputs":{"ctx":7936,"placement":"any:1"},"timeout_s":3600,"phases":[{"type":"load"},{"type":"context_fill","fill_pct":90,"recall":8,"stop_available_mib":2000},{"type":"unload"}]}`),
	}
	return d
}

// TemplateTasks and TemplateRecipe are the ids of the tasks and recipe templates.
const (
	TemplateTasks  = "tasks"
	TemplateRecipe = "cold-load-recipe"
	// TemplateContextFill is the id of the context_fill recipe template (B8).
	TemplateContextFill = "context-fill-recipe"
)

func itoa(n int) string { return strconv.Itoa(n) }

// TemplateColdReload is the id of the "cold, reload, loaded" template.
const TemplateColdReload = "cold-reload-loaded"

// TemplateByID is the template with that id.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Schema().Templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

func tpl(id, name, desc, js string) Template {
	return Template{ID: id, Name: name, Description: desc, Benchmark: json.RawMessage(js)}
}
