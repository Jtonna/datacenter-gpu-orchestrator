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

// A stored configuration where profiles carry their own --cache-ram /
// -cram / --ctx_checkpoints / --ctx-checkpoints loads with every profile
// (no fall back to built-in defaults), and an edit of another profile
// passes. Kept from v87 (185906d, tolerate stored host flags): reading
// stored documents must not regress. Since p33 such a write is accepted
// (the profile's value is passed at launch); only a form the server cannot
// read, such as --cache-ram=0, is refused.
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
	add.ExtraArgs = []string{"--cache-ram=0"}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "fine", add); err == nil || !strings.Contains(err.Error(), "does not read --cache-ram=value") {
		t.Fatalf("adding --cache-ram=0: %v", err)
	}
	add.ExtraArgs = []string{"--cache-ram", "0"}
	if _, err := svc.UpdateProfile(ctx, ui("x"), "fine", add); err != nil {
		t.Fatalf("adding --cache-ram 0: %v", err)
	}
}
