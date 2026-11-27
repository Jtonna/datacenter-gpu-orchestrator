package api

import (
	"testing"
)

// A forced profile names its zones by their NAMES, never by a hardware
// address (the reviewer, build-92): the plan's own name for the node first,
// then the zones list, then the id, which is shown only for a zone with no
// name (core/status.js zoneLabel, the profile editor's rule of build-89).
// The model page's Configurations table (Placement) and the profile panel's
// "Forced on" line both use it.
const (
	znNamedByPlan = "02:00:00:00:0a:01" // its plan's memory row names it zone-01
	znNamedByList = "02:00:00:00:0a:02" // the zones list names it zone-02
	znNoName      = "02:00:00:00:0a:09" // named nowhere
)

// znWithZone02 -> a copy of the status whose zones list names znNamedByList
func znWithZone02(t *testing.T, st any) map[string]any {
	s := mcpCopy(t, st)
	view, ok := s["view"].(map[string]any)
	if !ok {
		view = map[string]any{}
		s["view"] = view
	}
	nodes, _ := view["nodes"].([]any)
	view["nodes"] = append(nodes, map[string]any{"id": znNamedByList, "hostname": "zone-02", "kind": "agent"})
	return s
}

// znForced(p, ids...) -> p forced on ids, with a plan whose memory rows name znNamedByPlan only
func znForced(p map[string]any, ids ...string) {
	force := []any{}
	rows := []any{}
	for _, id := range ids {
		force = append(force, id)
		if id == znNamedByPlan {
			rows = append(rows, map[string]any{"id": id, "node": "zone-01", "will_take_mib": 4000})
		} else {
			rows = append(rows, map[string]any{"id": id, "will_take_mib": 4000})
		}
	}
	p["force_nodes"], p["placement"] = force, "forced"
	p["plan"] = map[string]any{"action": "start", "would_start": true, "nodes": []any{}, "reason": "", "memory": map[string]any{"nodes": rows}}
}

func TestUINextForcedZonesByName(t *testing.T) {
	// the model's page: the Configurations table's Placement column
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := znWithZone02(t, fetch["/api/status"])
	fetch["/api/status"] = st
	var entry map[string]any
	for _, x := range mcpCopy(t, fetch["/api/models"])["models"].([]any) {
		if m := x.(map[string]any); m["key"] == mdlRecent {
			entry = m
		}
	}
	if entry == nil {
		t.Fatal("no catalog entry for " + mdlRecent)
	}
	profs := entry["profiles"].([]any)
	if len(profs) == 0 {
		t.Fatal("the entry has no profile")
	}
	named := profs[0].(map[string]any)
	znForced(named, znNamedByPlan, znNamedByList)
	noname := mcpCopy(t, named)
	noname["id"] = "zone-test-noname"
	znForced(noname, znNoName)
	entry["profiles"] = []any{named, noname}
	fetch["/api/models/"+mdlRecent] = entry
	td := "td span.node"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "model page: Placement names the forced zones", "route": "#/models/" + mdlRecent, "status": st,
			"expect": []string{"zone-01", "zone-02"}, "absent": []string{znNamedByPlan, znNamedByList, "70:66:2a"},
			"has":  []string{td + "[data-zone='" + znNamedByPlan + "'][title='" + znNamedByPlan + "']", td + "[data-zone='" + znNamedByList + "']"},
			"none": []string{td + "[data-noname][data-zone='" + znNamedByPlan + "']", td + "[data-noname][data-zone='" + znNamedByList + "']"}},
		map[string]any{"name": "model page: a zone with no name shows its id", "route": "#/models/" + mdlRecent, "status": st,
			"expect": []string{znNoName}, "has": []string{td + ".mono[data-noname][data-zone='" + znNoName + "']"}},
	}})

	// the profile panel (Profiles and Test profiles): the "Forced on" line
	pf := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles/test", "/api/profiles", "/api/models", "/api/status")
	pst := znWithZone02(t, pf["/api/status"])
	pf["/api/status"] = pst
	const prod = "gemma-4-27b"
	var rows []any
	for _, x := range jlist(jmap(pf["/api/profiles"])["profiles"]) {
		if jstr(jmap(x)["id"]) == prod {
			znForced(jmap(x), znNamedByPlan, znNamedByList, znNoName)
		}
		rows = append(rows, x)
	}
	if len(rows) == 0 {
		t.Fatal("no profiles")
	}
	pf["/api/config/profiles/"+prod] = map[string]any{"revision": 412, "id": prod, "profile": map[string]any{"kind": "llama", "model": "gemma-4-27b-it-Q4_K_M.gguf", "ctx": 32768}}
	side := "aside.side section.prf-part span.node"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": pf, "cases": []any{
		map[string]any{"name": "profile panel: Forced on names the zones, the id only for one with no name", "route": "#/profiles?profile=" + prod, "status": pst,
			"page":      []string{"Forced on 3 zones", "zone-01", "zone-02", znNoName},
			"notOnPage": []string{znNamedByPlan, znNamedByList},
			"pageHas":   []string{side + "[data-zone='" + znNamedByPlan + "']", side + "[data-zone='" + znNamedByList + "']", side + ".mono[data-noname][data-zone='" + znNoName + "']"},
			"pageNone":  []string{side + "[data-noname][data-zone='" + znNamedByPlan + "']"}},
	}})
}
