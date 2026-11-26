package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// p33: a profile's own restore points per conversation slot (ctx_checkpoints)
// and prompt cache (cache_ram_mib). Absent is the cluster value, exactly as
// before; set, it is the profile's on either path; bounds 0..1024 and >= 0;
// llama only.
func TestProfileHostMemoryFields(t *testing.T) {
	y := prof("  plain:\n    model: m.gguf\n    ctx: 1\n" +
		"  own:\n    model: m.gguf\n    ctx: 1\n    ctx_checkpoints: 3\n    cache_ram_mib: 0\n" +
		"  onnode:\n    model: m.gguf\n    ctx: 1\n    host: node\n    force_nodes: [a]\n" +
		"  onnodeown:\n    model: m.gguf\n    ctx: 1\n    host: node\n    force_nodes: [a]\n    cache_ram_mib: 512\n")
	y = strings.Replace(y, "models_dir: /models\n", "models_dir: /models\ndefaults:\n  membership: agents\n", 1)
	c, err := ParseWithOptions([]byte(y), Options{FileExists: existsAll})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		ck, cache    int
		ckSet, caSet bool
	}
	for name, w := range map[string]want{"plain": {32, 2048, false, false}, "own": {3, 0, true, true},
		"onnode": {32, 0, false, false}, "onnodeown": {32, 512, false, true}} {
		p := c.Profiles[name]
		ck, ckSet := c.EffectiveCtxCheckpoints(p)
		ca, caSet := c.EffectiveCacheRAMMiB(p)
		if (want{ck, ca, ckSet, caSet}) != w {
			t.Errorf("%s: restore points %d (own %v), cache %d (own %v); want %+v", name, ck, ckSet, ca, caSet, w)
		}
	}
	if p := c.Profiles["plain"]; p.CtxCheckpoints != nil || p.CacheRAMMiB != nil {
		t.Fatal("absent fields are not nil")
	}

	refused := map[string]string{
		"ctx_checkpoints: -1":   "ctx_checkpoints: must be between 0 and 1024",
		"ctx_checkpoints: 1025": "ctx_checkpoints: must be between 0 and 1024",
		"cache_ram_mib: -1":     "cache_ram_mib: must not be negative",
	}
	for line, msg := range refused {
		if err := parseErr(prof("  p:\n    model: m.gguf\n    ctx: 1\n    "+line+"\n"), nil); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v", line, err)
		}
	}
	for _, line := range []string{"ctx_checkpoints: 0", "ctx_checkpoints: 1024", "cache_ram_mib: 0", "cache_ram_mib: 65536"} {
		if err := parseErr(prof("  p:\n    model: m.gguf\n    ctx: 1\n    "+line+"\n"), nil); err != nil {
			t.Errorf("%s refused: %v", line, err)
		}
	}
	for _, f := range []string{"ctx_checkpoints: 3", "cache_ram_mib: 0"} {
		err := parseErr(prof("  c:\n    kind: command\n    cmd: run ${PORT}\n    needs_mib: 10\n    "+f+"\n"), nil)
		if err == nil || !strings.Contains(err.Error(), "only valid with kind: llama") {
			t.Errorf("command %s: %v", f, err)
		}
	}
}

// The flags in extra_args are refused with a sentence pointing to the
// fields; a stored profile that already carries --ctx-checkpoints
// unchanged still loads (as in v81), its warning now naming the field.
func TestProfileHostMemoryFlagsPointToTheFields(t *testing.T) {
	for flag, field := range map[string]string{"--ctx-checkpoints": "ctx_checkpoints", "-ctxcp": "ctx_checkpoints", "--swa-checkpoints": "ctx_checkpoints",
		"--cache-ram": "cache_ram_mib", "-cram": "cache_ram_mib"} {
		err := parseErr(prof("  p:\n    model: m.gguf\n    ctx: 1\n    extra_args: [\""+flag+"\", \"3\"]\n"), nil)
		if err == nil || !strings.Contains(err.Error(), "set this profile's field "+field) || !strings.Contains(err.Error(), "leave the field empty for the cluster value") ||
			strings.Contains(err.Error(), "managed flag") {
			t.Errorf("%s: %v", flag, err)
		}
	}
	doc := NewDocument()
	ctx := 1
	doc.Profiles["p"] = &ProfileSpec{Model: "m.gguf", Ctx: &ctx, ExtraArgs: []string{"--ctx-checkpoints", "3"}}
	c, err := Build(doc, Bootstrap{}, Options{FileExists: existsAll, KeepArgs: map[string][]string{"p": {"--ctx-checkpoints", "3"}}})
	if err != nil {
		t.Fatalf("stored profile refused: %v", err)
	}
	if w := c.LateManagedArgs(); len(w) != 1 || !strings.Contains(w[0], "this profile's field ctx_checkpoints") {
		t.Fatalf("warning %q", w)
	}
}

// config.Schema() lists both, in the memory section, for llama, nullable,
// with their bounds; a document round trip keeps them and leaves them out
// when absent (an older image never sees them unless a profile sets them).
func TestProfileHostMemorySchemaAndDocument(t *testing.T) {
	got := map[string]Field{}
	for _, f := range Schema() {
		if f.Scope == ScopeProfile && (f.Key == "ctx_checkpoints" || f.Key == "cache_ram_mib") {
			got[f.Key] = f
		}
	}
	ck, ca := got["ctx_checkpoints"], got["cache_ram_mib"]
	if ck.Type != "int" || !ck.Nullable || ck.AppliesTo != "llama" || ck.Section != "memory" || *ck.Min != 0 || ck.Max == nil || *ck.Max != 1024 ||
		!strings.Contains(ck.Description, "with 3,") || !strings.Contains(ck.Description, "with 1,") {
		t.Fatalf("ctx_checkpoints %+v", ck)
	}
	if ca.Type != "int" || !ca.Nullable || ca.AppliesTo != "llama" || *ca.Min != 0 || ca.Max != nil || !strings.Contains(ca.Description, "0 keeps no prompt cache") {
		t.Fatalf("cache_ram_mib %+v", ca)
	}
	three, zero := 3, 0
	b, _ := json.Marshal(&ProfileSpec{Model: "m.gguf", CtxCheckpoints: &three, CacheRAMMiB: &zero})
	if !strings.Contains(string(b), `"ctx_checkpoints":3`) || !strings.Contains(string(b), `"cache_ram_mib":0`) {
		t.Fatalf("set: %s", b)
	}
	b, _ = json.Marshal(&ProfileSpec{Model: "m.gguf"})
	if strings.Contains(string(b), "ctx_checkpoints") || strings.Contains(string(b), "cache_ram_mib") {
		t.Fatalf("absent: %s", b)
	}
	var back ProfileSpec
	if err := json.Unmarshal([]byte(`{"model":"m.gguf","ctx_checkpoints":0}`), &back); err != nil || back.CtxCheckpoints == nil || *back.CtxCheckpoints != 0 || back.CacheRAMMiB != nil {
		t.Fatalf("0 is a value, not absent: %+v %v", back, err)
	}
	_ = core.HostNode
}
