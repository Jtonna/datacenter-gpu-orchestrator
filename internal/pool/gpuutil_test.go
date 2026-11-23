package pool

import (
	"strings"
	"testing"
)

// The node summary (/api/nodes) carries the GPU utilisation from the stats
// the agent sends (stats.gpus[].util_percent): the busiest GPU's. Absent
// when no GPU reports one, or the stats say nothing about GPUs.
func TestGPUUtilInTheNodeSummary(t *testing.T) {
	r := newRig(t)
	r.p.Register(regReq(upID))
	beat := func(gpus string) {
		t.Helper()
		stats := `,"stats":{"stats_version":1,"sources":{"node":{"status":"ok"}},"gpus":` + gpus + `}}`
		body := strings.TrimSuffix(strings.TrimSpace(string(hbBody("s1", "", true, "true"))), "}") + stats
		if res := r.p.Heartbeat(upID, []byte(body)); res.Status != 200 {
			t.Fatalf("heartbeat %d %s", res.Status, res.Err)
		}
	}
	util := func() *float64 {
		for _, f := range r.p.Facts() {
			if f.ID == upID {
				return f.GPUUtilPercent
			}
		}
		t.Fatal("no facts")
		return nil
	}
	beat(`[{"id":"a","vendor":"amd","kind":"integrated","util_percent":37.5,"mem_total_bytes":1,"mem_used_bytes":0,"mem_source":"x"},
		{"id":"b","vendor":"nvidia","kind":"discrete","util_percent":91,"mem_total_bytes":1,"mem_used_bytes":0,"mem_source":"x"}]`)
	if u := util(); u == nil || *u != 91 {
		t.Fatalf("util %v", u)
	}
	beat(`[{"id":"a","vendor":"amd","kind":"integrated","mem_total_bytes":1,"mem_used_bytes":0,"mem_source":"x"}]`)
	if u := util(); u != nil {
		t.Fatalf("no figure, got %v", *u)
	}
	r.heartbeat(upID) // no stats
	if u := util(); u != nil {
		t.Fatalf("no stats, got %v", *u)
	}
}
