package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// /api/profiles and /api/models carry the marking read only: stage (the
// effective one), stage_set, shown_name, and display_name when set. A
// profile write sets them (slice B2); one that leaves them out keeps them.
func TestProfileStageFieldsRead(t *testing.T) {
	e, svc, _ := cfgEnv(t)
	doc := svc.Document()
	spec := func(b string) *config.ProfileSpec {
		p, err := cfgsvc.ParseProfile([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	doc.Profiles["alpha"] = spec(`{"model":"a.gguf","ctx":4096,"needs_mib":1000,"stage":"production","display_name":"Alpha"}`)
	doc.Profiles["gamma"] = spec(`{"model":"g.gguf","ctx":4096,"needs_mib":500}`)
	doc.Profiles["bench-gamma-1"] = spec(`{"model":"g.gguf","ctx":4096,"needs_mib":500,"force_nodes":["host-1"]}`)
	if _, err := svc.Import(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t", Comment: "marks"}}, doc, true); err != nil {
		t.Fatal(err)
	}
	var pr struct {
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &pr); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, p := range pr.Profiles {
		got[p["id"].(string)] = p
	}
	want := map[string][4]any{
		"alpha":         {"production", true, "Alpha", "Alpha"},
		"gamma":         {"production", false, nil, "gamma"},
		"bench-gamma-1": {"test", false, nil, "TEST - bench-gamma-1"},
	}
	for id, w := range want {
		p := got[id]
		if p == nil || p["stage"] != w[0] || p["stage_set"] != w[1] || p["display_name"] != w[2] || p["shown_name"] != w[3] {
			t.Errorf("%s: %v %v %v %v, want %v", id, p["stage"], p["stage_set"], p["display_name"], p["shown_name"], w)
		}
	}
	// the catalog passes them on per configuration
	var cat struct {
		Models []struct {
			Profiles []map[string]any `json:"profiles"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/models"))), &cat); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range cat.Models {
		for _, p := range m.Profiles {
			if p["id"] == "alpha" && p["shown_name"] == "Alpha" && p["stage"] == "production" {
				seen++
			}
			if p["id"] == "bench-gamma-1" && p["shown_name"] == "TEST - bench-gamma-1" {
				seen++
			}
		}
	}
	if seen != 2 {
		t.Fatalf("catalog rows with the marks: %d", seen)
	}
	// writes (slice B2): a write that sets them writes them
	if st, m := e.cdo("PUT", "/api/config/profiles/alpha", `{"model":"a.gguf","ctx":4096,"needs_mib":1000,"stage":"test","display_name":"Alpha"}`, nil); st != 200 {
		t.Fatalf("set stage: %d %v", st, m)
	}
	if p := svc.Config().Profiles["alpha"]; p.Stage != "test" {
		t.Fatalf("stage not written: %+v", p)
	}
	if st, m := e.cdo("PUT", "/api/config/profiles/alpha", `{"model":"a.gguf","ctx":4096,"needs_mib":1000,"stage":"production","display_name":"Alpha"}`, nil); st != 200 {
		t.Fatalf("set stage back: %d %v", st, m)
	}
	if st, m := e.cdo("PUT", "/api/config/profiles/alpha", `{"model":"a.gguf","ctx":8192,"needs_mib":1000}`, nil); st != 200 {
		t.Fatalf("edit: %d %v", st, m)
	}
	if p := svc.Config().Profiles["alpha"]; p.Ctx != 8192 || p.DisplayName != "Alpha" || p.Stage != "production" {
		t.Fatalf("after the edit %+v", p)
	}
}

// POST /api/config/profiles/{id}/promote and /demote: a required comment,
// one revision audited as promote / demote, nothing written when nothing
// changes; a loaded instance is not made stale.
func TestPromoteDemoteRoutes(t *testing.T) {
	e, svc, cs := cfgEnv(t)
	doc := svc.Document()
	p, err := cfgsvc.ParseProfile([]byte(`{"model":"a.gguf","ctx":4096,"needs_mib":1000,"stage":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	doc.Profiles["alpha"] = p
	if _, err := svc.Import(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t", Comment: "start"}}, doc, true); err != nil {
		t.Fatal(err)
	}
	if st, m := e.cdo("POST", "/api/config/profiles/alpha/promote", `{}`, nil); st != 400 || !strings.Contains(fmt.Sprint(m), "comment is required") {
		t.Fatalf("no comment: %d %v", st, m)
	}
	// loaded, then promoted
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	rev := svc.Revision()
	st, m := e.cdo("POST", "/api/config/profiles/alpha/promote", `{"comment":"ready"}`, nil)
	if st != 200 || m["changed"] != true || m["revision"] != float64(rev+1) || m["stage"] != "production" {
		t.Fatalf("promote: %d %v", st, m)
	}
	es, _, err := cs.Query(context.Background(), cfgstore.Filter{Section: "profiles", Target: "alpha", Action: audit.ActionPromote})
	if err != nil || len(es) != 1 || es[0].Comment != "ready" {
		t.Fatalf("audit %+v %v", es, err)
	}
	stat, err := e.sched.Status(context.Background())
	if err != nil || len(stat.Instances) != 1 || stat.Instances[0].StaleProfile {
		t.Fatalf("after the promote: %+v %v", stat.Instances, err)
	}
	// nothing changes: no revision, no entry
	if st, m := e.cdo("POST", "/api/config/profiles/alpha/promote", `{"comment":"again"}`, nil); st != 200 || m["changed"] != false || svc.Revision() != rev+1 {
		t.Fatalf("second promote: %d %v", st, m)
	}
	if st, m := e.cdo("POST", "/api/config/profiles/alpha/demote", `{"comment":"back"}`, nil); st != 200 || m["changed"] != true || m["stage"] != "test" {
		t.Fatalf("demote: %d %v", st, m)
	}
	if st, _ := e.cdo("POST", "/api/config/profiles/nope/demote", `{"comment":"x"}`, nil); st != 404 {
		t.Fatalf("unknown id: %d", st)
	}
	if st, _ := e.cdo("POST", "/api/config/profiles/alpha/frobnicate", `{"comment":"x"}`, nil); st != 404 {
		t.Fatalf("unknown action: %d", st)
	}
}
