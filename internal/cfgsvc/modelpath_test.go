package cfgsvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// A stored profile whose model path the path rule now refuses (written by an
// older version) is treated as a profile whose file went missing: the
// scheduler starts with the stored configuration and keeps the profile (its
// load is refused at plan time, internal/scheduler), an edit that keeps the
// path passes, and a write that adds a profile for such a path or points one
// at it is refused.
func TestStoredProfileWithRefusedPathIsKeptLikeAMissingFile(t *testing.T) {
	ctx := context.Background()
	const bad = ".trash/0123456789abcdef/a.gguf"
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	d := config.NewDocument()
	d.Profiles = map[string]*config.ProfileSpec{"old": llama(bad, 1024), "fine": llama("here.gguf", 1024)}
	if _, _, err := cs.Commit(ctx, cfgstore.Change{Doc: d, Origin: audit.Origin{Actor: "older-version", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	everything := func(string) bool { return true } // every file is there: only the path is in question
	svc := New(ctx, Options{Store: cs, Parse: config.Options{FileExists: everything}})

	if e := svc.LoadError(); e != "" {
		t.Fatalf("start: load error %q, want the stored configuration used", e)
	}
	if p, ok := svc.Config().Profiles["old"]; !ok || p.Model != bad {
		t.Fatalf("start: profile old = %+v %v, want kept with its stored path", p, ok)
	}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "old", llama(bad, 2048)); err != nil {
		t.Fatalf("an edit that keeps the stored path: %v", err)
	}
	for _, w := range []struct {
		name string
		do   func() error
	}{
		{"a new profile for the path", func() error { _, err := svc.CreateProfile(ctx, ui("x"), "new", llama(bad, 1024)); return err }},
		{"a profile pointed at the path", func() error { _, err := svc.UpdateProfile(ctx, ui("x"), "fine", llama(bad, 1024)); return err }},
		{"a new profile for another dot-folder", func() error {
			_, err := svc.CreateProfile(ctx, ui("x"), "up", llama(".potluck-uploads/b.gguf", 1024))
			return err
		}},
	} {
		err := w.do()
		var ve *ValidationError
		if err == nil || !errors.As(err, &ve) || !strings.Contains(err.Error(), "starts with a dot") {
			t.Errorf("%s: %v, want a validation refusal naming the dot part", w.name, err)
		}
	}
	if _, err := svc.CreateProfile(ctx, ui("x"), "sub", llama("sub/c.gguf", 1024)); err != nil {
		t.Errorf("a plain sub-folder: %v, want accepted", err)
	}
	// An older revision is still shown.
	if v, _, err := svc.RevisionView(ctx, 1); err != nil || v == nil {
		t.Errorf("revision 1 view: %v", err)
	}
}
