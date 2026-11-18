package config

import (
	"strings"
	"testing"
)

const baseHeader = `
models_dir: /models
tiers:
  - match: { local: true }
    priority: 3
`

// prof wraps profile YAML (already indented under "profiles:") in a config.
func prof(body string) string { return baseHeader + "profiles:\n" + body }

func parseErr(yaml string, exists func(string) bool) error {
	if exists == nil {
		exists = existsAll
	}
	_, err := ParseWithOptions([]byte(yaml), Options{FileExists: exists})
	return err
}

func TestValidateRejects(t *testing.T) {
	llama := "  p:\n    model: m.gguf\n    ctx: 4096\n"
	tests := []struct {
		name string
		yaml string
		want []string // every substring must appear in the error
	}{
		{"unknown top-level field", baseHeader + "bogus: 1\nprofiles: {}\n", []string{"bogus"}},
		{"unknown profile field", prof(llama + "    ctxx: 1\n"), []string{"ctxx"}},
		{"forced_node_grace zero", baseHeader + "defaults: {forced_node_grace: 0}\nprofiles: {}\n", []string{"forced_node_grace", "positive"}},
		{"forced_node_grace negative", baseHeader + "defaults: {forced_node_grace: -5s}\nprofiles: {}\n", []string{"forced_node_grace", "positive"}},
		{"forced_fit_check unknown", baseHeader + "defaults: {forced_fit_check: maybe}\nprofiles: {}\n", []string{"forced_fit_check", "off", "warn", "reject"}},
		{"child_log_keep zero", baseHeader + "child_log_keep: 0\nprofiles: {}\n", []string{"child_log_keep", "at least 1"}},
		{"unknown defaults field", baseHeader + "defaults: {ttll: 5s}\n", []string{"ttll"}},
		{"duplicate profile name", prof(llama + "  p:\n    model: m.gguf\n    ctx: 1\n"), []string{"p"}},
		{"alias equals other profile", prof(llama + "  q:\n    model: m.gguf\n    ctx: 1\n    aliases: [p]\n"),
			[]string{`profile "q"`, "aliases", `"p"`, "profile name"}},
		{"alias equals own name", prof(llama + "    aliases: [p]\n"),
			[]string{`profile "p"`, "aliases", `"p"`}},
		{"alias used twice across profiles", prof("  p:\n    model: m.gguf\n    ctx: 1\n    aliases: [x]\n  q:\n    model: m.gguf\n    ctx: 1\n    aliases: [x]\n"),
			[]string{"aliases", `"x"`, "already used"}},
		{"alias repeated in one list", prof(llama + "    aliases: [x, x]\n"),
			[]string{`profile "p"`, "aliases", `"x"`}},
		{"force_nodes with allow_tiers", prof(llama + "    force_nodes: [a]\n    allow_tiers: [1]\n"),
			[]string{`profile "p"`, "force_nodes", "allow_tiers"}},
		{"tensor_split without force_nodes", prof(llama + "    tensor_split: [1, 2]\n"),
			[]string{`profile "p"`, "tensor_split", "force_nodes"}},
		{"tensor_split length differs", prof(llama + "    force_nodes: [a, b]\n    tensor_split: [1]\n"),
			[]string{`profile "p"`, "tensor_split", "length"}},
		{"tensor_split non-positive", prof(llama + "    force_nodes: [a, b]\n    tensor_split: [1, 0]\n"),
			[]string{`profile "p"`, "tensor_split"}},
		{"q8 kv without flash_attn", prof(llama + "    kv_quant: q8_0\n"),
			[]string{`profile "p"`, "kv_quant", "flash_attn"}},
		{"q4 kv with flash_attn false", prof(llama + "    kv_quant: q4_0\n    flash_attn: false\n"),
			[]string{`profile "p"`, "kv_quant", "flash_attn"}},
		{"kv_quant unknown", prof(llama + "    kv_quant: q5_0\n    flash_attn: true\n"),
			[]string{`profile "p"`, "kv_quant", "q5_0"}},
		{"ctx missing", prof("  p:\n    model: m.gguf\n"), []string{`profile "p"`, "ctx"}},
		{"ctx zero", prof("  p:\n    model: m.gguf\n    ctx: 0\n"), []string{`profile "p"`, "ctx", "positive"}},
		{"parallel zero", prof(llama + "    parallel: 0\n"), []string{`profile "p"`, "parallel", "positive"}},
		{"parallel negative", prof(llama + "    parallel: -2\n"), []string{`profile "p"`, "parallel"}},
		{"model missing", prof("  p:\n    ctx: 1\n"), []string{`profile "p"`, "model"}},
		{"needs_mib negative", prof(llama + "    needs_mib: -1\n"), []string{`profile "p"`, "needs_mib"}},
		{"allow_tiers below 1", prof(llama + "    allow_tiers: [0]\n"), []string{`profile "p"`, "allow_tiers"}},
		{"unknown kind", prof("  p:\n    kind: weird\n"), []string{`profile "p"`, "kind", "weird"}},
		{"negative ttl", prof(llama + "    ttl: -5s\n"), []string{`profile "p"`, "ttl"}},
		{"command without cmd", prof("  c:\n    kind: command\n    needs_mib: 10\n"),
			[]string{`profile "c"`, "cmd"}},
		{"command without needs_mib", prof("  c:\n    kind: command\n    cmd: x ${PORT}\n"),
			[]string{`profile "c"`, "needs_mib"}},
		{"command cmd lacks PORT", prof("  c:\n    kind: command\n    cmd: x\n    needs_mib: 1\n"),
			[]string{`profile "c"`, "cmd", "${PORT}"}},
		{"llama with cmd", prof(llama + "    cmd: x\n"), []string{`profile "p"`, "cmd", "kind: command"}},
		{"tier priority zero", "tiers:\n  - match: { local: true }\n    priority: 0\n", []string{"tiers[0]", "priority"}},
		{"tier empty match", "tiers:\n  - match: {}\n    priority: 1\n", []string{"tiers[0]", "match"}},
		{"tier bad glob", "tiers:\n  - match: { hostname: \"[\" }\n    priority: 1\n", []string{"tiers[0]", "hostname"}},
		{"tier negative headroom", "tiers:\n  - match: { local: true }\n    priority: 1\n    headroom_mib: -1\n", []string{"tiers[0]", "headroom_mib"}},
		{"override rpc no port", baseHeader + "node_overrides:\n  n: { rpc: \"10.0.0.3\" }\n", []string{`node_overrides "n"`, "rpc"}},
		{"override rpc hostname", baseHeader + "node_overrides:\n  n: { rpc: \"alien:50052\" }\n", []string{`node_overrides "n"`, "rpc"}},
		{"override rpc port range", baseHeader + "node_overrides:\n  n: { rpc: \"10.0.0.3:70000\" }\n", []string{`node_overrides "n"`, "rpc"}},
		{"override rpc ipv6", baseHeader + "node_overrides:\n  n: { rpc: \"[::1]:5\" }\n", []string{`node_overrides "n"`, "rpc"}},
		{"defaults max_pool zero", baseHeader + "defaults: {max_pool: 0}\n", []string{"defaults", "max_pool"}},
		{"stale_after zero", baseHeader + "fleet: {stale_after: 0s}\n", []string{"fleet", "stale_after", "positive"}},
		{"stale_after negative", baseHeader + "fleet: {stale_after: -1s}\n", []string{"fleet", "stale_after", "positive"}},
		{"min_hold zero", baseHeader + "defaults: {min_hold: 0s}\n", []string{"defaults", "min_hold", "positive"}},
		{"max_hold zero", baseHeader + "defaults: {min_hold: 1s, max_hold: 0s}\n", []string{"defaults", "max_hold", "positive"}},
		{"max_hold below min_hold", baseHeader + "defaults: {min_hold: 10s, max_hold: 5s}\n", []string{"defaults", "max_hold", "min_hold"}},
		{"drain_after zero", baseHeader + "defaults: {drain_after: 0s}\n", []string{"defaults", "drain_after", "positive"}},
		{"queue_timeout zero", baseHeader + "defaults: {queue_timeout: 0s}\n", []string{"defaults", "queue_timeout", "positive"}},
		{"shutdown_grace zero", baseHeader + "defaults: {shutdown_grace: 0s}\n", []string{"defaults", "shutdown_grace", "positive"}},
		{"command_load_timeout zero", baseHeader + "defaults: {command_load_timeout: 0s}\n", []string{"defaults", "command_load_timeout", "positive"}},
		{"max_pool negative", baseHeader + "defaults: {max_pool: -1}\n", []string{"defaults", "max_pool"}},
		{"min_share_mib negative", baseHeader + "defaults: {min_share_mib: -1}\n", []string{"defaults", "min_share_mib", "negative"}},
		{"device_overhead_mib negative", baseHeader + "defaults: {device_overhead_mib: -1}\n", []string{"defaults", "device_overhead_mib", "negative"}},
		{"cache_ram_mib negative", baseHeader + "defaults: {cache_ram_mib: -1}\n", []string{"defaults", "cache_ram_mib", "negative"}},
		{"bare nonzero duration", baseHeader + "defaults: {ttl: 300}\n", []string{"line 6", "invalid duration"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(tt.yaml, nil)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err.Error(), w)
				}
			}
		})
	}
}

func TestValidateCommandRejectsLlamaOnlyFields(t *testing.T) {
	fields := map[string]string{
		"model": "m.gguf", "ctx": "10", "parallel": "1", "kv_quant": "f16", "flash_attn": "false",
		"allow_tiers": "[1]", "force_nodes": "[a]", "tensor_split": "[1]", "extra_args": "[-x]",
	}
	for f, v := range fields {
		t.Run(f, func(t *testing.T) {
			y := prof("  c:\n    kind: command\n    cmd: run ${PORT}\n    needs_mib: 5\n    " + f + ": " + v + "\n")
			err := parseErr(y, nil)
			if err == nil || !strings.Contains(err.Error(), `profile "c"`) || !strings.Contains(err.Error(), f) {
				t.Fatalf("want error naming profile c and %q, got %v", f, err)
			}
		})
	}
}

func TestValidateMissingModelFile(t *testing.T) {
	exists := func(p string) bool { return !strings.HasSuffix(p, "gone.gguf") }
	y := prof("  ok:\n    model: here.gguf\n    ctx: 1\n  bad:\n    model: gone.gguf\n    ctx: 1\n")
	err := parseErr(y, exists)
	if err == nil || !strings.Contains(err.Error(), `profile "bad"`) ||
		!strings.Contains(err.Error(), "model") || !strings.Contains(err.Error(), "gone.gguf") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), `profile "ok"`) {
		t.Errorf("existing model flagged: %v", err)
	}
	// Command profiles have no model file and must not be checked.
	y = prof("  c:\n    kind: command\n    cmd: run ${PORT}\n    needs_mib: 5\n")
	if err := parseErr(y, func(p string) bool { t.Errorf("unexpected check of %q", p); return false }); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExtraArgsManagedFlags(t *testing.T) {
	flags := []string{
		"--rpc", "--device", "-dev", "--tensor-split", "-ts", "--split-mode", "-sm", "--host", "--port",
		"--model", "-m", "--ctx-size", "-c", "--n-gpu-layers", "--gpu-layers", "-ngl", "--parallel", "-np",
		"--cache-type-k", "-ctk", "--cache-type-v", "-ctv", "--fit", "-fit", "--cache-ram", "-cram",
		"--flash-attn", "-fa", "--main-gpu", "-mg",
		// log verbosity: the scheduler always passes -lv 4 (design 7.4)
		"-lv", "--verbosity", "--log-verbosity", "-v", "--verbose", "--log-verbose", "--log-disable",
	}
	for _, f := range flags {
		for _, form := range []string{f, f + "=1"} {
			t.Run(form, func(t *testing.T) {
				y := prof("  p:\n    model: m.gguf\n    ctx: 1\n    extra_args: [\"--ok\", \"" + form + "\"]\n")
				err := parseErr(y, nil)
				if err == nil || !strings.Contains(err.Error(), `profile "p"`) ||
					!strings.Contains(err.Error(), "extra_args") || !strings.Contains(err.Error(), f) {
					t.Fatalf("want extra_args error naming %q, got %v", f, err)
				}
			})
		}
	}
	// Unmanaged flags are fine, including look-alikes.
	y := prof("  p:\n    model: m.gguf\n    ctx: 1\n    extra_args: [\"--no-warmup\", \"--chat-template\", \"--port-ish\", \"--threads=4\"]\n")
	if err := parseErr(y, nil); err != nil {
		t.Fatalf("unmanaged extra_args rejected: %v", err)
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	y := prof(`  p:
    model: m.gguf
    ctx: 4096
    kv_quant: q8_0
    flash_attn: true
    force_nodes: [a, b]
    tensor_split: [60, 40]
    aliases: [old]
  q:
    model: m.gguf
    ctx: 4096
    aliases: [old2]
`) + "node_overrides:\n  n: { rpc: \"10.0.0.3:50052\" }\n"
	if err := parseErr(y, nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	y := prof("  a:\n    ctx: 1\n  b:\n    model: m.gguf\n")
	err := parseErr(y, nil)
	if err == nil || !strings.Contains(err.Error(), `profile "a"`) || !strings.Contains(err.Error(), `profile "b"`) {
		t.Fatalf("want both profiles reported, got %v", err)
	}
}

// ttl 0 means never unload and stall_timeout 0 disables the check: both stay legal.
func TestValidateAcceptsZeroTTLAndStallTimeout(t *testing.T) {
	if err := parseErr(baseHeader+"defaults: {ttl: 0s, stall_timeout: 0s}\nprofiles: {}\n", nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExcludeNodes(t *testing.T) {
	llama := "  p:\n    model: m.gguf\n    ctx: 4096\n"
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"empty entry", baseHeader + "exclude_nodes: [\"\"]\n", []string{"exclude_nodes", "empty"}},
		{"blank entry", baseHeader + "exclude_nodes: [\"  \"]\n", []string{"exclude_nodes", "empty"}},
		{"bad glob", baseHeader + "exclude_nodes: [\"ps5-[\"]\n", []string{"exclude_nodes", "invalid glob", "ps5-["}},
		{"forced profile names excluded id", prof(llama+"    force_nodes: [\"70:66:2A:A9:A7:65\"]\n") +
			"exclude_nodes: [\"70:66:2a:a9:a7:65\"]\n", []string{`profile "p"`, "force_nodes", "70:66:2A:A9:A7:65", "excluded"}},
		{"forced profile names hostname matched by glob", prof(llama+"    force_nodes: [alienware-llm, ps5-test1]\n") +
			"exclude_nodes: [\"PS5-TEST*\"]\n", []string{`profile "p"`, "force_nodes", "ps5-test1", "excluded"}},
		{"forced profile names excluded local node", prof(llama+"    force_nodes: [omen-runner]\n") +
			"exclude_nodes: [omen-runner]\n", []string{`profile "p"`, "omen-runner", "excluded"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseErr(tt.yaml, nil)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err.Error(), w)
				}
			}
		})
	}
	// A forced name that only a runtime lookup can tie to an exclusion is not a load-time error.
	ok := prof(llama+"    force_nodes: [\"70:66:2a:a9:a7:65\"]\n") + "exclude_nodes: [\"ps5-*\"]\n"
	if err := parseErr(ok, nil); err != nil {
		t.Fatalf("glob vs id must be left to runtime: %v", err)
	}
}
