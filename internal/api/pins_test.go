package api

import (
	"context"
	"testing"
)

// Models slice 2: PUT and DELETE /api/models/pin/{key...} pin and unpin a
// model by its catalog key, take a comment, are audited in section models,
// need the permission models (not observe), and the catalog says pinned.
func TestModelPins_RoutesCatalogAuditAndPermission(t *testing.T) {
	e, _, _ := cfgEnv(t)
	ui := map[string]string{"X-Source": "ui", "X-Actor": "alex"}
	e.cdo("POST", "/api/config/profiles", `{"id":"g1","model":"gemma-4-12b-it-Q4_K_M.gguf","ctx":2048,"needs_mib":100}`, nil)
	const g1 = "gemma-4-12b-it-Q4_K_M.gguf"
	pinned := func(key string) (any, any) {
		_, m := e.cdo("GET", "/api/models", "", nil)
		for _, x := range m["models"].([]any) {
			if x.(map[string]any)["key"] == key {
				return x.(map[string]any)["pinned"], x.(map[string]any)["pinned_at"]
			}
		}
		t.Fatalf("no %s in the catalog", key)
		return nil, nil
	}
	if p, at := pinned(g1); p != false || at != nil {
		t.Fatalf("before: %v %v", p, at)
	}

	// refused: no comment, an unknown model, unpinning what is not pinned
	if st, m := e.cdo("PUT", "/api/models/pin/"+g1, ``, ui); st != 400 {
		t.Fatalf("no comment: %d %v", st, m)
	}
	if st, m := e.cdo("PUT", "/api/models/pin/nope.gguf", `{"comment":"x"}`, ui); st != 404 {
		t.Fatalf("unknown model: %d %v", st, m)
	}
	if st, m := e.cdo("DELETE", "/api/models/pin/"+g1, `{"comment":"x"}`, ui); st != 404 {
		t.Fatalf("not pinned: %d %v", st, m)
	}

	st, m := e.cdo("PUT", "/api/models/pin/"+g1, `{"comment":"used daily"}`, ui)
	if st != 200 || m["pinned"] != true || m["changed"] != true || m["pinned_at"] == nil {
		t.Fatalf("pin: %d %v", st, m)
	}
	at := m["pinned_at"]
	if p, a := pinned(g1); p != true || a != at {
		t.Fatalf("after the pin: %v %v (want %v)", p, a, at)
	}
	// again: nothing changes, no second audit entry; the X-Comment header counts
	if st, m := e.cdo("PUT", "/api/models/pin/"+g1, ``, map[string]string{"X-Comment": "again"}); st != 200 || m["changed"] != false || m["pinned_at"] != at {
		t.Fatalf("pin again: %d %v", st, m)
	}
	// the icon route of the same model still answers as before
	if st, _, _ := e.hdo("PUT", "/api/models/"+g1, []byte("x"), nil); st != 404 {
		t.Fatalf("icon catch-all: %d", st)
	}

	// an observe-only key may not pin; a key with models may
	observer, _ := newKey(t, e, `{"name":"watcher"}`)
	if st, m := e.cdo("DELETE", "/api/models/pin/"+g1, `{"comment":"x"}`, bearer(observer)); st != 403 || errOf(m)["scope"] != "models" {
		t.Fatalf("observer: %d %v", st, m)
	}
	mover, _ := newKey(t, e, `{"name":"mover","permissions":{"models":"allow"}}`)
	if st, m := e.cdo("DELETE", "/api/models/pin/"+g1, `{"comment":"not any more"}`, bearer(mover)); st != 200 || m["pinned"] != false {
		t.Fatalf("unpin with models: %d %v", st, m)
	}
	if p, a := pinned(g1); p != false || a != nil {
		t.Fatalf("after the unpin: %v %v", p, a)
	}

	es := auditOf(t, e, "section=models")
	if len(es) != 2 {
		t.Fatalf("audit entries %d, want 2 (pin, unpin): %v", len(es), es)
	}
	if a := es[1]; a["action"] != "create" || a["target"] != "pin:"+g1 || a["comment"] != "used daily" || a["actor"] != "alex" {
		t.Errorf("the pin entry: %v", a)
	}
	if a := es[0]; a["action"] != "delete" || a["comment"] != "not any more" {
		t.Errorf("the unpin entry: %v", a)
	}
	if pins, _ := e.api.store.ModelPins(context.Background()); len(pins) != 0 {
		t.Errorf("stored pins: %v", pins)
	}
}
