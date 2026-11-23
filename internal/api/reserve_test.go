package api

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// planMemoryKeys and planMemoryNodeKeys are the memory section of a plan
// exactly as the server writes it (the UI reads only these; ui_reserve_test).
var (
	planMemoryKeys     = []string{"decides", "fits", "hypothetical", "nodes", "note", "placement_uses_reserved", "short_node", "shortfall_mib"}
	planMemoryNodeKeys = []string{"available_mib", "basis", "context_mib", "fill_reserve_mib", "free_avg_known", "free_avg_mib", "free_now_mib",
		"headroom_mib", "id", "margin_mib", "margin_rule", "node", "remains_mib", "reserved_by", "shared", "total_mib", "weights_mib", "will_take_mib"}
	nodeReserveKeys     = []string{"reserved", "reserved_mib", "measured_used_mib", "drift_mib", "drift_warning", "free_for_new_load_mib"}
	nodeReservationKeys = []string{"basis", "context_mib", "ctx", "fill_reserve_mib", "profile", "state", "total_mib", "weights_mib"}
)

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameKeys(t *testing.T, what string, m map[string]any, want []string) {
	t.Helper()
	got := keysOf(m)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if len(got) != len(w) {
		t.Fatalf("%s keys %v, want %v", what, got, w)
	}
	for i := range got {
		if got[i] != w[i] {
			t.Fatalf("%s keys %v, want %v", what, got, w)
		}
	}
}

// POST /api/plan carries the memory section; GET /api/profiles carries it in
// each plan; GET /api/system shows the settings.
func TestPlanMemorySection(t *testing.T) {
	e := newEnv(t, chatOK)
	p := jsonOf(t, body(t, e.post("/api/plan", `{"model":"alpha"}`)))
	m, ok := p["memory"].(map[string]any)
	if !ok {
		t.Fatalf("no memory section: %v", p)
	}
	sameKeys(t, "memory", m, planMemoryKeys)
	nodes := m["nodes"].([]any)
	if len(nodes) != 1 || m["fits"] != true || m["hypothetical"] != false {
		t.Fatalf("memory %v", m)
	}
	n := nodes[0].(map[string]any)
	sameKeys(t, "memory node", n, planMemoryNodeKeys)
	if n["weights_mib"].(float64)+n["context_mib"].(float64)+n["fill_reserve_mib"].(float64) != n["will_take_mib"].(float64) ||
		n["available_mib"].(float64)-n["will_take_mib"].(float64)-n["margin_mib"].(float64) != n["remains_mib"].(float64) {
		t.Fatalf("node does not add up: %v", n)
	}

	var prof struct {
		Profiles []map[string]any `json:"profiles"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/profiles"))), &prof)
	found := false
	for _, e := range prof.Profiles {
		if e["id"] == "alpha" {
			pm, ok := e["plan"].(map[string]any)["memory"].(map[string]any)
			found = ok && pm["fits"] == true
		}
	}
	if !found {
		t.Fatalf("no memory in the profiles' plan: %v", prof.Profiles)
	}

	sys := jsonOf(t, body(t, e.get("/api/system")))
	r, ok := sys["reserve"].(map[string]any)
	if !ok || r["fill_reserve_kib_per_token"] != 0.0 || r["placement_uses_reserved"] != false || r["reserved_drift_warn_mib"] != 500.0 ||
		r["free_average_window_s"] != 30.0 {
		t.Fatalf("system reserve %v", sys["reserve"])
	}
	if sys["read_panics"] != 0.0 {
		t.Fatalf("system read_panics %v, want 0", sys["read_panics"])
	}
}

// The agent facts (the agents of /api/status and the node feed) carry the
// reservation figures of the scheduler's node status; the feed carries the
// average free memory.
func TestNodeFeedReservation(t *testing.T) {
	e, p, _ := feedEnv(t)
	register(t, e, "02:00:00:00:00:01")
	feedHeartbeat(t, p, "02:00:00:00:00:01", "")
	f := readFeed(t, e)
	var n map[string]any
	json.Unmarshal(f.Nodes[0], &n)
	if n["mem_available_avg_mib"] != 9000.0 {
		t.Fatalf("mem_available_avg_mib %v", n["mem_available_avg_mib"])
	}

	used, drift := 9000, 1000
	st := &scheduler.Status{View: scheduler.ViewStatus{Nodes: []scheduler.NodeStatus{{ID: "02:00:00:00:00:01",
		Reserved:    []scheduler.NodeReservation{{Profile: "m", State: "ready", Basis: "measured", Ctx: 65536, WeightsMiB: 6000, ContextMiB: 2000, TotalMiB: 8000}},
		ReservedMiB: 8000, MeasuredUsedMiB: &used, DriftMiB: &drift, DriftWarning: "the node gave up more", FreeForNewLoadMiB: 3000}}}}
	facts := p.Facts()
	reservationFacts(facts, st)
	b, _ := json.Marshal(facts[0])
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range nodeReserveKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("facts lack %s: %s", k, b)
		}
	}
	r := m["reserved"].([]any)
	if len(r) != 1 || m["drift_mib"] != 1000.0 || m["free_for_new_load_mib"] != 3000.0 {
		t.Fatalf("facts %s", b)
	}
	sameKeys(t, "reservation", r[0].(map[string]any), nodeReservationKeys)
}
