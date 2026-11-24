package api

import (
	"strings"
	"testing"
)

// The word on the pages is "zone": the Profiles panel, the Models list and
// the model page count and name zones, not nodes. API fields and settings
// keys (exclude_nodes, no_auto_nodes, plan.nodes) keep their stored names.
func TestUINextZoneWord(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles", "/api/models", "/api/config", "/api/config/schema", "/api/status")
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "profiles panel: automatic, a count of zones", "route": "#/profiles?profile=qwen3-14b", "status": fetch["/api/status"],
			"page": []string{"would load on 1 zone"}, "pageHas": []string{"aside.side [data-would='1']"}, "notOnPage": []string{"1 node"}},
	}})
	// every user-visible sentence of these files that named a node
	for file, olds := range map[string][]string{
		"core/status.js":         {"node${n === 1"},
		"views/models/common.js": {"on the nodes, from the load log", "reported for this node", "memory on the nodes", "Upload to the nodes", "Measured from the nodes"},
		"views/models/detail.js": {"To the nodes, in MB/s", "uploaded the model to a node", "upload speed to the nodes"},
		"next/views/models.js":   {"runs on this node", "its first forced node", "needs on the nodes"},
		"next/core/shell.js":     {"Remote nodes are not used", "The node figures"},
		"next/core/ui.js":        {"fill the node up to here"},
	} {
		s := uiText(t, file)
		for _, old := range olds {
			if strings.Contains(s, old) {
				t.Errorf("%s still says %q", file, old)
			}
		}
	}
}
