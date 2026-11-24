package api

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/groups"
)

// A removed machine has no uptime: it is not in zones[], its seconds are
// in no group, not ungrouped and not in the fleet figure.
func TestUptime_LeavesRemovedMachinesOut(t *testing.T) {
	m := map[string]*availability.MachineTotals{
		"02:00:00:00:00:01": {ID: "02:00:00:00:00:01", Name: "zone-1", Totals: availability.Totals{FreeS: 3600}},
		"02:00:00:00:00:02": {ID: "02:00:00:00:00:02", Name: "zone-2", Totals: availability.Totals{LostS: 3600}},
		"02:00:00:00:00:03": {ID: "02:00:00:00:00:03", Totals: availability.Totals{LostS: 1800}},
		"02:00:00:00:00:04": {ID: "02:00:00:00:00:04", Name: "zone-4", Totals: availability.Totals{FreeS: 600}},
	}
	of := map[string]string{"02:00:00:00:00:01": "rack-a", "02:00:00:00:00:02": "rack-a"}
	list := []groups.Group{{Name: "rack-a", Members: []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}}}
	removed := map[string]bool{"02:00:00:00:00:02": true, "02:00:00:00:00:03": true}
	doc := sumUptime(availability.Window{Days: 7}, m, of, list, removed)
	if len(doc.Zones) != 2 || doc.Zones[0].ID != "02:00:00:00:00:01" || doc.Zones[1].ID != "02:00:00:00:00:04" {
		t.Fatalf("zones %+v", doc.Zones)
	}
	if g := doc.Groups[0]; g.CountedS != 3600 || *g.Uptime != 1 {
		t.Fatalf("group %+v", g)
	}
	if doc.Ungrouped.CountedS != 600 || doc.Fleet.CountedS != 4200 || *doc.Fleet.Uptime != 1 {
		t.Fatalf("ungrouped %+v fleet %+v", doc.Ungrouped, doc.Fleet)
	}
}
