// DEV-ONLY MOCK of a server contract that is not built yet: GET
// /api/bench/schema (phase types, templates and the benchmark and suite
// fields). It lives only in cmd/devcluster.

package main

type optionField struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"` // int|float|bool|string|text|enum|requests|turns
	Enum        []string `json:"enum,omitempty"`
	Default     any      `json:"default"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Description string   `json:"description"`
}

type phaseType struct {
	Type        string        `json:"type"`
	Label       string        `json:"label"`
	Description string        `json:"description"`
	Options     []optionField `json:"options"`
}

var phaseTypeDefs = []phaseType{
	{Type: "load", Label: "Load", Description: "Cold load of the model. No chat request is sent.",
		Options: []optionField{{Key: "cold", Type: "bool", Default: true, Description: "Unload the profile first so the load is measured."}}},
	{Type: "warmup", Label: "Warmup", Description: "Requests after the load that are recorded but left out of the statistics.",
		Options: []optionField{
			{Key: "requests", Type: "int", Default: 1, Min: fp(0), Max: fp(10), Description: "How many warmup requests."},
			{Key: "prompt", Type: "text", Default: "Say hello.", Description: "The user message of each warmup request."},
			{Key: "max_tokens", Type: "int", Default: 32, Min: fp(1), Max: fp(65536), Description: "Most tokens each reply may have."}}},
	{Type: "requests", Label: "Requests", Description: "Measured requests, each sent repeats times. Decode and prefill speed come from these.",
		Options: []optionField{
			{Key: "requests", Type: "requests", Default: []any{}, Description: "The requests: name, prompt, max_tokens and stream (streaming also measures time to first token)."},
			{Key: "repeats", Type: "int", Default: 1, Min: fp(1), Max: fp(100), Description: "How many times each request is sent."}}},
	{Type: "conversation", Label: "Conversation", Description: "A multi-turn chat: each turn sends the whole conversation so far, so the prompt cache is reused.",
		Options: []optionField{
			{Key: "turns", Type: "turns", Default: []any{}, Description: "The user messages, one per turn."},
			{Key: "max_tokens", Type: "int", Default: 160, Min: fp(1), Max: fp(65536), Description: "Most tokens each reply may have."},
			{Key: "stream", Type: "bool", Default: true, Description: "Stream the replies (measures time to first token)."},
			{Key: "cache_prompt", Type: "bool", Default: true, Description: "Let the server reuse the cached prompt of the earlier turns."}}},
	{Type: "hold", Label: "Hold", Description: "Keep the model loaded and idle, then send one request: does it still answer quickly?",
		Options: []optionField{
			{Key: "idle_s", Type: "int", Default: 60, Min: fp(1), Max: fp(3600), Description: "Seconds to stay idle."},
			{Key: "prompt", Type: "text", Default: "Are you still there?", Description: "The request sent after the idle time."},
			{Key: "max_tokens", Type: "int", Default: 32, Min: fp(1), Max: fp(65536), Description: "Most tokens of the reply."}}},
	{Type: "concurrent_requests", Label: "Simultaneous requests", Description: "Several requests sent at the same moment to one loaded model.",
		Options: []optionField{
			{Key: "concurrency", Type: "int", Default: 2, Min: fp(2), Max: fp(16), Description: "How many requests at once."},
			{Key: "prompt", Type: "text", Default: "Write a haiku about load balancing.", Description: "The user message of every request."},
			{Key: "max_tokens", Type: "int", Default: 96, Min: fp(1), Max: fp(65536), Description: "Most tokens each reply may have."}}},
	{Type: "unload", Label: "Unload", Description: "Unload the profile at the end of the benchmark.", Options: []optionField{}},
}

func phaseTypeDef(t string) (phaseType, bool) {
	for _, p := range phaseTypeDefs {
		if p.Type == t {
			return p, true
		}
	}
	return phaseType{}, false
}

func optionOf(p phaseType, key string) (optionField, bool) {
	for _, o := range p.Options {
		if o.Key == key {
			return o, true
		}
	}
	return optionField{}, false
}

func benchSchemaDoc() map[string]any {
	req := func(name, prompt string, max int, stream bool) map[string]any {
		return map[string]any{"name": name, "prompt": prompt, "max_tokens": max, "stream": stream}
	}
	templates := []map[string]any{
		{"id": "load-only", "name": "Load only", "description": "Measure a cold load and unload again. No chat request is sent.",
			"phases": []any{map[string]any{"type": "load", "cold": true}, map[string]any{"type": "unload"}}},
		{"id": "load-and-requests", "name": "Load and requests", "description": "A cold load, one warmup request, then measured requests.",
			"phases": []any{map[string]any{"type": "load", "cold": true}, map[string]any{"type": "warmup", "requests": 1},
				map[string]any{"type": "requests", "repeats": 3, "requests": []any{req("short", "Say hello in five words.", 32, false), req("streamed", "Count from one to ten.", 64, true)}},
				map[string]any{"type": "unload"}}},
		{"id": "load-and-conversation", "name": "Load and conversation", "description": "A cold load, then a multi-turn conversation that reuses the prompt cache.",
			"phases": []any{map[string]any{"type": "load", "cold": true},
				map[string]any{"type": "conversation", "turns": []any{"Plan a three-day trip to Lisbon.", "Make day two cheaper.", "Now write it as a table."}, "max_tokens": 200},
				map[string]any{"type": "unload"}}},
		{"id": "requests-loaded", "name": "Requests on a loaded model", "description": "Measured requests on a model that is already loaded; nothing is unloaded.",
			"phases": []any{map[string]any{"type": "load", "cold": false},
				map[string]any{"type": "requests", "repeats": 3, "requests": []any{req("short", "Say hello in five words.", 32, false)}}}},
		{"id": "simultaneous", "name": "Simultaneous requests", "description": "Several requests at the same moment on one loaded model.",
			"phases": []any{map[string]any{"type": "load", "cold": true}, map[string]any{"type": "concurrent_requests", "concurrency": 4, "max_tokens": 96},
				map[string]any{"type": "unload"}}},
	}
	benchmarkFields := []schemaField{
		{Key: "timeout_s", Section: "benchmark", Type: "int", Default: 600, Min: fp(1), Max: fp(86400), Unit: "s", Description: "The whole benchmark, load included, must finish within this."},
		{Key: "tags", Section: "benchmark", Type: "list", Default: []string{}, Description: "Free labels for filtering."},
		{Key: "expect.outcome", Section: "benchmark", Type: "enum", Enum: []string{"ready", "refused", "failed"}, Default: "ready", Description: "What the scheduler should do with the first request."},
		{Key: "expect.min_decode_tps", Section: "benchmark", Type: "float", Default: nil, Min: fp(0), Unit: "tok/s", Description: "The step fails when the mean decode speed is lower."},
		{Key: "expect.max_load_s", Section: "benchmark", Type: "float", Default: nil, Min: fp(0), Unit: "s", Description: "The step fails when the load takes longer."},
	}
	suiteFields := []schemaField{
		{Key: "stop_on_failure", Section: "suite", Type: "bool", Default: false, Description: "Stop the run at the first benchmark that fails."},
		{Key: "cooldown_s", Section: "suite", Type: "int", Default: 5, Min: fp(0), Max: fp(3600), Unit: "s", Description: "Pause between benchmarks."},
		{Key: "only_when_idle", Section: "suite", Type: "bool", Default: true, Description: "Wait while anything else uses the scheduler."},
		{Key: "idle_wait_s", Section: "suite", Type: "int", Default: 600, Min: fp(0), Max: fp(86400), Unit: "s", Description: "Longest wait for idle; then the run is marked skipped."},
		{Key: "schedule.every", Section: "suite", Type: "duration", Default: nil, Min: fp(600), Unit: "s", Description: "Run the suite this often (at least 10m)."},
		{Key: "schedule.at", Section: "suite", Type: "string", Default: nil, Description: "Run the suite every day at this local time, HH:MM."},
		{Key: "baseline", Section: "suite", Type: "string", Default: "", Description: "The run to compare with; empty means the previous completed run of the suite."},
	}
	return map[string]any{"phase_types": phaseTypeDefs, "templates": templates, "benchmark_fields": benchmarkFields, "suite_fields": suiteFields}
}

func fp(v float64) *float64 { return &v }
