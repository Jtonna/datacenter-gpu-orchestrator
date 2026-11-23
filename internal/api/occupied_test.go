package api

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

func occEvent(ts int64, outcome string, wait float64) history.Event {
	return history.Event{Ts: ts, Type: history.TOccupiedOnly, Severity: history.Info, Profile: "m", Nodes: []string{"a"},
		Summary: "m's only option was a, held by small.", Data: map[string]any{"outcome": outcome, "wait_s": wait,
			"occupied_nodes": []string{"a"}, "occupants": []string{"small"}}}
}

func occView(t *testing.T, e *env) map[string]any {
	t.Helper()
	m, _ := getJSONBody(t, e, "/api/system")["occupied_only"].(map[string]any)
	if m == nil {
		t.Fatalf("no occupied_only in /api/system")
	}
	return m
}

// The totals come from the history store: the events it holds at start,
// then every new one, and again after a restart on the same store.
func TestOccupiedOnlyTotalsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *history.Store {
		return history.Open(history.Options{Dir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}
	st := open()
	now := time.Now().UnixMilli()
	st.Record(occEvent(now-9000, "waited", 40))
	st.Record(occEvent(now-8000, "evicted", 12.5))
	st.Record(history.Event{Ts: now - 7000, Type: "loadStarted", Severity: "info", Profile: "m", Summary: "Loading m."})
	e := histEnv(t, st)
	m := occView(t, e)
	if m["total"] != 2.0 {
		t.Fatalf("seeded totals %v", m)
	}
	st.Record(occEvent(now-1000, "timed_out", 600))
	st.Record(occEvent(now-500, "cancelled", 3))
	m = occView(t, e)
	by := m["by_outcome"].(map[string]any)
	ws := m["wait_seconds"].(map[string]any)
	if m["total"] != 4.0 || by["waited"] != 1.0 || by["evicted"] != 1.0 || by["timed_out"] != 1.0 || by["cancelled"] != 1.0 ||
		ws["sum"] != 655.5 || ws["max"] != 600.0 || m["since"] == nil {
		t.Fatalf("totals %v", m)
	}
	st.Close()

	st2 := open()
	t.Cleanup(st2.Close)
	e2 := histEnv(t, st2)
	if m2 := occView(t, e2); m2["total"] != 4.0 || m2["wait_seconds"].(map[string]any)["max"] != 600.0 {
		t.Fatalf("after a restart %v", m2)
	}
	// and the events read back from /api/history
	h := getJSONBody(t, e2, "/api/history?type=occupiedOnly")
	if evs, _ := h["events"].([]any); len(evs) != 4 {
		t.Fatalf("history occupiedOnly events %v", h["events"])
	}
}

// With the history off the key is there, empty.
func TestOccupiedOnlyWithHistoryOff(t *testing.T) {
	e := newEnv(t, chatOK)
	if m := occView(t, e); m["total"] != 0.0 || m["since"] != nil {
		t.Fatalf("%v", m)
	}
}
