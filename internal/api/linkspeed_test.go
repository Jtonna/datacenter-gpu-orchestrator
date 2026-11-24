package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

func loadEvent(ts int64, profile, outcome string, loadS float64, detail any) history.Event {
	return history.Event{Ts: ts, Type: history.TLoadFinished, Severity: history.Info, Profile: profile,
		Data: map[string]any{"outcome": outcome, "load_s": loadS, "nodes_detail": detail}}
}

// The newest ready load that sent a zone at least 1 MiB gives its rate;
// failed loads, handshake-only bytes and older events do not.
func TestLoadLinks_NewestReadyLoad(t *testing.T) {
	ll := &loadLinks{byName: map[string]loadLinkAt{}}
	ll.add(loadEvent(2000, "gemma", "ready", 10, []map[string]any{{"node": "zone-1", "bytes": int64(120e6)}, {"node": "zone-2", "bytes": int64(1000)}}))
	ll.add(loadEvent(1000, "old", "ready", 1, []map[string]any{{"node": "zone-1", "bytes": int64(500e6)}}))   // older
	ll.add(loadEvent(3000, "qwen", "failed", 1, []map[string]any{{"node": "zone-1", "bytes": int64(500e6)}})) // failed
	// The form read back from a history file.
	var detail any
	json.Unmarshal([]byte(`[{"node":"zone-3","bytes":50000000,"memory_mib":100}]`), &detail)
	ll.add(loadEvent(4000, "qwen", "ready", 4, detail))
	if l := ll.get("zone-1"); l == nil || l.MBs != 12 || l.Model != "gemma" || l.Bytes != 120e6 || l.LoadS != 10 || l.At != "1970-01-01T00:00:02.000Z" {
		t.Fatalf("zone-1 %+v", l)
	}
	if ll.get("zone-2") != nil {
		t.Fatal("handshake bytes counted")
	}
	if l := ll.get("zone-3"); l == nil || l.MBs != 12.5 {
		t.Fatalf("zone-3 %+v", l)
	}
}

// /api/nodes carries last_load_link from the history, new events included.
func TestLoadLinks_OnTheNodeFeed(t *testing.T) {
	hist := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(func() { hist.Close() })
	e, _, _ := agentsEnv(t, func(o *Options) { o.History = hist })
	registerNamed(t, e, "02:00:00:00:00:01", "zone-1")
	hist.Record(loadEvent(time.Now().UnixMilli(), "gemma", "ready", 5, []map[string]any{{"node": "zone-1", "bytes": int64(60e6)}}))
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
	if l := f.Nodes[0].Link; l.MBs != 12 || l.Model != "gemma" {
		t.Fatalf("%+v", l)
	}
}
