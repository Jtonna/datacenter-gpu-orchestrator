package e2e

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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

	// The scheduler tagged the load with the run id: history and results join.
	c.waitFor("the tagged loadFinished record", 10*time.Second, func() bool {
		h := c.jsonGet("/api/history?type=loadFinished,benchRunFinished,benchStepFinished")
		tagged := map[string]bool{}
		for _, e := range h["events"].([]any) {
			em := e.(map[string]any)
			if em["data"].(map[string]any)["bench_run"] == id {
				tagged[em["type"].(string)] = true
			}
		}
		return tagged["loadFinished"] && tagged["benchRunFinished"] && tagged["benchStepFinished"]
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

// Without data_dir the feature is off and the rest works.
func TestBenchOffWithoutDataDir(t *testing.T) {
	c := withPS5(t, nil)
	st, b, _ := c.do("GET", "/api/bench/suites", "")
	if st != 503 || !strings.Contains(b, "data_dir") {
		t.Fatalf("%d %s", st, b)
	}
	if status, _ := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("serving broke: %d", status)
	}
}
