package history

import (
	"encoding/json"
	"testing"
	"time"
)

// The reader is lenient about fields it does not know: an event's data is a
// free map, and json.Unmarshal into Event ignores unknown top-level fields.
// So an older build reading a file with the instanceUnloaded event's
// cycle_peaks (scheduler/peakcycle.go) keeps every field it knows and reads
// the rest as plain data. Here: written, read back after a restart, and a
// line with an unknown top-level field besides.
func TestUnknownFieldsAreReadBack(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UnixMilli()
	s, _ := openT(t, dir)
	s.Record(Event{Ts: now, Type: TInstanceUnloaded, Severity: Info, Profile: "m", Nodes: []string{"a"}, Summary: "m was unloaded.",
		Data: map[string]any{"reason": "ttl", "requests_served": 2, "shape": "s", "ended_normally": true,
			"cycle_peaks": []map[string]any{{"node_id": "a", "node": "a", "peak_mib": 4800, "share_mib": 4000, "reserved_mib": 4512, "basis": "measured"},
				{"node_id": "b", "node": "b", "peak_mib": nil, "no_peak": "no free-memory reading while loaded"}}}})
	s.Close()
	s2, _ := openT(t, dir)
	r := s2.Query(Query{Types: []string{TInstanceUnloaded}})
	if len(r.Events) != 1 {
		t.Fatalf("%d events", len(r.Events))
	}
	e := r.Events[0]
	if e.Data["reason"] != "ttl" || e.Data["requests_served"] != 2.0 || e.Profile != "m" || len(e.Nodes) != 1 {
		t.Fatalf("known fields %+v", e)
	}
	if ps, ok := e.Data["cycle_peaks"].([]any); !ok || len(ps) != 2 || ps[1].(map[string]any)["peak_mib"] != nil {
		t.Fatalf("cycle_peaks %v", e.Data["cycle_peaks"])
	}
	var x Event
	line := `{"ts":1,"seq":1,"type":"instanceUnloaded","severity":"info","nodes":[],"summary":"s","data":{"reason":"ttl"},"later_field":{"a":1}}`
	if err := json.Unmarshal([]byte(line), &x); err != nil || x.Type != TInstanceUnloaded || x.Data["reason"] != "ttl" {
		t.Fatalf("%v %+v", err, x)
	}
}
