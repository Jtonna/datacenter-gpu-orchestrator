package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Run and Cancel from the Benchmarks pages (v88): the start gate, the dry
// read, the evict list, the comment, the plain-word refusals.

type auditCapture struct {
	mu      sync.Mutex
	entries []map[string]any
}

func (a *auditCapture) Record(r *http.Request, action, section, target string, before, after any) {
	o := audit.FromRequest(r)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, map[string]any{"action": action, "target": target, "actor": o.Actor, "comment": o.Comment, "after": after})
}

func (a *auditCapture) last(action string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.entries) - 1; i >= 0; i-- {
		if a.entries[i]["action"] == action {
			return a.entries[i]
		}
	}
	return nil
}

// startEnv is benchEnv with two benchmarks and a group of both.
func startEnv(t *testing.T) (*env, *auditCapture) {
	t.Helper()
	e := benchEnv(t)
	au := &auditCapture{}
	e.api.SetAuditHook(au)
	for _, b := range []string{
		`{"id":"b-alpha","name":"Alpha check","profile":"alpha","requests":[{"prompt":"hi","max_tokens":4}]}`,
		`{"id":"b-gamma","name":"Gamma check","profile":"gamma","requests":[{"prompt":"hi","max_tokens":4}]}`,
		`{"id":"b-beta","name":"Beta check","profile":"beta:64k","requests":[{"prompt":"hi","max_tokens":4}]}`,
	} {
		if st, b2, _ := e.do("POST", "/api/bench/benchmarks", b); st != 201 {
			t.Fatalf("%d %s", st, b2)
		}
	}
	if st, b, _ := e.do("POST", "/api/bench/groups", `{"id":"g","name":"Both","benchmarks":["b-alpha","b-gamma"],"cooldown_s":0,"only_when_idle":false}`); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	return e, au
}

func (e *env) jdo(method, path, body string, hdr map[string]string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.front.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{"raw": string(b)}
	}
	return resp.StatusCode, m
}

func refusalCodes(m map[string]any) []string {
	var out []string
	rs, _ := m["refusals"].([]any)
	for _, r := range rs {
		out = append(out, r.(map[string]any)["code"].(string))
	}
	return out
}

func (e *env) runCount() int {
	e.t.Helper()
	runs, err := e.api.opts.BenchStore.ListRuns(bench.RunFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(runs)
}

func (e *env) setConf(f func(c *config.Config)) {
	e.t.Helper()
	c := *e.sched.Config()
	profs := map[string]*core.Profile{}
	for k, v := range c.Profiles {
		cp := *v
		profs[k] = &cp
	}
	c.Profiles = profs
	f(&c)
	e.sched.ApplyConfig(&c)
}

func (e *env) instState(profile string) string {
	st, _ := e.sched.Status(context.Background())
	for _, i := range st.Instances {
		if i.Profile == profile {
			return i.State
		}
	}
	return ""
}

func TestRunStartAndCancelNeedAComment(t *testing.T) {
	e, au := startEnv(t)
	for _, path := range []string{"/api/bench/benchmarks/b-alpha/run", "/api/bench/groups/g/run"} {
		st, m := e.jdo("POST", path, `{"only_when_idle":false}`, nil)
		if st != 400 || !strings.Contains(errOf(m)["message"].(string), "a comment is required") {
			t.Fatalf("%s without a comment: %d %v", path, st, m)
		}
	}
	if e.runCount() != 0 {
		t.Fatal("a run without a comment was queued")
	}
	st, m := e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"only_when_idle":false,"comment":"after the driver update"}`, map[string]string{"X-Actor": "ann"})
	if st != 202 || m["started_by"] != "ann" || m["start_comment"] != "after the driver update" {
		t.Fatalf("start: %d %v", st, m)
	}
	a := au.last("run_start")
	if a == nil || a["comment"] != "after the driver update" || a["actor"] != "ann" {
		t.Fatalf("audit: %v", a)
	}
	id := m["id"].(string)
	waitRun(t, e, id)
	// a finished run reads back who started it and why (kept with its plan)
	if st, m := e.jdo("GET", "/api/bench/runs/"+id, "", nil); st != 200 || m["started_by"] != "ann" || m["start_comment"] != "after the driver update" {
		t.Fatalf("read back: %d %v %v", st, m["started_by"], m["start_comment"])
	}

	// cancel: a queued run (the first waits for idle: gamma is someone else's)
	body(t, e.post("/v1/chat/completions", chat("gamma")))
	st, m = e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"comment":"queued one"}`, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, m)
	}
	first := m["id"].(string)
	st, m = e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"comment":"second"}`, nil)
	second := m["id"].(string)
	if st, m := e.jdo("POST", "/api/bench/runs/"+second+"/cancel", ``, nil); st != 400 || !strings.Contains(errOf(m)["message"].(string), "a comment is required") {
		t.Fatalf("cancel without a comment: %d %v", st, m)
	}
	if st, m := e.jdo("POST", "/api/bench/runs/"+second+"/cancel", `{"comment":"wrong zones"}`, map[string]string{"X-Actor": "ann"}); st != 202 {
		t.Fatalf("cancel: %d %v", st, m)
	}
	r := waitRun(t, e, second)
	if r["state"] != "cancelled" || r["error"] != `cancelled before it started by ann: "wrong zones"` {
		t.Fatalf("%v %v", r["state"], r["error"])
	}
	if a := au.last("run_cancel"); a == nil || a["comment"] != "wrong zones" || a["actor"] != "ann" {
		t.Fatalf("audit: %v", a)
	}
	// the waiting one: cancelled while it waits until idle
	if st, m := e.jdo("POST", "/api/bench/runs/"+first+"/cancel", `{"comment":"not now"}`, map[string]string{"X-Actor": "ann"}); st != 202 {
		t.Fatalf("cancel: %d %v", st, m)
	}
	r = waitRun(t, e, first)
	if r["state"] != "cancelled" || !strings.Contains(r["error"].(string), `by ann: "not now"`) {
		t.Fatalf("%v %v", r["state"], r["error"])
	}
}

func waitRun(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, m := e.jdo("GET", "/api/bench/runs/"+id, "", nil)
		switch m["state"] {
		case "completed", "failed", "cancelled", "skipped":
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish: %v", id, m["state"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunDryReadSaysWhatWouldHappen(t *testing.T) {
	e, _ := startEnv(t)
	st, m := e.jdo("POST", "/api/bench/groups/g/run?dry_run=1", `{}`, nil)
	if st != 200 || m["dry_run"] != true || m["startable"] != true || m["step_count"] != float64(2) || m["max_run_minutes"] != float64(720) {
		t.Fatalf("%d %v", st, m)
	}
	loads := m["loads"].([]any)
	if len(loads) != 2 {
		t.Fatalf("loads %v", loads)
	}
	a := loads[0].(map[string]any)
	ms := a["machines"].([]any)
	if a["profile"] != "alpha" || a["profile_name"] != "alpha" || a["fits"] != "fits" || a["action"] != "start" || a["need_mib"] != float64(1000) ||
		len(ms) != 1 || ms[0].(map[string]any)["name"] != "host-1" {
		t.Fatalf("load %v", a)
	}
	if e.runCount() != 0 {
		t.Fatal("a dry read queued a run")
	}
	// loaded now, and by whom
	body(t, e.post("/v1/chat/completions", chat("gamma")))
	_, m = e.jdo("POST", "/api/bench/groups/g/run?dry_run=1", `{}`, nil)
	ln := m["loaded_now"].([]any)
	if len(ln) != 1 || ln[0].(map[string]any)["profile"] != "gamma" || ln[0].(map[string]any)["by"] == "" {
		t.Fatalf("loaded now %v", ln)
	}
	// the group does not wait for idle: gamma, which the run loads itself,
	// is unloaded first (named), alpha fits beside it
	wu := m["would_unload"].([]any)
	if len(wu) != 1 || wu[0].(map[string]any)["profile"] != "gamma" {
		t.Fatalf("would unload %v", wu)
	}
}

func TestRunRefusedOnTheSchedulersMachine(t *testing.T) {
	e, _ := startEnv(t)
	e.api.opts.benchLocalOK = false
	st, m := e.jdo("POST", "/api/bench/groups/g/run?dry_run=1", `{}`, nil)
	if st != 200 || m["startable"] != false || strings.Join(refusalCodes(m), ",") != "scheduler_gpu,scheduler_gpu" {
		t.Fatalf("%d %v", st, m["refusals"])
	}
	r0 := m["refusals"].([]any)[0].(map[string]any)
	if r0["entry"] != "Alpha check" || r0["machine"] != "host-1" || !strings.Contains(r0["message"].(string), "the scheduler's own machine") {
		t.Fatalf("%v", r0)
	}
	st, m = e.jdo("POST", "/api/bench/groups/g/run", `{"comment":"x"}`, nil)
	if st != 409 || errOf(m)["reason"] != "scheduler_gpu" || e.runCount() != 0 {
		t.Fatalf("%d %v runs %d", st, m, e.runCount())
	}
}

func TestRunRefusedOnAnExcludedZone(t *testing.T) {
	e, _ := startEnv(t)
	e.setConf(func(c *config.Config) {
		c.Profiles["gamma"].ForceNodes = []string{"host-1"}
		c.ExcludeNodes = []string{"host-1"}
	})
	st, m := e.jdo("POST", "/api/bench/groups/g/run?dry_run=1", `{}`, nil)
	codes := refusalCodes(m)
	if st != 200 || m["startable"] != false || len(codes) == 0 {
		t.Fatalf("%d %v", st, m["refusals"])
	}
	found := false
	for _, r := range m["refusals"].([]any) {
		r := r.(map[string]any)
		if r["code"] == "excluded_zone" && r["entry"] == "Gamma check" && r["machine"] == "host-1" && strings.Contains(r["message"].(string), "excluded as a zone") {
			found = true
		}
	}
	if !found {
		t.Fatalf("%v", m["refusals"])
	}
	// the whole group is refused, the entry that would fit included
	st, m = e.jdo("POST", "/api/bench/groups/g/run", `{"comment":"x"}`, nil)
	if st != 409 || e.runCount() != 0 {
		t.Fatalf("%d %v", st, m)
	}
}

func TestRunRefusedWholeWhenAnEntryIsNotStartable(t *testing.T) {
	e, _ := startEnv(t)
	e.do("POST", "/api/bench/groups", `{"id":"outer","name":"Outer","benchmarks":[{"group":"g"}],"cooldown_s":0,"only_when_idle":false}`)
	e.setConf(func(c *config.Config) { c.Bench.NotStartable = []string{"b-gamma"} })
	for _, path := range []string{"/api/bench/groups/g/run", "/api/bench/groups/outer/run", "/api/bench/benchmarks/b-gamma/run"} {
		st, m := e.jdo("POST", path+"?dry_run=1", `{}`, nil)
		if st != 200 || m["startable"] != false || refusalCodes(m)[0] != "not_startable" {
			t.Fatalf("%s: %d %v", path, st, m["refusals"])
		}
		st, m = e.jdo("POST", path, `{"comment":"x"}`, nil)
		if st != 409 || errOf(m)["reason"] != "not_startable" || !strings.Contains(errOf(m)["message"].(string), "bench_not_startable") {
			t.Fatalf("%s: %d %v", path, st, m)
		}
	}
	if e.runCount() != 0 {
		t.Fatal("a refused run was queued")
	}
	// the other entry alone starts
	if st, m := e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"comment":"x","only_when_idle":false}`, nil); st != 202 {
		t.Fatalf("%d %v", st, m)
	}
	// a group named in the list, held by another group
	e.setConf(func(c *config.Config) { c.Bench.NotStartable = []string{"g"} })
	if st, m := e.jdo("POST", "/api/bench/groups/outer/run?dry_run=1", `{}`, nil); m["startable"] != false || refusalCodes(m)[0] != "not_startable" {
		t.Fatalf("nested: %d %v", st, m["refusals"])
	}
}

// The evict list: a start names what the run may unload; when the list is
// not what the run would unload now, nothing starts and the answer has the
// new list.
func TestRunEvictListIsCheckedAgainAtStart(t *testing.T) {
	e, _ := startEnv(t)
	body(t, e.post("/v1/chat/completions", chat("alpha")))
	eventually(t, "alpha ready", func() bool { return e.instState("alpha") == "ready" })
	// beta:64k (7000) does not fit beside alpha (1000) on the 8000 MiB machine
	st, m := e.jdo("POST", "/api/bench/benchmarks/b-beta/run?dry_run=1", `{"evict_others":true}`, nil)
	wu, _ := m["would_unload"].([]any)
	if st != 200 || len(wu) != 1 || wu[0].(map[string]any)["profile"] != "alpha" {
		t.Fatalf("%d would unload %v loads %v", st, wu, m["loads"])
	}
	// waiting until idle without evict_others: nothing would be unloaded
	if _, m := e.jdo("POST", "/api/bench/benchmarks/b-beta/run?dry_run=1", `{}`, nil); len(m["would_unload"].([]any)) != 0 {
		t.Fatalf("%v", m["would_unload"])
	}
	// a start that names nothing is refused with the list
	st, m = e.jdo("POST", "/api/bench/benchmarks/b-beta/run", `{"evict_others":true,"comment":"x"}`, nil)
	if st != 409 || errOf(m)["reason"] != "evict_set_changed" || len(m["would_unload"].([]any)) != 1 || e.runCount() != 0 {
		t.Fatalf("%d %v", st, m)
	}
	// the list changed since it was shown: alpha is gone, gamma is not in it
	e.sched.Unload(context.Background(), "alpha", true)
	eventually(t, "alpha gone", func() bool { return e.instState("alpha") == "" })
	st, m = e.jdo("POST", "/api/bench/benchmarks/b-beta/run", `{"evict_others":true,"evict":["alpha"],"comment":"x"}`, nil)
	if st != 409 || errOf(m)["reason"] != "evict_set_changed" || len(m["would_unload"].([]any)) != 0 || e.runCount() != 0 {
		t.Fatalf("%d %v", st, m)
	}
	// the list as it is now: started
	body(t, e.post("/v1/chat/completions", chat("alpha")))
	eventually(t, "alpha ready", func() bool { return e.instState("alpha") == "ready" })
	st, m = e.jdo("POST", "/api/bench/benchmarks/b-beta/run", `{"evict_others":true,"evict":["alpha"],"comment":"x"}`, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, m)
	}
	waitRun(t, e, m["id"].(string))
}

func TestRunQueueFullInPlainWords(t *testing.T) {
	e, _ := startEnv(t)
	body(t, e.post("/v1/chat/completions", chat("gamma"))) // the runs wait until idle: nothing starts
	var ids []string
	for i := 0; i <= bench.MaxQueuedRuns; i++ {
		st, m := e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"comment":"fill","idle_wait_s":600}`, nil)
		if st != 202 {
			t.Fatalf("run %d: %d %v", i, st, m)
		}
		ids = append(ids, m["id"].(string))
	}
	st, m := e.jdo("POST", "/api/bench/benchmarks/b-alpha/run?dry_run=1", `{}`, nil)
	if st != 200 || m["startable"] != false || refusalCodes(m)[0] != "queue_full" {
		t.Fatalf("%d %v", st, m["refusals"])
	}
	st, m = e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"comment":"one more"}`, nil)
	if st != 429 || errOf(m)["reason"] != "queue_full" || errOf(m)["message"] != "20 runs are queued already, the most the queue holds: wait until one finishes, or cancel one" {
		t.Fatalf("%d %v", st, m)
	}
	for _, id := range ids {
		e.jdo("POST", "/api/bench/runs/"+id+"/cancel", `{"comment":"done"}`, nil)
	}
}

func TestRunAgainWarnsWhenTheDefinitionChanged(t *testing.T) {
	e, _ := startEnv(t)
	st, m := e.jdo("POST", "/api/bench/benchmarks/b-alpha/run", `{"comment":"x","only_when_idle":false}`, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, m)
	}
	id := m["id"].(string)
	waitRun(t, e, id)
	_, m = e.jdo("POST", "/api/bench/benchmarks/b-alpha/run?dry_run=1", `{"again_of":"`+id+`"}`, nil)
	if w := m["warnings"].([]any); len(w) != 0 {
		t.Fatalf("nothing changed: %v", w)
	}
	if st, b, _ := e.do("PUT", "/api/bench/benchmarks/b-alpha", `{"id":"b-alpha","name":"Alpha check","profile":"alpha","requests":[{"prompt":"hello","max_tokens":4}]}`); st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	_, m = e.jdo("POST", "/api/bench/benchmarks/b-alpha/run?dry_run=1", `{"again_of":"`+id+`"}`, nil)
	w := m["warnings"].([]any)
	if len(w) != 1 || w[0].(map[string]any)["code"] != "definition_changed" || !strings.Contains(w[0].(map[string]any)["message"].(string), "Alpha check") || m["startable"] != true {
		t.Fatalf("%v", m)
	}
}

// A benchmark whose profile went with its model file to deleted models is
// refused in plain words, naming the file, when and by whom.
func TestRunRefusedWhenTheModelIsInDeletedModels(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	m, err := bench.NewManager(bench.ManagerOptions{Store: te.bs, API: &bench.HTTPAPI{Base: te.front.URL}, Version: "t",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdlePoll: 5 * time.Millisecond, LoadPoll: 5 * time.Millisecond, ProfileOK: te.api.profileOK})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop(context.Background()) })
	te.api.opts.Bench = m
	te.api.opts.benchLocalOK = true
	if st, b, _ := te.do("POST", "/api/bench/benchmarks", `{"id":"on-a","name":"On a","profile":"a1","requests":[{"prompt":"hi","max_tokens":4}]}`); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	if st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI); st != 200 {
		t.Fatalf("delete: %d %v", st, m)
	}
	st, ans := te.jdo("POST", "/api/bench/benchmarks/on-a/run?dry_run=1", `{}`, nil)
	if st != 200 || ans["startable"] != false || refusalCodes(ans)[0] != "model_deleted" {
		t.Fatalf("%d %v", st, ans)
	}
	msg := ans["refusals"].([]any)[0].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "a.gguf") || !strings.Contains(msg, "alex") || !strings.Contains(msg, "deleted models") {
		t.Fatalf("%q", msg)
	}
	st, ans = te.jdo("POST", "/api/bench/benchmarks/on-a/run", `{"comment":"x"}`, nil)
	if st != 422 || errOf(ans)["reason"] != "model_deleted" {
		t.Fatalf("%d %v", st, ans)
	}
}

// The lists mark what bench_not_startable keeps from starting, with the
// reason, so the page shows "Not startable here: ..." in place of Run: the
// entry itself, and a group holding it at any depth.
func TestRunListsMarkWhatIsNotStartable(t *testing.T) {
	e, _ := startEnv(t)
	e.do("POST", "/api/bench/groups", `{"id":"outer","name":"Outer","benchmarks":[{"group":"g"}],"cooldown_s":0}`)
	e.setConf(func(c *config.Config) { c.Bench.NotStartable = []string{"b-gamma"} })
	marks := func(path, key string) map[string]string {
		_, m := e.jdo("GET", path, "", nil)
		out := map[string]string{}
		for _, it := range m[key].([]any) {
			x := it.(map[string]any)
			if s, _ := x["not_startable"].(string); s != "" {
				out[x["id"].(string)] = s
			}
		}
		return out
	}
	bs := marks("/api/bench/benchmarks", "benchmarks")
	if len(bs) != 1 || !strings.Contains(bs["b-gamma"], "bench_not_startable") {
		t.Fatalf("benchmarks: %v", bs)
	}
	gs := marks("/api/bench/groups", "groups")
	if len(gs) != 2 || !strings.Contains(gs["g"], "b-gamma") || !strings.Contains(gs["outer"], "b-gamma") {
		t.Fatalf("groups: %v", gs)
	}
	e.setConf(func(c *config.Config) { c.Bench.NotStartable = nil })
	if gs := marks("/api/bench/groups", "groups"); len(gs) != 0 {
		t.Fatalf("after the list was emptied: %v", gs)
	}
}
