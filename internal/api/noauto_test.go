package api

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// CR-9 review, C: GET and PUT /api/config/no-auto-nodes, in the style of
// exclude-nodes; the list is in GET /api/config and in the audit.
func TestConfigNoAutoNodes(t *testing.T) {
	e, _, _ := cfgEnv(t)
	st, m := e.cdo("GET", "/api/config", "", nil)
	if l, ok := m["no_auto_nodes"].([]any); st != 200 || !ok || len(l) != 0 {
		t.Fatalf("GET /api/config: %d no_auto_nodes %v", st, m["no_auto_nodes"])
	}
	if st, mm := e.cdo("PUT", "/api/config/no-auto-nodes", `{"no_auto_nodes":["laptop-*"],"comment":"keep the laptop for forced loads"}`, nil); st != 200 {
		t.Fatalf("PUT: %d %v", st, mm)
	}
	st, m = e.cdo("GET", "/api/config/no-auto-nodes", "", nil)
	if l, _ := m["no_auto_nodes"].([]any); st != 200 || len(l) != 1 || l[0] != "laptop-*" {
		t.Fatalf("GET: %d %v", st, m)
	}
	if st, mm := e.cdo("PUT", "/api/config/no-auto-nodes", `{"no_auto_nodes":["ps5-["]}`, nil); st == 200 {
		t.Fatalf("a bad glob was accepted: %v", mm)
	}
	st, m = e.cdo("GET", "/api/audit?section=no_auto_nodes", "", nil)
	if es, _ := m["entries"].([]any); st != 200 || len(es) != 1 {
		t.Fatalf("audit: %d %v", st, m)
	}
}

// CR-9 review, C: a view node carries "auto" (false for a node in
// no_auto_nodes), Machine details shows "Named only" for it, and the tiers
// page edits the list through PUT /api/config/no-auto-nodes.
func TestNoAutoNodesInStatusAndUI(t *testing.T) {
	m, _ := jsonView(scheduler.NodeStatus{ID: "a"}).(map[string]any)
	if v, ok := m["auto"]; !ok || v != false {
		t.Fatalf("view node: auto = %v (present %v)", v, ok)
	}
	if src := uiText(t, "views/nodes.js"); !strings.Contains(src, "n.auto === false") || !strings.Contains(src, "'Named only'") {
		t.Error("views/nodes.js does not show a Named only badge for auto false")
	}
	if src := uiText(t, uiNextDir+"views/tiers.js"); !strings.Contains(src, "'/api/config/no-auto-nodes'") || !strings.Contains(src, "no_auto_nodes: draft.noauto") {
		t.Error("next/views/tiers.js does not write no_auto_nodes")
	}
}
