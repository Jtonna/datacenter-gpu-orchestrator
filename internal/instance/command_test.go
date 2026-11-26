package instance

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

var testOpts = BuildOptions{
	ServerBin:   "llama-server-rpc",
	ModelsDir:   "/models",
	LocalDevice: "CUDA0",
	CacheRAMMiB: 2048, CtxCheckpoints: 32,
}

func remote(id, addr string, budget int) core.PlacedNode {
	return core.PlacedNode{ID: core.NodeID(id), Addr: addr, BudgetMiB: budget}
}

func local(budget int) core.PlacedNode {
	return core.PlacedNode{ID: "host-1", Local: true, BudgetMiB: budget}
}

func llamaProfile() core.Profile {
	return core.Profile{Name: "g", Kind: core.KindLlama, Model: "g.gguf", Ctx: 8192, Parallel: 1, KVQuant: "f16"}
}

func TestBuildArgsLlama(t *testing.T) {
	q8 := llamaProfile()
	q8.Ctx = 65536
	q8.KVQuant = "q8_0"
	q8.FlashAttn = true

	par := llamaProfile()
	par.Ctx = 4096
	par.Parallel = 2

	zeroPar := llamaProfile()
	zeroPar.Parallel = 0

	extra := llamaProfile()
	extra.ExtraArgs = []string{"--no-webui", "--threads", "4"}

	forced := llamaProfile()
	forced.TensorSplit = []int{60, 40}

	base := func(port, ctx, par string) []string {
		return []string{"llama-server-rpc", "--host", "127.0.0.1", "--port", port, "--model", "/models/g.gguf",
			"--n-gpu-layers", "999", "--fit", "off", "--ctx-size", ctx, "--parallel", par, "--jinja", "--cache-ram", "2048", "--ctx-checkpoints", "32", "-lv", "4"}
	}
	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}

	tests := []struct {
		name string
		prof core.Profile
		pl   core.Placement
		want []string
	}{
		{"local only", llamaProfile(), core.Placement{Nodes: []core.PlacedNode{local(7000)}},
			cat(base("5801", "8192", "1"), []string{"--device", "CUDA0"})},
		{"one remote q8 flash", q8, core.Placement{Nodes: []core.PlacedNode{remote("ps5", "192.0.2.2:50052", 14000)}},
			cat(base("5801", "65536", "1"), []string{"--rpc", "192.0.2.2:50052", "--device", "RPC0", "-ctk", "q8_0", "-ctv", "q8_0", "--flash-attn", "on"})},
		{"two remotes split from budgets", llamaProfile(), core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 14000), remote("b", "192.0.2.3:50052", 7000)}},
			cat(base("5801", "8192", "1"), []string{"--rpc", "192.0.2.2:50052,192.0.2.3:50052", "--device", "RPC0,RPC1", "--tensor-split", "14000,7000"})},
		{"local first then remotes", llamaProfile(), core.Placement{Nodes: []core.PlacedNode{local(7895), remote("a", "192.0.2.2:50052", 14000), remote("b", "192.0.2.3:50052", 7000)}},
			cat(base("5801", "8192", "1"), []string{"--rpc", "192.0.2.2:50052,192.0.2.3:50052", "--device", "CUDA0,RPC0,RPC1", "--tensor-split", "7895,14000,7000"})},
		{"local in the middle counts only remotes", llamaProfile(), core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 100), local(200), remote("b", "192.0.2.3:50052", 300)}},
			cat(base("5801", "8192", "1"), []string{"--rpc", "192.0.2.2:50052,192.0.2.3:50052", "--device", "RPC0,CUDA0,RPC1", "--tensor-split", "100,200,300"})},
		{"parallel multiplies ctx", par, core.Placement{Nodes: []core.PlacedNode{local(7000)}},
			cat(base("5801", "8192", "2"), []string{"--device", "CUDA0"})},
		{"parallel zero means one", zeroPar, core.Placement{Nodes: []core.PlacedNode{local(7000)}},
			cat(base("5801", "8192", "1"), []string{"--device", "CUDA0"})},
		{"profile tensor split wins over budgets", forced, core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 1), remote("b", "192.0.2.3:50052", 1)}},
			cat(base("5801", "8192", "1"), []string{"--rpc", "192.0.2.2:50052,192.0.2.3:50052", "--device", "RPC0,RPC1", "--tensor-split", "60,40"})},
		{"placement tensor split wins", llamaProfile(), core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 1), remote("b", "192.0.2.3:50052", 1)}, TensorSplit: []int{1, 3}},
			cat(base("5801", "8192", "1"), []string{"--rpc", "192.0.2.2:50052,192.0.2.3:50052", "--device", "RPC0,RPC1", "--tensor-split", "1,3"})},
		{"single device omits split even when one is given", forced, core.Placement{Nodes: []core.PlacedNode{local(5)}, TensorSplit: []int{100}},
			cat(base("5801", "8192", "1"), []string{"--device", "CUDA0"})},
		{"extra args last", extra, core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 14000)}},
			cat(base("5801", "8192", "1"), []string{"--rpc", "192.0.2.2:50052", "--device", "RPC0", "--no-webui", "--threads", "4"})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildArgs(tc.prof, tc.pl, 5801, testOpts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestBuildArgsMatchesAppendixA1(t *testing.T) {
	p := llamaProfile()
	p.Name = "gemma-4-12b:64k"
	p.Model = "gemma-4-12b-it-Q4_K_M.gguf"
	p.Ctx = 65536
	p.KVQuant = "q8_0"
	p.FlashAttn = true
	argv, err := BuildArgs(p, core.Placement{Nodes: []core.PlacedNode{remote("ps5", "192.0.2.2:50052", 14000)}}, 5801, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	want := "llama-server-rpc --host 127.0.0.1 --port 5801 --model /models/gemma-4-12b-it-Q4_K_M.gguf --n-gpu-layers 999 --fit off --ctx-size 65536 --parallel 1 --jinja --cache-ram 2048 --ctx-checkpoints 32 -lv 4 --rpc 192.0.2.2:50052 --device RPC0 -ctk q8_0 -ctv q8_0 --flash-attn on"
	if got := RenderCommand(argv); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestBuildArgsWindowsModelsDir(t *testing.T) {
	o := testOpts
	o.ModelsDir = `C:\models`
	argv, err := BuildArgs(llamaProfile(), core.Placement{Nodes: []core.PlacedNode{local(1)}}, 1, o)
	if err != nil {
		t.Fatal(err)
	}
	if argv[6] != `C:\models\g.gguf` {
		t.Fatalf("model arg = %q", argv[6])
	}
}

func TestBuildArgsErrors(t *testing.T) {
	noLocalDev := testOpts
	noLocalDev.LocalDevice = ""
	tests := []struct {
		name string
		opts BuildOptions
		pl   core.Placement
		want string
	}{
		{"empty placement", testOpts, core.Placement{}, "no nodes"},
		{"remote without address", testOpts, core.Placement{Nodes: []core.PlacedNode{remote("a", "", 5)}}, "address"},
		{"split length mismatch", testOpts, core.Placement{Nodes: []core.PlacedNode{local(1), remote("a", "x:1", 1)}, TensorSplit: []int{1}}, "tensor split"},
		{"explicit split with no positive value", testOpts, core.Placement{Nodes: []core.PlacedNode{local(5), remote("a", "x:1", 5)}, TensorSplit: []int{0, 0}}, "tensor split"},
		{"explicit split with a negative value", testOpts, core.Placement{Nodes: []core.PlacedNode{local(5), remote("a", "x:1", 5)}, TensorSplit: []int{-1, 3}}, "negative"},
		{"no local device configured", noLocalDev, core.Placement{Nodes: []core.PlacedNode{local(1)}}, "local device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildArgs(llamaProfile(), tc.pl, 1, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestBuildArgsCommandKind(t *testing.T) {
	p := core.Profile{Name: "w", Kind: core.KindCommand,
		Cmd: `env -u LD_LIBRARY_PATH /opt/whisper/venv/bin/python /opt/whisper/server.py --host 127.0.0.1 --port ${PORT} --model large-v3-turbo`}
	argv, err := BuildArgs(p, core.Placement{Nodes: []core.PlacedNode{local(5400)}}, 5802, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"env", "-u", "LD_LIBRARY_PATH", "/opt/whisper/venv/bin/python", "/opt/whisper/server.py", "--host", "127.0.0.1", "--port", "5802", "--model", "large-v3-turbo"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("got %q", argv)
	}
}

func TestExpandCommand(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
		wantErr  bool
	}{
		{"plain", "a b  c", []string{"a", "b", "c"}, false},
		{"port twice", "x --port ${PORT} --u http://h:${PORT}/", []string{"x", "--port", "7", "--u", "http://h:7/"}, false},
		{"double quotes keep spaces", `a "b c" d`, []string{"a", "b c", "d"}, false},
		{"single quotes literal", `a 'b "c" $d' e`, []string{"a", `b "c" $d`, "e"}, false},
		{"backslash space", `a b\ c`, []string{"a", "b c"}, false},
		{"escaped quote in double quotes", `a "b\"c"`, []string{"a", `b"c`}, false},
		{"backslash literal in double quotes", `a "b\nc"`, []string{"a", `b\nc`}, false},
		{"adjacent quoted pieces join", `a"b c"'d e'f`, []string{"ab cd ef"}, false},
		{"empty quoted arg kept", `a "" b`, []string{"a", "", "b"}, false},
		{"tabs and newlines separate", "a\tb\nc", []string{"a", "b", "c"}, false},
		{"unterminated double", `a "b`, nil, true},
		{"unterminated single", `a 'b`, nil, true},
		{"trailing backslash", `a b\`, nil, true},
		{"empty", "   ", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandCommand(tc.in, 7)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRenderCommand(t *testing.T) {
	tests := []struct {
		argv []string
		want string
	}{
		{[]string{"a", "b"}, "a b"},
		{[]string{"a", "b c"}, "a 'b c'"},
		{[]string{"a", ""}, "a ''"},
		{[]string{"a", "it's"}, `a 'it'\''s'`},
		{[]string{"--rpc", "1.2.3.4:5,6.7.8.9:1"}, "--rpc 1.2.3.4:5,6.7.8.9:1"},
	}
	for _, tc := range tests {
		if got := RenderCommand(tc.argv); got != tc.want {
			t.Errorf("RenderCommand(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// The hub extracts workers and context with these regexes (design A.1).
func TestRenderCommandHubRegexes(t *testing.T) {
	rpcRe := regexp.MustCompile(`--rpc[= ]+(\S+)`)
	ctxRe := regexp.MustCompile(`(?:--ctx-size|-c)[= ]+(\d+)`)
	p := llamaProfile()
	p.Ctx = 4096
	p.Parallel = 3
	argv, err := BuildArgs(p, core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 5), remote("b", "192.0.2.3:50052", 5)}}, 5801, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s := RenderCommand(argv)
	if m := rpcRe.FindStringSubmatch(s); m == nil || m[1] != "192.0.2.2:50052,192.0.2.3:50052" {
		t.Errorf("rpc regex on %q: %v", s, m)
	}
	if m := ctxRe.FindStringSubmatch(s); m == nil || m[1] != "12288" {
		t.Errorf("ctx regex on %q: %v", s, m)
	}
}

// Forced placement has no fit check (design 7.3), so a node's budget can be
// zero or negative (free below headroom); budget-derived weights are clamped
// to at least 1 instead of failing the load. Explicit splits are not.
func TestBuildArgsBudgetSplitClampedToOne(t *testing.T) {
	for _, tc := range []struct {
		budgets [2]int
		want    string
	}{
		{[2]int{-300, 5000}, "1,5000"},
		{[2]int{0, 0}, "1,1"},
		{[2]int{7000, -1}, "7000,1"},
	} {
		pl := core.Placement{Nodes: []core.PlacedNode{local(tc.budgets[0]), remote("a", "x:1", tc.budgets[1])}}
		argv, err := BuildArgs(llamaProfile(), pl, 1, testOpts)
		if err != nil {
			t.Fatalf("%v: %v", tc.budgets, err)
		}
		got := ""
		for i, a := range argv {
			if a == "--tensor-split" {
				got = argv[i+1]
			}
		}
		if got != tc.want {
			t.Errorf("%v: --tensor-split %q, want %q", tc.budgets, got, tc.want)
		}
	}
}

// kind: command profiles get exactly the expanded command: no verbosity flag.
func TestBuildArgsCommandKindHasNoVerbosityFlag(t *testing.T) {
	p := core.Profile{Name: "c", Kind: core.KindCommand, Cmd: "sd-server --port ${PORT}"}
	argv, err := BuildArgs(p, core.Placement{Nodes: []core.PlacedNode{local(1)}}, 7, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv, " "); got != "sd-server --port 7" {
		t.Fatalf("argv = %q", got)
	}
}

// Defect 2: llama initialises its CUDA backend whatever --device says, which
// costs ~102 MiB of the host GPU per child. A placement without the local node
// hides the GPU from the child; one with it leaves the variable inherited.
func TestChildEnvOverrides(t *testing.T) {
	remoteOnly := core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 1), remote("b", "192.0.2.3:50052", 1)}}
	withLocal := core.Placement{Nodes: []core.PlacedNode{remote("a", "192.0.2.2:50052", 1), local(1)}}
	localOnly := core.Placement{Nodes: []core.PlacedNode{local(1)}}
	cmdProf := core.Profile{Name: "c", Kind: core.KindCommand, Cmd: "x ${PORT}"}

	if got := ChildEnvOverrides(llamaProfile(), remoteOnly); !reflect.DeepEqual(got, []string{"CUDA_VISIBLE_DEVICES="}) {
		t.Errorf("remote only: %q", got)
	}
	if got := ChildEnvOverrides(llamaProfile(), withLocal); len(got) != 0 {
		t.Errorf("with local node: %q", got)
	}
	if got := ChildEnvOverrides(llamaProfile(), localOnly); len(got) != 0 {
		t.Errorf("local only: %q", got)
	}
	if got := ChildEnvOverrides(cmdProf, remoteOnly); len(got) != 0 {
		t.Errorf("kind: command must be unaffected: %q", got)
	}
}

// The instance command joins the model with the shared path rule
// (internal/modelpath): a sub-folder keeps working, a path the rule refuses
// never reaches llama-server.
func TestBuildArgsModelPathRule(t *testing.T) {
	pl := core.Placement{Nodes: []core.PlacedNode{local(1)}}
	p := llamaProfile()
	p.Model = "sub/g.gguf"
	argv, err := BuildArgs(p, pl, 1, testOpts)
	if err != nil || argv[6] != "/models/sub/g.gguf" {
		t.Fatalf("sub-folder: %q %v", argv, err)
	}
	for _, bad := range []string{".trash/0123/g.gguf", "../g.gguf", "/etc/g.gguf", `sub\g.gguf`} {
		p.Model = bad
		if argv, err := BuildArgs(p, pl, 1, testOpts); err == nil {
			t.Errorf("model %q: built %q, want refused", bad, argv)
		}
	}
}
