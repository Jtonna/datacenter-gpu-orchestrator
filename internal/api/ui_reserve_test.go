package api

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// statusNodeReserveKeys are the reservation keys of a view node in the
// status document; planReservedByKeys one reserved_by entry of a plan's
// memory node.
var (
	statusNodeReserveKeys = []string{"reserved", "reserved_mib", "measured_used_mib", "drift_mib", "drift_warning", "free_for_new_load_mib", "free_avg_mib"}
	planReservedByKeys    = []string{"profile", "mib", "state"}
)

// The key lists match what the server writes: a fully filled node status and
// reserved_by entry, through the same jsonView as the status and the plan.
func TestUIReserveKeyListsMatchTheServer(t *testing.T) {
	used, drift, avg := 1, 2, 3
	ns := jsonView(scheduler.NodeStatus{ID: "n1", Reserved: []scheduler.NodeReservation{{Profile: "m", State: "ready", Basis: "measured", Ctx: 1, WeightsMiB: 1, ContextMiB: 1, FillReserveMiB: 1, TotalMiB: 3}},
		ReservedMiB: 3, MeasuredUsedMiB: &used, DriftMiB: &drift, DriftWarning: "w", FreeForNewLoadMiB: 4, FreeAvgMiB: &avg}).(map[string]any)
	for _, k := range statusNodeReserveKeys {
		if _, ok := ns[k]; !ok {
			t.Errorf("a view node has no %s: %v", k, ns)
		}
	}
	sameKeys(t, "node reservation", ns["reserved"].([]any)[0].(map[string]any), nodeReservationKeys)
	sameKeys(t, "reserved_by", jsonView(scheduler.PlanReservedBy{Profile: "m", MiB: 1, State: "ready"}).(map[string]any), planReservedByKeys)

	// and the live status carries them on a real node
	e, _, _ := agentsEnv(t)
	register(t, e, "02:00:00:00:00:01")
	st := jsonOf(t, body(t, e.get("/api/status")))
	nodes, _ := st["view"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("status view nodes %v", st["view"])
	}
	n := nodes[0].(map[string]any)
	for _, k := range []string{"reserved", "reserved_mib", "free_for_new_load_mib"} {
		if _, ok := n[k]; !ok {
			t.Errorf("the status view node has no %s: %v", k, n)
		}
	}
}

// core/reserve.js reads server objects only through nd, res, mem, row and
// by, and every key it reads that way is one the server writes.
func TestUIReserveReadsOnlyServerKeys(t *testing.T) {
	src := uiText(t, "core/reserve.js")
	allowed := map[string][]string{
		"nd":  statusNodeReserveKeys,
		"res": nodeReservationKeys,
		"mem": planMemoryKeys,
		"row": append(slices.Clone(planMemoryNodeKeys), planMemoryNodeOptionalKeys...),
		"by":  planReservedByKeys,
	}
	// leave out the header comment, which names the keys in prose
	code := src[strings.Index(src, "\nimport "):]
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\b(nd|res|mem|row|by)\.([A-Za-z_]+)`).FindAllStringSubmatch(code, -1) {
		seen[m[1]] = true
		if !slices.Contains(allowed[m[1]], m[2]) {
			t.Errorf("core/reserve.js reads %s.%s, which the server does not write", m[1], m[2])
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Errorf("core/reserve.js reads nothing through %s: the test no longer covers it", name)
		}
	}
	// the two views use it
	if v := uiText(t, "views/nodes.js"); !strings.Contains(v, "reservedPart(n)") {
		t.Error("views/nodes.js does not show the reserved list")
	}
	if v := uiText(t, "views/placement.js"); !strings.Contains(v, "memoryTable(plan.memory") {
		t.Error("views/placement.js does not show the plan's memory table")
	}
}
