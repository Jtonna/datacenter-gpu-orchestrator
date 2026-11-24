package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/diskstat"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

const gb = int64(1_000_000_000)

func viewNode(id string, online bool, quarantined bool, excluded bool) scheduler.NodeStatus {
	n := scheduler.NodeStatus{ID: core.NodeID(id), Hostname: id, Online: online, Excluded: excluded}
	if quarantined {
		n.Quarantined = time.Unix(1_800_000_000, 0)
	}
	return n
}

func statusOf(nodes ...scheduler.NodeStatus) *scheduler.Status {
	st := &scheduler.Status{}
	st.View.Nodes = nodes
	return st
}

// Zones: down = lost or set aside (C2); switched off is reachable and not
// down; removed zones are left out; no machines is unknown.
func TestHealth_ZonesRow(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		name  string
		st    *scheduler.Status
		facts []pool.NodeFacts
		state string
		text  string
		count int64
	}{
		{"status not read", nil, nil, HealthUnknown, "Status not read yet", 0},
		{"no machines", statusOf(), nil, HealthUnknown, "No zones yet", 0},
		{"all reachable", statusOf(viewNode("ps5-01", true, false, false), viewNode("ps5-02", true, false, false)), nil, HealthOK, "2 of 2 reachable", 0},
		{"off is not down", statusOf(viewNode("ps5-01", true, false, false), viewNode("ps5-02", true, false, true)), nil, HealthOK, "2 of 2 reachable", 0},
		{"lost and set aside are down", statusOf(viewNode("ps5-01", true, false, false), viewNode("ps5-02", false, false, false), viewNode("ps5-03", true, true, false)),
			nil, HealthProblem, "1 of 3 reachable · 2 down", 2},
		{"removed left out, paused off", statusOf(), []pool.NodeFacts{{ID: "a", Name: "zone-a", State: pool.StateActive}, {ID: "b", Name: "zone-b", State: pool.StateRemoved},
			{ID: "c", Name: "zone-c", State: pool.StateActive, Paused: true}, {ID: "d", Name: "zone-d", State: pool.StateLost}}, HealthProblem, "2 of 3 reachable · 1 down", 1},
	} {
		row := judgeZones(c.st, c.facts, true, at)
		if row.State != c.state || row.Text != c.text || row.Count != c.count || row.ID != "zones" || row.Label != "Zones" || row.Link == "" {
			t.Errorf("%s: %+v", c.name, row)
		}
	}
	row := judgeZones(statusOf(viewNode("ps5-01", false, false, false)), nil, false, at)
	if row.Facts["status_fresh"] != false || row.Facts["status_at"] == nil || row.Facts["down_names"].([]string)[0] != "ps5-01" {
		t.Fatalf("stale status facts: %+v", row.Facts)
	}
}

func TestHealth_RunsRow(t *testing.T) {
	for _, c := range []struct {
		name  string
		in    runsIn
		state string
		text  string
		count int64
	}{
		{"request log off", runsIn{benchFailed: 1}, HealthUnknown, "Request log is off", 0},
		{"none", runsIn{logOn: true, total: 40}, HealthOK, "None failed in the last hour", 0},
		{"one request", runsIn{logOn: true, failed: 1, total: 40}, HealthAttention, "1 failed in the last hour", 1},
		{"one benchmark run", runsIn{logOn: true, benchFailed: 1}, HealthAttention, "1 failed in the last hour", 1},
		{"many, but few of the hour", runsIn{logOn: true, failed: 6, total: 40}, HealthAttention, "6 failed in the last hour", 6},
		{"most of the hour", runsIn{logOn: true, failed: 6, total: 8}, HealthProblem, "6 failed in the last hour", 6},
		{"four of four is attention", runsIn{logOn: true, failed: 4, total: 4}, HealthAttention, "4 failed in the last hour", 4},
		{"more than a page", runsIn{logOn: true, failed: 500, total: 500, capped: true}, HealthProblem, "500+ failed in the last hour", 500},
	} {
		if row := judgeRuns(c.in); row.State != c.state || row.Text != c.text || row.Count != c.count {
			t.Errorf("%s: %+v", c.name, row)
		}
	}
}

func TestHealth_SchedulerRow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ok := schedIn{version: "v31-abc1234", uptime: 12*24*time.Hour + time.Hour, now: now, statusFresh: true,
		loop: scheduler.LoopStats{WarnMs: 1000, Max10mMs: 30}, tasks: []supervise.TaskStats{{Name: "api.ws.status", Policy: "restart", State: "running"}}}
	with := func(f func(*schedIn)) schedIn { c := ok; f(&c); return c }
	for _, c := range []struct {
		name  string
		in    schedIn
		state string
		text  string
	}{
		{"ok", ok, HealthOK, "Running · release v31-abc1234 · up 12 d"},
		{"no version", with(func(c *schedIn) { c.version = ""; c.uptime = 90 * time.Second }), HealthOK, "Running · release unknown · up 1 min"},
		{"old task failure", with(func(c *schedIn) {
			c.tasks = []supervise.TaskStats{{Name: "x", Policy: "restart", State: "running", LastFailureAt: now.Add(-2 * time.Hour).UnixMilli()}}
		}), HealthOK, "Running · release v31-abc1234 · up 12 d"},
		{"task failed this hour", with(func(c *schedIn) {
			c.tasks = []supervise.TaskStats{{Name: "x", Policy: "restart", State: "running", LastFailureAt: now.Add(-10 * time.Minute).UnixMilli()}}
		}), HealthAttention, "1 task failed in the last hour · up 12 d"},
		{"slow step in 10 min", with(func(c *schedIn) { c.loop.Max10mMs = 1400 }), HealthAttention, "Slowest step 1.4 s in 10 min · up 12 d"},
		{"step at the threshold", with(func(c *schedIn) { c.loop.Max10mMs = 1000 }), HealthOK, "Running · release v31-abc1234 · up 12 d"},
		{"read panic", with(func(c *schedIn) { c.readPanics = 2 }), HealthAttention, "2 read panics · up 12 d"},
		{"newer settings", with(func(c *schedIn) { c.writesNeedForce = true }), HealthAttention, "Stored settings from a newer release · up 12 d"},
		{"busy now", with(func(c *schedIn) { c.loop.BusyNowMs = 6000; c.statusFresh = false }), HealthProblem, "Core loop busy 6.0 s on one step · up 12 d"},
		{"not answering", with(func(c *schedIn) { c.statusFresh = false }), HealthProblem, "Core loop not answering · up 12 d"},
		{"essential task stopped", with(func(c *schedIn) {
			c.tasks = []supervise.TaskStats{{Name: "scheduler.loop", Policy: "exit", State: "failed", LastFailureAt: now.UnixMilli()}}
		}), HealthProblem, "1 essential task stopped · 1 task failed in the last hour · up 12 d"},
	} {
		if row := judgeScheduler(c.in); row.State != c.state || row.Text != c.text {
			t.Errorf("%s: %s %q", c.name, row.State, row.Text)
		}
	}
	if row := judgeScheduler(ok); row.Facts["release"] != "v31-abc1234" || row.Facts["uptime_s"] != int64(12*24*3600+3600) || row.Link != "#/about" {
		t.Fatalf("facts %+v", row)
	}
}

func TestHealth_MCPRow(t *testing.T) {
	if r := judgeMCP(3, 0, true); r.State != HealthOK || r.Text != "Running · 3 keys active" || r.Count != 0 || r.Facts["keys_active"] != 3 {
		t.Fatalf("%+v", r)
	}
	if r := judgeMCP(1, 0, true); r.Text != "Running · 1 key active" {
		t.Fatalf("%+v", r)
	}
	if r := judgeMCP(3, 1, true); r.State != HealthAttention || r.Count != 1 || r.Text != "Running · 3 keys active · 1 expires soon" || r.Facts["keys_expiring"] != 1 {
		t.Fatalf("%+v", r)
	}
	if r := judgeMCP(0, 0, false); r.State != HealthOK || r.Text != "Running" {
		t.Fatalf("%+v", r)
	}
}

func disk(name string, total, used int64, state string) diskstat.Entry {
	e := diskstat.Entry{Name: name, Path: "/srv/" + name, State: state, SameDiskAs: []string{}}
	if state == diskstat.StateOK || state == diskstat.StateTimeout {
		e = readEntry(e, total, used)
	}
	return e
}

// readEntry gives an entry figures as a read that answered would.
func readEntry(e diskstat.Entry, total, used int64) diskstat.Entry {
	r := diskstat.New(diskstat.Options{Folders: []diskstat.Folder{{Name: e.Name, Path: e.Path}},
		Stat: func(string) (diskstat.FS, error) {
			return diskstat.FS{Total: uint64(total), Used: uint64(used), Free: uint64(total - used)}, nil
		},
		Files: func(string) (int64, int) { return used / 2, 3 }})
	r.Pass(context.Background())
	got, _ := r.Folder(e.Name)
	got.State = e.State
	return got
}

func TestHealth_StorageRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		folders []diskstat.Entry
		budget  int64
		state   string
		text    string
		count   int64
	}{
		{"nothing to watch", nil, 0, HealthUnknown, "No folders to watch", 0},
		{"not read yet", []diskstat.Entry{disk("data", 0, 0, diskstat.StateWaiting)}, 0, HealthUnknown, "Disk figures not read yet", 0},
		{"platform", []diskstat.Entry{disk("data", 0, 0, diskstat.StateUnknown)}, 0, HealthUnknown, "Disk figures not read on this platform", 0},
		{"the mockup", []diskstat.Entry{disk("data", 240*gb, 50_300_000_000, diskstat.StateOK)}, 0, HealthOK, "50.3 of 240 GB used", 0},
		{"fullest shown", []diskstat.Entry{disk("data", 240*gb, 50*gb, diskstat.StateOK), disk("models", 1000*gb, 800*gb, diskstat.StateOK)}, 0, HealthOK, "800 of 1000 GB used", 0},
		{"90 percent", []diskstat.Entry{disk("data", 1000*gb, 900*gb, diskstat.StateOK)}, 0, HealthAttention, "900 of 1000 GB used", 1},
		{"under 10 GB free", []diskstat.Entry{disk("data", 60*gb, 51*gb, diskstat.StateOK)}, 0, HealthAttention, "51.0 of 60.0 GB used · 9.0 GB free", 1},
		{"97 percent", []diskstat.Entry{disk("data", 1000*gb, 970*gb, diskstat.StateOK)}, 0, HealthProblem, "970 of 1000 GB used", 1},
		{"under 2 GB free", []diskstat.Entry{disk("data", 20*gb, 18_500_000_000, diskstat.StateOK)}, 0, HealthProblem, "18.5 of 20.0 GB used · 1.5 GB free", 1},
		{"a hung share keeps its figures", []diskstat.Entry{disk("data", 240*gb, 50*gb, diskstat.StateOK), disk("models", 500*gb, 100*gb, diskstat.StateTimeout)}, 0,
			HealthAttention, "Models folder not answering · 50.0 of 240 GB used", 1},
		{"a hung share never read", []diskstat.Entry{disk("models", 0, 0, diskstat.StateWaiting)}, 0, HealthUnknown, "Disk figures not read yet", 0},
		{"unreadable", []diskstat.Entry{disk("logs", 0, 0, diskstat.StateError)}, 0, HealthAttention, "Logs folder unreadable", 1},
		// the models folder's files are half of used (readEntry): 200 GB
		{"budget 90 percent", []diskstat.Entry{disk("models", 1000*gb, 400*gb, diskstat.StateOK)}, 220 * gb, HealthAttention, "models 200 of 220 GB budget · 400 of 1000 GB used", 1},
		{"over budget", []diskstat.Entry{disk("models", 1000*gb, 400*gb, diskstat.StateOK)}, 150 * gb, HealthProblem, "models 200 of 150 GB budget · 400 of 1000 GB used", 1},
		{"within budget", []diskstat.Entry{disk("models", 1000*gb, 400*gb, diskstat.StateOK)}, 400 * gb, HealthOK, "400 of 1000 GB used", 0},
	} {
		row := judgeStorage(c.folders, c.budget)
		if row.State != c.state || row.Text != c.text || row.Count != c.count {
			t.Errorf("%s: %s %q count %d", c.name, row.State, row.Text, row.Count)
		}
	}
	// the copy of live: 58 % used is no issue; the percent is in facts
	if row := judgeStorage([]diskstat.Entry{disk("data", 240*gb, 140*gb, diskstat.StateOK)}, 0); row.State != HealthOK || row.Count != 0 || row.Facts["used_percent"] != 58.3 {
		t.Fatalf("58 percent used: %s count %d facts %v", row.State, row.Count, row.Facts["used_percent"])
	}
	// two folders on one disk count once
	r := diskstat.New(diskstat.Options{Folders: []diskstat.Folder{{Name: "data", Path: "/a"}, {Name: "history", Path: "/b"}},
		Stat: func(string) (diskstat.FS, error) {
			return diskstat.FS{Total: uint64(100 * gb), Used: uint64(95 * gb), Free: uint64(5 * gb), Dev: 7}, nil
		}})
	r.Pass(context.Background())
	if row := judgeStorage(r.Folders(), 0); row.State != HealthAttention || row.Facts["fullest"] != "data" || row.Count != 1 || row.Facts["used_percent"] != 95.0 {
		t.Fatalf("same disk: %+v", row)
	}
}

func TestHealth_QueueRow(t *testing.T) {
	q := func(ages ...time.Duration) *scheduler.Status {
		st := &scheduler.Status{}
		for _, a := range ages {
			st.Queue = append(st.Queue, scheduler.EntryStatus{Profile: "p", Waiters: 1, Age: a})
		}
		return st
	}
	timeout := 600 * time.Second
	for _, c := range []struct {
		name  string
		st    *scheduler.Status
		state string
		text  string
	}{
		{"not read", nil, HealthUnknown, "Status not read yet"},
		{"empty", q(), HealthOK, "Nothing waiting"},
		{"the mockup", q(5*time.Minute+48*time.Second, 30*time.Second), HealthOK, "2 waiting · longest 5 min 48 s"},
		{"three quarters", q(450 * time.Second), HealthAttention, "1 waiting · longest 7 min 30 s"},
		{"nine tenths", q(540 * time.Second), HealthProblem, "1 waiting · longest 9 min 0 s"},
	} {
		row := judgeQueue(c.st, timeout)
		if row.State != c.state || row.Text != c.text {
			t.Errorf("%s: %s %q", c.name, row.State, row.Text)
		}
		if (row.State == HealthAttention || row.State == HealthProblem) != (row.Count > 0) {
			t.Errorf("%s: count %d at %s", c.name, row.Count, row.State)
		}
	}
}

func TestHealth_SummaryCountAndColour(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	row := func(id, state string) HealthRow { return HealthRow{ID: id, State: state} }
	for _, c := range []struct {
		rows                   []HealthRow
		state, color, sentence string
		issues                 int
	}{
		{[]HealthRow{row("zones", HealthProblem), row("runs", HealthAttention), row("scheduler", HealthOK), judgeAlerts()}, HealthProblem, "red", "2 things need attention", 2},
		{[]HealthRow{row("runs", HealthAttention), judgeAlerts()}, HealthAttention, "amber", "1 thing needs attention", 1},
		{[]HealthRow{row("zones", HealthOK), judgeAlerts()}, HealthOK, "green", "All good", 0},
		{[]HealthRow{row("zones", HealthUnknown), judgeAlerts()}, HealthUnknown, "grey", "Health not known yet", 0},
	} {
		s := summarize(c.rows, nil, now)
		if s.State != c.state || s.Color != c.color || s.Sentence != c.sentence || s.Issues != c.issues {
			t.Errorf("%+v", s)
		}
	}
	if a := judgeAlerts(); a.State != HealthUnknown || a.Text != "Not set up yet" || a.Link != "#/health/alerts" {
		t.Fatalf("alerts %+v", a)
	}
}

func TestHealth_ShortSentences(t *testing.T) {
	ev := func(typ, sev string, nodes []string, data map[string]any, summary string) history.Event {
		return history.Event{Ts: 1_800_000_000_000, Type: typ, Severity: sev, Nodes: nodes, Data: data, Summary: summary, Profile: "qwen-4b"}
	}
	for _, c := range []struct {
		e          history.Event
		text, node string
	}{
		{ev(history.TNodeQuarantined, "warn", []string{"ps5-05"}, map[string]any{"reason": scheduler.SetAsidePreflight, "until": int64(1_800_001_080_000)}, ""), "ps5-05 failed a load and was set aside", "ps5-05"},
		{ev(history.TNodeQuarantined, "warn", []string{"ps5-05"}, map[string]any{"reason": scheduler.SetAsideWorkerHung}, ""), "ps5-05 had a worker stop answering and was set aside", "ps5-05"},
		{ev(history.TNodeLost, "error", []string{"ra-36"}, nil, ""), "ra-36 went offline", "ra-36"},
		{ev(history.TNodeMembership, "error", []string{"ps5-06"}, map[string]any{"state": pool.StateLost}, "Node ps5-06 was lost: no heartbeat for 12s."), "ps5-06 went offline", "ps5-06"},
		{ev(history.TBenchRunFinished, "error", nil, map[string]any{"state": "failed", "bench_run": "r-12"}, ""), "Benchmark run r-12 failed", ""},
		{ev(history.TRequestRefused, "warn", nil, nil, ""), "A request for qwen-4b was refused", ""},
		{ev(history.TLoopSlowStep, "warn", nil, map[string]any{"ms": float64(1400)}, ""), "The core loop was busy 1.4 s on one step", ""},
		{ev("fleetViewStale", "warn", nil, nil, "The fleet view is 90 s old. Remote nodes are not used."), "The fleet view is 90 s old", ""},
	} {
		got := shortEvent(c.e)
		if got.Text != c.text || got.Node != c.node {
			t.Errorf("%s: %+v", c.e.Type, got)
		}
	}
	if got := shortEvent(ev(history.TNodeQuarantined, "warn", []string{"ps5-05"}, map[string]any{"until": float64(1_800_001_080_000)}, "")); got.Until != "2027-01-15T08:18:00.000Z" {
		t.Fatalf("until %q", got.Until)
	}
}

// The route with the request log off, a models share whose statfs hangs and
// one machine: it answers at once, each row says what it can.
func TestHealth_RouteUsageOffAndHungDisk(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e := newEnv(t, chatOK, func(o *Options) {
		o.System.DataDir, o.System.ModelsDir = "/srv/data", "/mnt/share"
		o.diskTimeout, o.diskEvery = 50*time.Millisecond, time.Hour
		o.diskStat = func(p string) (diskstat.FS, error) {
			if p == "/mnt/share" {
				<-release
			}
			return diskstat.FS{Total: uint64(240 * gb), Used: uint64(50 * gb), Free: uint64(190 * gb), Dev: 1}, nil
		}
		o.taskStats = func() []supervise.TaskStats { return nil }
	})
	eventually(t, "a disk pass", func() bool { return e.api.disks.Passes() >= 1 })
	t0 := time.Now()
	var sum HealthSummary
	if err := json.Unmarshal([]byte(body(t, e.get("/api/health"))), &sum); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("the summary took %v", d)
	}
	rows := map[string]HealthRow{}
	ids := []string{}
	for _, r := range sum.Rows {
		rows[r.ID] = r
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "zones,runs,scheduler,mcp,storage,queue,alerts" {
		t.Fatalf("rows %v", ids)
	}
	if r := rows["zones"]; r.State != HealthOK || r.Text != "1 of 1 reachable" {
		t.Errorf("zones %+v", r)
	}
	if r := rows["runs"]; r.State != HealthUnknown || r.Text != "Request log is off" {
		t.Errorf("runs %+v", r)
	}
	if r := rows["storage"]; r.State != HealthAttention || r.Text != "Models folder not answering · 50.0 of 240 GB used" {
		t.Errorf("storage %+v", r)
	}
	if r := rows["scheduler"]; r.State != HealthOK || !strings.HasPrefix(r.Text, "Running · release ") {
		t.Errorf("scheduler %+v", r)
	}
	if r := rows["queue"]; r.State != HealthOK || r.Text != "Nothing waiting" {
		t.Errorf("queue %+v", r)
	}
	if r := rows["mcp"]; r.State != HealthOK {
		t.Errorf("mcp %+v", r)
	}
	for _, r := range sum.Rows {
		if (r.State == HealthAttention || r.State == HealthProblem) != (r.Count > 0) {
			t.Errorf("%s: count %d at %s", r.ID, r.Count, r.State)
		}
	}
	if sum.State != HealthAttention || sum.Issues != 1 || sum.Color != "amber" || sum.Sentence != "1 thing needs attention" || len(sum.Recent) != 0 {
		t.Errorf("summary %+v", sum)
	}
	// /api/system and the catalog read the same cache
	var sys struct {
		Storage struct {
			Folders []diskstat.Entry `json:"folders"`
		} `json:"storage"`
		Loop scheduler.LoopStats `json:"loop"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/system"))), &sys)
	if len(sys.Storage.Folders) != 2 || sys.Storage.Folders[0].State != diskstat.StateOK || sys.Storage.Folders[1].State != diskstat.StateTimeout || sys.Loop.WarnMs != 1000 {
		t.Errorf("system %+v", sys)
	}
	var cat struct {
		Disk   map[string]any `json:"disk"`
		Models []any          `json:"models"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/models"))), &cat)
	if cat.Disk["state"] != diskstat.StateTimeout || cat.Disk["path"] != "/mnt/share" || cat.Models == nil {
		t.Errorf("catalog disk %+v", cat.Disk)
	}
}

// With the request log on, a failed benchmark run in the hour counts as a
// failed run, and the warnings of the last two hours come in short
// sentences, newest first; info events and older ones are left out. The
// /ws topic sends the summary on subscribe and again when it changes.
func TestHealth_RouteRecentRunsAndPush(t *testing.T) {
	dir := t.TempDir()
	store := usage.Open(usage.Options{Dir: dir})
	t.Cleanup(func() { store.Close() })
	hist := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(hist.Close)
	e := newEnv(t, chatOK, func(o *Options) {
		o.Usage, o.Requests, o.History = store, usage.NewRecorder(store), hist
		o.healthEvery = 20 * time.Millisecond
		o.taskStats = func() []supervise.TaskStats { return nil }
	})
	now := e.api.opts.Now()
	rec := func(ago time.Duration, typ, sev string, nodes []string, data map[string]any) {
		hist.Record(history.Event{Ts: now.Add(-ago).UnixMilli(), Type: typ, Severity: sev, Nodes: nodes, Data: data, Summary: typ + "."})
	}
	rec(3*time.Hour, history.TNodeLost, history.Error, []string{"zone-old"}, nil)
	rec(90*time.Minute, history.TNodeLost, history.Error, []string{"zone-b"}, nil)
	rec(30*time.Minute, history.TLoadStarted, history.Info, nil, nil)
	rec(20*time.Minute, history.TBenchRunFinished, history.Error, nil, map[string]any{"state": "failed", "bench_run": "r-1"})
	rec(10*time.Minute, history.TNodeQuarantined, history.Warn, []string{"zone-c"}, map[string]any{"reason": "other"})

	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{TopicHealth}})
	m := c.until(t, TopicHealth, 3*time.Second)
	var sum HealthSummary
	if err := json.Unmarshal(m.Data, &sum); err != nil {
		t.Fatal(err)
	}
	texts := []string{}
	for _, r := range sum.Recent {
		texts = append(texts, r.Text)
	}
	if strings.Join(texts, " | ") != "zone-c failed a load and was set aside | Benchmark run r-1 failed | zone-b went offline" {
		t.Fatalf("recent %q", texts)
	}
	var runs HealthRow
	for _, r := range sum.Rows {
		if r.ID == "runs" {
			runs = r
		}
	}
	if runs.State != HealthAttention || runs.Text != "1 failed in the last hour" || runs.Facts["request_log"] != "on" {
		t.Fatalf("runs %+v", runs)
	}
	// a change is pushed: a zone set aside now is the newest line
	rec(0, history.TNodeQuarantined, history.Warn, []string{"zone-d"}, map[string]any{"reason": "other"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		m := c.until(t, TopicHealth, time.Until(deadline))
		var s2 HealthSummary
		json.Unmarshal(m.Data, &s2)
		if len(s2.Recent) == 4 && s2.Recent[0].Node == "zone-d" {
			break
		}
	}
}

// The copy of live: seven active machines, one of them excluded (switched
// off: reachable, not down), and one removed machine (left out). Set aside
// (quarantined) is down.
func TestHealth_ZonesExcludedAndRemoved(t *testing.T) {
	facts := []pool.NodeFacts{{ID: "r", Name: "zone-removed", State: pool.StateRemoved}}
	var view []scheduler.NodeStatus
	for i := 1; i <= 7; i++ {
		id := "zone-" + string(rune('0'+i))
		f := pool.NodeFacts{ID: id, Name: id, State: pool.StateActive}
		f.Excluded = i == 7
		facts = append(facts, f)
		view = append(view, viewNode(id, true, false, i == 7))
	}
	row := judgeZones(statusOf(view...), facts, true, time.Time{})
	if row.State != HealthOK || row.Text != "7 of 7 reachable" || row.Count != 0 || row.Facts["off"] != int64(1) || row.Facts["total"] != int64(7) {
		t.Fatalf("excluded and removed: %+v", row)
	}
	view[0] = viewNode("zone-1", true, true, false)
	row = judgeZones(statusOf(view...), facts, true, time.Time{})
	if row.State != HealthProblem || row.Text != "6 of 7 reachable · 1 down" || row.Count != 1 || row.Facts["set_aside"] != int64(1) {
		t.Fatalf("set aside: %+v", row)
	}
}

// healthFrames reads the health frames that arrive within d.
func healthFrames(c *wsc, d time.Duration) []HealthSummary {
	var out []HealthSummary
	for _, m := range c.collect(d) {
		if m.Type == TopicHealth {
			var s HealthSummary
			json.Unmarshal(m.Data, &s)
			out = append(out, s)
		}
	}
	return out
}

// pushEnv is an env whose health push runs only when the test calls
// healthTick (the loop's own period is an hour), with a disk the test moves.
func pushEnv(t *testing.T) (*env, *atomic.Int64, *history.Store) {
	used := &atomic.Int64{}
	used.Store(50 * gb)
	hist := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(hist.Close)
	e := newEnv(t, chatOK, func(o *Options) {
		o.History = hist
		o.System.DataDir = "/srv/data"
		o.diskEvery, o.healthEvery = time.Hour, time.Hour
		o.diskStat = func(string) (diskstat.FS, error) {
			u := uint64(used.Load())
			return diskstat.FS{Total: uint64(240 * gb), Used: u, Free: uint64(240*gb) - u}, nil
		}
		o.taskStats = func() []supervise.TaskStats { return nil }
	})
	eventually(t, "a disk pass", func() bool { return e.api.disks.Passes() >= 1 })
	return e, used, hist
}

// subscribeHealth dials, subscribes to the health topic and takes the
// subscribe frame.
func subscribeHealth(t *testing.T, e *env) *wsc {
	t.Helper()
	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{TopicHealth}})
	if f := healthFrames(c, 300*time.Millisecond); len(f) != 1 {
		t.Fatalf("on subscribe: %d frames", len(f))
	}
	return c
}

// stillMinute evaluates the push every 2 s of clock for 60 s, the disk read
// every 30 s with its free bytes moving by less than the sentence shows.
func stillMinute(e *env, used *atomic.Int64) {
	for i := 1; i <= 30; i++ {
		e.advance(2 * time.Second)
		if i%15 == 0 {
			used.Add(1_000_000)
			e.api.disks.Pass(context.Background())
		}
		e.api.healthTick(context.Background())
	}
}

// Bar 1: 0 frames in 60 s idle, the minute in which the uptime goes from
// "under 1 min" to "1 min" included (the sentence carries it, the frame
// comparison does not), and none in the next four minutes either.
func TestHealth_Frames_ZeroIn60sIdle(t *testing.T) {
	e, used, _ := pushEnv(t)
	c := subscribeHealth(t, e)
	e.advance(30 * time.Second)
	stillMinute(e, used)
	if f := healthFrames(c, 300*time.Millisecond); len(f) != 0 {
		t.Fatalf("%d frames in 60 s idle", len(f))
	}
	var sum HealthSummary
	json.Unmarshal([]byte(body(t, e.get("/api/health"))), &sum)
	if txt := rowOf(sum, "scheduler").Text; !strings.HasSuffix(txt, " · up 1 min") {
		t.Fatalf("the route's sentence keeps the uptime: %q", txt)
	}
	for range 4 {
		stillMinute(e, used)
	}
	if f := healthFrames(c, 300*time.Millisecond); len(f) != 0 {
		t.Fatalf("%d frames in four more idle minutes", len(f))
	}
}

// Bar 2: 0 frames in 60 s with a model loaded and no request.
func TestHealth_Frames_ZeroIn60sWithModelLoaded(t *testing.T) {
	e, used, _ := pushEnv(t)
	if r := e.post("/v1/chat/completions", `{"model":"alpha","messages":[{"role":"user","content":"hi"}]}`); r.StatusCode != 200 {
		t.Fatalf("load: %d %s", r.StatusCode, body(t, r))
	} else {
		r.Body.Close()
	}
	eventually(t, "the model loaded", func() bool {
		st, err := e.sched.Status(context.Background())
		return err == nil && len(st.Instances) == 1 && st.Instances[0].State == "ready"
	})
	c := subscribeHealth(t, e)
	stillMinute(e, used)
	if f := healthFrames(c, 300*time.Millisecond); len(f) != 0 {
		t.Fatalf("%d frames in 60 s with a model loaded", len(f))
	}
}

// Bar 3: exactly 1 frame per real change: a row changing state, then two
// changes seen in one evaluation (a failed benchmark run: the Runs facts
// and a recent line), each followed by more evaluations. A second client's
// subscribe sends its frame to that client only.
func TestHealth_Frames_OnePerRealChange(t *testing.T) {
	e, used, hist := pushEnv(t)
	c := subscribeHealth(t, e)

	used.Store(230 * gb)
	e.api.disks.Pass(context.Background())
	for range 3 {
		e.api.healthTick(context.Background())
	}
	if f := healthFrames(c, 300*time.Millisecond); len(f) != 1 || rowOf(f[0], "storage").State != HealthAttention {
		t.Fatalf("a storage change: %d frames", len(f))
	}

	hist.Record(history.Event{Ts: e.api.opts.Now().UnixMilli(), Type: history.TBenchRunFinished, Severity: history.Error,
		Nodes: []string{}, Data: map[string]any{"state": "failed", "bench_run": "r-9"}, Summary: "x."})
	e.advance(11 * time.Second) // past the Runs cache
	for range 3 {
		e.api.healthTick(context.Background())
	}
	f := healthFrames(c, 300*time.Millisecond)
	if len(f) != 1 || len(f[0].Recent) != 1 || rowOf(f[0], "runs").Facts["failed_bench_runs"] != 1.0 {
		t.Fatalf("two changes in one evaluation: %d frames %+v", len(f), f)
	}

	c2 := dialWS(t, e, nil)
	c2.until(t, TopicHello, 3*time.Second)
	c2.send(t, map[string]any{"type": "subscribe", "topics": []string{TopicHealth}})
	if f := healthFrames(c2, 300*time.Millisecond); len(f) != 1 {
		t.Fatalf("second subscriber: %d frames", len(f))
	}
	if f := healthFrames(c, 100*time.Millisecond); len(f) != 0 {
		t.Fatalf("the first client got %d frames from the second's subscribe", len(f))
	}
}

func rowOf(s HealthSummary, id string) HealthRow {
	for _, r := range s.Rows {
		if r.ID == id {
			return r
		}
	}
	return HealthRow{}
}

// With no subscriber the summary is never evaluated.
func TestHealth_NoSubscriberNoWork(t *testing.T) {
	e := newEnv(t, chatOK, func(o *Options) { o.healthEvery = 5 * time.Millisecond })
	time.Sleep(100 * time.Millisecond)
	if n := e.api.health.evals.Load(); n != 0 {
		t.Fatalf("%d evaluations without a subscriber", n)
	}
	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{TopicHealth}})
	healthFrames(c, 100*time.Millisecond)
	if e.api.health.evals.Load() == 0 {
		t.Fatal("no evaluation with a subscriber")
	}
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{TopicStatus}})
	time.Sleep(50 * time.Millisecond)
	n := e.api.health.evals.Load()
	time.Sleep(100 * time.Millisecond)
	if m := e.api.health.evals.Load(); m != n {
		t.Fatalf("%d evaluations after the client left the topic", m-n)
	}
}

// A wait that only grows ("longest 1 min 40 s" to "1 min 42 s") and the
// uptime alone are not changes for the push; the count waiting is.
func TestHealth_TickingSentencePartsAreNotChanges(t *testing.T) {
	q := func(age time.Duration, n int) *scheduler.Status {
		return &scheduler.Status{Queue: []scheduler.EntryStatus{{Profile: "p", Waiters: n, Age: age}}}
	}
	sig := func(r HealthRow) string { return healthSig(HealthSummary{Rows: []HealthRow{r}}) }
	a, b := judgeQueue(q(100*time.Second, 1), 600*time.Second), judgeQueue(q(102*time.Second, 1), 600*time.Second)
	if a.Text == b.Text || sig(a) != sig(b) {
		t.Fatalf("a longer wait: %q / %q", a.Text, b.Text)
	}
	if sig(a) == sig(judgeQueue(q(100*time.Second, 2), 600*time.Second)) {
		t.Fatal("a second waiter is not a change")
	}
	in := schedIn{version: "v1", uptime: 59 * time.Second, statusFresh: true, now: time.Unix(1_800_000_000, 0)}
	x := judgeScheduler(in)
	in.uptime = 61 * time.Second
	y := judgeScheduler(in)
	if x.Text != "Running · release v1 · up under 1 min" || y.Text != "Running · release v1 · up 1 min" || sig(x) != sig(y) {
		t.Fatalf("uptime: %q / %q", x.Text, y.Text)
	}
}

// The Scheduler row's facts.release is the bare release string, and in the
// ok sentence it is the exact text between "Running · release " and
// " · up ", so the page can set it in mono without rewording the sentence.
func TestHealth_SchedulerReleaseFact(t *testing.T) {
	e := newEnv(t, chatOK, func(o *Options) {
		o.Version = "v36-311213b"
		o.taskStats = func() []supervise.TaskStats { return nil }
	})
	var h struct {
		Rows []struct {
			ID    string         `json:"id"`
			State string         `json:"state"`
			Text  string         `json:"text"`
			Facts map[string]any `json:"facts"`
		} `json:"rows"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/health"))), &h)
	for _, r := range h.Rows {
		if r.ID != "scheduler" {
			continue
		}
		rel, _ := r.Facts["release"].(string)
		if rel != "v36-311213b" || r.State != HealthOK || !strings.HasPrefix(r.Text, "Running · release "+rel+" · up ") ||
			strings.Count(r.Text, rel) != 1 {
			t.Fatalf("scheduler row: %+v", r)
		}
		return
	}
	t.Fatal("no scheduler row")
}

// An excluded machine that is absent after a restart (listed lost) reads
// as down in the Zones row, not as switched off.
func TestHealth_ZonesExcludedAbsentIsDown(t *testing.T) {
	facts := []pool.NodeFacts{{ID: "zone-x", Name: "zone-x", State: pool.StateLost, Excluded: true, AbsentSinceStart: true}}
	var view []scheduler.NodeStatus
	for i := 1; i <= 7; i++ {
		id := "zone-" + string(rune('0'+i))
		facts = append(facts, pool.NodeFacts{ID: id, Name: id, State: pool.StateActive})
		view = append(view, viewNode(id, true, false, false))
	}
	row := judgeZones(statusOf(view...), facts, true, time.Time{})
	if row.State != HealthProblem || row.Text != "7 of 8 reachable · 1 down" || row.Count != 1 || row.Facts["off"] != int64(0) ||
		row.Facts["lost"] != int64(1) || toJSONText(row.Facts["down_names"]) != `["zone-x"]` {
		t.Fatalf("%+v", row)
	}
}

func toJSONText(v any) string { b, _ := json.Marshal(v); return string(b) }
