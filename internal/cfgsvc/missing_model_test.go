package cfgsvc

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// files is a models directory the test changes: a model file exists while
// its base name is in the set.
type files struct {
	mu  sync.Mutex
	set map[string]bool
}

func (f *files) exists(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.set[filepath.Base(p)]
}

func (f *files) remove(name string) {
	f.mu.Lock()
	delete(f.set, name)
	f.mu.Unlock()
}

func missingEnv(t *testing.T, present ...string) (*Service, *files) {
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
	f := &files{set: map[string]bool{}}
	for _, p := range present {
		f.set[p] = true
	}
	return New(context.Background(), Options{Store: cs, Parse: config.Options{FileExists: f.exists}}), f
}

func wantMissingRefused(t *testing.T, err error, model string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Error(), "file \""+model+"\" not found") {
		t.Fatalf("want a refusal for the missing %s, got %v", model, err)
	}
}

// A model file that goes missing blocks only the writes that point a profile
// at it; the profiles that already did stay, and other writes go through
// (B4).
func TestMissingModelFileBlocksOnlyWritesThatPointAtIt(t *testing.T) {
	ctx := context.Background()
	svc, f := missingEnv(t, "gone.gguf", "here.gguf")
	for _, id := range []string{"gone-a", "gone-b"} {
		if _, err := svc.CreateProfile(ctx, ui("x"), id, llama("gone.gguf", 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.CreateProfile(ctx, ui("x"), "kept", llama("here.gguf", 1024)); err != nil {
		t.Fatal(err)
	}
	f.remove("gone.gguf")

	t.Run("an unrelated settings write passes", func(t *testing.T) {
		if res, err := svc.PatchSettings(ctx, ui("x"), map[string]any{"ttl": "7m"}); err != nil || !res.Changed {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("another profile's edit passes", func(t *testing.T) {
		if _, err := svc.UpdateProfile(ctx, ui("x"), "kept", llama("here.gguf", 2048)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("an edit that keeps the missing file passes", func(t *testing.T) {
		if _, err := svc.UpdateProfile(ctx, ui("x"), "gone-a", llama("gone.gguf", 2048)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("adding a profile for a missing file is refused", func(t *testing.T) {
		_, err := svc.CreateProfile(ctx, ui("x"), "new", llama("gone.gguf", 1024))
		wantMissingRefused(t, err, "gone.gguf")
		_, err = svc.CreateProfile(ctx, ui("x"), "new", llama("never.gguf", 1024))
		wantMissingRefused(t, err, "never.gguf")
	})
	t.Run("changing a profile to a missing file is refused", func(t *testing.T) {
		_, err := svc.UpdateProfile(ctx, ui("x"), "kept", llama("gone.gguf", 1024))
		wantMissingRefused(t, err, "gone.gguf")
	})
	t.Run("deleting the profiles passes", func(t *testing.T) {
		for _, id := range []string{"gone-a", "gone-b"} {
			if _, err := svc.DeleteProfile(ctx, ui("x"), id); err != nil {
				t.Fatalf("delete %s: %v", id, err)
			}
		}
		if _, ok := svc.View().Profiles["gone-a"]; ok {
			t.Fatal("not deleted")
		}
	})
}
