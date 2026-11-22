package bench

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// coldMgr is a manager whose cold loads neither wait for a heartbeat nor
// retry for long.
func coldMgr(t *testing.T, api *fakeAPI, mod ...func(*ManagerOptions)) *testMgr {
	return newTestMgr(t, api, append([]func(*ManagerOptions){func(o *ManagerOptions) {
		o.ColdSettle, o.ColdRetry = -1, 200*time.Millisecond
	}}, mod...)...)
}

const coldLoadBench = `{"id":"b","profile":"gemma","phases":[{"type":"load","cold":true},{"type":"unload"}]}`

func coldOf(t *testing.T, r *Run) *ColdResult {
	t.Helper()
	if r.State != RunCompleted || len(r.Steps) != 1 || len(r.Steps[0].Phases) == 0 {
		t.Fatalf("%s %s %+v", r.State, r.Error, r.Steps)
	}
	c := r.Steps[0].Phases[0].Cold
	if c == nil || !c.Requested {
		t.Fatalf("no cold block: %+v", r.Steps[0].Phases[0])
	}
	return c
}

func count(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestColdLoadVerifiedOnEveryNode(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{nodes: []string{"ps5-a", "ps5-b"}})
	api.planNodes = []string{"n1", "n2"}
	api.insts["gemma"] = &fakeInst{state: "ready"} // loaded: unloaded first, then evicted
	tm := coldMgr(t, api)
	r := tm.runOne(coldLoadBench)
	c := coldOf(t, r)
	if !c.Verified || c.Reason != "" || c.Local == nil || c.Local.FileBytes != 1000 || c.Local.CachedAfter != 0 || len(c.Nodes) != 2 {
		t.Fatalf("%+v", c)
	}
	if c.Nodes[0].Node != "ps5-a" || c.Nodes[0].ID != "n1" || c.Nodes[0].CachedBefore != 900 || c.Nodes[1].Node != "ps5-b" {
		t.Fatalf("%+v", c.Nodes)
	}
	calls := strings.Join(api.callList(), " ")
	iUnload, iEvict, iLoad := strings.Index(calls, "unload:gemma"), strings.Index(calls, "evict:gemma nodes=n1,n2 local=true"), strings.Index(calls, " load:gemma")
	if iUnload < 0 || iEvict < iUnload || iLoad < iEvict {
		t.Fatalf("want unload, evict, load: %s", calls)
	}
	// the plan is read again after the eviction, so the stored reload figures are the emptied caches'
	if n := count(api.callList(), "plan:gemma"); n != 2 {
		t.Fatalf("plan read %d times: %s", n, calls)
	}
	if r.Steps[0].Outcome != OutcomeReady || r.Steps[0].Phases[0].Load == nil {
		t.Fatalf("%+v", r.Steps[0])
	}
}

func TestColdLoadNodeWithoutTheCapability(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{nodes: []string{"ps5-a", "ps5-b"}})
	api.planNodes = []string{"n1", "n2"}
	api.evictNode = map[string][]NodeEvict{"n2": {{Capable: false, Reason: "unsupported", Error: "the agent does not advertise file_cache_evict"}}}
	r := coldMgr(t, api).runOne(coldLoadBench)
	c := coldOf(t, r)
	if c.Verified || !strings.Contains(c.Reason, "not guaranteed: node ps5-b has an agent without file cache eviction") {
		t.Fatalf("%+v", c)
	}
	if c.Nodes[1].Error == "" || c.Nodes[0].Error != "" {
		t.Fatalf("%+v", c.Nodes)
	}
	if r.Steps[0].Outcome != OutcomeReady { // the load still runs
		t.Fatalf("%+v", r.Steps[0])
	}
}

func TestColdLoadNodeRefuses(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.evictNode = map[string][]NodeEvict{"n1": {{Capable: true, Status: 400, Reason: "invalid_request", Error: `"m.gguf" resolves outside the model folder`}}}
	r := coldMgr(t, api).runOne(coldLoadBench)
	c := coldOf(t, r)
	if c.Verified || !strings.Contains(c.Reason, "node ps5-a refused the eviction (invalid_request: ") {
		t.Fatalf("%+v", c)
	}
	if n := count(api.callList(), "evict:"); n != 1 {
		t.Fatalf("a refusal that cannot change was asked again: %v", api.callList())
	}
}

func TestColdLoadRetriesANodeThatIsStillStopping(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	busy := NodeEvict{Capable: true, Status: 409, Reason: "busy", Error: "launch ln-1 exists on this node"}
	api.evictNode = map[string][]NodeEvict{"n1": {busy, busy, {Capable: true, Status: 200, EvictFigures: &EvictFigures{FileBytes: 1000, CachedBefore: 1000, CachedAfter: 2}}}}
	r := coldMgr(t, api, func(o *ManagerOptions) { o.ColdRetry = 5 * time.Second }).runOne(coldLoadBench)
	c := coldOf(t, r)
	if !c.Verified || c.Nodes[0].CachedAfter != 2 {
		t.Fatalf("%+v", c)
	}
	calls := api.callList()
	if count(calls, "evict:gemma nodes=n1 local=true") != 1 || count(calls, "evict:gemma nodes=n1 local=false") != 2 {
		t.Fatalf("the retries must ask the busy node only, without the local machine again: %v", calls)
	}
}

func TestColdLoadGivesUpOnAStillBusyNode(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.evictNode = map[string][]NodeEvict{"n1": {{Capable: true, Status: 409, Reason: "busy", Error: "launch ln-1 exists on this node"}}}
	r := coldMgr(t, api, func(o *ManagerOptions) { o.ColdRetry = 30 * time.Millisecond }).runOne(coldLoadBench)
	c := coldOf(t, r)
	if c.Verified || !strings.Contains(c.Reason, "node ps5-a refused the eviction (busy: launch ln-1") {
		t.Fatalf("%+v", c)
	}
}

func TestColdLoadPlacementDiffersFromThePlan(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{nodes: []string{"ps5-a", "ps5-b"}}) // the load lands on n1 and n2
	api.planNodes = []string{"n1"}                                              // the plan named n1 only
	r := coldMgr(t, api).runOne(coldLoadBench)
	c := coldOf(t, r)
	if c.Verified || !strings.Contains(c.Reason, "the load was placed on ps5-b, which the plan did not name") {
		t.Fatalf("%+v", c)
	}
	if len(c.Nodes) != 1 || c.Nodes[0].ID != "n1" {
		t.Fatalf("%+v", c.Nodes)
	}
}

func TestColdLoadOnePercentRule(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.evictNode = map[string][]NodeEvict{"n1": {{Capable: true, Status: 200, EvictFigures: &EvictFigures{FileBytes: 1000, CachedBefore: 1000, CachedAfter: 50}}}}
	api.evictLocal = &LocalEvict{EvictFigures: &EvictFigures{FileBytes: 1000, CachedBefore: 1000, CachedAfter: 9}} // 0.9 %: cold
	c := coldOf(t, coldMgr(t, api).runOne(coldLoadBench))
	if c.Verified || !strings.Contains(c.Reason, "node ps5-a still holds 5.0% of the file in its cache") || strings.Contains(c.Reason, "scheduler's machine") {
		t.Fatalf("%+v", c)
	}
}

func TestColdLoadLocalMachineFails(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.evictLocal = &LocalEvict{Reason: "unsupported", Error: "this system has no page cache calls"}
	c := coldOf(t, coldMgr(t, api).runOne(coldLoadBench))
	if c.Verified || c.Local == nil || c.Local.Error == "" || !strings.Contains(c.Reason, "the scheduler's machine could not drop the file") {
		t.Fatalf("%+v", c)
	}
}

func TestColdLoadEvictionCallFails(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	api.evictErr = errors.New("/api/cache/evict answered 404: no such path")
	r := coldMgr(t, api).runOne(coldLoadBench)
	c := coldOf(t, r)
	if c.Verified || !strings.Contains(c.Reason, "the eviction request failed") || r.Steps[0].Outcome != OutcomeReady {
		t.Fatalf("%+v", c)
	}
}

// noEvictAPI hides the fake's EvictCache, as a scripted API without it.
type noEvictAPI struct{ API }

func TestColdLoadWithAnAPIThatCannotEvict(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := coldMgr(t, api, func(o *ManagerOptions) { o.API = noEvictAPI{api} })
	c := coldOf(t, tm.runOne(coldLoadBench))
	if c.Verified || !strings.Contains(c.Reason, "cannot drop files from the file cache") {
		t.Fatalf("%+v", c)
	}
}

func TestLoadWithoutColdHasNoColdBlock(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	r := coldMgr(t, api).runOne(`{"id":"b","profile":"gemma","phases":[{"type":"load"},{"type":"unload"}]}`)
	if r.Steps[0].Phases[0].Cold != nil || count(api.callList(), "evict:") != 0 {
		t.Fatalf("%+v %v", r.Steps[0].Phases[0].Cold, api.callList())
	}
}

// The ready-made "cold, reload, loaded" shape: the first load is cold, the
// second (after an unload) is not, and every phase ran.
func TestColdReloadTemplateRuns(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	tm := coldMgr(t, api)
	js := strings.Replace(string(templateJSON(t, "cold-reload-loaded")), "your-profile", "gemma", 1)
	js = strings.Replace(js, `"id":"cold-reload-loaded-example"`, `"id":"b"`, 1)
	r := tm.runOne(js)
	st := r.Steps[0]
	if got := phaseTypesOf(st); got != "load:done,warmup:done,requests:done,unload:done,load:done,requests:done,unload:done" {
		t.Fatalf("%s (%s)", got, st.Error)
	}
	if st.Phases[0].Cold == nil || !st.Phases[0].Cold.Verified || st.Phases[4].Cold != nil || st.Phases[4].Label != "reload" {
		t.Fatalf("%+v / %+v", st.Phases[0].Cold, st.Phases[4])
	}
	if count(api.callList(), "evict:") != 1 || count(api.callList(), "load:gemma") != 2 {
		t.Fatalf("%v", api.callList())
	}
	for _, i := range []int{1, 5} { // a streamed first request after each load: its time to first token
		if rows := st.Requests; len(rows) == 0 {
			t.Fatal("no requests")
		}
		found := false
		for _, rq := range st.Requests {
			if rq.PhaseIdx == i && rq.TTFTMS != nil {
				found = true
			}
		}
		if !found {
			t.Fatalf("phase %d has no time to first token", i)
		}
	}
}

func templateJSON(t *testing.T, id string) []byte {
	t.Helper()
	tp, ok := benchspec.TemplateByID(id)
	if !ok {
		t.Fatalf("no template %q", id)
	}
	return tp.Benchmark
}
