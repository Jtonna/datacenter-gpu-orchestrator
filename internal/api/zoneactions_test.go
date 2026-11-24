package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The confirm dialog's figures: per zone what happens, the models that
// unload with every zone they leave, and the runs cut, each counted once.
func TestZoneActions_ForceUnloadPlan(t *testing.T) {
	facts := []pool.NodeFacts{
		{ID: "a", Name: "ps5-01", State: pool.StateActive}, {ID: "b", Name: "ps5-02", State: pool.StateActive},
		{ID: "c", Name: "ps5-03", State: pool.StateActive}, {ID: "d", Name: "ps5-04", State: pool.StateActive},
	}
	st := &scheduler.Status{View: scheduler.ViewStatus{Nodes: []scheduler.NodeStatus{
		{ID: "a", Hostname: "ps5-01", Online: true, Holder: "gemma"}, {ID: "b", Hostname: "ps5-02", Online: true, Holder: "gemma"},
		{ID: "c", Hostname: "ps5-03", Online: true, Holder: "qwen"}, {ID: "d", Hostname: "ps5-04", Online: true},
	}}, Instances: []scheduler.InstanceStatus{
		{Profile: "gemma", State: "ready", InFlight: 2, HostNode: "a", Nodes: []scheduler.PlacedNodeStatus{{ID: "a"}, {ID: "b"}}},
		{Profile: "qwen", State: "starting", Nodes: []scheduler.PlacedNodeStatus{{ID: "c"}}},
	}}
	p := planZoneAction(ZoneActionForceUnload, []string{"a", "c", "d"}, st, indexZones(st, facts))
	if p.RunsCut != 2 || len(p.Models) != 2 || len(p.Zones) != 3 {
		t.Fatalf("%+v", p)
	}
	g := p.Models[0]
	if g.Profile != "gemma" || g.Host != "ps5-01" || strings.Join(g.Zones, ",") != "ps5-01,ps5-02" || strings.Join(g.Outside, ",") != "ps5-02" || g.InFlight != 2 {
		t.Fatalf("gemma %+v", g)
	}
	if z := p.Zones[0]; z.Effect != "gemma unloads; 2 runs fail" || z.RunsCut != 2 || !z.Changes {
		t.Fatalf("a %+v", z)
	}
	if z := p.Zones[1]; z.Effect != "the load of qwen is cancelled" {
		t.Fatalf("c %+v", z)
	}
	if z := p.Zones[2]; z.Changes || z.Effect != "nothing loaded: nothing interrupted" {
		t.Fatalf("d %+v", z)
	}
	if p.Summary != "2 models unload; 2 runs fail straight away. They also leave ps5-02." {
		t.Fatalf("summary %q", p.Summary)
	}
	// Selecting both members counts the model's runs once.
	if p := planZoneAction(ZoneActionForceUnload, []string{"a", "b"}, st, indexZones(st, facts)); p.RunsCut != 2 || len(p.Models[0].Outside) != 0 {
		t.Fatalf("both members %+v", p)
	}
}

// Pause and unpause over a selection: a dry run changes nothing; the real
// one pauses each zone with its own audit entry.
func TestZoneActions_Route(t *testing.T) {
	e, p, au := groupsEnv(t)
	ids := `["02:00:00:00:00:01","02:00:00:00:00:02"]`
	var plan ZoneActionPlan
	json.Unmarshal([]byte(status(t, e.post("/api/zone-actions/pause?dry_run=1", `{"nodes":`+ids+`}`), http.StatusOK)), &plan)
	if !plan.DryRun || len(plan.Zones) != 2 || !plan.Zones[0].Changes || plan.Zones[0].Effect != "takes no new model" ||
		plan.Summary != "2 of 2 zones stop taking new models; nothing running is interrupted." {
		t.Fatalf("dry run %+v", plan)
	}
	for _, f := range p.Facts() {
		if f.Paused {
			t.Fatal("a dry run paused a zone")
		}
	}
	status(t, e.post("/api/zone-actions/pause", `{"nodes":`+ids+`}`), http.StatusBadRequest)           // no comment
	status(t, e.post("/api/zone-actions/pause", `{"nodes":["02:00:00:00:00:09"],"comment":"x"}`), 400) // unknown zone
	status(t, e.post("/api/zone-actions/reboot", `{"nodes":`+ids+`,"comment":"x"}`), http.StatusNotFound)
	status(t, e.post("/api/zone-actions/pause", `{"nodes":`+ids+`,"comment":"rack work"}`), http.StatusOK)
	n := 0
	for _, f := range p.Facts() {
		if f.Paused {
			n++
		}
	}
	au.mu.Lock()
	entries := 0
	for _, en := range au.es {
		if en.Section == SectionNodes && en.Action == "pause" && en.Comment == "rack work" {
			entries++
		}
	}
	au.mu.Unlock()
	if n != 2 || entries != 2 {
		t.Fatalf("paused %d, audit entries %d", n, entries)
	}
	json.Unmarshal([]byte(status(t, e.post("/api/zone-actions/unpause", `{"nodes":["02:00:00:00:00:01","02:00:00:00:00:03"],"dry_run":true}`), 200)), &plan)
	if plan.Zones[0].Effect != "takes models again" || plan.Zones[1].Changes || plan.Summary != "1 of 2 zones take models again." {
		t.Fatalf("unpause dry run %+v", plan)
	}
	json.Unmarshal([]byte(status(t, e.post("/api/zone-actions/force-unload?dry_run=1", `{"nodes":`+ids+`}`), 200)), &plan)
	if plan.RunsCut != 0 || plan.Summary != "Nothing is loaded on the 2 zones: nothing is interrupted." {
		t.Fatalf("force unload dry run %+v", plan)
	}
}

// Drain: the dialog's "Wait for the 2 runs, then unload · About 40 s".
func TestZoneActions_DrainPlan(t *testing.T) {
	facts := []pool.NodeFacts{{ID: "a", Name: "ps5-01", State: pool.StateActive}, {ID: "b", Name: "ps5-02", State: pool.StateActive},
		{ID: "c", Name: "ps5-03", State: pool.StateActive}}
	eta := 40.0
	st := &scheduler.Status{Instances: []scheduler.InstanceStatus{
		{Profile: "gemma", State: "ready", InFlight: 2, RunsEtaS: &eta, Nodes: []scheduler.PlacedNodeStatus{{ID: "a"}, {ID: "b"}}},
		{Profile: "qwen", State: "ready", Nodes: []scheduler.PlacedNodeStatus{{ID: "c"}}},
	}}
	p := planZoneAction(ZoneActionDrain, []string{"a", "c"}, st, indexZones(st, facts))
	if p.RunsCut != 0 || p.RunsWait != 2 || p.EtaS == nil || *p.EtaS != 40 || len(p.Models) != 2 {
		t.Fatalf("%+v", p)
	}
	if z := p.Zones[0]; z.Effect != "gemma unloads after its 2 runs (about 40 s); new runs queue until it loads again" || z.RunsCut != 0 {
		t.Fatalf("a %+v", z)
	}
	if z := p.Zones[1]; z.Effect != "qwen unloads now; nothing in flight" {
		t.Fatalf("c %+v", z)
	}
	if p.Summary != "2 models unload once their 2 runs finish (about 40 s); nothing is cut and new runs queue until they load again. They also leave ps5-02. The next load may start on another zone." {
		t.Fatalf("summary %q", p.Summary)
	}
	if p.NextLoad != NextLoadMayMove {
		t.Fatalf("next_load %q", p.NextLoad)
	}
	if fp := planZoneAction(ZoneActionPause, []string{"a"}, st, indexZones(st, facts)); fp.NextLoad != "" {
		t.Fatalf("pause plan says next_load %q", fp.NextLoad)
	}
}

// A selection may name zones by id or by name (any case), as /api/load
// does; a name two zones share is refused with a sentence that says so;
// an unknown one is still 400. The plan and the real call act on the ids.
func TestZoneActions_Names(t *testing.T) {
	e, p, _ := groupsEnv(t) // zone-1, zone-2, zone-3 on 02:00:00:00:00:01..03
	var plan ZoneActionPlan
	json.Unmarshal([]byte(status(t, e.post("/api/zone-actions/pause", `{"nodes":["zone-1","02:00:00:00:00:02"],"dry_run":true}`), http.StatusOK)), &plan)
	if len(plan.Zones) != 2 || plan.Zones[0].ID != "02:00:00:00:00:01" || plan.Zones[0].Name != "zone-1" || plan.Zones[1].ID != "02:00:00:00:00:02" {
		t.Fatalf("%+v", plan)
	}
	// the same zone by name and by id counts once
	json.Unmarshal([]byte(status(t, e.post("/api/zone-actions/pause", `{"nodes":["ZONE-1","02:00:00:00:00:01"],"dry_run":true}`), http.StatusOK)), &plan)
	if len(plan.Zones) != 1 {
		t.Fatalf("twice %+v", plan)
	}
	if got := status(t, e.post("/api/zone-actions/pause", `{"nodes":["no-such"],"dry_run":true}`), http.StatusBadRequest); !strings.Contains(got, "no-such is not a zone Potluck knows") {
		t.Fatal(got)
	}
	registerNamed(t, e, "02:00:00:00:00:04", "zone-1") // a second zone with that name
	if got := status(t, e.post("/api/zone-actions/pause", `{"nodes":["zone-1"],"dry_run":true}`), http.StatusBadRequest); !strings.Contains(got, "zone-1 is the name of 2 zones (02:00:00:00:00:01, 02:00:00:00:00:04): name the zone by its id") {
		t.Fatal(got)
	}
	status(t, e.post("/api/zone-actions/pause", `{"nodes":["zone-2","zone-3"],"comment":"rack work"}`), http.StatusOK)
	paused := map[string]bool{}
	for _, f := range p.Facts() {
		paused[f.ID] = f.Paused
	}
	if !paused["02:00:00:00:00:02"] || !paused["02:00:00:00:00:03"] || paused["02:00:00:00:00:01"] {
		t.Fatalf("paused %v", paused)
	}
}
