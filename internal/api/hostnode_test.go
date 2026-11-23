package api

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// GET /api/profiles shows a host: node profile's host node (by name), the
// overhead hosting adds there, and why it cannot host now.
func TestProfilesHostEntry(t *testing.T) {
	e := map[string]any{}
	hostEntry(e, scheduler.PlanReport{Host: core.HostNode, HostNode: "id-a", HostOverheadMiB: 1024,
		HostProblem: "host_not_capable: node console-a cannot host: its agent does not advertise host_launch"},
		map[string]string{"id-a": "console-a"})
	if e["host"] != "node" || e["host_node"] != "console-a" || e["host_overhead_mib"] != 1024 || e["host_problem"] == nil {
		t.Fatalf("%v", e)
	}
	e = map[string]any{}
	hostEntry(e, scheduler.PlanReport{Host: core.HostServer}, nil)
	if e["host"] != "server" || len(e) != 1 {
		t.Fatalf("%v", e)
	}
}
