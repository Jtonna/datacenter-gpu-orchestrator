package config

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/llamaflags"
)

// Kept from v87 (185906d, tolerate stored host flags), adapted to p33: a
// stored profile carrying any spelling of --ctx-checkpoints or --cache-ram
// LOADS with every profile. Since p33 the accepted forms are the profile's
// own values (no warning); a stored form a write refuses (here a negative
// value) still loads, with one warning, and its launch uses the cluster
// value. Every spelling is stripped before the launch adds the effective
// values, so the server never sees two.
var storedHostFlags = [][]string{
	{"--ctx-checkpoints", "3"}, {"-ctxcp", "1"}, {"--swa-checkpoints", "2"}, {"--ctx_checkpoints", "3"}, {"--swa_checkpoints", "3"},
	{"--cache-ram", "0"}, {"-cram", "512"}, {"--cache_ram", "256"}, {"--cache-ram", "-1"},
	{"--ctx-checkpoints", "3", "--cache-ram", "0", "--temp", "0.7"},
}

func TestTolerateStoredHostFlags_Load(t *testing.T) {
	doc := NewDocument()
	keep := map[string][]string{}
	one := 1
	for i, a := range storedHostFlags {
		name := "p" + string(rune('a'+i))
		doc.Profiles[name] = &ProfileSpec{Model: "m.gguf", Ctx: &one, ExtraArgs: a}
		keep[name] = a
	}
	doc.Profiles["fine"] = &ProfileSpec{Model: "m.gguf", Ctx: &one}
	c, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll, KeepArgs: keep})
	if err != nil {
		t.Fatalf("a stored configuration with the flags is refused: %v", err)
	}
	if len(c.Profiles) != len(storedHostFlags)+1 {
		t.Fatalf("%d profiles loaded", len(c.Profiles))
	}
	w := c.HostMemoryArgProblems()
	if len(w) != 1 || !strings.Contains(w[0], "--cache-ram -1") || !strings.Contains(w[0], "the launch uses the cluster value") {
		t.Fatalf("warnings %q, want one for --cache-ram -1", w)
	}
}

func TestTolerateStoredHostFlags_NewWrite(t *testing.T) {
	for _, a := range storedHostFlags {
		one := 1
		doc := NewDocument()
		doc.Profiles["p"] = &ProfileSpec{Model: "m.gguf", Ctx: &one, ExtraArgs: a}
		_, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll})
		if a[len(a)-1] == "-1" {
			if err == nil || !strings.Contains(err.Error(), "must not be negative") {
				t.Errorf("%v: a new write: %v", a, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: a new write refused: %v", a, err)
		}
	}
}

func TestTolerateStoredHostFlags_Stripped(t *testing.T) {
	for in, want := range map[string]string{
		"--ctx-checkpoints 3 --temp 0.7":    "--temp 0.7",
		"-ctxcp 1 -cram 0":                  "",
		"--swa_checkpoints 2 --jinja":       "--jinja",
		"--cache_ram 256 --top-k 5":         "--top-k 5",
		"--cache-ram -1 --top-k 5":          "--top-k 5",
		"--cache-ram=0 --ctx_checkpoints=1": "",
		"--checkpoint-min-step 64":          "--checkpoint-min-step 64",
	} {
		if got := strings.Join(llamaflags.Strip(strings.Fields(in)), " "); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
