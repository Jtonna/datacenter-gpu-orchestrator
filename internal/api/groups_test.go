package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"strconv"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/groups"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

func (e *env) send(method, path, body string) *http.Response {
	req, _ := http.NewRequest(method, e.front.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func registerNamed(t *testing.T, e *env, id, name string) {
	t.Helper()
	req := agentproto.RegisterRequest{ID: id, Name: name, AgentRelease: "0.1.0", AgentStartID: "s-" + id, OS: "linux", Arch: "amd64",
		Install: "bare", Addr: "192.0.2.10" + id[len(id)-1:], Backends: []string{"vulkan"}}
	req.FillEmpty()
	b, _ := json.Marshal(req)
	if resp := e.post(agentproto.PathRegister, string(b)); resp.StatusCode != 200 {
		t.Fatalf("register %s: %d %s", name, resp.StatusCode, body(t, resp))
	}
}

func groupsEnv(t *testing.T) (*env, *pool.Pool, *memAudit) {
	e, p, au := agentsEnv(t)
	registerNamed(t, e, "02:00:00:00:00:01", "zone-1")
	registerNamed(t, e, "02:00:00:00:00:02", "zone-2")
	registerNamed(t, e, "02:00:00:00:00:03", "zone-3")
	return e, p, au
}

type groupsDoc struct {
	Revision  int64 `json:"revision"`
	Groups    []GroupSummary
	Ungrouped GroupSummary `json:"ungrouped"`
}

func (d *groupsDoc) UnmarshalJSON(b []byte) error {
	var raw struct {
		Revision  int64          `json:"revision"`
		Groups    []GroupSummary `json:"groups"`
		Ungrouped GroupSummary   `json:"ungrouped"`
	}
	err := json.Unmarshal(b, &raw)
	d.Revision, d.Groups, d.Ungrouped = raw.Revision, raw.Groups, raw.Ungrouped
	return err
}

func getGroups(t *testing.T, e *env) groupsDoc {
	t.Helper()
	var d groupsDoc
	if err := json.Unmarshal([]byte(body(t, e.get("/api/groups"))), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func status(t *testing.T, resp *http.Response, want int) string {
	t.Helper()
	b := body(t, resp)
	if resp.StatusCode != want {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, b)
	}
	return b
}

func nodeGroup(t *testing.T, e *env, id string) string {
	var f struct {
		Nodes []struct {
			ID    string `json:"id"`
			Group string `json:"group"`
		} `json:"nodes"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/nodes"))), &f)
	for _, n := range f.Nodes {
		if n.ID == id {
			return n.Group
		}
	}
	return "?"
}

// Create, add, move, rename, remove and delete, each audited as the
// default user; /api/nodes names each zone's group.
func TestGroups_Routes(t *testing.T) {
	e, _, au := groupsEnv(t)
	status(t, e.post("/api/groups", `{"name":"rack-a","members":["02:00:00:00:00:01","02:00:00:00:00:02"],"comment":"first rack"}`), http.StatusCreated)
	status(t, e.post("/api/groups", `{"name":"RACK-A"}`), http.StatusConflict)
	status(t, e.post("/api/groups", `{"name":"rack a"}`), http.StatusBadRequest)
	status(t, e.post("/api/groups", `{"name":"home","members":["02:00:00:00:00:09"]}`), http.StatusBadRequest) // not a zone Potluck knows
	status(t, e.post("/api/groups", `{"name":"home"}`), http.StatusCreated)
	d := getGroups(t, e)
	if len(d.Groups) != 2 || d.Groups[0].Name != "rack-a" || d.Groups[1].Name != "home" || d.Groups[0].Count != 2 ||
		strings.Join(d.Groups[0].MemberNames, ",") != "zone-1,zone-2" || d.Ungrouped.Count != 2 || d.Ungrouped.Members[0] != "02:00:00:00:00:03" /* and the scheduler's own machine */ {
		t.Fatalf("%+v", d)
	}
	if nodeGroup(t, e, "02:00:00:00:00:01") != "rack-a" || nodeGroup(t, e, "02:00:00:00:00:03") != "" {
		t.Fatal("group on /api/nodes")
	}
	b := status(t, e.post("/api/groups/home/members", `{"nodes":["02:00:00:00:00:02","02:00:00:00:00:03"],"comment":"move"}`), http.StatusOK)
	if !strings.Contains(b, `"moved":{"02:00:00:00:00:02":"rack-a"}`) {
		t.Fatalf("move answer %s", b)
	}
	status(t, e.send("PATCH", "/api/groups/home", `{"name":"desk"}`), http.StatusOK)
	status(t, e.send("PATCH", "/api/groups/nope", `{"name":"x"}`), http.StatusNotFound)
	status(t, e.send("DELETE", "/api/groups/desk/members", `{"nodes":["02:00:00:00:00:03"]}`), http.StatusOK)
	if nodeGroup(t, e, "02:00:00:00:00:03") != "" || nodeGroup(t, e, "02:00:00:00:00:02") != "desk" {
		t.Fatal("after remove")
	}
	status(t, e.send("DELETE", "/api/groups/desk", `{}`), http.StatusOK)
	status(t, e.send("DELETE", "/api/groups/desk", `{}`), http.StatusNotFound)
	if d := getGroups(t, e); len(d.Groups) != 1 || d.Ungrouped.Count != 3 {
		t.Fatalf("after delete %+v", d)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var acts []string
	for _, en := range au.es {
		if en.Section != SectionGroups {
			continue
		}
		acts = append(acts, en.Action+" "+en.Target)
		if en.Actor != audit.DefaultActor {
			t.Errorf("actor %q", en.Actor)
		}
	}
	want := "create group:rack-a,create group:home,add_members group:home,rename group:desk,remove_members group:desk,delete group:desk"
	if strings.Join(acts, ",") != want {
		t.Fatalf("audit %v", acts)
	}
	if au.es[0].Comment != "first rack" {
		t.Errorf("comment %q", au.es[0].Comment)
	}
}

// "Accepting models" on a group pauses or unpauses each member, with one
// audit entry per member, and the group says all, none or mixed.
func TestGroups_PauseUnpause(t *testing.T) {
	e, p, au := groupsEnv(t)
	status(t, e.post("/api/groups", `{"name":"rack-a","members":["02:00:00:00:00:01","02:00:00:00:00:02"]}`), http.StatusCreated)
	if g := getGroups(t, e).Groups[0]; g.Accepting != AcceptingAll {
		t.Fatalf("accepting %q", g.Accepting)
	}
	status(t, e.post("/api/groups/rack-a/pause", `{}`), http.StatusBadRequest) // a comment is required, as for one zone
	status(t, e.post("/api/groups/rack-a/pause", `{"comment":"maintenance"}`), http.StatusOK)
	paused := 0
	for _, f := range p.Facts() {
		if f.Paused {
			paused++
		}
	}
	if paused != 2 || getGroups(t, e).Groups[0].Accepting != AcceptingNone {
		t.Fatalf("paused %d", paused)
	}
	n := 0
	au.mu.Lock()
	for _, en := range au.es {
		if en.Section == SectionNodes && en.Action == "pause" && en.Comment == "maintenance (group rack-a)" {
			n++
		}
	}
	au.mu.Unlock()
	if n != 2 {
		t.Fatalf("pause entries %d", n)
	}
	p.SetPaused("02:00:00:00:00:01", false)
	if g := getGroups(t, e).Groups[0]; g.Accepting != AcceptingMixed {
		t.Fatalf("accepting %q", g.Accepting)
	}
	status(t, e.post("/api/groups/rack-a/unpause", `{"comment":"back"}`), http.StatusOK)
	if g := getGroups(t, e).Groups[0]; g.Accepting != AcceptingAll {
		t.Fatalf("accepting %q", g.Accepting)
	}
}

// The groups topic sends the document when a client subscribes and again
// only when the groups change.
func TestGroups_WSOnlyOnChange(t *testing.T) {
	e, _, _ := groupsEnv(t)
	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{TopicGroups}})
	count := func(d time.Duration) int {
		n := 0
		for _, m := range c.collect(d) {
			if m.Type == TopicGroups {
				n++
			}
		}
		return n
	}
	if n := count(300 * time.Millisecond); n != 1 {
		t.Fatalf("on subscribe: %d", n)
	}
	if n := count(time.Second); n != 0 {
		t.Fatalf("idle: %d", n)
	}
	status(t, e.post("/api/groups", `{"name":"rack-a","members":["02:00:00:00:00:01"]}`), http.StatusCreated)
	if n := count(500 * time.Millisecond); n != 1 {
		t.Fatalf("after a change: %d", n)
	}
	status(t, e.post("/api/groups/rack-a/members", `{"nodes":["02:00:00:00:00:01"]}`), http.StatusOK) // already there: no change
	if n := count(500 * time.Millisecond); n != 0 {
		t.Fatalf("after no change: %d", n)
	}
}

// The group figures: counts by zone state, memory, temperature, the zones
// that need attention and the models that touch the group.
func TestGroups_Summary(t *testing.T) {
	temp := func(c float64) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"stats_version": 1, "headline_temp_c": c})
		return b
	}
	facts := []pool.NodeFacts{
		{ID: "a", Name: "zone-a", State: pool.StateActive, Stats: temp(50)},
		{ID: "b", Name: "zone-b", State: pool.StateActive, Stats: temp(70)},
		{ID: "c", Name: "zone-c", State: pool.StateLost},
		{ID: "d", Name: "zone-d", State: pool.StateActive, Paused: true, Stats: temp(60)},
	}
	st := &scheduler.Status{View: scheduler.ViewStatus{Nodes: []scheduler.NodeStatus{
		{ID: "a", Hostname: "zone-a", Online: true, TotalMiB: 14000, FreeMiB: 4000, Holder: "m1"},
		{ID: "b", Hostname: "zone-b", Online: true, TotalMiB: 14000, FreeMiB: 14000},
		{ID: "d", Hostname: "zone-d", Online: true, TotalMiB: 14000, FreeMiB: 14000},
	}}, Instances: []scheduler.InstanceStatus{{Profile: "m1", State: "ready", Parallel: 1, Nodes: []scheduler.PlacedNodeStatus{{ID: "a"}}}}}
	g := summarizeGroup(groups.Group{Name: "rack-a", Members: []string{"a", "b", "c", "d"}}, indexZones(st, facts))
	if g.Count != 4 || g.Counts.InUse != 1 || g.Counts.Free != 1 || g.Counts.Lost != 1 || g.Counts.Off != 1 || g.Counts.Down != 1 ||
		g.Counts.Reachable != 3 || g.Counts.Total != 4 {
		t.Fatalf("counts %+v", g.Counts)
	}
	if g.Memory.TotalMiB != 42000 || g.Memory.UsedMiB != 10000 || g.Memory.EachTotalMiB == nil || *g.Memory.EachTotalMiB != 14000 {
		t.Fatalf("memory %+v", g.Memory)
	}
	if g.Temp.AvgC == nil || *g.Temp.AvgC != 60 || *g.Temp.HottestC != 70 || g.Temp.HottestName != "zone-b" {
		t.Fatalf("temp %+v", g.Temp)
	}
	if len(g.NeedsAttention) != 1 || g.NeedsAttention[0].Name != "zone-c" || g.NeedsAttention[0].State != "lost" {
		t.Fatalf("attention %+v", g.NeedsAttention)
	}
	if len(g.Models) != 1 || g.Models[0].Profile != "m1" || g.Models[0].Zones != 1 || g.Accepting != AcceptingMixed {
		t.Fatalf("models %+v accepting %q", g.Models, g.Accepting)
	}
}

// One set of counts: GET /api/nodes answers the counts the Zones row of
// /api/health has, and each zone the state it was counted in.
func TestZoneCounts_NodesAndHealthAgree(t *testing.T) {
	e, p, _ := groupsEnv(t)
	p.SetPaused("02:00:00:00:00:02", true)
	var feed struct {
		Counts ZoneCounts `json:"counts"`
		Nodes  []struct {
			ID        string `json:"id"`
			ZoneState string `json:"zone_state"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/nodes"))), &feed); err != nil {
		t.Fatal(err)
	}
	var h struct {
		Rows []struct {
			ID    string         `json:"id"`
			Facts map[string]any `json:"facts"`
		} `json:"rows"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/health"))), &h)
	var zones map[string]any
	for _, r := range h.Rows {
		if r.ID == "zones" {
			zones = r.Facts
		}
	}
	c := feed.Counts
	if c.Total != 4 || c.Off != 1 || c.Up != 3 || c.Reachable != 4 || c.Down != 0 { // three agents and the test scheduler's own machine (in its view, not a pool node here)
		t.Fatalf("counts %+v", c)
	}
	for k, v := range map[string]int{"total": c.Total, "reachable": c.Reachable, "up": c.Up, "down": c.Down, "lost": c.Lost,
		"set_aside": c.SetAside, "off": c.Off, "in_use": c.InUse, "free": c.Free, "loading": c.Loading} {
		if f, _ := zones[k].(float64); int(f) != v {
			t.Errorf("health %s = %v, nodes %d", k, zones[k], v)
		}
	}
	states := map[string]string{}
	for _, n := range feed.Nodes {
		states[n.ID] = n.ZoneState
	}
	if states["02:00:00:00:00:02"] != "off" || states["02:00:00:00:00:01"] != "free" {
		t.Fatalf("zone states %v", states)
	}
}

// The attention order: the same rank on each zone and in ?sort=attention.
func TestZones_AttentionOrder(t *testing.T) {
	if attentionRank("lost", "", false, false) != RankDown || attentionRank("free", "", false, true) != RankDown ||
		attentionRank("set_aside", "", false, false) != RankTrouble || attentionRank("free", "no_tier_match", false, false) != RankTrouble ||
		attentionRank("loading", "", false, false) != RankLoading || attentionRank("in_use", "", false, false) != RankInUse ||
		attentionRank("off", "paused", false, false) != RankPaused || attentionRank("free", "", false, false) != RankFree ||
		attentionRank("off", "excluded", true, false) != RankExcluded || attentionRank("removed", "", false, false) != RankRemoved ||
		attentionRank("free", "host_machine", false, false) != RankFree {
		t.Fatal("ranks")
	}
	e, p, _ := groupsEnv(t)
	p.SetPaused("02:00:00:00:00:01", true)
	order := func(q string) []string {
		var f struct {
			Nodes []struct {
				Name string `json:"name"`
				Rank int    `json:"attention_rank"`
			} `json:"nodes"`
		}
		json.Unmarshal([]byte(status(t, e.get("/api/nodes"+q), 200)), &f)
		var out []string
		for _, n := range f.Nodes {
			out = append(out, n.Name+":"+strconv.Itoa(n.Rank))
		}
		return out
	}
	// The test agents match no tier: each is kept out for a fault (rank 1),
	// except the paused one.
	if got := strings.Join(order("?sort=attention"), " "); got != "zone-2:1 zone-3:1 zone-1:4" {
		t.Fatalf("attention: %s", got)
	}
	if got := strings.Join(order("?sort=name"), " "); got != "zone-1:4 zone-2:1 zone-3:1" {
		t.Fatalf("name: %s", got)
	}
	status(t, e.get("/api/nodes?sort=size"), 400)
}

// The chip is the stats' GPU name; a group's chip is common or "mixed".
func TestZones_Chip(t *testing.T) {
	gpu := func(names ...string) json.RawMessage {
		var gs []map[string]any
		for _, n := range names {
			gs = append(gs, map[string]any{"id": "g", "vendor": "nvidia", "name": n})
		}
		b, _ := json.Marshal(map[string]any{"stats_version": 1, "gpus": gs})
		return b
	}
	if chipOf(gpu("RTX 5060")) != "RTX 5060" || chipOf(gpu("A", "A", "B")) != "A + B" || chipOf(gpu("")) != "" || chipOf(nil) != "" {
		t.Fatal("chipOf")
	}
	for in, want := range map[string]string{"a,a": "a", "a,b": ChipMixed, "a,": ChipMixed, ",": "", "": ""} {
		var cs []string
		if in != "" {
			cs = strings.Split(in, ",")
		}
		if got := groupChip(cs); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	facts := []pool.NodeFacts{{ID: "a", Name: "a", State: pool.StateActive, Stats: gpu("RTX 3070 Ti")}, {ID: "b", Name: "b", State: pool.StateActive, Stats: gpu("RTX 3070 Ti")}}
	if g := summarizeGroup(groups.Group{Name: "row", Members: []string{"a", "b"}}, indexZones(nil, facts)); g.Chip != "RTX 3070 Ti" {
		t.Fatalf("group chip %q", g.Chip)
	}
}
