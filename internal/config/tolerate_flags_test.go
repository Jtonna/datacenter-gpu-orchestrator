package config

import (
	"slices"
	"strings"
	"testing"
)

// v87 (tolerate stored host flags): a stored profile carrying any spelling
// of --ctx-checkpoints or --cache-ram, as the next release writes them,
// LOADS with every profile; a new write that sets one is still refused; the
// launch drops every spelling (StripLateManaged) so the server never sees
// two values.
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
	w := c.LateManagedArgs()
	if len(w) != len(storedHostFlags)+1 { // the last profile carries two
		t.Fatalf("%d warnings: %q", len(w), w)
	}
	for _, x := range w {
		if !strings.Contains(x, "is not passed: the scheduler passes --") {
			t.Fatalf("warning %q", x)
		}
	}
	if !slices.ContainsFunc(w, func(s string) bool {
		return strings.Contains(s, "--cache-ram 0 is not passed: the scheduler passes --cache-ram from the setting cache_ram_mib")
	}) {
		t.Fatalf("no cache warning: %q", w)
	}
}

func TestTolerateStoredHostFlags_WriteRefused(t *testing.T) {
	for _, a := range storedHostFlags {
		one := 1
		doc := NewDocument()
		doc.Profiles["p"] = &ProfileSpec{Model: "m.gguf", Ctx: &one, ExtraArgs: a}
		_, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll})
		if err == nil || !strings.Contains(err.Error(), "which the scheduler passes on every launch from the setting") {
			t.Errorf("%v: a new write: %v", a, err)
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
		if got := strings.Join(StripLateManaged(strings.Fields(in)), " "); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
