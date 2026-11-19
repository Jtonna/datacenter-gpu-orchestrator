package main

// Cheap checks of the mock layer: no fake fleet, no fakellama, no network.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func call(h http.Handler, method, path, body string, hdr ...string) (int, map[string]any) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestConfigMock(t *testing.T) {
	audit := &auditLog{}
	s := newConfigStore(audit)
	s.reset(false)
	var notes []map[string]any
	s.notify = func(d map[string]any) { notes = append(notes, d) }
	s.profileRefs = func(id string) []string {
		if id == "qwen3.5-4b" {
			return []string{"qwen4b-simple"}
		}
		return nil
	}
	mux := http.NewServeMux()
	s.routes(mux)
	mux.HandleFunc("GET /api/audit", audit.handle)

	code, m := call(mux, "GET", "/api/config", "")
	if code != 200 || m["revision"] != 14.0 || m["configured"] != true || len(m["settings_set"].([]any)) != 3 {
		t.Fatalf("GET /api/config: %d %v", code, m)
	}
	code, m = call(mux, "PATCH", "/api/config/settings?dry_run=1", `{"queue_timeout":"1200s"}`)
	if code != 200 || m["dry_run"] != true || m["revision"] != 14.0 || len(m["diff"].([]any)) != 1 || len(notes) != 0 {
		t.Fatalf("dry run: %d %v", code, m)
	}
	code, m = call(mux, "PATCH", "/api/config/settings", `{"queue_timeout":"1200s","history_keep_files":null}`,
		"If-Match", "14", "X-Source", "ui", "X-Actor", "t", "X-Comment", "c")
	if code != 200 || m["revision"] != 15.0 || m["changed"] != true || len(m["audit_entry_ids"].([]any)) != 2 || len(notes) != 1 ||
		notes[0]["section"] != "multiple" || len(m["restart_required"].([]any)) != 1 {
		t.Fatalf("write: %d %v notes %v", code, m, notes)
	}
	code, m = call(mux, "PATCH", "/api/config/settings", `{"queue_timeout":"20m"}`)
	if code != 200 || m["changed"] != false || m["revision"] != 15.0 {
		t.Fatalf("no-op write: %d %v", code, m)
	}
	code, m = call(mux, "PATCH", "/api/config/settings", `{"queue_timeout":"1300s"}`, "If-Match", "14")
	e := m["error"].(map[string]any)
	if code != 409 || e["type"] != "stale_revision" || e["current_revision"] != 15.0 {
		t.Fatalf("stale If-Match: %d %v", code, m)
	}
	code, m = call(mux, "PATCH", "/api/config/settings", `{"max_pool":0,"min_hold":"20m"}`)
	if e := m["error"].(map[string]any); code != 400 || e["type"] != "invalid_config" || len(e["messages"].([]any)) != 2 {
		t.Fatalf("bad values: %d %v", code, m)
	}
	if code, m = call(mux, "DELETE", "/api/config/profiles/qwen3.5-4b", ""); code != 409 || m["error"].(map[string]any)["type"] != "profile_referenced" {
		t.Fatalf("referenced delete: %d %v", code, m)
	}
	code, m = call(mux, "POST", "/api/config/profiles", `{"id":"tiny","profile":{"model":"Qwen_Qwen3.5-4B-Q4_K_M.gguf","ctx":4096,"ttl":"90s"}}`)
	if code != 201 || m["id"] != "tiny" || m["revision"] != 16.0 {
		t.Fatalf("create: %d %v", code, m)
	}
	if code, _ = call(mux, "POST", "/api/config/profiles", `{"id":"tiny","model":"Qwen_Qwen3.5-4B-Q4_K_M.gguf","ctx":4096}`); code != 409 {
		t.Fatalf("duplicate create: %d", code)
	}
	code, m = call(mux, "GET", "/api/audit?limit=1", "")
	ae := m["entries"].([]any)[0].(map[string]any)
	if code != 200 || ae["section"] != "profiles" || ae["target"] != "tiny" || ae["action"] != "create" || ae["revision"] != 16.0 || ae["actor"] != "anonymous" {
		t.Fatalf("audit: %d %v", code, m)
	}
	if code, m = call(mux, "GET", "/api/config/revisions", ""); code != 200 || m["current"] != 16.0 || len(m["revisions"].([]any)) != 16 {
		t.Fatalf("revisions: %d %v", code, m)
	}
	if code, m = call(mux, "POST", "/api/config/revisions/1/rollback", `{"comment":"back"}`); code != 200 || m["revision"] != 17.0 || m["rolled_back_to"] != 1.0 {
		t.Fatalf("rollback: %d %v", code, m)
	}

	fresh := newConfigStore(&auditLog{})
	fresh.reset(true)
	if v := fresh.view(); v["revision"] != 0 || v["configured"] != false || len(asMap(v["profiles"])) != 0 {
		t.Fatalf("bare: %v", v)
	}
}

func TestBenchMock(t *testing.T) {
	audit := &auditLog{}
	cfg := newConfigStore(audit)
	cfg.reset(false)
	store := newBenchStore()
	eng := newBenchEngine(store)
	var frames []map[string]any
	eng.emit = func(ev map[string]any) { frames = append(frames, ev) }
	eng.reset(false, "load", time.Now())
	defer eng.reset(true, "", time.Now())
	mux := http.NewServeMux()
	(&benchAPI{store: store, engine: eng, cfg: cfg}).routes(mux)

	code, m := call(mux, "GET", "/api/bench/state", "")
	run, _ := m["run"].(map[string]any)
	if code != 200 || run == nil || run["state"] != "running" || m["current"] == nil {
		t.Fatalf("state: %d %v", code, m)
	}
	cur := run["current"].(map[string]any)
	phase := cur["phase"].(map[string]any)
	detail := cur["detail"].(map[string]any)
	if cur["benchmark_id"] != "kimi-72b-load-3w" || cur["index"] != 6.0 || phase["type"] != "load" || phase["index"] != 0.0 ||
		detail["text"] == nil || detail["load"].(map[string]any)["workers"] == nil || run["steps_done"] != 6.0 || len(run["queue"].([]any)) != 1 {
		t.Fatalf("progress: %v", run)
	}
	code, m = call(mux, "GET", "/api/bench/runs/"+run["id"].(string), "")
	if env := m["env"].(map[string]any); code != 200 || env["config_revision"] != "14" || len(env["workers"].([]any)) != 3 {
		t.Fatalf("run env: %d %v", code, m["env"])
	}
	conv := m["steps"].([]any)[3].(map[string]any)
	ph1 := conv["phases"].([]any)[1].(map[string]any)
	if ph1["type"] != "conversation" || len(ph1["turns"].([]any)) != 5 || conv["requests"].([]any)[0].(map[string]any)["phase_idx"] != 1.0 {
		t.Fatalf("conversation step: %v", conv)
	}
	code, m = call(mux, "GET", "/api/bench/schema", "")
	if code != 200 || len(m["phase_types"].([]any)) != 7 || len(m["templates"].([]any)) != 5 || m["limits"] == nil {
		t.Fatalf("schema: %d %v", code, m)
	}
	code, m = call(mux, "GET", "/api/bench/aggregates", "")
	if code != 200 || len(m["benchmarks"].([]any)) != 9 || len(m["by_model_nodes"].([]any)) == 0 || m["environment"] == nil {
		t.Fatalf("aggregates: %d %v", code, m)
	}
	for _, b := range m["benchmarks"].([]any) {
		b := b.(map[string]any)
		if b["benchmark_id"] == "gemma-conversation" && len(b["turns"].([]any)) != 5 {
			t.Fatalf("conversation turns: %v", b["turns"])
		}
		if b["benchmark_id"] == "gemma-load-1w" {
			ls := b["metrics"].(map[string]any)["load_s"].(map[string]any)
			if ls["threshold_pct"] != 15.0 || ls["points"].([]any)[0].(map[string]any)["at"] == nil {
				t.Fatalf("metric: %v", ls)
			}
		}
	}
	if code, m = call(mux, "GET", "/api/bench/aggregates?env=same", ""); code != 200 || m["runs_considered"].(float64) >= 60 {
		t.Fatalf("env=same: %d %v", code, m["runs_considered"])
	}
	if code, _ = call(mux, "POST", "/api/bench/benchmarks", `{"id":"x","profile":"nope","phases":[{"type":"load"}]}`); code != 422 {
		t.Fatalf("bad benchmark: %d", code)
	}
	if code, _ = call(mux, "POST", "/api/bench/benchmarks", `{"id":"x","profile":"qwen3.5-4b","phases":[{"type":"conversation","turns":["hi"]}]}`); code != 422 {
		t.Fatalf("string turn: %d", code)
	}
	code, m = call(mux, "POST", "/api/bench/benchmarks", `{"id":"x","profile":"qwen3.5-4b","phases":[{"type":"load"},{"type":"conversation","turns":[{"content":" "}],"max_tokens":0}]}`)
	if msg, _ := m["error"].(map[string]any)["message"].(string); code != 422 ||
		msg != "phases[1] (conversation): max_tokens must be a whole number from 1 to 65536, got 0\nphases[1] (conversation): turns[0].content is empty" {
		t.Fatalf("validation messages: %d %q", code, msg)
	}
	if code, _ = call(mux, "POST", "/api/bench/benchmarks", `{"id":"x","profile":"qwen3.5-4b","phases":[{"type":"load"},{"type":"conversation","turns":[{"content":"hi"}]},{"type":"unload"}]}`); code != 201 {
		t.Fatalf("good benchmark: %d", code)
	}
	if code, _ = call(mux, "DELETE", "/api/bench/benchmarks/gemma-load-1w", ""); code != 409 {
		t.Fatalf("delete of a benchmark a suite uses: %d", code)
	}
	if code, _ = call(mux, "GET", "/api/bench/runs/"+run["id"].(string)+"/export?format=csv", ""); code != 200 {
		t.Fatalf("export: %d", code)
	}
}

// TestBenchPlay plays the side-by-side suite (a parallel step, then a
// conversation) and checks the frames it sends.
func TestBenchPlay(t *testing.T) {
	if testing.Short() {
		t.Skip("plays a run in real time")
	}
	store := newBenchStore()
	eng := newBenchEngine(store)
	var mu sync.Mutex
	kinds := map[string]int{}
	var members, done map[string]any
	eng.emit = func(ev map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		k := ev["kind"].(string)
		kinds[k]++
		if k == "progress" && members == nil {
			if c, _ := ev["run"].(map[string]any)["current"].(map[string]any); c != nil {
				if d, _ := c["detail"].(map[string]any); d != nil && d["members"] != nil {
					members = c
				}
			}
		}
		if k == "aggregate_updated" {
			done = ev
		}
	}
	eng.reset(false, "", time.Now())
	defer eng.reset(true, "", time.Now())
	if _, status, msg := eng.enqueue("side-by-side", "", "test", "manual"); status != 0 {
		t.Fatal(status, msg)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		mu.Lock()
		d := done
		mu.Unlock()
		if d != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %v", kinds)
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, k := range []string{"run", "progress", "step_started", "step_finished", "request", "load", "aggregate_updated"} {
		if kinds[k] == 0 {
			t.Errorf("no %s frame: %v", k, kinds)
		}
	}
	if members == nil || members["profile"] != "qwen3.5-4b + bench-gemma-12b-1w" || len(members["detail"].(map[string]any)["members"].([]map[string]any)) != 2 {
		t.Errorf("parallel progress: %v", members)
	}
	if done["state"] != "completed" || len(done["benchmarks"].([]string)) != 3 {
		t.Errorf("aggregate_updated: %v", done)
	}
}

func TestRelayAugment(t *testing.T) {
	rl := newRelay("http://127.0.0.1:1", false, func() int { return 7 }, nil)
	out, hello := rl.augment([]byte(`{"type":"hello","ts":1,"data":{"bench":"on","topics":["status"]}}`))
	var f struct {
		Data map[string]any `json:"data"`
	}
	json.Unmarshal(out, &f)
	if !hello || f.Data["bench"] != "off" || f.Data["config_revision"] != 7.0 || len(f.Data["topics"].([]any)) != 2 {
		t.Fatalf("hello: %s", out)
	}
	out, _ = rl.augment([]byte(`{"type":"status","ts":1,"data":{"instances":[]}}`))
	if !strings.Contains(string(out), `"config_revision":7`) {
		t.Fatalf("status: %s", out)
	}
	if rl.upstream != "ws://127.0.0.1:1/ws" {
		t.Fatal(rl.upstream)
	}
}
