package cfgsvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// The readiness round trip: a newer version writes the stored configuration
// with fields this version does not know (an optional profile field, a new
// setting); this version starts on it. It keeps serving everything it
// understands (not revision 0 and no profiles), warns once per unknown
// field, reports them, and refuses every configuration write unless forced,
// because the write would drop them. A forced write drops them and clears
// the report.
func TestStoredUnknownFieldsAreToleratedAndBlockWrites(t *testing.T) {
	dir := t.TempDir()
	st, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }}}
	ctx := context.Background()
	old := New(ctx, opts)
	if _, err := old.CreateProfile(ctx, ui("a"), "qwen", llama("qwen.gguf", 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := old.CreateProfile(ctx, ui("a"), "gemma", llama("gemma.gguf", 2048)); err != nil {
		t.Fatal(err)
	}
	// "the newer version" rewrites the latest revision with two fields more
	if _, err := st.DB().Exec(`UPDATE config_revisions SET doc = replace(replace(doc, '"model":"qwen.gguf"', '"model":"qwen.gguf","description":"small and quick"'), '"settings":{}', '"settings":{"brand_new_setting":true}') WHERE revision = 2`); err != nil {
		t.Fatal(err)
	}
	if r, err := cs.Latest(ctx); err != nil || !reflect.DeepEqual(r.Unknown, []string{"profiles.*.description", "settings.brand_new_setting"}) {
		t.Fatalf("the edit did not take: %v %+v", err, r)
	}

	var logs bytes.Buffer
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	s := New(ctx, opts)
	if s.LoadError() != "" || s.Revision() != 2 || len(s.Config().Profiles) != 2 || s.Config().Profiles["qwen"].Ctx != 4096 || !s.Config().Configured {
		t.Fatalf("not serving what it understands: err %q rev %d profiles %d", s.LoadError(), s.Revision(), len(s.Config().Profiles))
	}
	want := []string{"profiles.*.description", "settings.brand_new_setting"}
	if got := s.UnknownFields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown fields %q", got)
	}
	for _, f := range want {
		if n := strings.Count(logs.String(), "field="+f); n != 1 || !strings.Contains(logs.String(), "level=WARN") {
			t.Fatalf("field %s warned %d times:\n%s", f, n, logs.String())
		}
	}

	// every write is refused without force, dry runs too
	writes := map[string]func(WriteOpts) error{
		"create":  func(o WriteOpts) error { _, err := s.CreateProfile(ctx, o, "new", llama("n.gguf", 1024)); return err },
		"exclude": func(o WriteOpts) error { _, err := s.SetExcludeNodes(ctx, o, []string{"x"}); return err },
		"setting": func(o WriteOpts) error {
			_, err := s.PatchSettings(ctx, o, map[string]any{"queue_timeout": "90s"})
			return err
		},
		"rollback": func(o WriteOpts) error { _, err := s.Rollback(ctx, o, 1); return err },
	}
	for name, w := range writes {
		for _, dry := range []bool{false, true} {
			o := ui("b")
			o.DryRun = dry
			err := w(o)
			var ue *UnknownFieldsError
			if !errors.As(err, &ue) || !reflect.DeepEqual(ue.Fields, want) || !strings.Contains(err.Error(), "force") {
				t.Fatalf("%s (dry run %v) not refused: %v", name, dry, err)
			}
		}
	}
	if s.Revision() != 2 {
		t.Fatalf("a refused write committed: revision %d", s.Revision())
	}

	// forced: written without the unknown fields, and the report clears
	o := ui("b")
	o.Force = true
	res, err := s.SetExcludeNodes(ctx, o, []string{"x"})
	if err != nil || res.Revision != 3 {
		t.Fatalf("forced write: %+v %v", res, err)
	}
	if len(s.UnknownFields()) != 0 {
		t.Fatalf("still reported after the forced write: %q", s.UnknownFields())
	}
	if r, err := cs.Latest(ctx); err != nil || len(r.Unknown) != 0 || len(r.Doc.Profiles) != 2 {
		t.Fatalf("revision 3: %v %+v", err, r)
	}
	if _, err := s.CreateProfile(ctx, ui("b"), "new", llama("n.gguf", 1024)); err != nil {
		t.Fatalf("writes are still refused: %v", err)
	}
}
