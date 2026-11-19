package config

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

func TestExampleConfigParses(t *testing.T) {
	c, err := LoadWithOptions(filepath.Join("..", "..", "configs", "example.yaml"),
		Options{FileExists: existsAll})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for n := range c.Profiles {
		got = append(got, n)
	}
	sort.Strings(got)
	want := []string{
		"devstral-24b", "devstral-24b:14k", "devstral-24b:14k-laptops", "devstral-24b:24k",
		"flux-1-dev", "flux-1-schnell", "gemma-4-12b", "gemma-4-12b:48k", "gemma-4-12b:64k",
		"gemma-4-12b:96k", "large-v3-turbo", "qwen3.5-4b", "tinyllama",
	}
	if len(got) != len(want) {
		t.Fatalf("profiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("profiles = %v, want %v", got, want)
		}
	}
	for old, new := range map[string]string{
		"gemma-4-12b-1pool-8k": "gemma-4-12b", "gemma-4-12b-3pool-64k": "gemma-4-12b:64k",
		"devstral-24b-3way": "devstral-24b:14k", "devstral-24b-q4": "devstral-24b:24k",
		"tinyllama-ps5": "tinyllama", "devstral-24b-f16": "devstral-24b",
	} {
		if p, isAlias, ok := c.Resolve(old); !ok || !isAlias || p.Name != new {
			t.Errorf("alias %q -> %v %v %v, want %q", old, p, isAlias, ok, new)
		}
	}
	if p := c.Profiles["devstral-24b:14k-laptops"]; len(p.ForceNodes) != 2 || p.TensorSplit[0] != 50 {
		t.Errorf("laptops profile: %+v", p)
	}
	if p := c.Profiles["large-v3-turbo"]; p.Kind != core.KindCommand || p.NeedsMiB != 5400 ||
		*p.StallTimeout != 0 {
		t.Errorf("whisper: %+v", p)
	}
	// Measured on hardware, 2026-11-17 and 2026-11-18 (docs/hardware-test/).
	for name, want := range map[string]int{"gemma-4-12b": 7900, "gemma-4-12b:48k": 8250, "gemma-4-12b:64k": 8600,
		"gemma-4-12b:96k": 9050, "devstral-24b": 14300, "qwen3.5-4b": 3400, "tinyllama": 700} {
		if p := c.Profiles[name]; p.NeedsMiB != want {
			t.Errorf("%s needs_mib = %d, want %d", name, p.NeedsMiB, want)
		}
	}
	for host, want := range map[string]int{"ps5-062e1d84": 100, "alienware-llm": 60} {
		if tier, ok := c.MatchTier(host, "", "", false); !ok || tier.LinkMBs != float64(want) {
			t.Errorf("%s link_mb_s = %+v, want %d", host, tier, want)
		}
	}
	if tier, ok := c.MatchTier("omen-runner", "", "", true); !ok || tier.HeadroomMiB != 450 {
		t.Errorf("local tier headroom = %+v, want 450", tier)
	}
	if tier, ok := c.MatchTier("ps5-062e1d84", "", "", false); !ok || tier.Priority != 1 {
		t.Errorf("ps5 tier: %+v %v", tier, ok)
	}
}

// deploy/seed-config.yaml is configs/example.yaml with a header and a commented
// template added: the two must stay in sync.
func TestDeployConfigMatchesExample(t *testing.T) {
	load := func(rel string) *Config {
		c, err := LoadWithOptions(filepath.Join("..", "..", rel), Options{FileExists: existsAll})
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return c
	}
	a, b := load("configs/example.yaml"), load("deploy/seed-config.yaml")
	// The deploy file turns on the child log directory; the example leaves it off.
	if b.ChildLogDir == "" {
		t.Errorf("deploy/seed-config.yaml should set child_log_dir")
	}
	b.ChildLogDir = a.ChildLogDir
	if b.HistoryDir == "" {
		t.Errorf("deploy/seed-config.yaml should set history_dir")
	}
	b.HistoryDir = a.HistoryDir
	if b.DataDir == "" || !b.Bench.Enabled {
		t.Errorf("deploy/seed-config.yaml should set data_dir (the benchmark feature is then on)")
	}
	b.DataDir, b.Bench.Enabled = a.DataDir, a.Bench.Enabled
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("deploy/seed-config.yaml parses differently from configs/example.yaml: %+v vs %+v", a, b)
	}
}

// The memory safety keys are documented as commented defaults in both files,
// with the same default values the parser applies.
func TestExampleConfigsDocumentTheMemorySafetyDefaults(t *testing.T) {
	for _, rel := range []string{"configs/example.yaml", "deploy/seed-config.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"# split_skew_margin: 0.15", "# estimate_margin: 0.05", "# load_memory_floor_mib: 768"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s lacks %q", rel, want)
			}
		}
	}
	d := DefaultDefaults()
	if d.SplitSkewMargin != 0.15 || d.EstimateMargin != 0.05 || d.LoadMemoryFloorMiB != 768 {
		t.Errorf("defaults %+v", d)
	}
}

// The history keys are documented as commented defaults in both files, with
// the values the parser applies.
func TestExampleConfigsDocumentTheHistoryDefaults(t *testing.T) {
	for _, rel := range []string{"configs/example.yaml", "deploy/seed-config.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"history_dir:", "# history_max_file_mib: 16", "# history_keep_files: 20", "# history_memory_events: 5000"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s lacks %q", rel, want)
			}
		}
	}
	c := mustParse(t, "profiles: {}\n")
	if c.HistoryMaxFileMiB != 16 || c.HistoryKeepFiles != 20 || c.HistoryMemoryEvents != 5000 {
		t.Errorf("defaults %+v", c)
	}
}
