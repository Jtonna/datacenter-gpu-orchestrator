package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Scopes of a Field: where its value lives in the configuration document.
const (
	ScopeSettings = "settings"
	ScopeRecovery = "recovery"
	ScopeFleet    = "fleet"
	ScopeProfile  = "profile"
	ScopeTier     = "tier"
)

// Field describes one configurable value, for validation and for generic forms.
type Field struct {
	Key             string   `json:"key"`
	Scope           string   `json:"scope"`
	Section         string   `json:"section"`
	Type            string   `json:"type"` // duration, int, float, bool, enum, string, list
	Enum            []string `json:"enum,omitempty"`
	Default         any      `json:"default"`
	Min             *float64 `json:"min,omitempty"`
	Max             *float64 `json:"max,omitempty"`
	Unit            string   `json:"unit,omitempty"`
	Description     string   `json:"description"`
	RestartRequired bool     `json:"restart_required"`
	Required        bool     `json:"required,omitempty"`
	AppliesTo       string   `json:"applies_to,omitempty"` // profile fields: llama or command
	Nullable        bool     `json:"nullable,omitempty"`   // null or absent means "use the default"

	ptr func(c *Config) any
}

func num(v float64) *float64 { return &v }

func mk(scope, section, key, typ, unit, desc string, ptr func(c *Config) any, min, max *float64) Field {
	return Field{Key: key, Scope: scope, Section: section, Type: typ, Unit: unit, Description: desc, ptr: ptr, Min: min, Max: max}
}

// settingFields is every key of the settings, recovery and fleet scopes.
var settingFields = buildSettingFields()

func buildSettingFields() []Field {
	S, R, F := ScopeSettings, ScopeRecovery, ScopeFleet
	d := func(key, sec, desc string, p func(c *Config) *time.Duration, min *float64) Field {
		return mk(S, sec, key, "duration", "seconds", desc, func(c *Config) any { return p(c) }, min, nil)
	}
	i := func(key, sec, desc, unit string, p func(c *Config) *int, min, max *float64) Field {
		return mk(S, sec, key, "int", unit, desc, func(c *Config) any { return p(c) }, min, max)
	}
	f := func(key, sec, desc string, p func(c *Config) *float64, min, max *float64) Field {
		return mk(S, sec, key, "float", "", desc, func(c *Config) any { return p(c) }, min, max)
	}
	b := func(key, sec, desc string, p func(c *Config) *bool) Field {
		return mk(S, sec, key, "bool", "", desc, func(c *Config) any { return p(c) }, nil, nil)
	}
	s := func(key, sec, desc string, p func(c *Config) *string) Field {
		return mk(S, sec, key, "string", "", desc, func(c *Config) any { return p(c) }, nil, nil)
	}
	fs := []Field{
		d("ttl", "timing", "How long a model may sit idle before it is unloaded; 0 means never unload.", func(c *Config) *time.Duration { return &c.Defaults.TTL }, num(0)),
		d("min_hold", "timing", "Least time an instance is protected from being displaced by other requests.", func(c *Config) *time.Duration { return &c.Defaults.MinHold }, num(1)),
		d("max_hold", "timing", "Longest time an instance is protected from being displaced; must be at least min_hold.", func(c *Config) *time.Duration { return &c.Defaults.MaxHold }, num(1)),
		d("drain_after", "timing", "How long a busy model is left alone before a waiting request asks it to drain.", func(c *Config) *time.Duration { return &c.Defaults.DrainAfter }, num(1)),
		d("queue_timeout", "timing", "Longest a request may wait in the queue without being given a model.", func(c *Config) *time.Duration { return &c.Defaults.QueueTimeout }, num(1)),
		d("stall_timeout", "timing", "Longest gap between streamed chunks once a response has begun; 0 disables the check.", func(c *Config) *time.Duration { return &c.Defaults.StallTimeout }, num(0)),
		d("command_load_timeout", "timing", "Load timeout for profiles of kind command.", func(c *Config) *time.Duration { return &c.Defaults.CommandLoadTimeout }, num(1)),
		d("load_stall_timeout", "timing", "Fail a load whose measured upload acknowledges no new bytes for this long; 0 turns the rule off.", func(c *Config) *time.Duration { return &c.Defaults.LoadStallTimeout }, num(0)),
		d("shutdown_grace", "timing", "How long a clean shutdown waits for in-flight requests before killing children.", func(c *Config) *time.Duration { return &c.Defaults.ShutdownGrace }, num(1)),
		d("forced_node_grace", "timing", "How long a forced node may be absent or offline before requests for the profile fail at once.", func(c *Config) *time.Duration { return &c.Defaults.ForcedNodeGrace }, num(1)),
		i("max_pool", "capacity", "Most nodes a single model may be split across.", "nodes", func(c *Config) *int { return &c.Defaults.MaxPool }, num(1), nil),
		i("device_overhead_mib", "capacity", "Per-device memory cost added to estimated needs only.", "MiB", func(c *Config) *int { return &c.Defaults.DeviceOverheadMiB }, num(0), nil),
		i("min_share_mib", "capacity", "Least memory a node must be able to hold to take part in a split.", "MiB", func(c *Config) *int { return &c.Defaults.MinShareMiB }, num(0), nil),
		i("cache_ram_mib", "capacity", "Per-instance prompt cache limit on the host.", "MiB", func(c *Config) *int { return &c.Defaults.CacheRAMMiB }, num(0), nil),
		s("server_bin", "runtime", "The llama.cpp server binary started for each model; read once at start because pre-flight probing uses it.", func(c *Config) *string { return &c.Defaults.ServerBin }),
		s("local_device", "runtime", "The llama.cpp device name of the local GPU (for example CUDA0).", func(c *Config) *string { return &c.Defaults.LocalDevice }),
		s("local_probe_cmd", "runtime", "Command that reports the local GPU's total and used memory.", func(c *Config) *string { return &c.LocalProbeCmd }),
		{Key: "forced_fit_check", Scope: S, Section: "safety", Type: "enum", Enum: []string{ForcedFitOff, ForcedFitWarn, ForcedFitReject},
			Description: "What a profile with force_nodes does when its memory need exceeds the forced nodes' free memory: off, warn (log and start) or reject.",
			ptr:         func(c *Config) any { return &c.Defaults.ForcedFitCheck }},
		f("split_skew_margin", "safety", "Extra share a node of a split model may receive beyond its split fraction (0 to 1).", func(c *Config) *float64 { return &c.Defaults.SplitSkewMargin }, num(0), num(1)),
		f("estimate_margin", "safety", "Margin added to every memory need that is only an estimate (0 to 1).", func(c *Config) *float64 { return &c.Defaults.EstimateMargin }, num(0), num(1)),
		i("load_memory_floor_mib", "safety", "Cancel a load when a shared-memory node's available memory falls below this; 0 turns the watch off.", "MiB", func(c *Config) *int { return &c.Defaults.LoadMemoryFloorMiB }, num(0), nil),
		d("worker_silence", "workers", "How long a worker's upload progress or fleet heartbeat may stay quiet before it counts as silent.", func(c *Config) *time.Duration { return &c.Defaults.WorkerSilence }, num(1)),
		d("worker_probe_interval", "workers", "How often a TCP connect probe checks workers; 0 turns it off (leave it off for rpc-server workers).", func(c *Config) *time.Duration { return &c.Defaults.WorkerProbeInterval }, num(0)),
		b("worker_probe_during_inference", "workers", "Let the connect probe run while the instance has a request in flight.", func(c *Config) *bool { return &c.Defaults.WorkerProbeDuringInference }),
		{Key: "membership", Scope: S, Section: "agents", Type: "enum", Enum: []string{MembershipFleet, MembershipAgents},
			Description: "Where the pool's nodes come from: fleet (the fleet console feed) or agents (registrations of potluck-agent). Read once at start.",
			ptr:         func(c *Config) any { return &c.Defaults.Membership }},
		b("worker_model_dir", "agents", "Ask agent workers to read model files themselves (model_dir in the worker launch); off sends the weights over the worker connection.", func(c *Config) *bool { return &c.Defaults.WorkerModelDir }),
		i("watchdog_floor_mib", "agents", "The agents' memory watchdog floor; a device that shares system memory counts at most the node's available memory minus this.", "MiB", func(c *Config) *int { return &c.Defaults.WatchdogFloorMiB }, num(0), nil),
		i("child_log_keep", "retention", "Most child log files kept in the child log directory; the oldest are deleted.", "files", func(c *Config) *int { return &c.ChildLogKeep }, num(1), nil),
		i("history_max_file_mib", "retention", "Size at which an event history file is rotated.", "MiB", func(c *Config) *int { return &c.HistoryMaxFileMiB }, num(1), nil),
		i("history_keep_files", "retention", "Most event history files kept; the oldest are deleted.", "files", func(c *Config) *int { return &c.HistoryKeepFiles }, num(1), nil),
		i("history_memory_events", "retention", "Newest events held in memory for history queries.", "events", func(c *Config) *int { return &c.HistoryMemoryEvents }, num(1), num(MaxHistoryMemoryEvents)),
		i("audit_retention_days", "retention", "Audit entries older than this many days may be pruned by an operator; the application itself never deletes entries (0 keeps everything and is the default).", "days", func(c *Config) *int { return &c.AuditRetentionDays }, num(0), nil),
		i("ws_max_clients", "api", "Most concurrent live (WebSocket) clients.", "clients", func(c *Config) *int { return &c.WSMaxClients }, num(1), num(MaxWSMaxClients)),
		b("bench_enabled", "bench", "Turn the benchmark feature on or off.", func(c *Config) *bool { return &c.Bench.EnabledSetting }),
		f("bench_regress_load_pct", "bench", "A load this much slower than the baseline run's is flagged as a regression.", func(c *Config) *float64 { return &c.Bench.RegressLoadPct }, num(0.001), num(1000)),
		f("bench_regress_decode_pct", "bench", "Decode tokens per second this much lower than the baseline's is flagged as a regression.", func(c *Config) *float64 { return &c.Bench.RegressDecodePct }, num(0.001), num(100)),
		i("bench_max_run_minutes", "bench", "A benchmark run that takes longer than this is stopped and marked failed.", "minutes", func(c *Config) *int { return &c.Bench.MaxRunMinutes }, num(1), num(10080)),

		{Key: "enabled", Scope: R, Section: "recovery", Type: "bool", Description: "Send recovery requests for hung workers; off means nothing is ever sent.", ptr: func(c *Config) any { return &c.Recovery.Enabled }},
		{Key: "actions", Scope: R, Section: "recovery", Type: "list", Enum: RecoveryActions, Description: "Recovery actions in escalation order; each may appear once, in the order restart_rpc_server, restart_docker, reboot.", ptr: func(c *Config) any { return &c.Recovery.Actions }},
		{Key: "min_interval", Scope: R, Section: "recovery", Type: "duration", Unit: "seconds", Min: num(60), Description: "Least time between two recovery requests for one node.", ptr: func(c *Config) any { return &c.Recovery.MinInterval }},
		{Key: "step_wait", Scope: R, Section: "recovery", Type: "duration", Unit: "seconds", Min: num(1), Description: "How long a recovery step gets to take effect before the next one is sent.", ptr: func(c *Config) any { return &c.Recovery.StepWait }},
		{Key: "request_ttl", Scope: R, Section: "recovery", Type: "duration", Unit: "seconds", Min: num(1), Max: num(60), Description: "How long a recovery request stays valid at the node.", ptr: func(c *Config) any { return &c.Recovery.RequestTTL }},
		{Key: "http_timeout", Scope: R, Section: "recovery", Type: "duration", Unit: "seconds", Min: num(1), Description: "Timeout of the request that hands a recovery request to the fleet manager.", ptr: func(c *Config) any { return &c.Recovery.HTTPTimeout }},

		{Key: "url", Scope: F, Section: "fleet", Type: "string", Description: "Base URL of the fleet console; empty means only the local node is used. Read once when the fleet client starts.", ptr: func(c *Config) any { return &c.Fleet.URL }},
		{Key: "local_node", Scope: F, Section: "fleet", Type: "string", Description: "The fleet name of the host the scheduler runs on. Read once when the fleet client starts.", ptr: func(c *Config) any { return &c.Fleet.LocalNode }},
		{Key: "stale_after", Scope: F, Section: "fleet", Type: "duration", Unit: "seconds", Min: num(1), Description: "Age of the fleet view beyond which remote nodes are not used.", ptr: func(c *Config) any { return &c.Fleet.StaleAfter }},
	}
	// Restart-required: read once at startup by long-lived components.
	restart := map[string]string{"server_bin": S, "child_log_keep": S, "history_max_file_mib": S, "history_keep_files": S,
		"history_memory_events": S, "ws_max_clients": S, "bench_enabled": S, "bench_regress_load_pct": S, "bench_regress_decode_pct": S,
		"bench_max_run_minutes": S, "url": F, "local_node": F, "membership": S}
	base := newBase(Bootstrap{})
	for n := range fs {
		if sc, ok := restart[fs[n].Key]; ok && sc == fs[n].Scope {
			fs[n].RestartRequired = true
		}
		fs[n].Default = normalizeValue(derefPtr(fs[n].ptr(base)))
	}
	return fs
}

func derefPtr(p any) any {
	switch v := p.(type) {
	case *time.Duration:
		return *v
	case *int:
		return *v
	case *float64:
		return *v
	case *bool:
		return *v
	case *string:
		return *v
	case *[]string:
		out := make([]any, len(*v))
		for i, s := range *v {
			out[i] = s
		}
		return out
	}
	return nil
}

// normalizeValue gives the JSON form of a typed value (durations become strings).
func normalizeValue(v any) any {
	switch x := v.(type) {
	case time.Duration:
		return x.String()
	case int:
		return float64(x)
	}
	return v
}

// SettingFields returns the fields of a scope (settings, recovery or fleet).
func SettingFields(scope string) []Field {
	var out []Field
	for _, f := range settingFields {
		if f.Scope == scope {
			out = append(out, f)
		}
	}
	return out
}

func findField(scope, key string) (*Field, bool) {
	for i := range settingFields {
		if settingFields[i].Scope == scope && settingFields[i].Key == key {
			return &settingFields[i], true
		}
	}
	return nil, false
}

// SettingValue is the current value of a field of c in JSON form.
func SettingValue(c *Config, scope, key string) any {
	f, ok := findField(scope, key)
	if !ok {
		return nil
	}
	return normalizeValue(derefPtr(f.ptr(c)))
}

// CoerceSetting checks that v (a decoded JSON value) fits the field's type and returns its stored JSON form.
func CoerceSetting(scope, key string, v any) (any, error) {
	f, ok := findField(scope, key)
	if !ok {
		return nil, fmt.Errorf("unknown %s setting %q", scope, key)
	}
	switch f.Type {
	case "duration":
		d, err := ParseDurationValue(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", key, err)
		}
		return d.String(), nil
	case "int":
		x, ok := v.(float64)
		if !ok || x != math.Trunc(x) || math.Abs(x) > 1e12 {
			return nil, fmt.Errorf("%s: must be a whole number", key)
		}
		return x, nil
	case "float":
		x, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("%s: must be a number", key)
		}
		return x, nil
	case "bool":
		x, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s: must be true or false", key)
		}
		return x, nil
	case "string", "enum":
		x, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: must be a string", key)
		}
		return strings.TrimSpace(x), nil
	case "list":
		raw, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a list of strings", key)
		}
		out := make([]any, len(raw))
		for i, e := range raw {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s: entry %d must be a string", key, i)
			}
			out[i] = strings.TrimSpace(s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: unsupported type %s", key, f.Type)
}

func applyValue(f *Field, c *Config, v any) {
	switch p := f.ptr(c).(type) {
	case *time.Duration:
		d, _ := ParseDurationValue(v)
		*p = d
	case *int:
		*p = int(v.(float64))
	case *float64:
		*p = v.(float64)
	case *bool:
		*p = v.(bool)
	case *string:
		*p = v.(string)
	case *[]string:
		out := []string{}
		for _, e := range v.([]any) {
			out = append(out, e.(string))
		}
		*p = out
	}
}

// profileFields and tierFields describe the profile and tier documents.
var profileFields = []Field{
	{Key: "kind", Section: "identity", Type: "enum", Enum: []string{string("llama"), string("command")}, Default: "llama", Description: "How the profile is started: llama (a llama-server the scheduler places on nodes) or command (a fixed command on the local node)."},
	{Key: "model", Section: "model", Type: "string", AppliesTo: "llama", Required: true, Description: "Model file name under the models directory."},
	{Key: "ctx", Section: "model", Type: "int", AppliesTo: "llama", Required: true, Min: num(1), Unit: "tokens", Description: "Context size per slot."},
	{Key: "parallel", Section: "model", Type: "int", AppliesTo: "llama", Default: 1.0, Min: num(1), Description: "Number of parallel slots."},
	{Key: "kv_quant", Section: "model", Type: "enum", AppliesTo: "llama", Enum: []string{"f16", "q8_0", "q4_0"}, Default: "f16", Description: "Key-value cache precision; q8_0 and q4_0 need flash_attn."},
	{Key: "flash_attn", Section: "model", Type: "bool", AppliesTo: "llama", Default: false, Description: "Turn flash attention on."},
	{Key: "needs_mib", Section: "memory", Type: "int", Default: 0.0, Min: num(0), Unit: "MiB", Description: "Measured memory need; 0 means estimate from the model file (required for kind command)."},
	{Key: "allow_tiers", Section: "placement", Type: "list", AppliesTo: "llama", Description: "Tier priorities this profile may use; empty means all."},
	{Key: "force_nodes", Section: "placement", Type: "list", AppliesTo: "llama", Description: "Node ids or hostnames, in device order, to use instead of automatic placement."},
	{Key: "tensor_split", Section: "placement", Type: "list", AppliesTo: "llama", Description: "Explicit split weights, one per forced node."},
	{Key: "extra_args", Section: "model", Type: "list", AppliesTo: "llama", Description: "Extra llama-server arguments appended last; flags the scheduler manages are refused."},
	{Key: "cmd", Section: "command", Type: "string", AppliesTo: "command", Required: true, Description: "Command line containing ${PORT}."},
	{Key: "check_endpoint", Section: "command", Type: "string", AppliesTo: "command", Description: "Readiness path polled on the command's port."},
	{Key: "aliases", Section: "identity", Type: "list", Description: "Other names that resolve to this profile."},
	{Key: "generated", Section: "identity", Type: "bool", Nullable: true, Description: "Marks a profile that a tool generated (a benchmark matrix, a sweep) rather than a person wrote. Lists of models keep such profiles out of the way. Empty means not said: the id decides (bench-*, b-* and m-* count as generated)."},
	{Key: "ttl", Section: "timing", Type: "duration", Unit: "seconds", Nullable: true, Min: num(0), Description: "Idle unload time for this profile; empty uses the default, 0 never unloads."},
	{Key: "load_timeout", Section: "timing", Type: "duration", Unit: "seconds", Default: "0s", Min: num(0), Description: "Load timeout for this profile; 0 uses the computed default."},
	{Key: "stall_timeout", Section: "timing", Type: "duration", Unit: "seconds", Nullable: true, Min: num(0), Description: "Longest gap between streamed chunks; empty uses the default, 0 disables the check."},
	{Key: "load_stall_timeout", Section: "timing", Type: "duration", Unit: "seconds", Nullable: true, Min: num(0), Description: "Fail a load with no acknowledged upload progress for this long; empty uses the default, 0 turns it off."},
}

var tierFields = []Field{
	{Key: "match.hostname", Section: "match", Type: "string", Description: "Case-insensitive hostname glob a node must match."},
	{Key: "match.role", Section: "match", Type: "string", Description: "Fleet role a node must have."},
	{Key: "match.source", Section: "match", Type: "string", Description: "Fleet source a node must have."},
	{Key: "match.local", Section: "match", Type: "bool", Nullable: true, Description: "True matches only the local host, false only remote nodes."},
	{Key: "priority", Section: "tier", Type: "int", Required: true, Min: num(1), Description: "Tier priority; lower is preferred."},
	{Key: "headroom_mib", Section: "tier", Type: "int", Default: 0.0, Min: num(0), Unit: "MiB", Description: "Memory kept free on a node of this tier."},
	{Key: "shared_memory", Section: "tier", Type: "bool", Default: false, Description: "The node's GPU memory is system RAM."},
	{Key: "link_mb_s", Section: "tier", Type: "float", Default: 0.0, Min: num(0), Unit: "MB/s", Description: "Link speed used to size load timeouts."},
	{Key: "settle", Section: "tier", Type: "duration", Unit: "seconds", Default: "0s", Min: num(0), Description: "How long a node is unusable after an instance releases it."},
}

// Schema returns every field: settings, recovery, fleet, profile and tier.
func Schema() []Field {
	out := append([]Field(nil), settingFields...)
	for _, f := range profileFields {
		f.Scope = ScopeProfile
		out = append(out, f)
	}
	for _, f := range tierFields {
		f.Scope = ScopeTier
		out = append(out, f)
	}
	return out
}

// SettingFieldByKey looks up a field of the settings, recovery or fleet scope.
func SettingFieldByKey(scope, key string) (Field, bool) {
	f, ok := findField(scope, key)
	if !ok {
		return Field{}, false
	}
	return *f, true
}
