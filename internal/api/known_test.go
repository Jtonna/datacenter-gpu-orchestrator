package api

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// A machine known from an earlier run whose agent has not registered since
// the start: during the grace no route lists it; after the grace /api/nodes
// and /api/status agents list it as lost with its last seen, /api/health
// counts it down, and the availability sample is lost. The machine that
// is connected is unchanged, and the scheduler's view does not hold the
// known one (placement and plans never see it).
func TestKnown_RoutesListOffMachine(t *testing.T) {
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT-BINARY")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("RPC-SERVER")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d}
	var off atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(off.Load())) }
	p, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: rel},
		PotluckRelease: "0.1.0", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, chatOK, func(o *Options) { o.Pool, o.Audit = p, &memAudit{} })
	p.SetForceUnload(e.sched.ForceUnloadNode)
	register(t, e, "02:00:00:00:00:01")
	seen := clock().Add(-70 * time.Minute)
	p.SetKnown([]pool.KnownNode{{ID: "02:00:00:00:00:02", Name: "console-2", LastSeen: seen},
		{ID: "02:00:00:00:00:01", Name: "console-1", LastSeen: seen}}) // the connected one is listed once, as its agent

	type node struct {
		ID               string   `json:"id"`
		Name             string   `json:"name"`
		State            string   `json:"state"`
		Placeable        bool     `json:"placeable"`
		LastSeenAt       string   `json:"last_seen_at"`
		LastSeenAgoS     *float64 `json:"last_seen_ago_s"`
		AbsentSinceStart bool     `json:"absent_since_start"`
	}
	feed := func() map[string]node {
		var f struct {
			Nodes []node `json:"nodes"`
		}
		json.Unmarshal([]byte(body(t, e.get("/api/nodes"))), &f)
		m := map[string]node{}
		for _, n := range f.Nodes {
			m[n.ID] = n
		}
		return m
	}
	status := func() (map[string]node, []string) {
		var s struct {
			Agents []node `json:"agents"`
			View   struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"view"`
		}
		json.Unmarshal([]byte(body(t, e.get("/api/status"))), &s)
		m := map[string]node{}
		for _, n := range s.Agents {
			m[n.ID] = n
		}
		var view []string
		for _, n := range s.View.Nodes {
			view = append(view, n.ID)
		}
		return m, view
	}
	zones := func() HealthRow {
		var h HealthSummary
		json.Unmarshal([]byte(body(t, e.get("/api/health"))), &h)
		for _, r := range h.Rows {
			if r.ID == "zones" {
				return r
			}
		}
		t.Fatal("no zones row")
		return HealthRow{}
	}

	if _, ok := feed()["02:00:00:00:00:02"]; ok {
		t.Fatal("/api/nodes lists the known machine during the grace")
	}
	before := zones()
	n, _ := before.Facts["total"].(float64)
	if before.State != HealthOK || before.Count != 0 || n < 1 {
		t.Fatalf("health during the grace: %+v", before)
	}

	off.Store(int64(pool.KnownGrace))
	f := feed()
	k := f["02:00:00:00:00:02"]
	if len(f) != 2 || k.State != pool.StateLost || k.Name != "console-2" || k.Placeable || !k.AbsentSinceStart ||
		k.LastSeenAgoS == nil || *k.LastSeenAgoS < 70*60 {
		t.Fatalf("/api/nodes: %+v", f)
	}
	if at, err := time.Parse(time.RFC3339Nano, k.LastSeenAt); err != nil || !at.Equal(seen) {
		t.Fatalf("last_seen_at %q: %v", k.LastSeenAt, err)
	}
	if c := f["02:00:00:00:00:01"]; c.State != pool.StateActive || c.AbsentSinceStart || c.LastSeenAt != "" {
		t.Fatalf("the connected machine changed: %+v", c)
	}
	agents, view := status()
	if a := agents["02:00:00:00:00:02"]; a.State != pool.StateLost || !a.AbsentSinceStart || a.LastSeenAt == "" {
		t.Fatalf("/api/status agents: %+v", agents)
	}
	for _, id := range view {
		if id == "02:00:00:00:00:02" {
			t.Fatal("the scheduler's view holds the known machine")
		}
	}
	if r, want := zones(), fmt.Sprintf("%d of %d reachable · 1 down", int(n), int(n)+1); r.State != HealthProblem || r.Text != want || r.Count != 1 {
		t.Fatalf("health after the grace: %+v", r)
	}
	st, _ := e.sched.Status(t.Context())
	for _, smp := range availability.Classify(&st, p.Facts()) {
		if smp.Node == "02:00:00:00:00:02" && (smp.State != availability.Lost || smp.Reachable) {
			t.Fatalf("availability sample: %+v", smp)
		}
	}
}
