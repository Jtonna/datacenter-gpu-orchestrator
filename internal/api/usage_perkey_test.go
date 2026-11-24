package api

import (
	"encoding/json"
	"testing"
	"time"
)

type keyUsage struct {
	Days     int   `json:"days"`
	Requests int64 `json:"requests"`
	Denied   int64 `json:"denied"`
	Asked    int64 `json:"asked"`
	ByDay    []struct {
		Day      string `json:"day"`
		Requests int64  `json:"requests"`
		Denied   int64  `json:"denied"`
		Asked    int64  `json:"asked"`
	} `json:"by_day"`
}

func usageByName(t *testing.T, e *env) map[string]keyUsage {
	t.Helper()
	var doc struct {
		Agents []struct {
			Name  string   `json:"name"`
			Usage keyUsage `json:"usage"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/agents"))), &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]keyUsage{}
	for _, a := range doc.Agents {
		out[a.Name] = a.Usage
	}
	return out
}

// P6: each key's requests per UTC day, with the ones refused for want of a
// permission (denied, REST and MCP) and the ones refused because the
// permission is Ask me (asked). GET /api/agents gives the last 7 days, before
// and after the counters are written.
func TestAccessKeys_UsagePerKey(t *testing.T) {
	e, mcp, _ := accessEnv(t)
	observer, _ := newKey(t, e, `{"name":"watcher"}`)
	asker, _ := newKey(t, e, `{"name":"asker","permissions":{"zones":"ask","models":"ask","settings":"ask"}}`)

	for i := 0; i < 3; i++ {
		e.cdo("GET", "/api/status", "", bearer(observer))
	}
	e.cdo("POST", "/api/nodes/x/pause", `{}`, bearer(observer)) // denied
	e.cdo("GET", "/api/agents", "", bearer(observer))           // owner only: denied
	mcpCall(mcp, "pause_node", `{"id":"x","comment":"c"}`, bearer(observer))
	e.cdo("POST", "/api/unload", `{}`, bearer(asker)) // asked
	mcpCall(mcp, "load_model", `{"model":"m"}`, bearer(asker))
	e.advance(24 * time.Hour)
	e.cdo("GET", "/api/status", "", bearer(observer))

	check := func(when string) {
		u := usageByName(t, e)
		w, a := u["watcher"], u["asker"]
		if w.Days != 7 || w.Requests != 7 || w.Denied != 3 || w.Asked != 0 || a.Requests != 2 || a.Asked != 2 || a.Denied != 0 {
			t.Fatalf("%s: watcher %+v asker %+v", when, w, a)
		}
		if len(w.ByDay) != 7 || w.ByDay[5].Day != "2026-11-17" || w.ByDay[5].Requests != 6 || w.ByDay[5].Denied != 3 ||
			w.ByDay[6].Day != "2026-11-18" || w.ByDay[6].Requests != 1 || w.ByDay[0].Day != "2026-11-12" || w.ByDay[0].Requests != 0 {
			t.Fatalf("%s: watcher by day %+v", when, w.ByDay)
		}
	}
	check("before the flush")
	e.api.agents.flush()
	check("after the flush")
	e.cdo("POST", "/api/nodes/x/pause", `{}`, bearer(observer))
	if w := usageByName(t, e)["watcher"]; w.Requests != 8 || w.Denied != 4 || w.ByDay[6].Denied != 1 {
		t.Fatalf("after one more: %+v", w)
	}

	// A day leaves the 7 days after a week, and the stored days are trimmed
	// after 90 by the hourly key task.
	e.api.agents.flush()
	e.advance(7 * 24 * time.Hour)
	if w := usageByName(t, e)["watcher"]; w.Requests != 0 || len(w.ByDay) != 7 {
		t.Fatalf("a week later: %+v", w)
	}
	e.advance(90 * 24 * time.Hour)
	e.api.checkKeyExpiry(t.Context(), e.api.opts.Now())
	if n, err := e.api.store.AgentUsage(t.Context(), 0); err != nil || len(n) != 0 {
		t.Fatalf("after 90 days: %v %v", n, err)
	}
}
