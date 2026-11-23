package api

import (
	"encoding/json"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// A plan from the model's own line carries derived_slope (additive); a plan
// without one has no such key.
func TestPlanDerivedSlopeJSON(t *testing.T) {
	b, _ := json.Marshal(jsonView(scheduler.PlanReport{NeedMiB: 9200, DerivedSlope: &scheduler.DerivedSlope{
		Devices: 6, MiBPer1kCtx: 40, FromCtx: []int{2048, 8192}, ConfirmedBy: []int{32768}, Confirmed: true}}))
	var doc map[string]any
	json.Unmarshal(b, &doc)
	sl, ok := doc["derived_slope"].(map[string]any)
	if !ok || sl["mib_per1k_ctx"] != float64(40) || sl["devices"] != float64(6) || sl["confirmed"] != true ||
		len(sl["from_ctx"].([]any)) != 2 || len(sl["confirmed_by"].([]any)) != 1 {
		t.Fatalf("derived_slope %s", b)
	}
	b, _ = json.Marshal(jsonView(scheduler.PlanReport{NeedMiB: 9200}))
	json.Unmarshal(b, &doc)
	doc = map[string]any{}
	json.Unmarshal(b, &doc)
	if _, ok := doc["derived_slope"]; ok {
		t.Fatalf("derived_slope without a line: %s", b)
	}
}
