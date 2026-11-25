package config

import (
	"strings"
	"testing"
)

// --ctx-checkpoints (and its spellings) is a managed flag: the scheduler
// passes it on every launch from the setting ctx_checkpoints. A profile may
// not set it: refused with a sentence naming the setting.
func TestCtxCheckpointsIsManaged(t *testing.T) {
	for _, f := range []string{"--ctx-checkpoints", "-ctxcp", "--swa-checkpoints"} {
		for _, form := range []string{f, f + "=3"} {
			y := prof("  p:\n    model: m.gguf\n    ctx: 1\n    extra_args: [\"" + form + "\", \"3\"]\n")
			err := parseErr(y, nil)
			if err == nil || !strings.Contains(err.Error(), `profile "p"`) || !strings.Contains(err.Error(), f) ||
				!strings.Contains(err.Error(), "ctx_checkpoints") {
				t.Fatalf("%s: want a refusal naming the setting, got %v", form, err)
			}
		}
	}
}

// A STORED profile that already carries the flag is not refused (an older
// configuration stays loadable): Options.KeepArgs names the extra_args the
// profile had before; unchanged, they pass, and LateManagedArgs says what the
// launch drops. A write that adds the flag, or changes those extra_args, is
// refused.
func TestCtxCheckpointsStoredProfileStaysLoadable(t *testing.T) {
	doc := NewDocument()
	ctx := 1
	doc.Profiles["p"] = &ProfileSpec{Model: "m.gguf", Ctx: &ctx, ExtraArgs: []string{"--no-warmup", "--ctx-checkpoints", "3"}}
	if _, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll}); err == nil {
		t.Fatal("a new write with --ctx-checkpoints was accepted")
	}
	keep := map[string][]string{"p": {"--no-warmup", "--ctx-checkpoints", "3"}}
	c, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll, KeepArgs: keep})
	if err != nil {
		t.Fatalf("the stored profile is refused: %v", err)
	}
	if w := c.LateManagedArgs(); len(w) != 1 || !strings.Contains(w[0], `profile "p"`) || !strings.Contains(w[0], "--ctx-checkpoints 3") ||
		!strings.Contains(w[0], "ctx_checkpoints") {
		t.Fatalf("warnings %q", w)
	}
	doc.Profiles["p"].ExtraArgs = []string{"--ctx-checkpoints", "5"}
	if _, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll, KeepArgs: keep}); err == nil {
		t.Fatal("a changed --ctx-checkpoints was accepted")
	}
}
