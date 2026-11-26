package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// catalogEntry is the /api/models entry of model file key.
func catalogEntry(t *testing.T, e *env, key string) map[string]any {
	t.Helper()
	for _, m := range getJSONBody(t, e, "/api/models")["models"].([]any) {
		if mm := m.(map[string]any); mm["key"] == key || mm["file"] == key {
			return mm
		}
	}
	t.Fatalf("no catalog entry %s", key)
	return nil
}

// A scoped instance counts only for itself: the status names its production
// profile and scope; the catalog lists it among the model's instances,
// marked, and the model's state is production's own (not loaded while only
// the scoped instance is); once production loads too, the state is ready.
func TestScopedVisibility_CatalogCountsProductionOnly(t *testing.T) {
	e := newConsolesEnv(t, chatOK)
	status(t, e.post("/api/load", `{"model":"alpha","scope":"e2","ctx":8192,"nodes":["con-2"]}`), http.StatusAccepted)
	eventually(t, "scoped ready", func() bool { in := instancesOf(t, e)["alpha@e2"]; return in != nil && in["state"] == "ready" })
	in := instancesOf(t, e)["alpha@e2"]
	if in["production"] != "alpha" || in["scope"] != "e2" {
		t.Fatalf("status instance %v", in)
	}
	m := catalogEntry(t, e, "a.gguf")
	if m["state"] != "not_loaded" {
		t.Fatalf("model state %v with only a scoped instance", m["state"])
	}
	insts := m["instances"].([]any)
	if len(insts) != 1 {
		t.Fatalf("instances %v", insts)
	}
	si := insts[0].(map[string]any)
	if si["profile"] != "alpha@e2" || si["scope"] != "e2" || si["production"] != "alpha" {
		t.Fatalf("catalog instance %v", si)
	}
	status(t, e.post("/api/load", `{"model":"alpha"}`), http.StatusAccepted)
	eventually(t, "production ready", func() bool { in := instancesOf(t, e)["alpha"]; return in != nil && in["state"] == "ready" })
	m = catalogEntry(t, e, "a.gguf")
	if m["state"] != "ready" || len(m["instances"].([]any)) != 2 {
		t.Fatalf("model state %v instances %v", m["state"], m["instances"])
	}
	for _, x := range m["instances"].([]any) {
		if xi := x.(map[string]any); xi["profile"] == "alpha" && (xi["scope"] != nil || xi["production"] != nil) {
			t.Fatalf("production instance marked scoped: %v", xi)
		}
	}
}

// /api/needs says a row came from a run-scoped load: a learned row measured
// by a scoped name has scopes [e2]; a peak_cycles row counts the scope of
// each unload event that carries one.
func TestScopedVisibility_NeedsRowsSayScoped(t *testing.T) {
	r := needRowJSON(scheduler.LearnedRow{MeasuredBy: "qwen3.5-4b@e2"})
	if len(r.Scopes) != 1 || r.Scopes[0] != "e2" {
		t.Fatalf("scoped row scopes %v", r.Scopes)
	}
	if r := needRowJSON(scheduler.LearnedRow{MeasuredBy: "qwen3.5-4b"}); r.Scopes == nil || len(r.Scopes) != 0 {
		t.Fatalf("production row scopes %v", r.Scopes)
	}
	st := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(st.Close)
	now := time.Now().UnixMilli()
	const s = `["llama","q.gguf",7936,1,"f16",false,[]]`
	ev := unloadEvent(now-2000, "qwen3.5-4b", s, true, map[string]*int{"a": mib(3071)}, "a")
	ev.Data["scope"] = "e2"
	st.Record(ev)
	const p = `["llama","q.gguf",4096,1,"f16",false,[]]`
	st.Record(unloadEvent(now-1000, "qwen3.5-4b", p, true, map[string]*int{"a": mib(2500)}, "a"))
	e := histEnv(t, st)
	m := peakView(t, e)
	row := peakRow(t, m, s, 1)
	if sc, _ := row["scopes"].([]any); len(sc) != 1 || sc[0] != "e2" {
		t.Fatalf("scoped shape row scopes %v (row %v)", row["scopes"], row)
	}
	if ps := row["profiles"].([]any); len(ps) != 1 || ps[0] != "qwen3.5-4b" {
		t.Fatalf("profiles %v", ps)
	}
	if sc, ok := peakRow(t, m, p, 1)["scopes"].([]any); !ok || len(sc) != 0 {
		t.Fatalf("production row scopes %v", sc)
	}
}
