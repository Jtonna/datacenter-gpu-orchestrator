package api

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The memory reservation rule in the API (scheduler/reserve.go). The plan's
// memory section comes with the plan itself (POST /api/plan, and the plan of
// each entry of GET /api/profiles); this file adds the node side to the agent
// facts (the agents of /api/status and the node feed GET /api/nodes, built by
// the one function agentFacts) and the settings to GET /api/system.

// reservationFacts copies each node's reservation figures from the
// scheduler's node status into its facts. Nothing is added when st is nil
// or does not list the node.
func reservationFacts(facts []pool.NodeFacts, st *scheduler.Status) {
	if st == nil {
		return
	}
	byID := map[string]scheduler.NodeStatus{}
	for _, n := range st.View.Nodes {
		byID[string(n.ID)] = n
	}
	for i := range facts {
		n, ok := byID[facts[i].ID]
		if !ok {
			continue
		}
		f := &facts[i]
		f.Reserved = jsonView(n.Reserved)
		reserved, free := n.ReservedMiB, n.FreeForNewLoadMiB
		f.ReservedMiB, f.FreeForNewLoadMiB = &reserved, &free
		f.MeasuredUsedMiB, f.DriftMiB, f.DriftWarning = n.MeasuredUsedMiB, n.DriftMiB, n.DriftWarning
	}
}

// reserveSettings is the reservation rule's settings as GET /api/system
// shows them, read from the configuration in force (they change at run time).
func reserveSettings(c *config.Config) map[string]any {
	d := c.Defaults
	return map[string]any{
		"fill_reserve_kib_per_token": d.FillReserveKiBPerToken, "placement_uses_reserved": d.PlacementUsesReserved,
		"reserved_drift_warn_mib": d.ReservedDriftWarnMiB, "margin_measured_mib": d.MarginMeasuredMiB,
		"margin_estimated_percent": d.MarginEstimatedPercent, "margin_estimated_min_mib": d.MarginEstimatedMinMiB,
		"free_average_window_s": d.FreeAverageWindow.Seconds(),
	}
}
