package scheduler

import (
	"fmt"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// freedHere is the MiB a profile in freed holds on node id that the row
// counts as freed (memoryRows), and that profile; 0 when none.
func freedHere(c *schedCore, id core.NodeID, freed map[string]bool) freedOn {
	in, held, ok := c.reservedOn(id)
	if !ok || !freed[in.prof.Name] {
		return freedOn{}
	}
	return freedOn{mib: held.Total, profile: in.prof.Name}
}

// freedOn is what a plan row counts as freed on its node.
type freedOn struct {
	mib     int
	profile string
}

// planFigures fills in what a plan row is built from (PlanMemoryNode): the
// console's MemAvailable reading, the floor and the headroom, so that
// reading - floor - headroom (+ adjust) = placeable, and the predicted lowest
// available memory during the load.
func (c *schedCore) planFigures(e *PlanMemoryNode, id core.NodeID, n core.Node, freed freedOn) {
	e.PlaceableMiB = e.AvailableMiB
	if !n.SharedMemory || n.MemAvailableMiB <= 0 {
		return
	}
	cur, floor := n.MemAvailableMiB, n.WatchdogFloorMiB
	e.MemAvailableMiB, e.WatchdogFloorMiB = &cur, &floor
	reading, from := "current", cur
	if n.MemAvailableAvgKnown {
		avg := n.MemAvailableAvgMiB
		e.MemAvailableAvgMiB = &avg
		if c.cfg.Defaults.PlacementUsesReserved && n.FreeAvgKnown && !c.avgStale(id) && n.FreeAvgMiB < n.FreeMiB {
			reading, from = "average", avg
		}
	}
	e.MemAvailableReading = &reading
	pred := cur - e.WillTakeMiB
	e.PredictedLowestAvailableMiB = &pred
	d := e.PlaceableMiB - (from - floor - n.HeadroomMiB)
	if d == 0 {
		return
	}
	var why []string
	if freed.mib > 0 && freed.profile != "" {
		why = append(why, fmt.Sprintf("%d MiB %s holds here counted as freed", freed.mib, freed.profile))
	}
	devFree, usable := n.FreeMiB, cur-floor
	if reading == "average" {
		devFree, usable = n.FreeAvgMiB, from-floor
	}
	if devFree < usable {
		why = append(why, fmt.Sprintf("the device reports %d MiB free, less than the reading less the floor (%d)", devFree, usable))
	}
	if pf, ok := c.preflightFree(id); ok && pf < devFree {
		why = append(why, fmt.Sprintf("a pre-flight reading of %d MiB free", pf))
	}
	if u := c.unmeasuredReserve(id); u > 0 && c.cfg.Defaults.PlacementUsesReserved {
		why = append(why, fmt.Sprintf("%d MiB reserved for a load not measured yet", u))
	}
	if len(why) == 0 {
		why = append(why, "the reservation rule's other figures")
	}
	w := strings.Join(why, "; ")
	e.PlaceableAdjustMiB, e.PlaceableAdjustWhy = &d, &w
}
