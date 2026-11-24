package api

import (
	"testing"
	"time"
)

// The Models list has the classic list's Needs column: the memory of the
// lightest configuration (fit.need_mib) and how it is known (measured,
// estimated, set in the profile), the sentence of fitOf on hover; a dash
// when it is not known yet.
func TestUINextModelsNeeds(t *testing.T) {
	now := time.Now()
	fetch := ovFetch(now, ovRequests(now), true)
	fetch["/api/models"] = map[string]any{"version": 1, "models": []any{
		map[string]any{"key": "alpha-27b", "kind": "llama", "exists": true, "file": "/models/Alpha-27B-Q4_K_M.gguf", "size_bytes": 16000000000, "profile_count": 1,
			"profiles": []any{map[string]any{"id": "alpha", "ctx": 32768}},
			"fit":      map[string]any{"need_mib": 18432, "basis": "measured", "ran_on_nodes": 2, "min_nodes": 2}},
		map[string]any{"key": "beta-4b", "kind": "llama", "exists": true, "file": "/models/Beta-4B-Q8_0.gguf", "size_bytes": 4200000000, "profile_count": 1,
			"profiles": []any{map[string]any{"id": "beta", "ctx": 8192}},
			"fit":      map[string]any{"need_mib": 5120, "basis": "estimated", "min_nodes": 1}},
		map[string]any{"key": "gamma-1b", "kind": "llama", "exists": true, "file": "/models/Gamma-1B-Q8_0.gguf", "size_bytes": 1100000000, "profile_count": 0},
	}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: the Needs column", "route": "#/models", "status": ovStatus(now), "width": 1536,
			"expect": []string{"Needs", "18.0 GB", "measured", "5.0 GB", "estimated", "Runs on", "Configurations"},
			"has":    []string{"td[data-col=needs] span.mut"},
			"titles": []string{"The memory its lightest configuration needs on the nodes", "18,432 MiB measured on a load across 2 nodes"}},
	}})
}
