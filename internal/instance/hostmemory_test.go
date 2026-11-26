package instance

import (
	"slices"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// hostPart is what a launch line says about the host process's own memory:
// --cache-ram and --ctx-checkpoints with their values, in order.
func hostPart(args []string) []string {
	var out []string
	for i, a := range args {
		if (a == "--cache-ram" || a == "--ctx-checkpoints") && i+1 < len(args) {
			out = append(out, a, args[i+1])
		}
	}
	return out
}

// Both launch paths pass the same explicit host-memory arguments for the same
// settings: the scheduler's launch (a host on the scheduler's machine) with
// cache_ram_mib, the agent's (a host on a node) with node_host_cache_ram_mib,
// both --ctx-checkpoints from ctx_checkpoints; the rest of the shared part
// (profile head and tail) is the same on both.
func TestHostMemoryArgs_BothPathsAlike(t *testing.T) {
	p := core.Profile{Name: "x", Kind: core.KindLlama, Model: "m.gguf", Ctx: 8192, Parallel: 2, KVQuant: "q8_0", ExtraArgs: []string{"--temp", "0.7"}}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.12:50052", BudgetMiB: 1000}}}
	for _, tc := range []struct{ server, node, ckpt int }{{2048, 0, 32}, {0, 0, 0}, {1024, 1024, 3}, {2048, 512, 1}} {
		cfg := &config.Config{Defaults: config.Defaults{ServerBin: "llama-server", CacheRAMMiB: tc.server, NodeHostCacheRAMMiB: tc.node, CtxCheckpoints: tc.ckpt}, ModelsDir: "/models"}
		full, err := BuildArgs(p, pl, 9000, OptionsFromConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		host := HostLaunchArgs(p, HostOptionsFromConfig(cfg))
		want := func(cache int) []string { return HostMemoryArgs(cache, tc.ckpt) }
		if got := hostPart(full); !slices.Equal(got, want(tc.server)) {
			t.Errorf("%+v: scheduler's launch %v, want %v", tc, got, want(tc.server))
		}
		if got := hostPart(host); !slices.Equal(got, want(tc.node)) {
			t.Errorf("%+v: agent's launch %v, want %v", tc, got, want(tc.node))
		}
		if tc.server == tc.node { // the same values: the shared part is the same, word for word
			from := slices.Index(full, "--jinja")
			if !slices.Equal(full[from:from+len(profileHead(tc.server, tc.ckpt))], host[4:4+len(profileHead(tc.node, tc.ckpt))]) {
				t.Errorf("%+v: heads differ: %v / %v", tc, full, host)
			}
		}
	}
}

// No change by default: with the built-in settings the two golden lines are
// today's (v76) plus --ctx-checkpoints 32 after --cache-ram.
func TestHostMemoryArgs_GoldenDefaults(t *testing.T) {
	cfg, err := config.ParseWithOptions([]byte("profiles: {}\n"), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	p := core.Profile{Name: "x", Kind: core.KindLlama, Model: "m.gguf", Ctx: 7936, Parallel: 1, KVQuant: "f16"}
	server, err := BuildArgs(p, core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.12:50052", BudgetMiB: 9000}}}, 9000, OptionsFromConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	wantServer := []string{"llama-server-rpc", "--host", "127.0.0.1", "--port", "9000", "--model", "/models/m.gguf", "--n-gpu-layers", "999",
		"--fit", "off", "--ctx-size", "7936", "--parallel", "1", "--jinja", "--cache-ram", "2048", "--ctx-checkpoints", "32", "-lv", "4",
		"--rpc", "192.0.2.12:50052", "--device", "RPC0"}
	if !slices.Equal(server, wantServer) {
		t.Errorf("host on the scheduler's machine:\n got %v\nwant %v", server, wantServer)
	}
	node := HostLaunchArgs(p, HostOptionsFromConfig(cfg))
	wantNode := []string{"--n-gpu-layers", "999", "--fit", "off", "--jinja", "--cache-ram", "0", "--ctx-checkpoints", "32", "-lv", "4"}
	if !slices.Equal(node, wantNode) {
		t.Errorf("host on a node:\n got %v\nwant %v", node, wantNode)
	}
}

// p33 (the owner, 26 Nov): a profile's own --ctx-checkpoints and
// --cache-ram in extra_args are passed IN PLACE of the cluster values, on
// both paths, each once and in the cluster value's place; the rest of
// extra_args stays last. A stored "=" form (refused on write: the server
// does not read it) is never passed, and the cluster value is used.
func TestHostMemoryArgs_ProfileFlagsReplaceTheCluster(t *testing.T) {
	cfg := &config.Config{Defaults: config.Defaults{ServerBin: "s", CacheRAMMiB: 2048, NodeHostCacheRAMMiB: 0, CtxCheckpoints: 32}, ModelsDir: "/models"}
	pl := core.Placement{Nodes: []core.PlacedNode{{ID: "a", Addr: "192.0.2.12:50052", BudgetMiB: 1}}}
	for _, c := range []struct {
		extra        string
		server, node string
	}{
		{"--temp 0.7", "--cache-ram 2048 --ctx-checkpoints 32", "--cache-ram 0 --ctx-checkpoints 32"},
		{"--temp 0.7 --ctx-checkpoints 3", "--cache-ram 2048 --ctx-checkpoints 3", "--cache-ram 0 --ctx-checkpoints 3"},
		{"--cache-ram 0 --temp 0.7", "--cache-ram 0 --ctx-checkpoints 32", "--cache-ram 0 --ctx-checkpoints 32"},
		{"--ctx-checkpoints 3 --cache-ram 512 --temp 0.7", "--cache-ram 512 --ctx-checkpoints 3", "--cache-ram 512 --ctx-checkpoints 3"},
		{"-ctxcp 1 -cram 256 --temp 0.7", "--cache-ram 256 --ctx-checkpoints 1", "--cache-ram 256 --ctx-checkpoints 1"},
		{"--swa_checkpoints 2 --temp 0.7", "--cache-ram 2048 --ctx-checkpoints 2", "--cache-ram 0 --ctx-checkpoints 2"},
		{"--ctx-checkpoints=3 --temp 0.7", "--cache-ram 2048 --ctx-checkpoints 32", "--cache-ram 0 --ctx-checkpoints 32"},
	} {
		p := core.Profile{Name: "x", Kind: core.KindLlama, Model: "m.gguf", Ctx: 8192, Parallel: 1, KVQuant: "f16", ExtraArgs: strings.Fields(c.extra)}
		full, err := BuildArgs(p, pl, 1, OptionsFromConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		want := "s --host 127.0.0.1 --port 1 --model /models/m.gguf --n-gpu-layers 999 --fit off --ctx-size 8192 --parallel 1 --jinja " + c.server + " -lv 4 --rpc 192.0.2.12:50052 --device RPC0 --temp 0.7"
		if got := strings.Join(full, " "); got != want {
			t.Errorf("%q on the scheduler's machine:\n got %s\nwant %s", c.extra, got, want)
		}
		want = "--n-gpu-layers 999 --fit off --jinja " + c.node + " -lv 4 --temp 0.7"
		if got := strings.Join(HostLaunchArgs(p, HostOptionsFromConfig(cfg)), " "); got != want {
			t.Errorf("%q on a node:\n got %s\nwant %s", c.extra, got, want)
		}
	}
}
