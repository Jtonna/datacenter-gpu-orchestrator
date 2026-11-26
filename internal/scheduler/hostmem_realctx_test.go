package scheduler

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// Where the real context (capCtx) meets the explicit host memory: a profile
// capped at its trained context launches with "--ctx-size <trained>" and the
// explicit "--cache-ram … --ctx-checkpoints 32"; one with ctx_above_trained
// gets the --override-kv after them, in a stable order, on both launch paths.
func TestHostMemory_CappedAndOverriddenLaunchLines(t *testing.T) {
	cfg, err := config.ParseWithOptions([]byte("profiles: {}\n"), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.12:50052", BudgetMiB: 1000}}}
	base := core.Profile{Name: "x", Kind: core.KindLlama, Model: "m.gguf", Ctx: 8192, Parallel: 1, ExtraArgs: []string{"--temp", "0.7"}}

	capped := capCtx(&base, 2048, "qwen35", true)
	if capped == nil || capped.Ctx != 2048 {
		t.Fatalf("%+v", capped)
	}
	full, err := instance.BuildArgs(*capped, pl, 9000, instance.OptionsFromConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(full, " ")
	for _, w := range []string{"--ctx-size 2048", "--cache-ram 2048 --ctx-checkpoints 32"} {
		if !strings.Contains(line, w) {
			t.Fatalf("capped: %q lacks %q", line, w)
		}
	}
	host := strings.Join(instance.HostLaunchArgs(*capped, instance.HostOptionsFromConfig(cfg)), " ")
	if !strings.Contains(host, "--cache-ram 0 --ctx-checkpoints 32") || strings.Contains(host, "override-kv") {
		t.Fatalf("capped, host: %q", host)
	}

	above := base
	above.CtxAboveTrained = true
	over := capCtx(&above, 2048, "qwen35", true)
	if over == nil || over.Ctx != 8192 {
		t.Fatalf("%+v", over)
	}
	full, err = instance.BuildArgs(*over, pl, 9000, instance.OptionsFromConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	line = strings.Join(full, " ")
	const tail = "--cache-ram 2048 --ctx-checkpoints 32 -lv 4 --rpc 192.0.2.12:50052 --device RPC0 --temp 0.7 --override-kv qwen35.context_length=int:8192"
	if !strings.Contains(line, "--ctx-size 8192") || !strings.Contains(line, tail) {
		t.Fatalf("ctx_above_trained: %q", line)
	}
	host = strings.Join(instance.HostLaunchArgs(*over, instance.HostOptionsFromConfig(cfg)), " ")
	if !strings.Contains(host, "--cache-ram 0 --ctx-checkpoints 32 -lv 4 --temp 0.7 --override-kv qwen35.context_length=int:8192") {
		t.Fatalf("ctx_above_trained, host: %q", host)
	}
}
