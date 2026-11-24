package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// node= takes a list (ids or names) and group= a zone group's members;
// an event naming any of them matches, by id or by name.
func TestHistory_NodeListAndGroup(t *testing.T) {
	hist := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(func() { hist.Close() })
	e, _, _ := agentsEnv(t, func(o *Options) { o.History = hist })
	registerNamed(t, e, "02:00:00:00:00:01", "zone-1")
	registerNamed(t, e, "02:00:00:00:00:02", "zone-2")
	registerNamed(t, e, "02:00:00:00:00:03", "zone-3")
	now := time.Now().UnixMilli()
	for i, n := range []string{"zone-1", "zone-2", "zone-3", "02:00:00:00:00:02"} {
		hist.Record(history.Event{Ts: now + int64(i), Type: "testEvent", Severity: history.Info, Nodes: []string{n}, Summary: "event " + n})
	}
	hist.Flush()
	got := func(q string, want int) []string {
		t.Helper()
		resp := e.get("/api/history?type=testEvent&" + q)
		b := body(t, resp)
		if resp.StatusCode != want {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, b)
		}
		var a struct {
			Events []history.Event `json:"events"`
		}
		json.Unmarshal([]byte(b), &a)
		var out []string
		for _, ev := range a.Events {
			out = append(out, ev.Nodes[0])
		}
		sort.Strings(out)
		return out
	}
	if g := got("node=zone-1,02:00:00:00:00:02", 200); strings.Join(g, " ") != "02:00:00:00:00:02 zone-1 zone-2" {
		t.Fatalf("list: %v", g)
	}
	status(t, e.post("/api/groups", `{"name":"rack-a","members":["02:00:00:00:00:02","02:00:00:00:00:03"]}`), http.StatusCreated)
	if g := got("group=rack-a", 200); strings.Join(g, " ") != "02:00:00:00:00:02 zone-2 zone-3" {
		t.Fatalf("group: %v", g)
	}
	if g := got("group=rack-a&node=zone-1", 200); len(g) != 4 {
		t.Fatalf("group and node: %v", g)
	}
	got("group=nope", 404)
	status(t, e.post("/api/groups", `{"name":"empty"}`), http.StatusCreated)
	if g := got("group=empty", 200); len(g) != 0 {
		t.Fatalf("an empty group matches nothing: %v", g)
	}
}
