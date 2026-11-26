package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
)

// GET /api/profiles/test: the test profiles only, each with the context it
// really runs at (a bench profile written above the trained context shows
// the trained one), the reason it is test, and its counts by model and
// reason. A promote takes it off the list.
func TestTestProfilesRoute(t *testing.T) {
	e, svc, _ := ssEnv(t)
	doc := svc.Document()
	add := func(id, b string) {
		p, err := cfgsvc.ParseProfile([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		doc.Profiles[id] = p
	}
	tiny := "tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf"
	add("bench-tinyllama-auto-16k", `{"model":"`+tiny+`","ctx":16384,"generated":true}`)
	add("pinned", `{"model":"nometa.gguf","ctx":4096,"force_nodes":["con-2"]}`)
	add("marked", `{"model":"nometa.gguf","ctx":4096,"stage":"test","display_name":"Trial"}`)
	if _, err := svc.Import(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t", Comment: "x"}}, doc, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Profiles        []map[string]any `json:"profiles"`
		Count           int              `json:"count"`
		ProductionCount int              `json:"production_count"`
		Models          map[string]int   `json:"models"`
		Reasons         map[string]int   `json:"reasons"`
	}
	read := func() {
		if err := json.Unmarshal([]byte(body(t, e.get("/api/profiles/test"))), &got); err != nil {
			t.Fatal(err)
		}
	}
	read()
	rows := map[string]map[string]any{}
	for _, p := range got.Profiles {
		rows[p["id"].(string)] = p
	}
	if got.Count != 3 || got.ProductionCount != 2 || len(rows) != 3 {
		t.Fatalf("%+v", got)
	}
	b := rows["bench-tinyllama-auto-16k"]
	if b["ctx"] != float64(16384) || b["ctx_real"] != float64(2048) || b["ctx_note"] == nil || b["reason"] != "generated" || b["shown_name"] != "TEST - bench-tinyllama-auto-16k" {
		t.Fatalf("bench row %v", b)
	}
	if r := rows["pinned"]; r["reason"] != "forced zones" || r["ctx_real"] != float64(4096) {
		t.Fatalf("pinned %v", r)
	}
	if r := rows["marked"]; r["reason"] != "marked" || r["shown_name"] != "TEST - Trial" {
		t.Fatalf("marked %v", r)
	}
	if !reflect.DeepEqual(got.Reasons, map[string]int{"generated": 1, "forced zones": 1, "marked": 1}) || got.Models["nometa.gguf"] != 2 {
		t.Fatalf("counts %v %v", got.Reasons, got.Models)
	}
	if st, m := e.cdo("POST", "/api/config/profiles/pinned/promote", `{"comment":"in use"}`, nil); st != 200 {
		t.Fatalf("promote %d %v", st, m)
	}
	read()
	if got.Count != 2 || got.ProductionCount != 3 {
		t.Fatalf("after the promote %+v", got)
	}
}
