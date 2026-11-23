package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// queryAudit is an audit log that answers Query (section, target, action;
// newest first; limit), as cfgstore does.
type queryAudit struct {
	mu sync.Mutex
	es []audit.Entry
}

func (q *queryAudit) Record(_ context.Context, e audit.Entry) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e.ID = int64(len(q.es) + 1)
	q.es = append(q.es, e)
	return e.ID, nil
}

func (q *queryAudit) Query(_ context.Context, f cfgstore.Filter) ([]audit.Entry, int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []audit.Entry
	for i := len(q.es) - 1; i >= 0; i-- {
		e := q.es[i]
		if (f.Section == "" || e.Section == f.Section) && (f.Target == "" || e.Target == f.Target) && (f.Action == "" || e.Action == f.Action) {
			out = append(out, e)
		}
	}
	n := len(out)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, n, nil
}

func (q *queryAudit) GetEntry(context.Context, int64) (*audit.Entry, error) { return nil, nil }

// zoneEnv: agents membership, a queryable audit log and a configuration the
// test changes (exclude_nodes).
func zoneEnv(t *testing.T) (*env, *queryAudit, func(exclude ...string)) {
	t.Helper()
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT-BINARY")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("RPC-SERVER")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d}
	p, err := pool.New(pool.Options{Config: func() *config.Config { mu.Lock(); defer mu.Unlock(); return cfg }, Programs: pool.Programs{Release: rel}, PotluckRelease: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	au := &queryAudit{}
	e := newEnv(t, chatOK, func(o *Options) { o.Pool, o.Audit = p, au })
	setExclude := func(list ...string) {
		mu.Lock()
		defer mu.Unlock()
		c := *cfg
		c.ExcludeNodes = list
		cfg = &c
	}
	return e, au, setExclude
}

func nodeEntry(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	resp := e.get("/api/nodes")
	var feed struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &feed); err != nil {
		t.Fatal(err)
	}
	for _, n := range feed.Nodes {
		if n["id"] == id {
			return n
		}
	}
	t.Fatalf("no node %s in %+v", id, feed.Nodes)
	return nil
}

func TestZoneFacts_PausedSaysWhoAndWhenFromTheAuditLog(t *testing.T) {
	e, _, _ := zoneEnv(t)
	const id = "02:00:00:00:00:01"
	register(t, e, id)
	if n := nodeEntry(t, e, id); n["paused_at"] != nil || n["paused_actor"] != nil || n["member"] != nil || n["set_aside"] != nil {
		t.Fatalf("an idle node has none of the zone facts: %v", n)
	}
	req, _ := http.NewRequest("POST", e.front.URL+"/api/nodes/"+id+"/pause", strings.NewReader(`{"comment":"dusting the shelf"}`))
	req.Header.Set("X-Actor", "operator-1")
	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err, resp.StatusCode)
	}
	resp.Body.Close()
	n := nodeEntry(t, e, id)
	at, _ := time.Parse(time.RFC3339Nano, toString(n["paused_at"]))
	if n["paused"] != true || n["paused_actor"] != "operator-1" || at.Before(t0.Add(-time.Second)) || at.After(time.Now()) {
		t.Fatalf("%v %v", n["paused_at"], n["paused_actor"])
	}
	// Unpaused: no longer said; paused again by someone else: the new one.
	for _, c := range []struct{ path, actor string }{{"unpause", "operator-1"}, {"pause", "operator-2"}} {
		req, _ := http.NewRequest("POST", e.front.URL+"/api/nodes/"+id+"/"+c.path, strings.NewReader(`{"comment":"again"}`))
		req.Header.Set("X-Actor", c.actor)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatal(err)
		}
		resp.Body.Close()
		n := nodeEntry(t, e, id)
		if c.path == "unpause" && (n["paused_at"] != nil || n["paused_actor"] != nil) {
			t.Fatalf("after unpause: %v", n)
		}
		if c.path == "pause" && n["paused_actor"] != "operator-2" {
			t.Fatalf("after the second pause: %v", n["paused_actor"])
		}
	}
}

func TestZoneFacts_ExcludedSaysWhoAddedTheMatchingEntry(t *testing.T) {
	e, au, setExclude := zoneEnv(t)
	const id = "02:00:00:00:00:01" // registers as console-1
	register(t, e, id)
	ts := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC).UnixMilli()
	raw := func(l ...string) json.RawMessage { b, _ := json.Marshal(l); return b }
	for _, x := range []audit.Entry{
		{TS: ts - 60_000, Actor: "early", Section: "exclude_nodes", Before: raw(), After: raw("console-*")},
		{TS: ts - 30_000, Actor: "undo", Section: "exclude_nodes", Before: raw("console-*"), After: raw()},
		{TS: ts, Actor: "operator-3", Section: "exclude_nodes", Before: raw(), After: raw("console-*")},
		{TS: ts + 60_000, Actor: "other", Section: "exclude_nodes", Before: raw("console-*"), After: raw("console-*", "rack-b-*")},
		{TS: ts + 90_000, Actor: "profiles", Section: "profiles", After: raw("console-*")},
	} {
		au.Record(context.Background(), x)
	}
	setExclude("console-*")
	n := nodeEntry(t, e, id)
	if n["excluded"] != true || n["excluded_by"] != "console-*" || n["excluded_actor"] != "operator-3" || n["excluded_at"] != isoTime(time.UnixMilli(ts)) {
		t.Fatalf("%v %v %v %v", n["excluded"], n["excluded_by"], n["excluded_actor"], n["excluded_at"])
	}
	// An entry the audit log never saw added (an old import): no who, no when.
	setExclude("console-1")
	if n := nodeEntry(t, e, id); n["excluded"] != true || n["excluded_actor"] != nil || n["excluded_at"] != nil {
		t.Fatalf("%v", n)
	}
}

func TestZoneFacts_MemberSetAsideAndFreeAtFromTheStatus(t *testing.T) {
	e := newEnv(t, chatOK)
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	st := &scheduler.Status{Time: now,
		Instances: []scheduler.InstanceStatus{{Profile: "gemma-27b", State: "ready", Host: scheduler.HostOnNode, HostNode: "n1",
			Nodes: []scheduler.PlacedNodeStatus{{ID: "n1", Role: scheduler.RoleHost, Share: .4}, {ID: "n2", Role: scheduler.RoleHelper, Share: .35},
				{ID: "n3", Role: scheduler.RoleHelper, Share: .25}}}},
		Quarantines: []scheduler.QuarantineStatus{{Node: "n5", Until: now.Add(20 * time.Minute), Level: 2, Reason: scheduler.SetAsidePreflight,
			Message: "pre-flight: low memory", Profile: "qwen3-14b", Since: now.Add(-time.Minute)}},
		View: scheduler.ViewStatus{Nodes: []scheduler.NodeStatus{
			{ID: "n4", ClaimedBy: "qwen3-14b", FreeBasis: scheduler.FreeSettling, FreeAt: now.Add(12 * time.Second)},
			{ID: "n6", ClaimedBy: "qwen3-14b", FreeBasis: scheduler.FreeDraining},
		}},
	}
	sum := 0.0
	for _, id := range []string{"n1", "n2", "n3"} {
		z := e.api.zoneFactsOf(id, st, false, "")
		if z.Member == nil || z.Member.Profile != "gemma-27b" || z.Member.Host != "node" || z.Member.HostNode != "n1" {
			t.Fatalf("%s: %+v", id, z.Member)
		}
		sum += z.Member.Share
	}
	if z := e.api.zoneFactsOf("n1", st, false, ""); z.Member.Role != "host" || z.Member.Share != .4 {
		t.Fatalf("%+v", z.Member)
	}
	if sum < .999999 || sum > 1.000001 {
		t.Fatalf("shares add up to %v", sum)
	}
	z := e.api.zoneFactsOf("n5", st, false, "")
	if s := z.SetAside; s == nil || s.Reason != "preflight_failed" || s.Message != "pre-flight: low memory" || s.Profile != "qwen3-14b" ||
		s.Until != isoTime(now.Add(20*time.Minute)) || s.Since != isoTime(now.Add(-time.Minute)) || s.Level != 2 || z.Member != nil {
		t.Fatalf("%+v", z.SetAside)
	}
	z = e.api.zoneFactsOf("n4", st, false, "")
	if z.FreeBasis != "settling" || z.Next != "qwen3-14b" || z.FreeAt != isoTime(now.Add(12*time.Second)) || z.FreeInS == nil || *z.FreeInS != 12 {
		t.Fatalf("%+v", z)
	}
	z = e.api.zoneFactsOf("n6", st, false, "")
	if z.FreeBasis != "draining" || z.FreeAt != "" || z.FreeInS != nil || z.Next != "qwen3-14b" {
		t.Fatalf("%+v", z)
	}
	// The status view carries the scheduler's own new fields as they are.
	v := jsonView(*st).(map[string]any)
	in := v["instances"].([]any)[0].(map[string]any)
	m := in["nodes"].([]any)[0].(map[string]any)
	if in["host"] != "node" || in["host_node"] != "n1" || m["role"] != "host" || m["share"] != .4 {
		t.Fatalf("%v %v", in, m)
	}
	q := v["quarantines"].([]any)[0].(map[string]any)
	if q["reason"] != "preflight_failed" || q["profile"] != "qwen3-14b" {
		t.Fatalf("%v", q)
	}
}
