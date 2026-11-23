package api

import (
	"context"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// A stored configuration with a field this version does not know: the API
// serves it, GET /api/system names the field, a configuration write answers
// 409 unless ?force=1, and what clients send stays strict.
func TestAPIStoredUnknownFields(t *testing.T) {
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	opts := cfgsvc.Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }}}
	first := cfgsvc.New(context.Background(), opts)
	p, err := cfgsvc.ParseProfile([]byte(`{"model":"q.gguf","ctx":2048,"needs_mib":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateProfile(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t"}}, "q", p); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE config_revisions SET doc = replace(replace(doc, '"model":"q.gguf"', '"model":"q.gguf","x_field_from_the_future":"newer"'), '"settings":{}', '"settings":{"x_setting_from_the_future":true},"x_top_from_the_future":{"a":1}')`); err != nil {
		t.Fatal(err)
	}

	var e *env
	opts.Apply = func(c *config.Config) { e.sched.ApplyConfig(c) }
	opts.OnChange = func(ev cfgsvc.ChangeEvent) { e.api.PublishConfig(ev) }
	svc := cfgsvc.New(context.Background(), opts)
	e = newEnv(t, okUpstream(), func(o *Options) { o.Config, o.Audit = svc, cs })

	_, sys := e.cdo("GET", "/api/system", "", nil)
	sc, _ := sys["stored_config"].(map[string]any)
	if f, _ := sc["unknown_fields"].([]any); len(f) != 3 || f[0] != "profiles.*.x_field_from_the_future" || f[1] != "settings.x_setting_from_the_future" || f[2] != "x_top_from_the_future" || sc["writes_need_force"] != true {
		t.Fatalf("system stored_config %v", sys["stored_config"])
	}
	if code, m := e.cdo("GET", "/api/config", "", nil); code != 200 || m["revision"] != 1.0 {
		t.Fatalf("config %d %v", code, m["revision"])
	}
	code, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":["x"]}`, nil)
	er, _ := m["error"].(map[string]any)
	if code != 409 || er["type"] != "unknown_stored_fields" {
		t.Fatalf("write without force: %d %v", code, m)
	}
	// what a client sends is still strict
	if code, _ := e.cdo("POST", "/api/config/profiles?force=1", `{"id":"n","model":"n.gguf","ctx":2048,"needs_mib":100,"x_field_from_the_future":"x"}`, nil); code != 400 {
		t.Fatalf("a client field this version does not know: %d, want 400", code)
	}
	if code, m := e.cdo("PUT", "/api/config/exclude-nodes?force=1", `{"exclude_nodes":["x"]}`, nil); code != 200 {
		t.Fatalf("forced write: %d %v", code, m)
	}
	_, sys = e.cdo("GET", "/api/system", "", nil)
	if sc, _ := sys["stored_config"].(map[string]any); sc["writes_need_force"] != false {
		t.Fatalf("after the forced write: %v", sys["stored_config"])
	}
}
