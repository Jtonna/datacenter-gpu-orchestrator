package config

import (
	"path/filepath"
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
	if p := c.Profiles["gemma-4-12b"]; p.NeedsMiB != 7800 {
		t.Errorf("gemma needs_mib = %d", p.NeedsMiB)
	}
	if tier, ok := c.MatchTier("ps5-062e1d84", "", "", false); !ok || tier.Priority != 1 {
		t.Errorf("ps5 tier: %+v %v", tier, ok)
	}
}
