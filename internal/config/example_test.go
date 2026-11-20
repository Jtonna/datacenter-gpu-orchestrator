package config

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
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
	want := []string{"gemma-3-12b", "gemma-3-12b:32k", "qwen3-4b"}
	if len(got) != len(want) {
		t.Fatalf("profiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("profiles = %v, want %v", got, want)
		}
	}
	if p, isAlias, ok := c.Resolve("qwen-small"); !ok || !isAlias || p.Name != "qwen3-4b" {
		t.Errorf("alias qwen-small -> %v %v %v, want qwen3-4b", p, isAlias, ok)
	}
	if p := c.Profiles["gemma-3-12b:32k"]; p.KVQuant != "q8_0" || !p.FlashAttn || len(p.AllowTiers) != 2 || p.Ctx != 32768 {
		t.Errorf("gemma 32k variant: %+v", p)
	}
	for name, want := range map[string]int{"qwen3-4b": 3400, "gemma-3-12b": 7900, "gemma-3-12b:32k": 0} {
		if p := c.Profiles[name]; p.NeedsMiB != want {
			t.Errorf("%s needs_mib = %d, want %d", name, p.NeedsMiB, want)
		}
	}
	for host, want := range map[string]int{"console-1": 100, "laptop-1": 60} {
		if tier, ok := c.MatchTier(host, "", "", false); !ok || tier.LinkMBs != float64(want) {
			t.Errorf("%s link_mb_s = %+v, want %d", host, tier, want)
		}
	}
	if tier, ok := c.MatchTier("host-1", "", "", true); !ok || tier.HeadroomMiB != 450 {
		t.Errorf("local tier headroom = %+v, want 450", tier)
	}
	if tier, ok := c.MatchTier("console-1", "", "", false); !ok || tier.Priority != 1 || !tier.SharedMemory {
		t.Errorf("console tier: %+v %v", tier, ok)
	}
	if c.Fleet.LocalNode != "host-1" {
		t.Errorf("local_node = %q", c.Fleet.LocalNode)
	}
}

// deploy/seed-config.yaml is configs/example.yaml with a header, the deployment
// paths and a commented template added: the two must stay in sync.
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
