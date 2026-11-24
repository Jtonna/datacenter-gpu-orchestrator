package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func existsAll(string) bool { return true }

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	c, err := ParseWithOptions([]byte(yaml), Options{FileExists: existsAll})
	if err != nil {
		t.Fatalf("ParseWithOptions: %v", err)
	}
	return c
}

func TestParseDefaultsApplied(t *testing.T) {
	c := mustParse(t, "profiles: {}\n")
	d := c.Defaults
	want := Defaults{
		TTL: 300 * time.Second, MinHold: 30 * time.Second, MaxHold: 900 * time.Second,
		DrainAfter: 60 * time.Second, QueueTimeout: 600 * time.Second,
		StallTimeout: 300 * time.Second, CommandLoadTimeout: 900 * time.Second, LoadStallTimeout: 180 * time.Second,
		ShutdownGrace: 300 * time.Second, MaxPool: 4, DeviceOverheadMiB: 600,
		MinShareMiB: 1024, CacheRAMMiB: 2048, ServerBin: "llama-server-rpc", LocalDevice: "CUDA0",
		ForcedNodeGrace: 30 * time.Second, ForcedFitCheck: ForcedFitOff,
		WorkerSilence:   30 * time.Second,
		SplitSkewMargin: 0.15, EstimateMargin: 0.05, LoadMemoryFloorMiB: 768,
		Membership: MembershipFleet, WatchdogFloorMiB: 1000, WorkerStartTimeout: 15 * time.Second,
		ReservedDriftWarnMiB: 500, FreeAverageWindow: 30 * time.Second,
		HostOverheadMiB: 1024, PlacementToleranceMiB: 128,
	}
	if d != want {
		t.Fatalf("defaults = %+v\nwant      %+v", d, want)
	}
	if c.Fleet.StaleAfter != 60*time.Second {
		t.Errorf("stale_after default = %v", c.Fleet.StaleAfter)
	}
}

func TestParseOverridesDefaultsIncludingZero(t *testing.T) {
	c := mustParse(t, `
listen: ":9"
models_dir: /m
local_probe_cmd: fake-smi
fleet: {url: "http://x:1", local_node: host-1, stale_after: 5s}
defaults:
  ttl: 0
  command_load_timeout: 2s
  max_pool: 2
tiers:
  - match: { hostname: "ps5-*", local: false }
    priority: 1
    headroom_mib: 1024
    shared_memory: true
    link_mb_s: 11
    settle: 3s
node_overrides:
  laptop: { rpc: "192.0.2.3:50052" }
`)
	if c.Listen != ":9" || c.ModelsDir != "/m" || c.LocalProbeCmd != "fake-smi" {
		t.Errorf("top-level: %+v", c)
	}
	if c.Fleet.URL != "http://x:1" || c.Fleet.LocalNode != "host-1" || c.Fleet.StaleAfter != 5*time.Second {
		t.Errorf("fleet: %+v", c.Fleet)
	}
	if c.Defaults.TTL != 0 || c.Defaults.CommandLoadTimeout != 2*time.Second || c.Defaults.MaxPool != 2 {
		t.Errorf("defaults: %+v", c.Defaults)
	}
	if c.Defaults.MinHold != 30*time.Second {
		t.Errorf("untouched default lost: %v", c.Defaults.MinHold)
	}
	if len(c.Tiers) != 1 {
		t.Fatalf("tiers: %+v", c.Tiers)
	}
	tr := c.Tiers[0]
	if tr.Priority != 1 || tr.HeadroomMiB != 1024 || !tr.SharedMemory || tr.LinkMBs != 11 || tr.Settle != 3*time.Second {
		t.Errorf("tier: %+v", tr)
	}
	if tr.Match.Hostname != "ps5-*" || tr.Match.Local == nil || *tr.Match.Local {
		t.Errorf("match: %+v", tr.Match)
	}
	if c.NodeOverrides["laptop"].RPC != "192.0.2.3:50052" {
		t.Errorf("overrides: %+v", c.NodeOverrides)
	}
}

func TestParseProfileFields(t *testing.T) {
	c := mustParse(t, `
profiles:
  a:
    model: a.gguf
    ctx: 4096
  "b:64k":
    model: b.gguf
    ctx: 65536
    parallel: 2
    kv_quant: q8_0
    flash_attn: true
    needs_mib: 7800
    allow_tiers: [1, 2]
    extra_args: ["--no-warmup"]
    aliases: [old-b]
    ttl: 0
    stall_timeout: 0
    load_timeout: 30m
  cmdp:
    kind: command
    cmd: run --port ${PORT}
    check_endpoint: /health
    needs_mib: 100
`)
	a := c.Profiles["a"]
	if a == nil || a.Name != "a" || a.Kind != "llama" || a.Parallel != 1 || a.KVQuant != "f16" || a.Ctx != 4096 {
		t.Fatalf("a = %+v", a)
	}
	if a.TTL != nil || a.StallTimeout != nil || a.LoadTimeout != 0 {
		t.Errorf("unset durations must stay unset: %+v", a)
	}
	b := c.Profiles["b:64k"]
	if b.Parallel != 2 || b.KVQuant != "q8_0" || !b.FlashAttn || b.NeedsMiB != 7800 ||
		len(b.AllowTiers) != 2 || b.ExtraArgs[0] != "--no-warmup" || b.Aliases[0] != "old-b" {
		t.Errorf("b = %+v", b)
	}
	if b.TTL == nil || *b.TTL != 0 || b.StallTimeout == nil || *b.StallTimeout != 0 || b.LoadTimeout != 30*time.Minute {
		t.Errorf("b durations = %+v", b)
	}
	p := c.Profiles["cmdp"]
	if p.Kind != "command" || p.Cmd != "run --port ${PORT}" || p.CheckEndpoint != "/health" || p.NeedsMiB != 100 {
		t.Errorf("cmdp = %+v", p)
	}
}

func TestLoadReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("listen: \":1\"\nprofiles: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Listen != ":1" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseForcedNodeGraceAndChildLogKeys(t *testing.T) {
	c := mustParse(t, "profiles: {}\n")
	if c.Defaults.ForcedNodeGrace != 30*time.Second || c.ChildLogDir != "" || c.ChildLogKeep != 200 {
		t.Fatalf("defaults: grace %v, dir %q, keep %d", c.Defaults.ForcedNodeGrace, c.ChildLogDir, c.ChildLogKeep)
	}
	c = mustParse(t, "child_log_dir: /var/log/children\nchild_log_keep: 7\ndefaults: {forced_node_grace: 5s}\nprofiles: {}\n")
	if c.Defaults.ForcedNodeGrace != 5*time.Second || c.ChildLogDir != "/var/log/children" || c.ChildLogKeep != 7 {
		t.Fatalf("set: grace %v, dir %q, keep %d", c.Defaults.ForcedNodeGrace, c.ChildLogDir, c.ChildLogKeep)
	}
}

func TestParseForcedFitCheck(t *testing.T) {
	if c := mustParse(t, "profiles: {}\n"); c.Defaults.ForcedFitCheck != ForcedFitOff {
		t.Fatalf("default = %q, want %q", c.Defaults.ForcedFitCheck, ForcedFitOff)
	}
	for _, v := range []string{ForcedFitOff, ForcedFitWarn, ForcedFitReject} {
		c := mustParse(t, "defaults: {forced_fit_check: "+v+"}\nprofiles: {}\n")
		if c.Defaults.ForcedFitCheck != v {
			t.Fatalf("forced_fit_check %s parsed as %q", v, c.Defaults.ForcedFitCheck)
		}
	}
}
