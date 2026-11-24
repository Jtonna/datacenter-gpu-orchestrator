package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

func loadEvent(ts int64, profile, outcome string, loadS float64, detail any) history.Event {
	return history.Event{Ts: ts, Type: history.TLoadFinished, Severity: history.Info, Profile: profile,
		Data: map[string]any{"outcome": outcome, "load_s": loadS, "bytes_measured": true, "nodes_detail": detail}}
}

func zoneBytes(pairs ...any) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]any{"node": pairs[i], "bytes": int64(pairs[i+1].(int))})
	}
	return out
}

// The figure is the last cold load: the newest ready load that really
// moved the model to the zone (measured bytes, at least 256 MiB to that
// zone, and at least 2 s). Shapes as live's loadFinished events.
func TestLoadLinks_LastColdLoad(t *testing.T) {
	ll := &loadLinks{byName: map[string]loadLinkAt{}}
	// zone-a: a real load of 2.6 GB in 25 s, then a warm one (live: 38,225 bytes in 1.8 s).
	ll.add(loadEvent(2000, "qwen", "ready", 25, zoneBytes("zone-a", 2600000000)))
	ll.add(loadEvent(5000, "qwen", "ready", 1.8, zoneBytes("zone-a", 38225)))
	// A mostly cached load: 400,933 bytes in 76 s (live, bytes_measured true).
	ll.add(loadEvent(6000, "coder", "ready", 76, zoneBytes("zone-a", 400933, "zone-b", 400933)))
	// zone-b otherwise: a short load of real size, under 2 s.
	ll.add(loadEvent(7000, "small", "ready", 1.5, zoneBytes("zone-b", 300<<20)))
	// zone-c: unmeasured bytes (the file size, 3,013 MB in 1 s).
	un := loadEvent(8000, "old-agent", "ready", 1, zoneBytes("zone-c", 3013000000))
	un.Data["bytes_measured"] = false
	ll.add(un)
	// zone-d: an event without bytes_measured (written before it existed) is not counted.
	old := loadEvent(8500, "older", "ready", 30, zoneBytes("zone-d", 3000000000))
	delete(old.Data, "bytes_measured")
	ll.add(old)
	// Failed loads and older events never replace a figure.
	ll.add(loadEvent(9000, "qwen", "failed", 30, zoneBytes("zone-a", 3000000000)))
	ll.add(loadEvent(1000, "older", "ready", 10, zoneBytes("zone-a", 2000000000)))
	if l := ll.get("zone-a"); l == nil || l.MBs != 104 || l.Model != "qwen" || l.Bytes != 2600000000 || l.LoadS != 25 ||
		l.At != "1970-01-01T00:00:02.000Z" || l.Kind != "lower_bound" {
		t.Fatalf("zone-a %+v", l)
	}
	for _, z := range []string{"zone-b", "zone-c", "zone-d"} {
		if l := ll.get(z); l != nil {
			t.Errorf("%s has a figure %+v", z, l)
		}
	}
	// A later cold load replaces it; the form read back from a history file.
	var detail any
	json.Unmarshal([]byte(`[{"node":"zone-a","bytes":1300000000,"memory_mib":100}]`), &detail)
	ll.add(loadEvent(10000, "gemma", "ready", 10, detail))
	if l := ll.get("zone-a"); l == nil || l.MBs != 130 || l.Model != "gemma" {
		t.Fatalf("zone-a after a second cold load %+v", l)
	}
}

// /api/nodes carries last_load_link from the history, new events included.
func TestLoadLinks_OnTheNodeFeed(t *testing.T) {
	hist := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(func() { hist.Close() })
	e, _, _ := agentsEnv(t, func(o *Options) { o.History = hist })
	registerNamed(t, e, "02:00:00:00:00:01", "zone-1")
	hist.Record(loadEvent(time.Now().UnixMilli(), "gemma", "ready", 5, zoneBytes("zone-1", 600000000)))
	var f struct {
		Nodes []struct {
			Name string    `json:"name"`
			Link *LoadLink `json:"last_load_link"`
		} `json:"nodes"`
	}
	waitFor(t, func() bool {
		json.Unmarshal([]byte(body(t, e.get("/api/nodes"))), &f)
		return len(f.Nodes) > 0 && f.Nodes[0].Link != nil
	})
	if l := f.Nodes[0].Link; l.MBs != 120 || l.Model != "gemma" || l.Kind != "lower_bound" {
		t.Fatalf("%+v", l)
	}
}
