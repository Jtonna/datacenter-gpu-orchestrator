package api

import (
	"strings"
	"testing"
)

// The About page shows the background tasks from GET /api/system, with a
// warning after a recent failure, and the Activity page names the event.
func TestUIBackgroundTasks(t *testing.T) {
	about := uiText(t, "views/about.js")
	for _, want := range []string{"h('h2', 'Background tasks')", "sys.tasks", "t.failures_last_min", "t.last_failure_at", "t.restarts",
		"sys.internal_failures_total", "A background task failed in the last hour", "tasksCard(sys)"} {
		if !strings.Contains(about, want) {
			t.Errorf("views/about.js lacks %q", want)
		}
	}
	if act := uiText(t, "views/activity.js"); !strings.Contains(act, "'internalFailure'") {
		t.Error("views/activity.js does not list the internalFailure event")
	}
}
