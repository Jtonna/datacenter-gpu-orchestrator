package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// 0.1.7, the live case: excluding the local node and an agent node while six
// saved profiles force them is accepted, with one warning per profile. The
// profiles stay as they are, the profile list marks them, the plan and a load
// refuse them with the entry, a new profile that forces an excluded node is
// accepted with the same warning, and lifting the exclusion makes them work
// again untouched.
func TestExclusionWithForcedProfiles(t *testing.T) {
	e, _, _ := cfgEnv(t)
	const agent = "02:00:00:00:00:31"
	if st, m := e.cdo("PUT", "/api/config/tiers", `[{"match":{"local":true},"priority":1,"headroom_mib":100}]`, nil); st != 200 {
		t.Fatalf("tiers: %d %v", st, m)
	}
	for i, node := range []string{"host-1", "host-1", "host-1", agent, agent, agent} {
		id := "f" + string(rune('1'+i))
		b := `{"id":"` + id + `","model":"` + id + `.gguf","ctx":2048,"needs_mib":100,"force_nodes":["` + node + `"]}`
		if st, m := e.cdo("POST", "/api/config/profiles", b, nil); st != 200 && st != 201 {
			t.Fatalf("%s: %d %v", id, st, m)
		} else if w, _ := m["warnings"].([]any); len(w) != 0 {
			t.Fatalf("%s: warnings before any exclusion %v", id, w)
		}
	}
	st, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":["host-1","`+agent+`"],"comment":"keep both out"}`, nil)
	w, _ := m["warnings"].([]any)
	if st != 200 || len(w) != 6 {
		t.Fatalf("exclude: %d %v", st, m)
	}
	if s, _ := w[0].(string); s != `profile "f1": force_nodes: node "host-1" is excluded by exclude_nodes entry "host-1"; the profile is kept and is refused until the exclusion is lifted` {
		t.Fatalf("warning %q", s)
	}
	if bl, _ := m["blocked_by_exclusion"].([]any); len(bl) != 6 {
		t.Fatalf("blocked_by_exclusion %v", m["blocked_by_exclusion"])
	}

	_, pm := e.cdo("GET", "/api/profiles", "", nil)
	blocked := 0
	for _, x := range pm["profiles"].([]any) {
		p := x.(map[string]any)
		id, _ := p["id"].(string)
		if !strings.HasPrefix(id, "f") {
			continue
		}
		if p["blocked_by_exclusion"] == nil {
			t.Fatalf("%s not marked: %v", id, p)
		}
		blocked++
		plan := p["plan"].(map[string]any)
		if plan["action"] != "reject" || !strings.Contains(plan["reason"].(string), "exclude_nodes entry") {
			t.Fatalf("%s plan %v", id, plan)
		}
	}
	if blocked != 6 {
		t.Fatalf("%d marked", blocked)
	}
	if st, m := e.cdo("POST", "/api/load", `{"model":"f1"}`, nil); st != 503 || !strings.Contains(strings.ToLower(toJSON(m)), "exclude_nodes entry") {
		t.Fatalf("load f1: %d %v", st, m)
	}
	// The reverse: a new profile that forces an excluded node.
	st, m = e.cdo("POST", "/api/config/profiles", `{"id":"f7","model":"f7.gguf","ctx":2048,"needs_mib":100,"force_nodes":["host-1"]}`, nil)
	if w, _ := m["warnings"].([]any); (st != 200 && st != 201) || len(w) != 7 || !strings.Contains(toJSON(w), `profile \"f7\"`) {
		t.Fatalf("create f7: %d %v", st, m)
	}
	// Lifting the exclusion: f1 starts again, untouched.
	if st, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":[]}`, nil); st != 200 || len(m["warnings"].([]any)) != 0 {
		t.Fatalf("lift: %d %v", st, m)
	}
	if st, m := e.cdo("POST", "/api/plan", `{"model":"f1"}`, nil); st != 200 || m["action"] != "start" {
		t.Fatalf("plan after lifting: %d %v", st, m)
	}
}
