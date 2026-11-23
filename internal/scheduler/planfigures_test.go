package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// A console reporting 13,656 MiB available with a watchdog floor of 1,000
// and a headroom of 1,024: free_now (after the floor) 12,656, placeable
// (after the floor and the headroom) 11,632. The plan row says each figure
// it is built from, so nobody takes the floor off twice, and the figure to
// compare with the console's own available memory during the load.
func floorConsole(h *harness) {
	h.view.modify("a", func(n *core.Node) {
		n.SharedMemory, n.HeadroomMiB, n.WatchdogFloorMiB = true, 1024, 1000
		n.MemAvailableMiB, n.FreeMiB, n.TotalMiB = 13656, 12656, 16000
		n.FreeAvgKnown, n.MemAvailableAvgKnown = false, false
	})
	h.viewChanged()
}

func floorHarness(t *testing.T) *harness {
	h := inMode(newHarness(t).nodes(psNode("a", 2, 12000)), "agents")
	liveTight(h)
	h.llama("q", 0, func(p *core.Profile) { p.Ctx, p.LoadTimeout, p.ForceNodes = 4096, time.Hour, []string{"a"} })
	h.needs["q"] = NeedInfo{Need: placer.Measured(6000), FileBytes: 5000 << 20}
	floorConsole(h)
	return h
}

func TestPlanRowSaysWhatItIsBuiltFrom(t *testing.T) {
	h := floorHarness(t)
	r := h.c.dryPlan("q")
	if r.Memory == nil || len(r.Memory.Nodes) != 1 {
		t.Fatalf("no memory rows: %+v", r)
	}
	row := r.Memory.Nodes[0]
	if row.FreeNowMiB != 12656 || row.AvailableMiB != 11632 || row.PlaceableMiB != 11632 {
		t.Fatalf("free_now %d available %d placeable %d", row.FreeNowMiB, row.AvailableMiB, row.PlaceableMiB)
	}
	if row.MemAvailableMiB == nil || *row.MemAvailableMiB != 13656 || row.MemAvailableReading == nil || *row.MemAvailableReading != "current" ||
		row.WatchdogFloorMiB == nil || *row.WatchdogFloorMiB != 1000 || row.HeadroomMiB != 1024 {
		t.Fatalf("figures %+v", row)
	}
	if row.PlaceableAdjustMiB != nil {
		t.Fatalf("an adjustment where A - F - H = P: %d %v", *row.PlaceableAdjustMiB, row.PlaceableAdjustWhy)
	}
	if row.PredictedLowestAvailableMiB == nil || *row.PredictedLowestAvailableMiB != 13656-row.WillTakeMiB {
		t.Fatalf("predicted lowest %v, will take %d", row.PredictedLowestAvailableMiB, row.WillTakeMiB)
	}
	// the arithmetic the UI line shows
	if 13656-1000-1024 != row.PlaceableMiB {
		t.Fatal("A - F - H != P")
	}
}

// When the placeable figure is built from the average reading, the row says
// so and carries both readings; anything else that lowers it (here memory
// reserved for a load not measured yet) is one signed term with its reason,
// so the line still adds up.
func TestPlanRowOnTheAverageAndOtherDeductions(t *testing.T) {
	h := floorHarness(t)
	h.view.modify("a", func(n *core.Node) {
		n.FreeAvgMiB, n.FreeAvgKnown = 12156, true // the average after the floor
		n.MemAvailableAvgMiB, n.MemAvailableAvgKnown = 13156, true
	})
	h.c.avgAlways = true
	h.viewChanged()
	row := h.c.dryPlan("q").Memory.Nodes[0]
	if row.MemAvailableReading == nil || *row.MemAvailableReading != "average" || row.MemAvailableAvgMiB == nil || *row.MemAvailableAvgMiB != 13156 ||
		*row.MemAvailableMiB != 13656 || row.PlaceableMiB != 13156-1000-1024 || row.PlaceableAdjustMiB != nil {
		t.Fatalf("average: %+v", row)
	}
	// the prediction stays on the current reading: what a human sees now
	if *row.PredictedLowestAvailableMiB != 13656-row.WillTakeMiB {
		t.Fatalf("predicted %d", *row.PredictedLowestAvailableMiB)
	}

	// a lower device figure than MemAvailable - floor: one signed term with its reason
	h.view.modify("a", func(n *core.Node) { n.FreeMiB, n.FreeAvgKnown, n.MemAvailableAvgKnown = 12000, false, false })
	h.viewChanged()
	row = h.c.dryPlan("q").Memory.Nodes[0]
	if row.PlaceableMiB != 12000-1024 || row.PlaceableAdjustMiB == nil || *row.PlaceableAdjustMiB != -656 ||
		row.PlaceableAdjustWhy == nil || !strings.Contains(*row.PlaceableAdjustWhy, "device") {
		t.Fatalf("device lower: placeable %d adjust %v why %v", row.PlaceableMiB, row.PlaceableAdjustMiB, row.PlaceableAdjustWhy)
	}
	if 13656-1000-1024+*row.PlaceableAdjustMiB != row.PlaceableMiB {
		t.Fatal("the line does not add up")
	}
}

// A node with memory of its own (a discrete GPU) has no MemAvailable line:
// placeable is its device's free figure less the headroom, and nothing else
// is claimed.
func TestPlanRowDedicatedNode(t *testing.T) {
	h := newHarness(t).nodes(laptop("l", 7000))
	h.llama("q", 0, func(p *core.Profile) { p.ForceNodes, p.LoadTimeout = []string{"l"}, time.Hour })
	h.needs["q"] = NeedInfo{Need: placer.Measured(3000), FileBytes: 2000 << 20}
	h.viewChanged()
	row := h.c.dryPlan("q").Memory.Nodes[0]
	if row.PlaceableMiB != row.AvailableMiB || row.MemAvailableMiB != nil || row.WatchdogFloorMiB != nil || row.PredictedLowestAvailableMiB != nil {
		t.Fatalf("%+v", row)
	}
}
