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

// 0.1.7 review, ES1: a forced instance that is loaded when its node is
// excluded keeps running and serving (it is not drained). The write that
// excludes the node says so, under loaded_on_excluded_nodes and as a
// warning, with the profile, the nodes, the entries and since when.
func TestExclusionListsInstancesStillLoaded(t *testing.T) {
	e, _, _ := cfgEnv(t)
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"f1","model":"f1.gguf","ctx":2048,"needs_mib":100,"force_nodes":["host-1"]}`, nil); st != 200 && st != 201 {
		t.Fatalf("create: %d %v", st, m)
	}
	if resp := e.post("/v1/chat/completions", chat("f1")); resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, body(t, resp))
	}
	st, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":["host-*"]}`, nil)
	l, _ := m["loaded_on_excluded_nodes"].([]any)
	if st != 200 || len(l) != 1 {
		t.Fatalf("exclude: %d %v", st, m)
	}
	x := l[0].(map[string]any)
	if x["profile"] != "f1" || toJSON(x["nodes"]) != `["host-1"]` || toJSON(x["entries"]) != `["host-*"]` || x["state"] != "ready" {
		t.Fatalf("entry %v", x)
	}
	if since, _ := x["loaded_since"].(float64); since <= 0 {
		t.Fatalf("loaded_since %v", x["loaded_since"])
	}
	found := false
	for _, w := range m["warnings"].([]any) {
		if s, _ := w.(string); strings.Contains(s, `profile "f1" is loaded on excluded node host-1`) && strings.Contains(s, "until it is unloaded") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning for the loaded instance: %v", m["warnings"])
	}
	// Not drained: it still serves.
	if resp := e.post("/v1/chat/completions", chat("f1")); resp.StatusCode != 200 {
		t.Fatalf("chat after the exclusion: %d %s", resp.StatusCode, body(t, resp))
	}
	// Lifting the exclusion: nothing listed any more.
	if _, m := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":[]}`, nil); len(m["loaded_on_excluded_nodes"].([]any)) != 0 {
		t.Fatalf("after lifting: %v", m)
	}
}
