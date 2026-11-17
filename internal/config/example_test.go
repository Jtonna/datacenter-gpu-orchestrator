package config

import (
	"path/filepath"
	"reflect"
	"sort"
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
	// Measured on hardware, 2026-11-17 (docs/hardware-test/RESULTS.md).
	for name, want := range map[string]int{"gemma-4-12b": 7850, "gemma-4-12b:64k": 8600, "qwen3.5-4b": 3400, "tinyllama": 700} {
		if p := c.Profiles[name]; p.NeedsMiB != want {
			t.Errorf("%s needs_mib = %d, want %d", name, p.NeedsMiB, want)
		}
	}
	for _, name := range []string{"gemma-4-12b:48k", "gemma-4-12b:96k"} { // still placeholders
		if p := c.Profiles[name]; p.NeedsMiB == 0 {
			t.Errorf("%s lost its placeholder needs_mib", name)
		}
	}
	for host, want := range map[string]int{"ps5-062e1d84": 8, "alienware-llm": 60} {
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

// deploy/config.yaml is configs/example.yaml with a header and a commented
// template added: the two must stay in sync.
func TestDeployConfigMatchesExample(t *testing.T) {
	load := func(rel string) *Config {
		c, err := LoadWithOptions(filepath.Join("..", "..", rel), Options{FileExists: existsAll})
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return c
	}
	a, b := load("configs/example.yaml"), load("deploy/config.yaml")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("deploy/config.yaml parses differently from configs/example.yaml: %+v vs %+v", a, b)
	}
}
