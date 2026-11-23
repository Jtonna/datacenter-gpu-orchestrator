package api

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The additive status facts (zonefacts.go) apply to some nodes and
// instances only; an idle fleet's /api/status and /api/nodes bytes carry
// none of them, not even as "", 0 or null.
func TestView_IdleFleetSaysNoneOfTheAdditiveFacts(t *testing.T) {
	absent := []string{`"host"`, `"host_node"`, `"parallel"`, `"role"`, `"share"`,
		`"reason"`, `"since"`, `"quarantine_reason"`, `"free_at"`, `"free_basis"`,
		`"member"`, `"set_aside"`, `"paused_at"`, `"paused_actor"`, `"excluded_at"`, `"excluded_actor"`,
		`"free_in_s"`, `"next"`}
	check := func(name, raw string) {
		t.Helper()
		for _, k := range absent {
			if strings.Contains(raw, k+":") {
				t.Errorf("%s says %s on an idle fleet: %s", name, k, raw)
			}
		}
	}
	e := newEnv(t, chatOK)
	check("/api/status (fleet)", body(t, e.get("/api/status")))

	z, _, _ := zoneEnv(t)
	register(t, z, "02:00:00:00:00:01")
	for _, p := range []string{"/api/status", "/api/nodes"} {
		r := z.get(p)
		raw := body(t, r)
		if r.StatusCode != 200 || !strings.Contains(raw, "02:00:00:00:00:01") {
			t.Fatalf("%s: %d %s", p, r.StatusCode, raw)
		}
		check(p+" (agents)", raw)
	}
}

// A fact that applies is said, a zero one is left out; untagged zero
// fields are said as before.
func TestView_OmitemptyTagLeavesOnlyZeroTaggedFieldsOut(t *testing.T) {
	got := jsonView(scheduler.PlacedNodeStatus{ID: "n1", Role: "helper", Share: 0.25}).(map[string]any)
	if got["role"] != "helper" || got["share"] != 0.25 || got["addr"] != "" {
		t.Fatalf("%v", got)
	}
	got = jsonView(scheduler.PlacedNodeStatus{ID: "n1"}).(map[string]any)
	if _, ok := got["role"]; ok {
		t.Fatalf("%v", got)
	}
	if _, ok := got["share"]; ok {
		t.Fatalf("%v", got)
	}
	if _, ok := got["local"]; !ok {
		t.Fatalf("an untagged zero field was left out: %v", got)
	}
}
