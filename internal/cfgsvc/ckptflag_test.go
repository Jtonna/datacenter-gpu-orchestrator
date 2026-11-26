package cfgsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// A stored profile written before --ctx-checkpoints became managed still
// loads (the scheduler does not fall back to built-in defaults); an edit of
// another profile passes; a write that adds the flag to a profile, or keeps
// the stored one but changes its value, is refused.
func TestStoredCtxCheckpointsFlagStaysLoadable(t *testing.T) {
	ctx := context.Background()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	old := llama("a.gguf", 1024)
	old.ExtraArgs = []string{"--ctx-checkpoints", "3"}
	d := config.NewDocument()
	d.Profiles = map[string]*config.ProfileSpec{"old": old, "fine": llama("b.gguf", 1024)}
	if _, _, err := cs.Commit(ctx, cfgstore.Change{Doc: d, Origin: audit.Origin{Actor: "older-version", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	svc := New(ctx, Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }}})
	if e := svc.LoadError(); e != "" {
		t.Fatalf("load error %q", e)
	}
	if p := svc.Config().Profiles["old"]; p == nil || len(p.ExtraArgs) != 2 {
		t.Fatalf("profile old %+v", p)
	}
	if w := svc.Config().LateManagedArgs(); len(w) != 1 || !strings.Contains(w[0], "--ctx-checkpoints 3") {
		t.Fatalf("warnings %q", w)
	}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "fine", llama("b.gguf", 2048)); err != nil {
		t.Fatalf("an edit of another profile: %v", err)
	}
	add := llama("b.gguf", 1024)
	add.ExtraArgs = []string{"-ctxcp", "1"}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "fine", add); err == nil || !strings.Contains(err.Error(), "ctx_checkpoints") {
		t.Fatalf("adding the flag: %v", err)
	}
	changed := llama("a.gguf", 1024)
	changed.ExtraArgs = []string{"--ctx-checkpoints", "1"}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "old", changed); err == nil {
		t.Fatal("changing the stored flag was accepted")
	}
}

// v87: a configuration stored by the next release, where profiles carry
// their own --cache-ram / -cram / --ctx_checkpoints, loads with every
// profile (no fall back to built-in defaults: a rollback is clean); a
// write that adds --cache-ram is still refused.
func TestStoredHostFlagsFromTheNextReleaseLoad(t *testing.T) {
	ctx := context.Background()
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
	d.Profiles = map[string]*config.ProfileSpec{"fine": llama("b.gguf", 1024)}
	for name, args := range map[string][]string{
		"cache": {"--cache-ram", "0"}, "cram": {"-cram", "512"}, "under": {"--ctx_checkpoints", "3", "--cache_ram", "256"},
		"both": {"--ctx-checkpoints", "3", "--cache-ram", "0", "--temp", "0.7"},
	} {
		p := llama("a.gguf", 1024)
		p.ExtraArgs = args
		d.Profiles[name] = p
	}
	if _, _, err := cs.Commit(ctx, cfgstore.Change{Doc: d, Origin: audit.Origin{Actor: "newer-version", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	svc := New(ctx, Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }}})
	if e := svc.LoadError(); e != "" {
		t.Fatalf("load error %q", e)
	}
	if n := len(svc.Config().Profiles); n != 5 {
		t.Fatalf("%d profiles loaded, want 5", n)
	}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "fine", llama("b.gguf", 2048)); err != nil {
		t.Fatalf("an edit of another profile: %v", err)
	}
	add := llama("b.gguf", 1024)
	add.ExtraArgs = []string{"--cache-ram", "0"}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "fine", add); err == nil || !strings.Contains(err.Error(), "cache_ram_mib") {
		t.Fatalf("adding --cache-ram: %v", err)
	}
}
