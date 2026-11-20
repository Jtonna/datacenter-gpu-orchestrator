package api

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// GET /api/status shows where each instance's full child output is kept.
func TestStatusInstanceShowsLogFile(t *testing.T) {
	m := jsonView(scheduler.InstanceStatus{Profile: "p", LogFile: "/logs/20261118T064512Z-p-1.log"}).(map[string]any)
	if m["log_file"] != "/logs/20261118T064512Z-p-1.log" {
		t.Fatalf("log_file = %v in %v", m["log_file"], m)
	}
}
