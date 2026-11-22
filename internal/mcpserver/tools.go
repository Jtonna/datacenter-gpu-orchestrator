package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// A tool is a name, a schema for an agent, and a recipe that turns its
// arguments into one REST request (see Server.callTool).
type tool struct {
	Name, Desc  string
	Props       map[string]any
	Required    []string
	ReadOnly    bool
	Destructive bool
	Idempotent  bool

	Method  string
	Path    string   // {name} placeholders are filled from the argument of that name
	Query   []string // arguments sent as query parameters, same name
	Body    []string // arguments gathered into the JSON object body
	BodyArg string   // or: the argument whose value is the whole body
	Pre     func(args map[string]any)
	// Derive reshapes the parsed JSON answer (status-based tools).
	Derive func(doc map[string]any, args map[string]any) any
	// Fallback answers a tool whose endpoint this build lacks.
	Fallback *fallback

	configWrite bool // takes comment, dry_run, if_revision
	write       bool // takes actor
}

type fallback struct {
	Method, Path string
	Reshape      func(doc map[string]any, args map[string]any) (any, bool)
}

// ---- schema helpers ----

type P = map[string]any

func s(desc string) P    { return P{"type": "string", "description": desc} }
func i(desc string) P    { return P{"type": "integer", "description": desc} }
func b(desc string) P    { return P{"type": "boolean", "description": desc} }
func obj(desc string) P  { return P{"type": "object", "description": desc} }
func anyv(desc string) P { return P{"description": desc} }
func en(desc string, vals ...string) P {
	return P{"type": "string", "description": desc, "enum": vals}
}

// schema is the JSON Schema of the tool's input; strict: no extra properties.
func (t *tool) schema() map[string]any {
	props := map[string]any{}
	for k, v := range t.Props {
		props[k] = v
	}
	if t.configWrite {
		props["comment"] = s("Optional one-line reason, stored with the change in the audit log (sent as X-Comment).")
		props["dry_run"] = b("If true, validate and report what would change without saving anything.")
		props["if_revision"] = i("Optional guard: only apply if the configuration is still at this revision number (sent as If-Match); otherwise the call fails and you should re-read.")
	}
	if t.write {
		props["actor"] = s("Optional label for who is asking (for example the agent's task); it is added to the audit log entry next to the MCP client name.")
	}
	req := t.Required
	if req == nil {
		req = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}

func (t *tool) annotations() map[string]any {
	a := map[string]any{"readOnlyHint": t.ReadOnly, "openWorldHint": false}
	if !t.ReadOnly {
		a["destructiveHint"] = t.Destructive
		a["idempotentHint"] = t.Idempotent
	}
	return a
}

// ---- argument validation against the tool's own schema ----

func (t *tool) validate(args map[string]any) error {
	props := t.schema()["properties"].(map[string]any)
	for k := range args {
		if _, ok := props[k]; !ok {
			return fmt.Errorf("unknown argument %q", k)
		}
	}
	for _, r := range t.Required {
		if v, ok := args[r]; !ok || v == nil {
			return fmt.Errorf("missing required argument %q", r)
		}
	}
	for k, v := range args {
		sc := props[k].(P)
		typ, _ := sc["type"].(string)
		ok := true
		switch typ {
		case "string":
			_, ok = v.(string)
		case "integer":
			f, isNum := v.(float64)
			ok = isNum && f == float64(int64(f))
		case "boolean":
			_, ok = v.(bool)
		case "object":
			_, ok = v.(map[string]any)
		case "array":
			_, ok = v.([]any)
		}
		if !ok {
			return fmt.Errorf("argument %q must be of type %s", k, typ)
		}
		if vals, has := sc["enum"].([]string); has {
			found := false
			for _, e := range vals {
				if v == e {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("argument %q must be one of %s", k, strings.Join(vals, ", "))
			}
		}
	}
	return nil
}

// ---- building the REST request ----

type restReq struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (t *tool) build(args map[string]any) (restReq, error) {
	if t.Pre != nil {
		t.Pre(args)
	}
	r := restReq{Method: t.Method, Query: url.Values{}, Header: http.Header{}}
	path := t.Path
	for {
		a := strings.IndexByte(path, '{')
		if a < 0 {
			break
		}
		e := strings.IndexByte(path[a:], '}') + a
		name := path[a+1 : e]
		v, ok := args[name]
		if !ok {
			return r, fmt.Errorf("missing required argument %q", name)
		}
		sv := scalar(v)
		if sv == "" {
			return r, fmt.Errorf("argument %q must not be empty", name)
		}
		path = path[:a] + url.PathEscape(sv) + path[e+1:]
	}
	r.Path = path
	for _, q := range t.Query {
		if v, ok := args[q]; ok {
			r.Query.Set(q, scalar(v))
		}
	}
	if t.BodyArg != "" {
		if v, ok := args[t.BodyArg]; ok {
			r.Body, _ = json.Marshal(v)
		}
	} else if len(t.Body) > 0 {
		m := map[string]any{}
		for _, k := range t.Body {
			if v, ok := args[k]; ok {
				m[k] = v
			}
		}
		if len(m) > 0 {
			r.Body, _ = json.Marshal(m)
		}
	}
	if t.configWrite {
		if v, _ := args["dry_run"].(bool); v {
			r.Query.Set("dry_run", "1")
		}
		if v, ok := args["comment"].(string); ok && v != "" {
			r.Header.Set("X-Comment", v)
		}
		if v, ok := args["if_revision"]; ok {
			r.Header.Set("If-Match", scalar(v))
		}
	}
	return r, nil
}

// ---- the catalogue ----

const (
	cfgNote = " Configuration changes are audited and can be rolled back with rollback_config."
	valNote = "The new value of the whole section, in the shape get_config shows for it (tiers: a list; exclude_nodes and no_auto_nodes: a list of node ids or hostname globs; node_overrides: an object of node id to override; recovery and fleet: an object). Sent as the request body."
)

func rd(name, desc string, props P, req []string, method, path string, query ...string) *tool {
	return &tool{Name: name, Desc: desc, Props: props, Required: req, ReadOnly: true, Method: method, Path: path, Query: query}
}

func allTools() []*tool {
	idArg := func(what string) P { return P{"id": s("The " + what + " id.")} }
	T := []*tool{
		// ---- read ----
		{Name: "get_status", Desc: "Model-centric snapshot of the scheduler: every loaded or loading instance (profile, model file, state, nodes, memory, requests in flight, load progress), then nodes, queue, quarantines and recent loads. Start here. Pass verbose for the complete /api/status object.",
			Props: P{"verbose": b("Return the full, untrimmed status object (large).")}, ReadOnly: true, Method: "GET", Path: "/api/status", Derive: deriveStatus},
		rd("list_models", "List the models (profiles) a client may request, in OpenAI list format.", nil, nil, "GET", "/v1/models"),
		{Name: "list_nodes", Desc: "List every node (PS5 workers and the local GPU host) with online state, tier priority, memory budgets, which instance holds or claims it, health and recovery state.",
			ReadOnly: true, Method: "GET", Path: "/api/status", Derive: deriveNodes},
		{Name: "get_queue", Desc: "Show the requests waiting for a model to load: each queue entry with its age, waiters, claimed nodes and why it has not been placed, plus quarantined nodes and backed-off profiles.",
			ReadOnly: true, Method: "GET", Path: "/api/status", Derive: deriveQueue},
		rd("list_profiles", "List every profile (model configuration) with its memory need and what a request for it would do right now (a dry-run plan).", nil, nil, "GET", "/api/profiles"),
		{Name: "get_profile", Desc: "Get one profile's configuration by id (answers {revision, id, profile}).", Props: idArg("profile"), Required: []string{"id"}, ReadOnly: true, Method: "GET", Path: "/api/config/profiles/{id}",
			Fallback: &fallback{Method: "GET", Path: "/api/profiles", Reshape: profileFromList}},
		rd("list_model_files", "The model catalog: one entry per model file the cluster has (not per profile), with its name, size, quantization, how many configurations (profiles) use it, whether it is loaded and on which nodes, the memory it was measured to need per node count, load times from the history, the last refusal and benchmark figures. Use it to answer \"which models are there\"; list_profiles lists every configuration instead.", nil, nil, "GET", "/api/models"),
		rd("get_model_file", "One model of the catalog by file name (or command:<profile id> for a command profile), with its configurations, current instances, recent history events and the benchmarks that use it.", P{"file": s("The model file name as list_model_files shows it in key, for example gemma-4-12b-it-Q4_K_M.gguf.")}, []string{"file"}, "GET", "/api/models/{file}"),
		{Name: "plan_load", Desc: "Dry run: what would happen if this model were requested now (placement, which instances would be evicted, or why it cannot be placed). Changes nothing. Call this before load_model.",
			Props: P{"model": s("Profile id or alias.")}, Required: []string{"model"}, ReadOnly: true, Method: "POST", Path: "/api/plan", Body: []string{"model"}},
		rd("query_history", "Search the persisted event history (loads, refusals, stalls, node loss, recoveries, evictions). Newest first by default.", P{
			"since": i("Only events at or after this unix time in milliseconds."), "until": i("Only events before this unix time in milliseconds."),
			"type": s("Comma-separated event types, for example loadFinished,nodeLost."), "severity": en("Minimum severity.", "info", "warn", "error"),
			"node": s("Node hostname or id."), "profile": s("Profile id."), "session_id": s("A load session id such as ld-1795055840000-3."),
			"q": s("Case-insensitive substring of the event summary."), "limit": i("Maximum events (default 200, at most 2000)."), "order": en("Sort order.", "desc", "asc")},
			nil, "GET", "/api/history", "since", "until", "type", "severity", "node", "profile", "session_id", "q", "limit", "order"),
		rd("get_history_summary", "Aggregate the event history over a time range: counts by type and severity, load statistics per profile, per-node incidents and every error event.", P{
			"since": i("Range start, unix milliseconds."), "until": i("Range end, unix milliseconds.")}, nil, "GET", "/api/history/summary", "since", "until"),
		rd("query_logs", "Search the central log store (CR-10) with a LogsQL query: Potluck's own log (source:=scheduler), the output of the programs it starts (source:=host), each agent's log, launch output and machine journal (source:=agent, launch, journal). Fields: node_id, node_name, source, profile, launch_id, level, unit, _msg, _time. Examples: node_id:=\"02:00:00:00:00:01\" level:=error; profile:=\"gemma-4-12b\" _time:1h. Answers JSON lines, newest last; answers 404 when central logging is off.", P{
			"query": s("A LogsQL query. Add a _time filter (for example _time:15m) to keep it fast."), "limit": i("Maximum lines (100 when left out here; the REST route allows at most 10000)."),
			"start": s("Start: RFC 3339, Unix seconds, or a span back from now such as 1h."), "end": s("End, same forms as start.")},
			[]string{"query"}, "GET", "/api/logs/query", "query", "limit", "start", "end"),
		rd("get_system_info", "Scheduler version, uptime, listen address, safety settings, benchmark settings and storage locations with sizes.", nil, nil, "GET", "/api/system"),
		rd("get_config", "The full current configuration (profiles, tiers, settings, exclude_nodes, no_auto_nodes, node_overrides, recovery, fleet) with its revision number.", nil, nil, "GET", "/api/config"),
		rd("get_config_schema", "The schema of the settings: {\"fields\":[...]} with each key, type, default, limits and whether a change needs a restart. Read it before update_settings.", nil, nil, "GET", "/api/config/schema"),
		rd("list_config_revisions", "List the saved configuration revisions, newest first, with time, source, actor and comment, and the current revision.", P{"limit": i("Maximum revisions.")}, nil, "GET", "/api/config/revisions", "limit"),
		rd("get_config_revision", "Get one saved configuration revision by number.", P{"revision": i("Revision number.")}, []string{"revision"}, "GET", "/api/config/revisions/{revision}"),
		rd("query_audit", "Search the audit log of configuration and control changes (who changed what, through REST or MCP).", P{
			"since": s("Start time, unix milliseconds or RFC 3339."), "until": s("End time, unix milliseconds or RFC 3339."),
			"source": en("Where the change came from: ui (the web page), rest, mcp, import (import-config or /api/config/import) or system (the scheduler itself, for example a benchmark run's loads).", "ui", "rest", "mcp", "import", "system"),
			"actor":  s("Who made it (the MCP client name, or the X-Actor of a REST call)."), "section": s("A configuration section (profiles, tiers, settings, exclude_nodes, no_auto_nodes, node_overrides, recovery, fleet), bench (benchmarks, suites, runs) or runtime (load and unload)."),
			"target": s("The item changed, for example a profile id, benchmark:<id>, suite:<id> or run:<id>."), "action": s("create, update, delete, rollback, import, load, unload, run_start or run_cancel."),
			"q": s("Free-text search."), "limit": i("Maximum entries (newest first by default)."), "order": en("Sort order.", "desc", "asc")},
			nil, "GET", "/api/audit", "since", "until", "source", "actor", "section", "target", "action", "q", "limit", "order"),
		rd("list_benchmarks", "List the benchmark definitions (test cases). Needs data_dir; otherwise the call fails and says so.", nil, nil, "GET", "/api/bench/benchmarks"),
		rd("get_benchmark", "Get one benchmark definition by id.", idArg("benchmark"), []string{"id"}, "GET", "/api/bench/benchmarks/{id}"),
		rd("list_suites", "List the benchmark suites with their last run.", nil, nil, "GET", "/api/bench/suites"),
		rd("get_suite", "Get one benchmark suite by id.", idArg("suite"), []string{"id"}, "GET", "/api/bench/suites/{id}"),
		rd("get_bench_state", "The benchmark engine's state: the current run with progress, the queue of runs and the next scheduled run.", nil, nil, "GET", "/api/bench/state"),
		rd("list_runs", "List benchmark runs, newest first, without steps.", P{"suite": s("Only runs of this suite id."), "state": en("Only runs in this state.", "queued", "running", "paused", "completed", "failed", "cancelled", "skipped"),
			"limit": i("Maximum runs.")}, nil, "GET", "/api/bench/runs", "suite", "state", "limit"),
		rd("get_run", "Get one benchmark run with its steps, requests, statistics and comparison against the baseline.", idArg("run"), []string{"id"}, "GET", "/api/bench/runs/{id}"),
		rd("export_run", "Export a benchmark run as a markdown table, CSV or JSON, returned as text.", P{"id": s("The run id."), "format": en("Export format (json by default).", "markdown", "csv", "json"),
			"detail": en("csv only: phases or turns for one row per phase or per turn (default one row per benchmark).", "phases", "turns")},
			[]string{"id"}, "GET", "/api/bench/runs/{id}/export", "format", "detail"),
		rd("get_bench_aggregates", "Aggregated benchmark statistics across completed runs (per benchmark and profile: counts, load, decode and prefill figures), filtered by suite, benchmark, profile, time range and environment.", P{
			"suite": s("Suite id."), "benchmark": s("Benchmark id."), "profile": s("Profile id."),
			"since": s("Start: unix milliseconds or seconds, an RFC 3339 time, or a span back from now such as 24h, 7d or 90m."), "until": s("End, same forms as since."),
			"limit_runs": i("Use only the newest N runs."), "env": en("same: only runs whose environment equals the newest run's (comparable runs). Leave out for all.", "same")},
			nil, "GET", "/api/bench/aggregates", "suite", "benchmark", "profile", "since", "until", "limit_runs", "env"),
		rd("get_bench_trends", "A metric of one benchmark over successive runs, oldest first.", P{"benchmark": s("Benchmark id."), "metric": en("Metric.", "load_s", "load_mb_s", "memory_mib", "decode_tps", "prefill_tps", "ttft_ms", "unload_s", "concurrent_wall_s"),
			"limit": i("Newest N points.")}, []string{"benchmark", "metric"}, "GET", "/api/bench/trends", "benchmark", "metric", "limit"),
		rd("get_bench_schema", "The schema of benchmark and suite definitions: every field, type, default and limit. Read it before create_benchmark or create_suite.", nil, nil, "GET", "/api/bench/schema"),
		rd("list_external_results", "List the external benchmark result sets (results produced outside the bench runner and stored for the Benchmarks page): id, title, note, the tables with their columns and row counts, and the attachment names. No rows.", nil, nil, "GET", "/api/bench/external"),
		rd("get_external_results", "Get one external benchmark result set with its tables, rows and attachment texts.", idArg("external result set"), []string{"id"}, "GET", "/api/bench/external/{id}"),

		// ---- write ----
		{Name: "load_model", Desc: "Start loading a model ahead of time. Returns at once (HTTP 202); a load takes minutes, so poll get_status. Run plan_load first to see whether it fits and what it would evict.",
			Props: P{"model": s("Profile id or alias.")}, Required: []string{"model"}, Method: "POST", Path: "/api/load", Body: []string{"model"}},
		{Name: "unload_model", Desc: "Stop a loaded model. Without force it only stops an instance with no requests in flight. force=true cuts off running generations: use it only when the owner asked.",
			Props: P{"model": s("Profile id or alias."), "force": b("Stop even if requests are in flight (they are cut off). Default false.")}, Required: []string{"model"}, Destructive: true, Idempotent: true,
			Method: "POST", Path: "/api/unload", Body: []string{"model", "force"}},
		{Name: "update_settings", Desc: "Change global settings (timeouts, defaults, limits) by merging the given fields; fields you leave out keep their value." + cfgNote,
			Props: P{"settings": obj("Only the settings to change, as in get_config under settings.")}, Required: []string{"settings"}, Idempotent: true, Method: "PATCH", Path: "/api/config/settings", BodyArg: "settings"},
		{Name: "create_profile", Desc: "Add a profile (a model configuration). Check get_config_schema for the fields." + cfgNote,
			Props: P{"profile": obj("The profile, including its id.")}, Required: []string{"profile"}, Method: "POST", Path: "/api/config/profiles", BodyArg: "profile"},
		{Name: "update_profile", Desc: "Replace a profile with the given definition (send the whole profile, not a patch)." + cfgNote,
			Props: P{"id": s("The profile id."), "profile": obj("The complete new profile.")}, Required: []string{"id", "profile"}, Destructive: true, Idempotent: true,
			Method: "PUT", Path: "/api/config/profiles/{id}", BodyArg: "profile"},
		{Name: "delete_profile", Desc: "Delete a profile. Fails or is refused while it is loaded or referenced." + cfgNote,
			Props: idArg("profile"), Required: []string{"id"}, Destructive: true, Idempotent: true, Method: "DELETE", Path: "/api/config/profiles/{id}"},
		{Name: "set_tiers", Desc: "Replace the node tier list (how nodes are matched, prioritised and budgeted)." + cfgNote,
			Props: P{"value": anyv(valNote)}, Required: []string{"value"}, Destructive: true, Idempotent: true, Method: "PUT", Path: "/api/config/tiers", BodyArg: "value"},
		{Name: "set_exclude_nodes", Desc: "Replace the list of nodes kept out of the pool." + cfgNote,
			Props: P{"value": anyv(valNote)}, Required: []string{"value"}, Destructive: true, Idempotent: true, Method: "PUT", Path: "/api/config/exclude-nodes", BodyArg: "value"},
		{Name: "set_no_auto_nodes", Desc: "Replace the list of nodes that automatic placement never picks. A profile that names such a node in force_nodes can still use it; exclude_nodes still wins." + cfgNote,
			Props: P{"value": anyv(valNote)}, Required: []string{"value"}, Destructive: true, Idempotent: true, Method: "PUT", Path: "/api/config/no-auto-nodes", BodyArg: "value"},
		{Name: "set_node_overrides", Desc: "Replace the per-node overrides (address, port and similar for nodes the fleet reports incompletely)." + cfgNote,
			Props: P{"value": anyv(valNote)}, Required: []string{"value"}, Destructive: true, Idempotent: true, Method: "PUT", Path: "/api/config/node-overrides", BodyArg: "value"},
		{Name: "set_recovery_settings", Desc: "Replace the worker-health and recovery-request settings. Not used with membership agents." + cfgNote,
			Props: P{"value": anyv(valNote)}, Required: []string{"value"}, Destructive: true, Idempotent: true, Method: "PUT", Path: "/api/config/recovery", BodyArg: "value"},
		{Name: "set_fleet_settings", Desc: "Replace the fleet-manager connection settings. Not used with membership agents (the scheduler's own node name is the local_node setting)." + cfgNote,
			Props: P{"value": anyv(valNote)}, Required: []string{"value"}, Destructive: true, Idempotent: true, Method: "PUT", Path: "/api/config/fleet", BodyArg: "value"},
		{Name: "rollback_config", Desc: "Restore the configuration saved as an earlier revision. This itself creates a new revision." + cfgNote,
			Props: P{"revision": i("The revision number to restore (see list_config_revisions).")}, Required: []string{"revision"}, Destructive: true,
			Method: "POST", Path: "/api/config/revisions/{revision}/rollback"},
		{Name: "create_benchmark", Desc: "Create a benchmark definition (a test case run through the scheduler). Read get_bench_schema first.",
			Props: P{"benchmark": obj("The benchmark, including its id.")}, Required: []string{"benchmark"}, Method: "POST", Path: "/api/bench/benchmarks", BodyArg: "benchmark"},
		{Name: "update_benchmark", Desc: "Replace a benchmark definition (send the whole definition).",
			Props: P{"id": s("The benchmark id."), "benchmark": obj("The complete new benchmark.")}, Required: []string{"id", "benchmark"}, Destructive: true, Idempotent: true,
			Method: "PUT", Path: "/api/bench/benchmarks/{id}", BodyArg: "benchmark"},
		{Name: "delete_benchmark", Desc: "Delete a benchmark definition. Refused while a suite names it.",
			Props: idArg("benchmark"), Required: []string{"id"}, Destructive: true, Idempotent: true, Method: "DELETE", Path: "/api/bench/benchmarks/{id}"},
		{Name: "create_suite", Desc: "Create a benchmark suite: an ordered list of benchmarks with run options. A scenario that cannot run yet takes status planned or blocked, a reason, and may list no benchmark.",
			Props: P{"suite": obj("The suite, including its id.")}, Required: []string{"suite"}, Method: "POST", Path: "/api/bench/suites", BodyArg: "suite"},
		{Name: "update_suite", Desc: "Replace a benchmark suite (send the whole definition).",
			Props: P{"id": s("The suite id."), "suite": obj("The complete new suite.")}, Required: []string{"id", "suite"}, Destructive: true, Idempotent: true,
			Method: "PUT", Path: "/api/bench/suites/{id}", BodyArg: "suite"},
		{Name: "delete_suite", Desc: "Delete a benchmark suite (its past runs are kept).",
			Props: idArg("suite"), Required: []string{"id"}, Destructive: true, Idempotent: true, Method: "DELETE", Path: "/api/bench/suites/{id}"},
		{Name: "run_suite", Desc: "Queue a run of a suite. Benchmarks load and unload real models, so by default the run waits until the scheduler is idle. Returns the queued run; follow it with get_bench_state or get_run. A planned or blocked suite is refused (409) with its reason.",
			Props: P{"id": s("The suite id."), "baseline": s("Optional run id to compare against (default: the previous completed run)."), "note": s("Optional note stored with the run."),
				"evict_others": b("Optional, for this run only (default: the suite's setting): do not wait for other users' idle loaded models.")},
			Required: []string{"id"}, Method: "POST", Path: "/api/bench/suites/{id}/run", Body: []string{"baseline", "note", "evict_others"}},
		{Name: "cancel_run", Desc: "Cancel a queued or running benchmark run; what it loaded is unloaded.",
			Props: idArg("run"), Required: []string{"id"}, Destructive: true, Idempotent: true, Method: "POST", Path: "/api/bench/runs/{id}/cancel"},

		// ---- agent nodes (membership agents; agent contract 7.2) ----
		{Name: "pause_node", Desc: "Pause an agent node: no new load is placed on it; a load already using it continues. Stored, survives restarts. Only with membership agents.",
			Props: P{"id": s("The node id (the agent's id, a MAC address)."), "comment": s("Required: why, stored in the audit log.")}, Required: []string{"id", "comment"}, Idempotent: true,
			Method: "POST", Path: "/api/nodes/{id}/pause", Body: []string{"comment"}},
		{Name: "unpause_node", Desc: "Return a paused agent node to the pool. Only with membership agents.",
			Props: P{"id": s("The node id."), "comment": s("Required: why, stored in the audit log.")}, Required: []string{"id", "comment"}, Idempotent: true,
			Method: "POST", Path: "/api/nodes/{id}/unpause", Body: []string{"comment"}},
		{Name: "force_unload_node", Desc: "Force unload an agent node: the model instance using it is killed (requests in flight are cut off) and the node's launch is stopped with force, then the agent is reset if it does not stop within 5 s. Use it only when the owner asked. Only with membership agents.",
			Props: P{"id": s("The node id."), "comment": s("Required: why, stored in the audit log.")}, Required: []string{"id", "comment"}, Destructive: true, Idempotent: true,
			Method: "POST", Path: "/api/nodes/{id}/force-unload", Body: []string{"comment"}},
	}
	for _, t := range T {
		if !t.ReadOnly {
			t.write = true
			t.configWrite = strings.HasPrefix(t.Path, "/api/config")
		}
		if t.Name == "query_logs" {
			t.Pre = func(a map[string]any) {
				if _, ok := a["limit"]; !ok {
					a["limit"] = 100 // an answer is held in memory whole; keep it small unless asked
				}
			}
		}
		if t.Name == "export_run" {
			t.Pre = func(a map[string]any) {
				if a["format"] == "markdown" {
					a["format"] = "md"
				}
			}
		}
	}
	return T
}
