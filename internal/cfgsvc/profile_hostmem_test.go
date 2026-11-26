package cfgsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// p33: this image knows the two profile fields. A stored document carrying
// them (the same one the v86 test stores, scratchpad p33_older_image_test)
// loads with no unknown field and no write refusal; a write sets and
// validates them; one written without them stores none (absent stays the
// cluster value).
func TestProfileHostMemoryFieldsAreKnown(t *testing.T) {
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
	opts := Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }}}
	first := New(ctx, opts)
	if _, err := first.CreateProfile(ctx, ui("a"), "qwen", llama("qwen.gguf", 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE config_revisions SET doc = replace(doc, '"model":"qwen.gguf"', '"model":"qwen.gguf","ctx_checkpoints":3,"cache_ram_mib":0') WHERE revision = 1`); err != nil {
		t.Fatal(err)
	}
	s := New(ctx, opts)
	if s.LoadError() != "" || len(s.UnknownFields()) != 0 {
		t.Fatalf("load %q unknown %q", s.LoadError(), s.UnknownFields())
	}
	p := s.Config().Profiles["qwen"]
	if p.CtxCheckpoints == nil || *p.CtxCheckpoints != 3 || p.CacheRAMMiB == nil || *p.CacheRAMMiB != 0 {
		t.Fatalf("profile %+v", p)
	}
	// a write passes (no force), and the stored fields stay
	if _, err := s.CreateProfile(ctx, ui("b"), "plain", llama("p.gguf", 1024)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if r, _ := cs.Latest(ctx); r.Doc.Profiles["qwen"].CtxCheckpoints == nil || r.Doc.Profiles["plain"].CtxCheckpoints != nil || r.Doc.Profiles["plain"].CacheRAMMiB != nil {
		t.Fatalf("stored %+v / %+v", r.Doc.Profiles["qwen"], r.Doc.Profiles["plain"])
	}
	bad, _ := ParseProfile([]byte(`{"model":"q.gguf","ctx":2048,"ctx_checkpoints":2000}`))
	if _, err := s.UpdateProfile(ctx, ui("b"), "qwen", bad); err == nil || !strings.Contains(err.Error(), "ctx_checkpoints: must be between 0 and 1024") {
		t.Fatalf("out of bounds: %v", err)
	}
	flag, _ := ParseProfile([]byte(`{"model":"q.gguf","ctx":2048,"extra_args":["--cache-ram","0"]}`))
	if _, err := s.UpdateProfile(ctx, ui("b"), "qwen", flag); err == nil || !strings.Contains(err.Error(), "set this profile's field cache_ram_mib") {
		t.Fatalf("flag in extra_args: %v", err)
	}
	ok, err := ParseProfile([]byte(`{"model":"q.gguf","ctx":2048,"ctx_checkpoints":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile(ctx, ui("b"), "qwen", ok); err != nil {
		t.Fatal(err)
	}
	if p := s.Config().Profiles["qwen"]; *p.CtxCheckpoints != 1 || p.CacheRAMMiB != nil {
		t.Fatalf("after the update (a profile write replaces the profile, as with ttl): %+v", p)
	}
}
