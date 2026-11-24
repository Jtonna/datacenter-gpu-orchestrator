package api

import (
	"context"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// ui_model_actions existed in v47 and v48 (default true) and is removed:
// the page's Load, Unload and Force unload are part of the page. It is not
// in the schema or the settings; a PATCH that sends it is answered as any
// key the schema does not know.
func TestUIModelActions_Removed(t *testing.T) {
	e, _, _ := cfgEnvWith(t, func(o *Options) { o.keyExpiryEvery = -1 })
	plain := map[string]string{"Content-Type": "application/json"}
	_, sm := e.cdo("GET", "/api/config/settings", "", nil)
	if _, ok := findSetting(sm, "ui_model_actions"); ok {
		t.Fatalf("still in the settings: %s", rawJSON(sm))
	}
	_, sc := e.cdo("GET", "/api/config/schema", "", nil)
	for _, f := range sc["fields"].([]any) {
		if f := f.(map[string]any); f["key"] == "ui_model_actions" {
			t.Fatalf("still in the schema: %v", f)
		}
	}
	stU, mU := e.cdo("PATCH", "/api/config/settings", `{"ui_model_actions":false}`, plain)
	stX, mX := e.cdo("PATCH", "/api/config/settings", `{"no_such_setting_here":false}`, plain)
	erU, _ := mU["error"].(map[string]any)
	erX, _ := mX["error"].(map[string]any)
	if stU != stX || stU < 400 || erU["type"] != erX["type"] ||
		strings.ReplaceAll(toS(erU["message"]), "ui_model_actions", "K") != strings.ReplaceAll(toS(erX["message"]), "no_such_setting_here", "K") {
		t.Fatalf("PATCH with it: %d %v; with an unknown key: %d %v", stU, mU, stX, mX)
	}
	t.Logf("PATCH {\"ui_model_actions\":false} -> %d %v", stU, mU)
}

func toS(v any) string { s, _ := v.(string); return s }

// A database where v47 or v48 stored ui_model_actions: this image starts and
// serves, the stored value is dropped without an error and without counting
// as an unknown field (so configuration writes need no force), and the next
// write stores a document without it.
func TestUIModelActions_StoredValueIgnored(t *testing.T) {
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
	// What v47/v48 store after PATCH {"ui_model_actions": false}.
	if _, err := st.DB().Exec(`UPDATE config_revisions SET doc = replace(doc, '"settings":{}', '"settings":{"ui_model_actions":false}')`); err != nil {
		t.Fatal(err)
	}
	var doc string
	st.DB().QueryRow(`SELECT doc FROM config_revisions ORDER BY rowid DESC LIMIT 1`).Scan(&doc)
	if !strings.Contains(doc, `"ui_model_actions":false`) {
		t.Fatalf("fixture: %s", doc)
	}
	var e *env
	opts.Apply = func(c *config.Config) { e.sched.ApplyConfig(c) }
	opts.OnChange = func(ev cfgsvc.ChangeEvent) { e.api.PublishConfig(ev) }
	svc := cfgsvc.New(context.Background(), opts)
	e = newEnv(t, okUpstream(), func(o *Options) { o.Config, o.Audit = svc, cs })

	_, sys := e.cdo("GET", "/api/system", "", nil)
	sc, _ := sys["stored_config"].(map[string]any)
	if f, _ := sc["unknown_fields"].([]any); len(f) != 0 || sc["writes_need_force"] == true {
		t.Fatalf("stored_config %v", sys["stored_config"])
	}
	if code, m := e.cdo("GET", "/api/config", "", nil); code != 200 || m["revision"] != 1.0 {
		t.Fatalf("config %d %v", code, m["revision"])
	}
	_, sm := e.cdo("GET", "/api/config/settings", "", nil)
	if _, ok := findSetting(sm, "ui_model_actions"); ok {
		t.Fatalf("served: %s", rawJSON(sm))
	}
	if code, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":["x"]}`, nil); code != 200 {
		t.Fatalf("a write without force: %d %v", code, m)
	}
	st.DB().QueryRow(`SELECT doc FROM config_revisions ORDER BY rowid DESC LIMIT 1`).Scan(&doc)
	if strings.Contains(doc, "ui_model_actions") {
		t.Fatalf("the new revision still stores it: %s", doc)
	}
}

func findSetting(m map[string]any, key string) (any, bool) {
	set, _ := m["settings"].(map[string]any)
	v, ok := set[key]
	return v, ok
}
