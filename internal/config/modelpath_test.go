package config

import (
	"strings"
	"testing"
)

// A profile's model must be a plain path under models_dir: no part that
// starts with a dot (the trash, the owner's and the upload folders), not
// absolute, no "..", no backslash, no drive, no NUL. The file existing does
// not make such a path acceptable. A plain sub-folder stays allowed.
func TestProfileModelPathRule(t *testing.T) {
	for _, bad := range []struct{ model, want string }{
		{".trash/0123456789abcdef/a.gguf", "starts with a dot"},
		{".incoming/a.gguf", "starts with a dot"},
		{".potluck-uploads/a.gguf", "starts with a dot"},
		{"sub/.x/a.gguf", "starts with a dot"},
		{"../a.gguf", ".."},
		{"sub/../a.gguf", ".."},
		{"/models/a.gguf", "absolute"},
		{`sub\\a.gguf`, "backslash"},
		{"C:/m/a.gguf", "drive"},
	} {
		y := prof("  p:\n    model: \"" + bad.model + "\"\n    ctx: 4096\n")
		err := parseErr(y, existsAll) // the file "exists": the path alone is refused
		if err == nil {
			t.Errorf("model %q: accepted, want refused", bad.model)
			continue
		}
		for _, w := range []string{`profile "p"`, "model", bad.want} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("model %q: error %q does not say %q", bad.model, err, w)
			}
		}
	}
	for _, ok := range []string{"a.gguf", "sub/a.gguf", "sub/deeper/a.b.gguf"} {
		if err := parseErr(prof("  p:\n    model: \""+ok+"\"\n    ctx: 4096\n"), existsAll); err != nil {
			t.Errorf("model %q: %v, want accepted", ok, err)
		}
	}
	// NUL cannot be written in the YAML above; the document path takes it.
	d := NewDocument()
	d.Profiles = map[string]*ProfileSpec{"p": {Model: "a\x00.gguf", Ctx: ctx4096()}}
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll}); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("model with a NUL byte: %v, want refused", err)
	}
}

// A stored profile that already names such a path is kept the way a profile
// whose file went missing is kept (Options.KeepModels): an edit that keeps
// the path passes; a write that adds a profile or points one at such a path
// is refused.
func TestProfileModelPathRule_KeptLikeAMissingFile(t *testing.T) {
	const old = ".trash/0123456789abcdef/a.gguf"
	d := NewDocument()
	d.Profiles = map[string]*ProfileSpec{"p": {Model: old, Ctx: ctx4096()}}
	if _, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, KeepModels: map[string]string{"p": old}}); err != nil {
		t.Fatalf("a profile that keeps its stored path: %v, want accepted", err)
	}
	d.Profiles["q"] = &ProfileSpec{Model: old, Ctx: ctx4096()}
	_, err := Build(d, Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll, KeepModels: map[string]string{"p": old}})
	if err == nil || !strings.Contains(err.Error(), `profile "q"`) || strings.Contains(err.Error(), `profile "p"`) {
		t.Fatalf("a new profile naming the path: %v, want only q refused", err)
	}
}

func ctx4096() *int { n := 4096; return &n }
