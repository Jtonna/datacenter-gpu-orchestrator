package e2e

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const benchProfiles = profilesBasic + `  too-big:
    model: too-big.gguf
    ctx: 4096
    needs_mib: 99000
    allow_tiers: [1]
`

func benchCluster(t *testing.T) *cluster {
	t.Helper()
	dir := t.TempDir()
	top := "data_dir: \"" + filepath.ToSlash(filepath.Join(dir, "data")) + "\"\nhistory_dir: \"" + filepath.ToSlash(filepath.Join(dir, "history")) + "\"\n"
	o := options{profiles: benchProfiles, topLevel: top}
	o.members = append(o.members, ps5(psA, "ps5-062e1d84", "10.0.0.2"))
	return startCluster(t, o)
}

func mustStatus(t *testing.T, c *cluster, method, path, body string, want int) map[string]any {
	t.Helper()
	st, b, _ := c.do(method, path, body)
	if st != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, st, want, b)
	}
	var m map[string]any
	json.Unmarshal([]byte(b), &m)
	return m
}

// A two-benchmark suite through the real binary: one benchmark loads and is
// measured, one is supposed to be refused (and passes because the scheduler
// refuses). The results, the markdown export, the history join and the live
// channel are checked.
func TestBenchSuiteEndToEnd(t *testing.T) {
	c := benchCluster(t)
	mustStatus(t, c, "POST", "/api/bench/benchmarks", `{"id":"tiny-cold","name":"Tiny cold","profile":"tiny:2k","repeats":3,
	  "requests":[{"name":"short","prompt":"Say hi.","max_tokens":16}]}`, 201)
	mustStatus(t, c, "POST", "/api/bench/benchmarks", `{"id":"too-big","name":"Too big","profile":"too-big","expect":{"outcome":"refused"},
	  "requests":[{"prompt":"x","max_tokens":4}]}`, 201)
	mustStatus(t, c, "POST", "/api/bench/suites", `{"id":"smoke","name":"Smoke","benchmarks":["tiny-cold","too-big"],"cooldown_s":0}`, 201)

	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(c.url, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	msgs := make(chan map[string]any, 1024)
	go func() {
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				close(msgs)
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			msgs <- m
		}
	}()
	hello := <-msgs
	if hello["type"] != "hello" || hello["data"].(map[string]any)["bench"] != "on" {
		t.Fatalf("hello %v", hello)
	}

	run := mustStatus(t, c, "POST", "/api/bench/suites/smoke/run", `{"note":"e2e"}`, 202)
	id := run["id"].(string)
	var detail map[string]any
	c.waitFor("the run to finish", 60*time.Second, func() bool {
		_, b, _ := c.do("GET", "/api/bench/runs/"+id, "")
		detail = nil
		json.Unmarshal([]byte(b), &detail)
		s, _ := detail["state"].(string)
		return s == "completed" || s == "failed" || s == "cancelled" || s == "skipped"
	})
	if detail["state"] != "completed" {
		t.Fatalf("run ended %v: %v", detail["state"], detail["error"])
	}
	steps := detail["steps"].([]any)
	s0, s1 := steps[0].(map[string]any), steps[1].(map[string]any)
	if s0["outcome"] != "ready" || s0["load"] == nil || s0["expect"].(map[string]any)["pass"] != true {
		t.Fatalf("step 0: %v", s0)
	}
	stats := s0["stats"].(map[string]any)
	if d := stats["decode_tps"].(map[string]any); d["n"].(float64) != 3 || d["mean"].(float64) != 100 {
		t.Fatalf("decode stats %v (warmup must be excluded, fakellama reports 100 tok/s)", d)
	}
	if s1["outcome"] != "refused" || s1["http_status"].(float64) != 503 || s1["expect"].(map[string]any)["pass"] != true ||
		!strings.Contains(s1["reason"].(string), "does not fit") {
		t.Fatalf("step 1: %v", s1)
	}
	if got := c.runningModels(); len(got) != 0 {
		t.Fatalf("the run left models loaded: %v", got)
	}

	_, md, _ := c.do("GET", "/api/bench/runs/"+id+"/export?format=md", "")
	t.Logf("markdown export:\n%s", md)
	if !strings.Contains(md, "| Tiny cold | tiny:2k |") || !strings.Contains(md, "refused, passed") || !strings.Contains(md, "100.00") {
		t.Fatalf("markdown:\n%s", md)
	}
	_, js, _ := c.do("GET", "/api/bench/runs/"+id+"/export?format=json", "")
	t.Logf("run JSON:\n%s", js)

	// History and results join: the run and its steps are tagged with the run id, and the
	// load phase of the step names the scheduler's load session (POST /api/load carries no
	// run tag, so the session id is the join key for the load itself).
	sid, _ := s0["load"].(map[string]any)["session_id"].(string)
	if sid == "" {
		t.Fatalf("the step's load names no session: %v", s0["load"])
	}
	c.waitFor("the loadFinished record of the load session and the tagged run records", 10*time.Second, func() bool {
		h := c.jsonGet("/api/history?type=benchRunFinished,benchStepFinished")
		tagged := map[string]bool{}
		for _, e := range h["events"].([]any) {
			em := e.(map[string]any)
			if em["data"].(map[string]any)["bench_run"] == id {
				tagged[em["type"].(string)] = true
			}
		}
		l := c.jsonGet("/api/history?type=loadFinished&session_id=" + sid)
		return len(l["events"].([]any)) == 1 && tagged["benchRunFinished"] && tagged["benchStepFinished"]
	})

	// The live channel carried the run.
	kinds := map[string]bool{}
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				break collect
			}
			if m["type"] == "bench" {
				kinds[m["data"].(map[string]any)["kind"].(string)] = true
			}
			if kinds["step_finished"] && kinds["run"] && kinds["progress"] {
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	for _, k := range []string{"run", "step_started", "step_finished", "progress"} {
		if !kinds[k] {
			t.Errorf("no bench %q message on /ws; saw %v", k, kinds)
		}
	}
	if st := mustStatus(t, c, "GET", "/api/bench/state", "", 200); st["enabled"] != true {
		t.Fatalf("%v", st)
	}
}

// With the bench_enabled setting off the feature is off and the rest works.
func TestBenchOffWhenSettingOff(t *testing.T) {
	o := options{profiles: profilesBasic, topLevel: "bench:\n  enabled: false\n"}
	o.members = append(o.members, ps5(psA, "ps5-062e1d84", "10.0.0.2"))
	c := startCluster(t, o)
	st, b, _ := c.do("GET", "/api/bench/suites", "")
	if st != 503 {
		t.Fatalf("%d %s", st, b)
	}
	if status, _ := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("serving broke: %d", status)
	}
}

// A load-only benchmark and a conversation benchmark in one suite, through the
// real binary: the load sends no chat request, the conversation is recorded
// turn by turn, the live channel carries the progress model, and the
// aggregates and the schema answer.
func TestBenchPhasesEndToEnd(t *testing.T) {
	c := benchCluster(t)
	mustStatus(t, c, "POST", "/api/bench/benchmarks", `{"id":"load-only","name":"Load only","description":"cold load of tiny, nothing else","profile":"tiny:2k",
	  "phases":[{"type":"load"},{"type":"unload"}],"expect":{"max_load_s":120}}`, 201)
	mustStatus(t, c, "POST", "/api/bench/benchmarks", `{"id":"conv","name":"Conversation","description":"three turns on tiny","profile":"tiny:2k",
	  "phases":[{"type":"load"},{"type":"conversation","system":"Be brief.","stream":true,"max_tokens":16,"turns":[{"content":"Hello."},{"content":"And again."},{"content":"Bye."}]},{"type":"unload"}],
	  "expect":{"min_decode_tps":10,"max_ttft_ms":5000}}`, 201)
	mustStatus(t, c, "POST", "/api/bench/suites", `{"id":"phases","name":"Phases","benchmarks":["load-only","conv"],"cooldown_s":0}`, 201)

	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(c.url, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	var mu sync.Mutex
	var bench []map[string]any
	go func() {
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			if m["type"] == "bench" {
				mu.Lock()
				bench = append(bench, m["data"].(map[string]any))
				mu.Unlock()
			}
		}
	}()

	run := mustStatus(t, c, "POST", "/api/bench/suites/phases/run", `{"note":"phases e2e"}`, 202)
	id := run["id"].(string)
	var detail map[string]any
	c.waitFor("the run to finish", 60*time.Second, func() bool {
		_, b, _ := c.do("GET", "/api/bench/runs/"+id, "")
		detail = nil
		json.Unmarshal([]byte(b), &detail)
		s, _ := detail["state"].(string)
		return s == "completed" || s == "failed" || s == "cancelled" || s == "skipped"
	})
	if detail["state"] != "completed" {
		t.Fatalf("run ended %v: %v", detail["state"], detail["error"])
	}
	steps := detail["steps"].([]any)
	s0, s1 := steps[0].(map[string]any), steps[1].(map[string]any)
	if reqs, _ := s0["requests"].([]any); len(reqs) != 0 || len(s0["phases"].([]any)) != 2 || s0["phases"].([]any)[0].(map[string]any)["load"] == nil {
		t.Fatalf("load-only step: %v", s0)
	}
	if s0["expect"].(map[string]any)["pass"] != true || s1["expect"].(map[string]any)["pass"] != true {
		t.Fatalf("expectations: %v %v", s0["expect"], s1["expect"])
	}
	conv := s1["phases"].([]any)[1].(map[string]any)
	turns := conv["turns"].([]any)
	if len(turns) != 3 || turns[0].(map[string]any)["decode_tps"].(float64) != 100 || turns[0].(map[string]any)["ttft_ms"] == nil {
		t.Fatalf("conversation turns: %v", conv)
	}
	if reqs := s1["requests"].([]any); len(reqs) != 3 || reqs[2].(map[string]any)["phase"] != "conversation" || reqs[2].(map[string]any)["turn"].(float64) != 3 {
		t.Fatalf("conversation requests: %v", reqs)
	}
	env := detail["env"].(map[string]any)
	t.Logf("run environment: %v", env)
	if env["scheduler_version"] == "" || s1["model_file"] != "tiny.gguf" {
		t.Fatalf("env %v model %v", env, s1["model_file"])
	}
	if got := c.runningModels(); len(got) != 0 {
		t.Fatalf("the run left models loaded: %v", got)
	}

	// the live channel: the progress model, monotonic, 100 only at the end
	c.waitFor("the aggregate_updated message", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range bench {
			if m["kind"] == "aggregate_updated" {
				return true
			}
		}
		return false
	})
	mu.Lock()
	msgs := append([]map[string]any(nil), bench...)
	mu.Unlock()
	last, n := -1.0, 0
	phasesSeen := map[string]bool{}
	for _, m := range msgs {
		if m["kind"] != "progress" {
			continue
		}
		n++
		r := m["run"].(map[string]any)
		p := r["percent"].(float64)
		if p < last {
			t.Fatalf("progress went back: %v after %v", p, last)
		}
		last = p
		if p >= 100 && r["state"] != "completed" {
			t.Fatalf("100 percent while %v", r["state"])
		}
		if cur, _ := r["current"].(map[string]any); cur != nil {
			phasesSeen[cur["benchmark_id"].(string)+"/"+cur["phase"].(map[string]any)["type"].(string)] = true
			if cur["benchmark_id"] == "load-only" {
				if nx := r["next"].(map[string]any); nx["benchmark_id"] != "conv" || nx["description"] != "three turns on tiny" {
					t.Fatalf("next %v", nx)
				}
			}
		}
	}
	if n < 3 || last != 100 || !phasesSeen["load-only/load"] || !phasesSeen["conv/conversation"] {
		t.Fatalf("%d progress messages, last %v, phases seen %v", n, last, phasesSeen)
	}
	if st := mustStatus(t, c, "GET", "/api/bench/state", "", 200); st["run"] != nil {
		t.Fatalf("an idle scheduler has no run progress: %v", st["run"])
	}

	// aggregates and the schema
	ag := mustStatus(t, c, "GET", "/api/bench/aggregates", "", 200)
	t.Logf("aggregates: %v", ag["benchmarks"])
	if ag["runs_considered"].(float64) != 1 || len(ag["benchmarks"].([]any)) != 2 || len(ag["by_model"].([]any)) != 1 {
		t.Fatalf("%v", ag)
	}
	var cb map[string]any
	for _, b := range ag["benchmarks"].([]any) {
		if b.(map[string]any)["benchmark_id"] == "conv" {
			cb = b.(map[string]any)
		}
	}
	if cb == nil || len(cb["turns"].([]any)) != 3 || cb["metrics"].(map[string]any)["decode_tps"].(map[string]any)["last"].(float64) != 100 {
		t.Fatalf("%v", cb)
	}
	sc := mustStatus(t, c, "GET", "/api/bench/schema", "", 200)
	if len(sc["phase_types"].([]any)) != 7 || len(sc["templates"].([]any)) != 5 {
		t.Fatalf("%v", sc)
	}
}
