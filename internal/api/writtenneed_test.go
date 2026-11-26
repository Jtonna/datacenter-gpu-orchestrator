package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// A need that is written in the profile (needs_mib) is shown whatever the
// plan says (the reviewer's ruling, 26 Nov): live's three "program on this
// machine" rows showed a dash because their plans reject (the local machine
// is excluded by config) and a rejected plan carried no need. Now the plan,
// GET /api/profiles and GET /api/models carry the written need with its
// basis, the plan's reject reason beside it; a dash stays only for a need
// nobody knows.
func TestWrittenNeedWhenThePlanRejects(t *testing.T) {
	e, _, _ := cfgEnv(t)
	for _, b := range []string{
		`{"id":"image-gen","kind":"command","cmd":"run ${PORT}","needs_mib":5427}`,
		`{"id":"tied","model":"tied.gguf","ctx":2048,"needs_mib":3100,"force_nodes":["host-1"]}`,
	} {
		if st, m := e.cdo("POST", "/api/config/profiles", b, nil); st != 200 && st != 201 {
			t.Fatalf("%s: %d %v", b, st, m)
		}
	}
	if st, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":["host-1"],"comment":"keep the local machine out"}`, nil); st != 200 {
		t.Fatalf("exclude: %d %v", st, m)
	}
	want := map[string]float64{"image-gen": 5427, "tied": 3100}

	// POST /api/plan
	for id, mib := range want {
		_, pl := e.cdo("POST", "/api/plan", `{"model":"`+id+`"}`, nil)
		if pl["action"] != "reject" || !strings.Contains(toJSON(pl), "excluded by config") {
			t.Fatalf("%s: plan %v, want a reject for the exclusion", id, pl)
		}
		if got, _ := pl["need_mib"].(float64); got != mib {
			t.Fatalf("%s: plan need_mib %v, want the written %v (plan %v)", id, pl["need_mib"], mib, pl)
		}
	}

	// GET /api/profiles
	_, pm := e.cdo("GET", "/api/profiles", "", nil)
	seen := 0
	for _, x := range pm["profiles"].([]any) {
		p := x.(map[string]any)
		id, _ := p["id"].(string)
		plan, _ := p["plan"].(map[string]any)
		if mib, ok := want[id]; ok {
			seen++
			if plan["action"] != "reject" {
				t.Fatalf("%s: plan %v", id, plan)
			}
			if p["needs_mib"] != mib || p["needs_written"] != true || p["needs_basis"] != "measured, needs_mib" {
				t.Fatalf("%s: needs_mib %v needs_written %v needs_basis %v, want %v written by hand", id, p["needs_mib"], p["needs_written"], p["needs_basis"], mib)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("%d of the two profiles listed", seen)
	}

	// GET /api/models: the catalog's profile entries
	var cat struct {
		Models []struct {
			Key      string           `json:"key"`
			Profiles []map[string]any `json:"profiles"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/models"))), &cat); err != nil {
		t.Fatal(err)
	}
	seen = 0
	for _, m := range cat.Models {
		for _, p := range m.Profiles {
			id, _ := p["id"].(string)
			mib, ok := want[id]
			if !ok {
				continue
			}
			seen++
			plan, _ := p["plan"].(map[string]any)
			if plan["action"] != "reject" || !strings.Contains(toJSON(plan["reason"]), "excluded by config") {
				t.Fatalf("%s in %s: plan %v", id, m.Key, plan)
			}
			if p["needs_mib"] != mib || p["needs_written"] != true {
				t.Fatalf("%s in %s: needs_mib %v needs_written %v, want %v written by hand", id, m.Key, p["needs_mib"], p["needs_written"], mib)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("%d of the two profiles in the catalog", seen)
	}
}
