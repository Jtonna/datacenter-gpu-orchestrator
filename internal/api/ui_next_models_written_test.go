package api

import (
	"testing"
	"time"
)

// A need written in the profile is shown whatever the plan says (the
// reviewer's ruling, 26 Nov): live's three "program on this machine" rows
// showed a dash because their plans reject (the local machine is excluded by
// config). The row says "~5.3 GB, set by hand" with the reason beside it in
// Runs on ("Runs only on host-1, which is excluded"); the panel says
// "set by hand" with the same reason; a need nobody knows keeps the dash.
func TestUINextModelsWrittenNeedWhenRejected(t *testing.T) {
	now := time.Now()
	fetch := ovFetch(now, ovRequests(now), true)
	why := `profile image-gen is a command profile and runs only on the local node host-1, which is excluded by config (exclude_nodes entry "host-1"; excluding wins: change exclude_nodes)`
	reject := map[string]any{"action": "reject", "would_start": false, "nodes": []any{}, "reason": why}
	fetch["/api/models"] = map[string]any{"version": 1, "models": []any{
		map[string]any{"key": "command:image-gen", "kind": "command", "name": "image-gen", "file": "", "state": "not_loaded", "profile_count": 1,
			"profiles": []any{map[string]any{"id": "image-gen", "stage": "production", "needs_mib": 5427, "needs_written": true, "needs_measured": true,
				"needs_basis": "measured, needs_mib", "plan": reject}}},
		map[string]any{"key": "tied-4b", "kind": "llama", "exists": true, "file": "/models/Tied-4B-Q8_0.gguf", "size_bytes": 4200000000, "state": "not_loaded", "profile_count": 1,
			"profiles": []any{map[string]any{"id": "tied", "ctx": 8192, "stage": "production", "needs_mib": 3174, "needs_written": true, "needs_measured": true,
				"needs_basis": "measured, needs_mib", "plan": map[string]any{"action": "reject", "would_start": false, "nodes": []any{},
					"reason": `profile tied forces node host-1, but node host-1 is excluded by config (exclude_nodes entry "host-1"; excluding wins: change exclude_nodes or force_nodes)`}}},
			"fit": map[string]any{"need_mib": 3174, "basis": "configured", "min_nodes": 1}},
		map[string]any{"key": "gamma-1b", "kind": "llama", "exists": true, "file": "/models/Gamma-1B-Q8_0.gguf", "size_bytes": 1100000000, "profile_count": 0},
	}}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "models: written needs of rejected plans", "route": "#/models", "status": ovStatus(now), "width": 1536,
			"expect": []string{"A program on this machine · need set by hand · runs only on host-1, which is excluded", "4.2 GB · need set by hand · ", "host-1 is excluded"},
			"absent": []string{"Cannot be placed right now", "GB, set by hand"},
			// the Needs cell holds the figure alone, as the range rows (v88 refused: the reason was painted over Profiles)
			"text": map[string]any{"div.mr[data-model='command:image-gen'] span.mdl-needs": "~5.3 GB", "div.mr[data-model=tied-4b] span.mdl-needs": "~3.1 GB"},
			"has":  []string{"span.mdl-needs[data-col=needs] span.mut"}}, // gamma: nobody knows its need
		map[string]any{"name": "models: the panel of the tied file", "route": "#/models?model=tied-4b", "status": ovStatus(now), "width": 1536,
			// how the need is known: in the Configurations table (the line of profiles under "Runs on" is gone, build-92 W1)
			"page": []string{"needs ~3.1 GB", "set by hand", "host-1 is excluded"}, "pageNone": []string{"aside.side .mdl-need-ends"}},
		map[string]any{"name": "models: the panel of the program", "route": "#/models?model=command:image-gen", "status": ovStatus(now), "width": 1536,
			"page": []string{"needs ~5.3 GB", "set by hand", "Runs only on host-1, which is excluded"}},
	}})
}
