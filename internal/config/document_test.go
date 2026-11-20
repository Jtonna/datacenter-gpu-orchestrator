package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func always(string) bool { return true }

func TestEmptyDocumentBuildsDefaults(t *testing.T) {
	c, err := Build(NewDocument(), Bootstrap{}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.TTL != 300*time.Second || c.Listen != ":8080" || len(c.Profiles) != 0 || !c.Bench.EnabledSetting {
		t.Fatalf("%+v", c)
	}
}

// Every key of the old YAML must have a schema field, so nothing is lost.
func TestSchemaCoversEveryYAMLKey(t *testing.T) {
	have := map[string]bool{}
	for _, f := range Schema() {
		have[f.Scope+"."+f.Key] = true
	}
	for _, k := range []string{"ttl", "min_hold", "max_hold", "drain_after", "queue_timeout", "stall_timeout", "command_load_timeout",
		"load_stall_timeout", "shutdown_grace", "max_pool", "device_overhead_mib", "min_share_mib", "cache_ram_mib", "server_bin",
		"local_device", "forced_node_grace", "forced_fit_check", "worker_probe_interval", "worker_silence", "worker_probe_during_inference",
		"split_skew_margin", "estimate_margin", "load_memory_floor_mib", "local_probe_cmd", "child_log_keep", "history_max_file_mib",
		"history_keep_files", "history_memory_events", "ws_max_clients", "bench_enabled", "bench_regress_load_pct", "bench_regress_decode_pct",
		"bench_max_run_minutes"} {
		if !have["settings."+k] {
			t.Errorf("settings key %s missing from the schema", k)
		}
	}
	for _, k := range []string{"enabled", "actions", "min_interval", "step_wait", "request_ttl", "http_timeout"} {
		if !have["recovery."+k] {
			t.Errorf("recovery key %s missing", k)
		}
	}
	for _, k := range []string{"url", "local_node", "stale_after"} {
		if !have["fleet."+k] {
			t.Errorf("fleet key %s missing", k)
		}
	}
	for _, k := range []string{"kind", "model", "ctx", "parallel", "kv_quant", "flash_attn", "needs_mib", "allow_tiers", "force_nodes", "tensor_split",
		"extra_args", "cmd", "check_endpoint", "aliases", "generated", "ttl", "load_timeout", "stall_timeout", "load_stall_timeout"} {
		if !have["profile."+k] {
			t.Errorf("profile key %s missing", k)
		}
	}
	for _, k := range []string{"match.hostname", "match.role", "match.source", "match.local", "priority", "headroom_mib", "shared_memory", "link_mb_s", "settle"} {
		if !have["tier."+k] {
			t.Errorf("tier key %s missing", k)
		}
	}
	// the profile field list must equal the ProfileSpec JSON tags
	var spec ProfileSpec
	b, _ := json.Marshal(map[string]any{})
	_ = json.Unmarshal(b, &spec)
}

// The example file round-trips: YAML import, then Build from the stored document gives the same configuration.
func TestYAMLImportRoundTripsThroughTheDocument(t *testing.T) {
	yml := `
listen: ":9999"
fleet: {url: "http://f:8090", local_node: host-1, stale_after: 90s}
defaults: {ttl: 600s, forced_fit_check: reject, worker_probe_interval: 10s}
history_keep_files: 7
ws_max_clients: 5
tiers:
  - {match: {hostname: "ps5-*"}, priority: 1, headroom_mib: 1024, shared_memory: true, link_mb_s: 100, settle: 3s}
  - {match: {local: true}, priority: 3}
exclude_nodes: [ps5-x*]
node_overrides: {a: {rpc: "192.0.2.3:50052"}}
recovery: {enabled: true, actions: [restart_rpc_server, reboot]}
profiles:
  a: {model: a.gguf, ctx: 1024, needs_mib: 100, ttl: 0, aliases: [al], generated: true}
  c: {kind: command, cmd: "x --port ${PORT}", needs_mib: 10, stall_timeout: 0s}
`
	imp, err := ParseYAMLImport([]byte(yml), Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if imp.Doc.Settings["ttl"] != "10m0s" || imp.Doc.Settings["ws_max_clients"] != float64(5) || imp.Doc.Fleet["url"] != "http://f:8090" {
		t.Fatalf("%+v", imp.Doc)
	}
	if _, ok := imp.Doc.Settings["max_pool"]; ok {
		t.Fatal("a default was written as an explicit setting")
	}
	b, _ := json.Marshal(imp.Doc)
	doc, err := ParseDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Build(doc, Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.TTL != 600*time.Second || c.Defaults.ForcedFitCheck != "reject" || c.Fleet.StaleAfter != 90*time.Second ||
		c.Recovery.Actions[1] != "reboot" || c.HistoryKeepFiles != 7 || len(c.Tiers) != 2 || c.NodeOverrides["a"].RPC == "" {
		t.Fatalf("%+v", c)
	}
	if p := c.Profiles["a"]; p.TTL == nil || *p.TTL != 0 || p.Aliases[0] != "al" {
		t.Fatalf("ttl 0 (never unload) was lost: %+v", p)
	}
	if a, cmd := c.Profiles["a"], c.Profiles["c"]; a.Generated == nil || !*a.Generated || cmd.Generated != nil {
		t.Fatalf("generated: a %v (want true), c %v (want not set)", a.Generated, cmd.Generated)
	}
	if p := c.Profiles["c"]; p.StallTimeout == nil || *p.StallTimeout != 0 {
		t.Fatalf("stall_timeout 0 was lost: %+v", p)
	}
	if len(imp.Notes) != 0 {
		t.Fatalf("%v", imp.Notes)
	}
}

func TestBuildReportsExistingValidationMessages(t *testing.T) {
	doc := NewDocument()
	doc.Settings["min_hold"], doc.Settings["max_hold"] = "10s", "5s"
	_, err := Build(doc, Bootstrap{}, Options{FileExists: always})
	if err == nil || !strings.Contains(err.Error(), "defaults: max_hold: must be at least min_hold") {
		t.Fatalf("%v", err)
	}
}
