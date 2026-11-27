package config

import (
	"strings"
	"testing"
)

// p33 (reverses v81): --ctx-checkpoints and --cache-ram, every spelling the
// pinned server takes, are accepted in a profile's extra_args; a form the
// server would not take, a negative or missing value, or a flag given twice
// is refused with a sentence.
func TestHostMemoryFlagsOnWrite(t *testing.T) {
	ok := []string{`["--ctx-checkpoints", "3"]`, `["-ctxcp", "0"]`, `["--swa-checkpoints", "1"]`, `["--ctx_checkpoints", "2"]`,
		`["--cache-ram", "0"]`, `["-cram", "512"]`, `["--ctx-checkpoints", "3", "--cache-ram", "0", "--temp", "0.7"]`}
	for _, a := range ok {
		if err := parseErr(prof("  p:\n    model: m.gguf\n    ctx: 1\n    extra_args: "+a+"\n"), nil); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
	bad := map[string]string{
		`["--ctx-checkpoints=3"]`:                   "the model server does not read --ctx-checkpoints=value",
		`["--cache-ram"]`:                           "--cache-ram has no value after it",
		`["--cache-ram", "much"]`:                   "the value must be a whole number",
		`["--cache-ram", "-1"]`:                     "the value must not be negative",
		`["-ctxcp", "-2"]`:                          "the value must not be negative",
		`["--ctx-checkpoints", "3", "-ctxcp", "1"]`: "is given twice",
		`["--cache-ram", "0", "--cache_ram", "0"]`:  "is given twice",
		`["--ctx-checkpoints", "4096"]`:             "at most 1024",
	}
	for a, want := range bad {
		err := parseErr(prof("  p:\n    model: m.gguf\n    ctx: 1\n    extra_args: "+a+"\n"), nil)
		if err == nil || !strings.Contains(err.Error(), `profile "p": extra_args`) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", a, err, want)
		}
	}
}

// A STORED profile whose form is no longer accepted, unchanged, still loads
// (one warning) and its launch uses the cluster value; a write that keeps
// it changed is refused.
func TestHostMemoryFlagsStoredStayLoadable(t *testing.T) {
	doc := NewDocument()
	ctx := 1
	doc.Profiles["p"] = &ProfileSpec{Model: "m.gguf", Ctx: &ctx, ExtraArgs: []string{"--ctx-checkpoints=3"}}
	if _, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll}); err == nil {
		t.Fatal("a new write with --ctx-checkpoints=3 was accepted")
	}
	c, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll, KeepArgs: map[string][]string{"p": {"--ctx-checkpoints=3"}}})
	if err != nil {
		t.Fatalf("the stored profile is refused: %v", err)
	}
	if w := c.HostMemoryArgProblems(); len(w) != 1 || !strings.Contains(w[0], "the launch uses the cluster value") {
		t.Fatalf("warnings %q", w)
	}
	if l := c.HostMemoryOf(c.Profiles["p"]); l.CtxCheckpoints != 32 || l.CtxCheckpointsFrom != FromCluster {
		t.Fatalf("effective %+v", l)
	}
}

// The effective figures and where they come from.
func TestHostMemoryOf(t *testing.T) {
	y := prof("  plain:\n    model: m.gguf\n    ctx: 1\n" +
		"  own:\n    model: m.gguf\n    ctx: 1\n    extra_args: [\"--ctx-checkpoints\", \"3\", \"--cache-ram\", \"0\"]\n" +
		"  onlycache:\n    model: m.gguf\n    ctx: 1\n    extra_args: [\"-cram\", \"512\"]\n")
	c, err := ParseWithOptions([]byte(y), Options{FileExists: existsAll})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]HostMemoryLimits{
		"plain":     {32, FromCluster, 2048, FromCluster},
		"own":       {3, FromProfileArgs, 0, FromProfileArgs},
		"onlycache": {32, FromCluster, 512, FromProfileArgs},
	} {
		if got := c.HostMemoryOf(c.Profiles[name]); got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	if c.Defaults.CountHostMemory {
		t.Fatal("count_host_memory is on by default; off is the default (opt-in per profile)")
	}
}
