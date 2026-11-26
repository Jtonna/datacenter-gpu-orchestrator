package api

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// unloadEvent is an instanceUnloaded event as the scheduler records it
// (scheduler/peakcycle.go); a nil peak is a node without a reading.
func unloadEvent(ts int64, profile, shape string, normal bool, peaks map[string]*int, order ...string) history.Event {
	nodes := make([]map[string]any, 0, len(order))
	for _, id := range order {
		e := map[string]any{"node_id": id, "node": "name-" + id, "share_mib": 4000, "reserved_mib": 4512, "basis": "measured"}
		if p := peaks[id]; p != nil {
			e["peak_mib"] = *p
		} else {
			e["peak_mib"], e["no_peak"] = nil, "no free-memory reading while loaded"
		}
		nodes = append(nodes, e)
	}
	return history.Event{Ts: ts, Type: history.TInstanceUnloaded, Severity: history.Info, Profile: profile, Nodes: order,
		Summary: profile + " was unloaded.", Data: map[string]any{"reason": "ttl", "loaded_for_s": 60.0, "requests_served": 1,
			"shape": shape, "ended_normally": normal, "cycle_peaks": nodes}}
}

func mib(v int) *int { return &v }

func peakView(t *testing.T, e *env) map[string]any {
	t.Helper()
	m, _ := getJSONBody(t, e, "/api/needs")["peak_cycles"].(map[string]any)
	if m == nil {
		t.Fatal("no peak_cycles in /api/needs")
	}
	return m
}

func peakRow(t *testing.T, m map[string]any, shape string, nodes int) map[string]any {
	t.Helper()
	for _, r := range m["rows"].([]any) {
		row := r.(map[string]any)
		if row["shape"] == shape && len(row["placement"].([]any)) == nodes {
			return row
		}
	}
	t.Fatalf("no row %s on %d nodes in %v", shape, nodes, m["rows"])
	return nil
}

// GET /api/needs peak_cycles: per shape and node set, the cycles with a
// peak on every node in the history (from the store at start, then new
// events, and again after a restart from the files), lowest, highest and
// latest, first and last; a cycle missing a node's peak is counted apart;
// an unload event of an older build (no cycle_peaks) is skipped.
func TestNeedsPeakCyclesFromHistory(t *testing.T) {
	dir := t.TempDir()
	open := func() *history.Store {
		return history.Open(history.Options{Dir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}
	st := open()
	now := time.Now().UnixMilli()
	const s = `["llama","m.gguf",8192,1,"f16",false,[]]`
	st.Record(unloadEvent(now-9000, "m", s, true, map[string]*int{"a": mib(4800), "b": mib(4700)}, "a", "b"))
	st.Record(unloadEvent(now-8000, "m", s, false, map[string]*int{"a": mib(5300), "b": mib(4600)}, "a", "b"))
	st.Record(unloadEvent(now-7000, "m", s, true, map[string]*int{"a": mib(4900), "b": nil}, "a", "b"))
	st.Record(unloadEvent(now-6500, "m1", s, true, map[string]*int{"a": mib(9100)}, "a"))
	st.Record(history.Event{Ts: now - 6000, Type: history.TInstanceUnloaded, Severity: "info", Profile: "m", Nodes: []string{"a"},
		Summary: "m was unloaded.", Data: map[string]any{"reason": "ttl"}}) // an older build's
	e := histEnv(t, st)
	m := peakView(t, e)
	if tot := m["totals"].(map[string]any); tot["rows"] != 2.0 || tot["at_least_1"] != 2.0 || tot["at_least_3"] != 0.0 || m["since"] == nil {
		t.Fatalf("seeded %v", m)
	}
	st.Record(unloadEvent(now-1000, "m", s, true, map[string]*int{"a": mib(4400), "b": mib(4500)}, "a", "b"))
	check := func(m map[string]any) {
		t.Helper()
		r := peakRow(t, m, s, 2)
		tot := r["total"].(map[string]any)
		if r["cycles"] != 3.0 || r["normal_cycles"] != 2.0 || r["cycles_without_peak"] != 1.0 ||
			tot["low_mib"] != 8900.0 || tot["high_mib"] != 9900.0 || tot["latest_mib"] != 8900.0 {
			t.Fatalf("two-node row %v", r)
		}
		a := r["nodes"].([]any)[0].(map[string]any)
		if a["node"] != "a" || a["low_mib"] != 4400.0 || a["high_mib"] != 5300.0 || a["latest_mib"] != 4400.0 {
			t.Fatalf("node a %v", a)
		}
		if r["first_at"] != isoTime(time.UnixMilli(now-9000)) || r["last_at"] != isoTime(time.UnixMilli(now-1000)) {
			t.Fatalf("dates %v %v", r["first_at"], r["last_at"])
		}
		if ps := r["profiles"].([]any); len(ps) != 1 || ps[0] != "m" {
			t.Fatalf("profiles %v", ps)
		}
		one := peakRow(t, m, s, 1)
		if one["cycles"] != 1.0 || one["total"].(map[string]any)["latest_mib"] != 9100.0 {
			t.Fatalf("one-node row %v", one)
		}
		if tot := m["totals"].(map[string]any); tot["at_least_1"] != 2.0 || tot["at_least_3"] != 1.0 || tot["at_least_5"] != 0.0 {
			t.Fatalf("totals %v", tot)
		}
	}
	check(peakView(t, e))
	st.Close()

	st2 := open() // read back from the files: JSON values, not Go values
	t.Cleanup(st2.Close)
	check(peakView(t, histEnv(t, st2)))
}

// With the history off the block is there, empty.
func TestNeedsPeakCyclesWithHistoryOff(t *testing.T) {
	e := newEnv(t, chatOK)
	m := peakView(t, e)
	if rows := m["rows"].([]any); len(rows) != 0 || m["since"] != nil || m["totals"].(map[string]any)["at_least_1"] != 0.0 {
		t.Fatalf("%v", m)
	}
}
