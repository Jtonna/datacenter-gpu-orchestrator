package api

import (
	"testing"
)

// ui_model_actions: a page switch (default true) in the api section of the
// settings; read with the other settings, changed (and audited) through
// PATCH /api/config/settings. The model routes are unchanged by it.
func TestUIModelActions_Setting(t *testing.T) {
	e, _, _ := cfgEnvWith(t, func(o *Options) { o.keyExpiryEvery = -1 })
	plain := map[string]string{"Content-Type": "application/json"}
	_, sm := e.cdo("GET", "/api/config/settings", "", nil)
	if v, ok := findSetting(sm, "ui_model_actions"); !ok || v != true {
		t.Fatalf("default: %v %v in %s", v, ok, rawJSON(sm))
	}
	_, sc := e.cdo("GET", "/api/config/schema", "", nil)
	found := false
	for _, f := range sc["fields"].([]any) {
		if f := f.(map[string]any); f["key"] == "ui_model_actions" && f["scope"] == "settings" {
			found = true
			if f["default"] != true || f["type"] != "bool" || f["section"] != "api" || f["restart_required"] != false ||
				f["description"] != "Load and unload models from the web UI (with a confirm and an audit entry)" {
				t.Fatalf("schema: %v", f)
			}
		}
	}
	if !found {
		t.Fatal("ui_model_actions is not in the schema")
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"ui_model_actions":"no"}`, plain); st != 400 && st != 422 {
		t.Fatalf("bad value: %d %v", st, m)
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"ui_model_actions":false}`, plain); st != 200 {
		t.Fatalf("set: %d %v", st, m)
	}
	_, sm = e.cdo("GET", "/api/config/settings", "", nil)
	if v, _ := findSetting(sm, "ui_model_actions"); v != false {
		t.Fatalf("after the change: %v", v)
	}
	if e.api.conf().UIModelActions {
		t.Fatal("config not applied")
	}
	if es := auditOf(t, e, "section=settings"); len(es) == 0 {
		t.Fatal("no audit entry")
	}
}

func findSetting(m map[string]any, key string) (any, bool) {
	set, _ := m["settings"].(map[string]any)
	v, ok := set[key]
	return v, ok
}
