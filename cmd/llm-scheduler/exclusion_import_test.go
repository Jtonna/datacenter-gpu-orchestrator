package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// 0.1.7: import-config (and its --check) accepts a file whose profiles force
// excluded nodes, prints one warning note per such profile, and seeds the
// benchmarks and suites of the file that use them.
func TestImportAcceptsForcedProfilesOnExcludedNodes(t *testing.T) {
	const yaml = `local_node: host-1
defaults: {membership: agents}
exclude_nodes: [host-1, "02:00:00:00:00:31"]
profiles:
  on-host: {model: a.gguf, ctx: 2048, force_nodes: [host-1]}
  on-agent: {model: b.gguf, ctx: 2048, force_nodes: ["02:00:00:00:00:31"]}
  free: {model: c.gguf, ctx: 2048}
bench:
  benchmarks:
    - {id: host-step, profile: on-host, requests: [{prompt: hi, max_tokens: 4}]}
    - {id: free-step, profile: free, requests: [{prompt: hi, max_tokens: 4}]}
  suites:
    - {id: both, benchmarks: [host-step, free-step]}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(file, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	for _, args := range [][]string{
		{"import-config", "--check", "--no-model-check", "--data-dir", data, file},
		{"import-config", "--no-model-check", "--data-dir", data, file},
	} {
		var out, errb bytes.Buffer
		if code := realMain(args, &out, &errb); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errb.String())
		}
		if n := strings.Count(out.String(), "is excluded by exclude_nodes entry"); n != 2 {
			t.Fatalf("%v: %d warnings:\n%s", args, n, out.String())
		}
	}
	st, err := bench.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if b, err := st.GetBenchmark("host-step"); err != nil || b == nil {
		t.Fatalf("benchmark not seeded: %v", err)
	}
}
