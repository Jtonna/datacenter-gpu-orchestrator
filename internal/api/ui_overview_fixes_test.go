package api

import (
	"testing"
	"time"
)

// ovWith adds view nodes and agents to the Overview's fixture status.
func ovWith(now time.Time, nodes, agents []any) map[string]any {
	st := ovStatus(now)
	v := st["view"].(map[string]any)
	v["nodes"] = append(v["nodes"].([]any), nodes...)
	st["agents"] = append(st["agents"].([]any), agents...)
	return st
}

// (f) A machine an operator removed from the pool stays in the answers (by
// its MAC, with no stats) but is not a zone: the table, the pills' counts
// and "Showing N of M" leave it out.
func TestUINextOverviewLeavesRemovedOut(t *testing.T) {
	now := time.Now()
	const gone = "02:00:00:00:00:27"
	st := ovWith(now,
		[]any{map[string]any{"id": gone, "hostname": "", "online": false, "total_mib": 0, "free_mib": 0, "excluded": false, "state": "removed"}},
		[]any{map[string]any{"id": gone, "name": "", "state": "removed", "paused": false, "placeable": false}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": ovFetch(now, ovRequests(now), true), "cases": []any{
		map[string]any{"name": "removed: not drawn", "route": "#/overview", "status": st,
			"expect": []string{"Showing 6 of 6", "3 in use", "1 free", "1 down", "1 off"},
			"absent": []string{gone, "Removed", "Showing 7"}, "none": []string{"div.zr[data-zone='" + gone + "']"}},
		map[string]any{"name": "removed: not under the down pill", "route": "#/overview?show=down", "status": st,
			"expect": []string{"Showing 1 of 1", "node-e"}, "absent": []string{gone}},
	}})
}
