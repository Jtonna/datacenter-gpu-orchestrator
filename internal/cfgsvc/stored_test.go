package cfgsvc

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// storedEnv is a service whose trained contexts are set by the test
// (nothing known at first: profiles are written as on live before v79).
func storedEnv(t *testing.T) (*Service, func(file string, n int)) {
	t.Helper()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	known := map[string]int{}
	tc := func(p string) (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := known[filepath.Base(p)]
		return v, ok
	}
	svc := New(context.Background(), Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }, TrainedCtx: tc}})
	return svc, func(file string, n int) { mu.Lock(); known[file] = n; mu.Unlock() }
}

func lateArgs(p *config.ProfileSpec) *config.ProfileSpec {
	p.ExtraArgs = []string{"--ctx-checkpoints=3"}
	return p
}

// A profile put back as it was stored passes every check a stored profile
// passes: a ctx above the trained context, a flag managed later in
// extra_args, a model path the path rule now refuses. Each comes back as a
// warning. The same profiles through an ordinary write are refused: no hole.
func TestAddStoredProfiles_EveryStoredExemption(t *testing.T) {
	ctx := context.Background()
	svc, train := storedEnv(t)
	train("tiny.gguf", 2048)
	ps := map[string]*config.ProfileSpec{
		"above": llama("tiny.gguf", 8192),
		"late":  lateArgs(llama("tiny.gguf", 2048)),
		"path":  llama("sub//tiny.gguf", 2048),
		"plain": llama("tiny.gguf", 2048),
	}
	for id, p := range ps {
		one := map[string]*config.ProfileSpec{id: p}
		if id != "plain" {
			if _, err := svc.AddProfiles(ctx, ui("x"), one); err == nil {
				t.Fatalf("%s: an ordinary add accepted it", id)
			}
			if _, err := svc.CreateProfile(ctx, ui("x"), id, p); err == nil {
				t.Fatalf("%s: a create accepted it", id)
			}
		}
	}
	rev := svc.Revision()
	res, err := svc.AddStoredProfiles(ctx, ui("x"), ps, "restored profiles")
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != rev+1 || len(svc.Document().Profiles) != 4 {
		t.Fatalf("revision %d (was %d), profiles %d", res.Revision, rev, len(svc.Document().Profiles))
	}
	w := strings.Join(res.Warnings, " | ")
	for _, want := range []string{
		"1 of the 3 restored profiles of tiny.gguf have a context above the model's trained 2,048 and run at 2,048",
		`profile "path" names the model path "sub//tiny.gguf"`,
		`profile "late": extra_args "--ctx-checkpoints=3": the model server does not read --ctx-checkpoints=value`,
	} {
		if !strings.Contains(w, want) {
			t.Fatalf("warnings %q lack %q", w, want)
		}
	}
	// stored, they load at a restart and an edit that keeps them passes
	svc2 := New(ctx, svc.o)
	if e := svc2.LoadError(); e != "" {
		t.Fatalf("load: %s", e)
	}
	p := llama("tiny.gguf", 8192)
	p.Description = "edited"
	if _, err := svc2.UpdateProfile(ctx, ui("x"), "above", p); err != nil {
		t.Fatal(err)
	}
	// raising it is a new value: refused
	if _, err := svc2.UpdateProfile(ctx, ui("x"), "above", llama("tiny.gguf", 16384)); err == nil {
		t.Fatal("raising a restored ctx accepted")
	}
}

// A rollback to a revision that holds profiles a new write would refuse is
// accepted (a stored revision is restorable), with the warnings; a file it
// names must still be there.
func TestRollback_StoredProfilesAboveTrained(t *testing.T) {
	ctx := context.Background()
	svc, train := storedEnv(t)
	for _, n := range []int{2304, 8192} {
		if _, err := svc.CreateProfile(ctx, ui("x"), "bench-"+map[int]string{2304: "2304", 8192: "8192"}[n], llama("tiny.gguf", n)); err != nil {
			t.Fatal(err)
		}
	}
	back := svc.Revision()
	if _, err := svc.RemoveProfiles(ctx, ui("x"), []string{"bench-2304", "bench-8192"}); err != nil {
		t.Fatal(err)
	}
	train("tiny.gguf", 2048)
	res, err := svc.Rollback(ctx, ui("x"), back)
	if err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	if len(svc.Document().Profiles) != 2 || !strings.Contains(strings.Join(res.Warnings, " "), "2 of the 2 profiles of revision") {
		t.Fatalf("profiles %d, warnings %v", len(svc.Document().Profiles), res.Warnings)
	}
	// a rollback that changes nothing about them names nothing
	res, err = svc.PatchSettings(ctx, ui("x"), map[string]any{"ttl": "20m"})
	if err != nil {
		t.Fatal(err)
	}
	res, err = svc.Rollback(ctx, ui("x"), svc.Revision()-1)
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("rollback of a setting: %v %v", res.Warnings, err)
	}
}

// An import of an export that holds such profiles passes when the
// configuration already holds them unchanged (same id, model and ctx: the
// stored rule), which is what an export of this scheduler imported back is.
// A NEW profile in an imported document is an ordinary write: refused.
func TestImport_StoredProfilesAboveTrained(t *testing.T) {
	ctx := context.Background()
	svc, train := storedEnv(t)
	if _, err := svc.CreateProfile(ctx, ui("x"), "bench-8192", llama("tiny.gguf", 8192)); err != nil {
		t.Fatal(err)
	}
	train("tiny.gguf", 2048)
	doc := svc.Document()
	doc.Profiles["plain"] = llama("tiny.gguf", 2048)
	if _, err := svc.Import(ctx, ui("x"), doc, true); err != nil {
		t.Fatalf("import of the export: %v", err)
	}
	doc = svc.Document()
	doc.Profiles["bench-new-8192"] = llama("tiny.gguf", 8192)
	if _, err := svc.Import(ctx, ui("x"), doc, true); err == nil || !strings.Contains(err.Error(), "above this model's trained context") {
		t.Fatalf("a new profile above in an import: %v", err)
	}
	doc = svc.Document()
	doc.Profiles["bench-renamed-8192"] = doc.Profiles["bench-8192"]
	delete(doc.Profiles, "bench-8192")
	if _, err := svc.Import(ctx, ui("x"), doc, true); err == nil {
		t.Fatal("an import that renames a profile above the trained context was accepted (it is a new id)")
	}
}
