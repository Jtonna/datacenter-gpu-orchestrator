package api

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// One set of zone counts for every reader: the Zones row of /api/health,
// the counts of GET /api/nodes (the Zones page heading) and each group's
// counts all come from zoneCountsOf, one availability.Classify of the
// status and the pool's facts. Each zone carries the state it was counted
// in as zone_state.

// ZoneStateRemoved is the zone_state of a removed zone (not counted).
const ZoneStateRemoved = "removed"

// zoneCountsOf classifies every zone once: the counts (removed zones left
// out), each zone's state by id, and the names of the zones that are down
// (lost or set aside), in the order Classify gives them.
func zoneCountsOf(st *scheduler.Status, facts []pool.NodeFacts) (ZoneCounts, map[string]string, []string) {
	var c ZoneCounts
	states := map[string]string{}
	down := []string{}
	removed := map[string]bool{}
	for _, f := range facts {
		if f.State == pool.StateRemoved {
			removed[f.ID] = true
		}
	}
	if st == nil {
		st = &scheduler.Status{}
	}
	for _, smp := range availability.Classify(st, facts) {
		if smp.Removed || removed[smp.Node] {
			states[smp.Node] = ZoneStateRemoved
			continue
		}
		states[smp.Node] = string(smp.State)
		c.add(smp.State)
		if smp.State == availability.Lost || smp.State == availability.SetAside {
			down = append(down, smp.Name)
		}
	}
	return c, states, down
}

// setZoneStates writes zone_state on each fact.
func setZoneStates(facts []pool.NodeFacts, states map[string]string) {
	for i := range facts {
		facts[i].ZoneState = states[facts[i].ID]
	}
}
