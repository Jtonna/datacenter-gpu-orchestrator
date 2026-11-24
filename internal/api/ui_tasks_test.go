package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
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

// The About page is executed with a real GET /api/system after a recovered
// failure: the card draws the task, its failure and the warning.
func TestUIAboutBackgroundTasksExecutes(t *testing.T) {
	isolate(t)
	e := newEnv(t, chatOK)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	failed := make(chan struct{})
	supervise.Go(ctx, supervise.Spec{Name: "test.task", Policy: supervise.Restart}, func(ctx context.Context) {
		if runs.Add(1) == 1 {
			close(failed)
			panic("injected")
		}
		<-ctx.Done()
	})
	<-failed
	for deadline := time.Now().Add(5 * time.Second); runs.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	supervise.WaitRecords(time.Second)
	var sys map[string]any
	if err := json.Unmarshal([]byte(body(t, e.get("/api/system"))), &sys); err != nil {
		t.Fatal(err)
	}
	history := map[string]any{"history": "on", "events": []any{
		map[string]any{"ts": time.Now().UnixMilli(), "type": "internalFailure", "severity": "error", "summary": "Background task test.task failed and was restarted.",
			"data": map[string]any{"task": "test.task", "kind": "restart", "panic": "injected", "escalating": false}},
	}}
	// The UI mounts the shared about.js and activity.js over next/'s core
	// modules (the classic UI is removed).
	for _, shell := range []string{"next"} {
		t.Run(shell, func(t *testing.T) {
			uiExec(t, map[string]any{"shell": shell, "modules": []string{"views/about.js", "views/activity.js"},
				"fetch": map[string]any{"/api/system": sys, "/api/about": map[string]any{},
					"/api/history": history, "/api/history/summary": map[string]any{"counts": map[string]any{}}, "/api/metrics/activity": map[string]any{"data": []any{}}},
				"cases": []any{
					map[string]any{"name": "about: a recovered failure", "route": "#/about", "status": nil,
						"expect": []string{"Background tasks", "test.task", "Restarts alone", "injected", "A background task failed in the last hour", "1 failure recovered since start."}},
					map[string]any{"name": "activity: an internal failure", "route": "#/activity", "status": nil,
						"expect": []string{"Internal failure", "Background task test.task failed and was restarted."}},
				}})
		})
	}
}
