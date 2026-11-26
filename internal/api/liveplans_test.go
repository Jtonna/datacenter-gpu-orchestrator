package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The live channel's model catalog and the profiles' plans (catalogplans.go):
// a page that holds the catalog open must not make the server compute every
// profile's plan again on every heartbeat, and a page that does not hold it
// open must not make it compute any.

const livePlanProfiles = 24

// driftView is a fleet view whose nodes the test changes: a heartbeat moves
// the free memory, a node can leave.
type driftView struct {
	mu    sync.Mutex
	nodes []core.Node
	ch    chan struct{}
}

func (v *driftView) Nodes() []core.Node {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]core.Node(nil), v.nodes...)
}
func (v *driftView) Age() time.Duration                       { return 0 }
func (v *driftView) Connected() bool                          { return true }
func (v *driftView) LastReconnect() time.Time                 { return time.Time{} }
func (v *driftView) Changed() <-chan struct{}                 { return v.ch }
func (v *driftView) Resolve(name string) (core.NodeID, error) { return core.NodeID(name), nil }
func (v *driftView) FetchNode(_ context.Context, id core.NodeID) (core.Node, error) {
	for _, n := range v.Nodes() {
		if n.ID == id {
			return n, nil
		}
	}
	return core.Node{}, context.Canceled
}

// heartbeat sets every node's free memory to base+delta (a heartbeat with
// the same facts and a little drift) and tells the scheduler.
func (v *driftView) heartbeat(delta int) {
	func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		for i := range v.nodes {
			v.nodes[i].FreeMiB = 14000 + delta
		}
	}()
	select {
	case v.ch <- struct{}{}:
	default:
	}
}

func (v *driftView) remove(id core.NodeID) {
	func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		var keep []core.Node
		for _, n := range v.nodes {
			if n.ID != id {
				keep = append(keep, n)
			}
		}
		v.nodes = keep
	}()
	select {
	case v.ch <- struct{}{}:
	default:
	}
}

func livePlanYAML(extra string) string {
	var b strings.Builder
	b.WriteString(`
listen: ":0"
fleet: {url: "http://127.0.0.1:1", local_node: host-1}
models_dir: /models
defaults: {ttl: 0s, min_hold: 1ms, max_hold: 1s, stall_timeout: 0s}
tiers:
  - match: {local: true}
    priority: 1
    headroom_mib: 256
profiles:
`)
	for i := 0; i < livePlanProfiles; i++ {
		fmt.Fprintf(&b, "  p%02d:\n    model: m%02d.gguf\n    ctx: 4096\n    needs_mib: %d\n%s", i, i, 1000+100*i, extra)
	}
	return b.String()
}

type liveEnv struct {
	*env
	view *driftView
	cur  int            // the free memory's delta of the last heartbeat
	cfg2 *config.Config // the same profiles with another description (a configuration write)
	tick func(d time.Duration)
}

// newLiveEnv is a real scheduler with livePlanProfiles profiles on one
// local node whose free memory the test moves, the API in front of it, and
// a fake clock for the catalog's gaps (minGap: 0 keeps catalogMinGap).
func newLiveEnv(t *testing.T, minGap time.Duration) *liveEnv {
	t.Helper()
	parse := func(y string) *config.Config {
		cfg, err := config.ParseWithOptions([]byte(y), config.Options{FileExists: func(string) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := parse(livePlanYAML(""))
	up := httptest.NewServer(chatOK)
	t.Cleanup(up.Close)
	port := 0
	if i := strings.LastIndex(up.URL, ":"); i > 0 {
		for _, c := range up.URL[i+1:] {
			port = port*10 + int(c-'0')
		}
	}
	view := &driftView{ch: make(chan struct{}, 1), nodes: []core.Node{
		{ID: "host-1", Hostname: "host-1", Local: true, Online: true, TotalMiB: 16000, FreeMiB: 14000, Priority: 1, HeadroomMiB: 256},
		{ID: "host-2", Hostname: "host-2", Local: true, Online: true, TotalMiB: 16000, FreeMiB: 14000, Priority: 1, HeadroomMiB: 256},
	}}
	var clockMu sync.Mutex
	clock := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	e := &env{t: t, cfg: cfg, upstream: up, logs: &syncBuf{}, clock: clock, launcher: &fakeLauncher{port: port}}
	sched, err := scheduler.New(cfg, scheduler.Options{
		View: view, Prober: noProber{}, Launcher: e.launcher,
		Needs: scheduler.NeedFunc(func(p *core.Profile) (scheduler.NeedInfo, error) {
			return scheduler.NeedInfo{Need: placer.Measured(p.NeedsMiB), FileBytes: 1 << 20}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.sched = sched
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	e.api = New(cfg, sched, Options{Logger: slog.New(slog.NewTextHandler(e.logs, nil)), PingInterval: time.Second, Now: time.Now,
		catalogClock:  func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock },
		catalogMinGap: minGap})
	e.front = httptest.NewServer(e.api)
	t.Cleanup(func() {
		e.front.CloseClientConnections()
		e.front.Close()
		cancel()
		<-done
		e.api.Close()
	})
	return &liveEnv{env: e, view: view, cfg2: parse(livePlanYAML("    description: changed\n")),
		tick: func(d time.Duration) { clockMu.Lock(); defer clockMu.Unlock(); clock = clock.Add(d) }}
}

// heartbeat moves the free memory and waits until the status shows it.
func (le *liveEnv) heartbeat(delta int) {
	le.t.Helper()
	le.cur = delta
	le.view.heartbeat(delta)
	want := 14000 + delta
	eventually(le.t, "the heartbeat in the status", func() bool {
		st, err := le.sched.Status(context.Background())
		if err != nil {
			return false
		}
		for _, n := range st.View.Nodes {
			if n.FreeMiB != want {
				return false
			}
		}
		return len(st.View.Nodes) > 0
	})
}

// waitReady waits until profile's instance is ready.
func (le *liveEnv) waitReady(profile string) {
	le.t.Helper()
	eventually(le.t, profile+" ready", func() bool {
		st, err := le.sched.Status(context.Background())
		if err != nil {
			return false
		}
		for _, i := range st.Instances {
			if i.Profile == profile && i.State == "ready" {
				return true
			}
		}
		return false
	})
}

func (le *liveEnv) rounds() int64 { return le.api.catalog.planRounds.Load() }

// build is one rebuild of the live channel's catalog (what the rebuild loop
// does after catalogStatus poked it), at the fake clock's time.
func (le *liveEnv) build() *catalogSnap {
	le.t.Helper()
	snap, err := le.api.catalogBuild(context.Background(), nil, false)
	if err != nil {
		le.t.Fatal(err)
	}
	return snap
}

func planOf(t *testing.T, snap *catalogSnap, profile string) map[string]any {
	t.Helper()
	for _, m := range BuildModelCatalog(snap.in, "") {
		for _, p := range jlist(m["profiles"]) {
			if pm := jmap(p); jstr(pm["id"]) == profile {
				return jmap(pm["plan"])
			}
		}
	}
	t.Fatalf("no plan of %s in the catalog", profile)
	return nil
}

// A steady fleet, one heartbeat every 2 s for ten minutes of the fake clock,
// and a rebuild of the live catalog after EVERY heartbeat (the worst the
// rebuild loop can do): every profile's plan is computed at most once per
// catalogDriftGap (60 s) while the memory drifts, and only once at all while
// the heartbeats change nothing. Before the fix it was once per rebuild.
func TestLivePlansSteadyFleetBounded(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprintf("drift=%v", drift), func(t *testing.T) {
			le := newLiveEnv(t, 0)
			le.build()
			if r := le.rounds(); r != 1 {
				t.Fatalf("first build: %d plan rounds, want 1", r)
			}
			const beats = 300 // 10 minutes at one every 2 s
			for i := 1; i <= beats; i++ {
				le.tick(2 * time.Second)
				d := 0
				if drift {
					d = 40 * (i % 5) // a few dozen MiB up and down, as the page cache and the agent move
				}
				le.heartbeat(d)
				le.build()
			}
			r := le.rounds()
			// at most one round per started catalogDriftGap, plus the first
			bound := int64(1)
			if drift {
				bound = 1 + int64(beats*2*time.Second/catalogDriftGap)
			}
			t.Logf("drift %v: %d plan rounds (%d plans) in 10 minutes, bound %d", drift, r, le.api.catalog.planReads.Load(), bound)
			if r > bound || r < 1 {
				t.Fatalf("%d plan rounds in 10 minutes, want at most %d", r, bound)
			}
			if got, want := le.api.catalog.planReads.Load(), r*livePlanProfiles; got != want {
				t.Fatalf("%d single plans, want %d (%d rounds of %d profiles)", got, want, r, livePlanProfiles)
			}
		})
	}
}

// A change that matters gives fresh plans at the next rebuild, however
// recent the last round: an instance becomes ready, a node leaves, the
// configuration is written, a node's free memory moves by a gigabyte.
func TestLivePlansChangeThatMattersRecomputes(t *testing.T) {
	le := newLiveEnv(t, 0)
	snap := le.build()
	if a := jstr(planOf(t, snap, "p00")["action"]); a != string(scheduler.PlanStart) {
		t.Fatalf("p00 before: %s", a)
	}
	step := func(what string, change func()) *catalogSnap {
		t.Helper()
		before := le.rounds()
		le.tick(2 * time.Second) // catalogMinGap: the rebuild loop's spacing
		change()
		s := le.build()
		if r := le.rounds(); r != before+1 {
			t.Fatalf("%s: %d plan rounds, want %d", what, r, before+1)
		}
		// and nothing more while nothing else changes
		le.tick(2 * time.Second)
		le.heartbeat(le.cur + 10)
		le.build()
		if r := le.rounds(); r != before+1 {
			t.Fatalf("%s: a heartbeat after it computed the plans again (%d rounds)", what, r)
		}
		return s
	}
	snap = step("an instance becomes ready", func() {
		if err := le.sched.Load(context.Background(), "p00"); err != nil {
			t.Fatal(err)
		}
		le.waitReady("p00")
	})
	if a := jstr(planOf(t, snap, "p00")["action"]); a != string(scheduler.PlanServe) {
		t.Fatalf("p00 after its load: plan %s, want %s", a, scheduler.PlanServe)
	}
	step("a node's free memory moves by a gigabyte", func() { le.heartbeat(-1500) })
	step("a node leaves", func() {
		le.view.remove("host-2")
		eventually(t, "host-2 gone", func() bool {
			st, _ := le.sched.Status(context.Background())
			return len(st.View.Nodes) == 1
		})
	})
	snap = step("a configuration write", func() {
		le.sched.ApplyConfig(le.cfg2)
	})
	var desc string
	for _, m := range BuildModelCatalog(snap.in, "") {
		for _, p := range jlist(m["profiles"]) {
			if pm := jmap(p); jstr(pm["id"]) == "p01" {
				desc = jstr(pm["description"])
			}
		}
	}
	if desc != "changed" {
		t.Fatalf("after the configuration write the catalog has description %q", desc)
	}
}

// REST callers answer as before: GET /api/models after a heartbeat that only
// moved the memory has plans computed under the status it answers with,
// even when the live channel's catalog reused older ones.
func TestLivePlansRESTStaysFresh(t *testing.T) {
	le := newLiveEnv(t, 0)
	le.build()
	le.tick(2 * time.Second)
	le.heartbeat(100)
	le.build() // live: reuses the plans
	if r := le.rounds(); r != 1 {
		t.Fatalf("live rebuild after a drift: %d rounds", r)
	}
	b := body(t, le.get("/api/models"))
	if r := le.rounds(); r != 2 {
		t.Fatalf("GET /api/models after a drift: %d rounds, want a fresh one (2)", r)
	}
	if !strings.Contains(b, `"p00"`) {
		t.Fatalf("GET /api/models: %.200s", b)
	}
	// the same status again: the stored document, no new round
	body(t, le.get("/api/models"))
	if r := le.rounds(); r != 2 {
		t.Fatalf("a second GET with nothing changed: %d rounds", r)
	}
	// GET /api/profiles always computes (it never used the catalog)
	reads := le.api.catalog.planReads.Load()
	body(t, le.get("/api/profiles"))
	if got := le.api.catalog.planReads.Load(); got != reads+livePlanProfiles {
		t.Fatalf("GET /api/profiles computed %d plans, want %d", got-reads, livePlanProfiles)
	}
}

// liveClient dials /ws and subscribes to topics.
func liveClient(t *testing.T, e *env, topics ...string) *wsc {
	t.Helper()
	w := dialWS(t, e, nil)
	w.conn.SetReadLimit(1 << 24) // the whole catalog of many profiles is one frame
	w.until(t, "hello", 2*time.Second)
	b, _ := json.Marshal(map[string]any{"type": "subscribe", "topics": topics})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	return w
}

// drift runs heartbeats that only move the memory for about d of real time
// (the hub's status loop and the rebuild loop run for real; catalogMinGap is
// short in these tests so that the old rule would rebuild many times).
func (le *liveEnv) drift(d time.Duration) {
	le.t.Helper()
	end := time.Now().Add(d)
	for i := 1; time.Now().Before(end); i++ {
		le.tick(time.Second)
		le.heartbeat(30 * (i % 4))
		time.Sleep(60 * time.Millisecond)
	}
}

// One, then three connected clients on the models topic: the plans are
// computed once and shared, and drifting heartbeats compute none again.
func TestLivePlansSharedAmongClients(t *testing.T) {
	count := func(clients int) int64 {
		le := newLiveEnv(t, 50*time.Millisecond)
		for i := 0; i < clients; i++ {
			w := liveClient(t, le.env, "status", "models")
			w.until(t, "models", 5*time.Second) // the whole catalog
		}
		start := le.rounds()
		le.drift(1500 * time.Millisecond) // 25 fake seconds: under catalogDriftGap
		got := le.rounds() - start
		t.Logf("%d clients: %d plan rounds at the first catalog, %d while the memory drifted (%d catalog builds)", clients, start, got, le.api.catalog.builds.Load())
		if start != 1 {
			t.Fatalf("%d clients: %d plan rounds for the first catalog, want 1", clients, start)
		}
		return got
	}
	one, three := count(1), count(3)
	if one != 0 || three != 0 {
		t.Fatalf("plan rounds while only the memory drifted: %d with one client, %d with three; want 0 and 0", one, three)
	}
}

// A client that did not subscribe to "models" causes no plan computation,
// however much the status changes.
func TestLivePlansNoTopicNoPlans(t *testing.T) {
	le := newLiveEnv(t, 50*time.Millisecond)
	liveClient(t, le.env, "status", "event", "bench", "config")
	w2 := dialWS(t, le.env, nil) // the default topics
	w2.until(t, "hello", 2*time.Second)
	le.drift(time.Second)
	if err := le.sched.Load(context.Background(), "p03"); err != nil { // and a change that matters
		t.Fatal(err)
	}
	le.drift(500 * time.Millisecond)
	if r, n := le.rounds(), le.api.catalog.planReads.Load(); r != 0 || n != 0 {
		t.Fatalf("clients without the models topic: %d plan rounds, %d plans; want none", r, n)
	}
}

// A change that matters reaches a client of the models topic within
// catalogMinGap (2 s) and the settle and status periods: the profile's plan
// in the change message says it serves now.
func TestLivePlansChangeReachesClient(t *testing.T) {
	le := newLiveEnv(t, 0)
	w := liveClient(t, le.env, "status", "models")
	w.until(t, "models", 5*time.Second)
	// the fake clock runs at four times real time, so catalogMinGap is
	// half a second of the test's time
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
				le.tick(time.Second)
			}
		}
	}()
	if err := le.sched.Load(context.Background(), "p02"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(catalogMinGap + 2*time.Second)
	for time.Now().Before(deadline) {
		m := w.until(t, "models", time.Until(deadline))
		var d struct{ Changed []map[string]any }
		if json.Unmarshal(m.Data, &d) != nil {
			continue
		}
		for _, c := range d.Changed {
			for _, p := range jlist(c["profiles"]) {
				if pm := jmap(p); jstr(pm["id"]) == "p02" && jstr(jmap(pm["plan"])["action"]) == string(scheduler.PlanServe) {
					return
				}
			}
		}
	}
	t.Fatal("the client did not get p02's new plan in time")
}

// One open of a page costs at most one round of plans, counted from every
// source (REST, the live channel's subscribe, the rebuild loop), under the
// conditions on live: memory moving on every heartbeat (a background feed,
// not a step the test waits on), another browser tab already connected
// (watching the catalog live, or not), and plans older than catalogDriftGap
// when nobody watches them. Each page's reads are what its screen sends on
// open (pinned by the UI tests TestUINextBenchPageOpenReads and
// TestUIModelsOpenReadsCatalogOnce):
//   - Overview and the benchmark pages: GET /api/models (names only), the
//     live channel without "models", one GET /api/models/<key>/icon per
//     avatar (an icon read needs only the model's family: on live v83 it
//     asked catalogGet, a second fresh round whenever the memory had moved
//     since GET /api/models), and the status reads;
//   - Models: GET /api/models, the disk alone (?only=disk, no plans; its
//     30 s re-read too), then the "models" subscribe once the REST answer
//     is in (the server reuses the plans it just computed).
//
// GET /api/models itself stays fresh (TestLivePlansRESTStaysFresh).
func TestPageOpenPlanReads(t *testing.T) {
	base := []string{"status", "event", "bench", "config", "health", "requests"}
	type page struct {
		name   string
		models bool // the screen watches the catalog live
		gets   []string
	}
	icons := []string{}
	for i := 0; i < 6; i++ {
		icons = append(icons, fmt.Sprintf("/api/models/m%02d.gguf/icon", i))
	}
	pages := []page{
		{"overview", false, append([]string{"/api/health", "/api/status"}, icons...)},
		{"benchmarks/runs", false, icons},
		{"models", true, append([]string{"/api/models?only=disk"}, icons...)},
	}
	for _, standingLive := range []bool{false, true} {
		for _, pg := range pages {
			t.Run(fmt.Sprintf("%s, the other tab watching the catalog %v", pg.name, standingLive), func(t *testing.T) {
				le := newLiveEnv(t, 50*time.Millisecond)
				st, err := bench.OpenStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { st.Close() })
				cs, err := cfgstore.Open(st)
				if err != nil {
					t.Fatal(err)
				}
				le.api.store = cs
				// the memory moves on every heartbeat, all along
				stop, fed := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(fed)
					for i := 1; ; i++ {
						select {
						case <-stop:
							return
						case <-time.After(40 * time.Millisecond):
						}
						le.tick(100 * time.Millisecond)
						le.view.heartbeat(37 * (i % 7))
					}
				}()
				defer func() { close(stop); <-fed }()
				// the other tab, connected before the open
				topics := base
				if standingLive {
					topics = append(append([]string{}, base...), "models")
				}
				other := liveClient(t, le.env, topics...)
				other.until(t, "status", 3*time.Second)
				if standingLive {
					other.until(t, "models", 5*time.Second)
				}
				time.Sleep(time.Second)
				if !standingLive {
					le.tick(catalogDriftGap + time.Second) // nobody watched the plans for a while
				}
				before, reads := le.rounds(), le.api.catalog.planReads.Load()

				// the page opens
				res := le.get("/api/models")
				res.Body.Close()
				own := base
				if pg.models {
					own = append(append([]string{}, base...), "models")
				}
				c := liveClient(t, le.env, own...)
				for _, p := range pg.gets {
					res := le.get(p)
					res.Body.Close()
					if res.StatusCode >= 500 || (res.StatusCode != 200 && !strings.HasSuffix(p, "/icon")) {
						t.Fatalf("GET %s: %d", p, res.StatusCode)
					}
				}
				c.until(t, "status", 3*time.Second)
				if pg.models {
					c.until(t, "models", 5*time.Second)
				}
				time.Sleep(1500 * time.Millisecond) // the rebuild loop and the hub settle
				if pg.models {
					res := le.get("/api/models?only=disk") // the 30 s re-read of the disk figures
					res.Body.Close()
				}
				got, plans := le.rounds()-before, le.api.catalog.planReads.Load()-reads
				t.Logf("one open of %s: %d rounds of plans (%d plan reads, %d profiles)", pg.name, got, plans, livePlanProfiles)
				if got > 1 {
					t.Fatalf("one open of %s computed %d rounds of plans, want at most 1", pg.name, got)
				}
			})
		}
	}
}

// The order the Models page keeps (the REST read, then the subscribe) is
// what makes its open one round: a subscribe first, with plans older than
// catalogDriftGap, computes them for the channel, and the REST read that
// follows computes them again under the status of its own moment. Logged,
// not judged: the client's order is what is pinned
// (TestUIModelsOpenReadsCatalogOnce).
func TestPageOpenSubscribeFirstCostsTwo(t *testing.T) {
	le := newLiveEnv(t, 50*time.Millisecond)
	le.tick(catalogDriftGap + time.Second)
	before := le.rounds()
	c := liveClient(t, le.env, "status", "models")
	c.until(t, "models", 5*time.Second)
	le.tick(time.Second)
	le.heartbeat(100)
	body(t, le.get("/api/models"))
	t.Logf("subscribe first, then GET /api/models: %d rounds of plans", le.rounds()-before)
}

// GET /api/models?only=disk is the disk block alone and computes no plans,
// however much the memory moved; any other value of only is refused.
func TestModelsDiskOnlyComputesNoPlans(t *testing.T) {
	le := newLiveEnv(t, 0)
	body(t, le.get("/api/models"))
	before := le.rounds()
	for i := 1; i <= 3; i++ {
		le.tick(catalogDriftGap)
		le.heartbeat(2000 * i)
		doc := jsonOf(t, body(t, le.get("/api/models?only=disk")))
		if _, ok := doc["disk"]; !ok || len(doc) != 1 {
			t.Fatalf("only=disk answered %v, want the disk block alone", doc)
		}
	}
	if r := le.rounds() - before; r != 0 {
		t.Fatalf("three disk reads computed %d rounds of plans, want 0", r)
	}
	if res := le.get("/api/models?only=plans"); res.StatusCode != 400 {
		t.Fatalf("only=plans: %d, want 400", res.StatusCode)
	} else {
		res.Body.Close()
	}
}
