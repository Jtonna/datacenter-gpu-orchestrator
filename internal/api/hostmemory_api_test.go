package api

import (
	"reflect"
	"testing"
)

// The host-memory sum over the instances loaded on the scheduler's machine
// is in /api/status (server_host_memory) and, the same document, in
// /api/system (host_memory).
func TestHostMemorySumInStatusAndSystem(t *testing.T) {
	e, _, _ := cfgEnv(t)
	st := jsonOf(t, body(t, e.get("/api/status")))
	sys := jsonOf(t, body(t, e.get("/api/system")))
	a, _ := st["server_host_memory"].(map[string]any)
	b, _ := sys["host_memory"].(map[string]any)
	if a == nil || b == nil {
		t.Fatalf("status %v / system %v", st["server_host_memory"], sys["host_memory"])
	}
	for _, k := range []string{"machine", "checkpoints_mib", "prompt_cache_mib", "instances", "size_unknown"} {
		if _, ok := b[k]; !ok {
			t.Errorf("/api/system host_memory has no %s: %v", k, b)
		}
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("status %v, system %v", a, b)
	}
}
