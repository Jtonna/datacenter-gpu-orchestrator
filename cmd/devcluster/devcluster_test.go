package main

// Cheap checks of the mock layer: no fake fleet, no fakellama, no network.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if code != 200 || run == nil || run["state"] != "running" || run["current"].(map[string]any)["benchmark_id"] != "kimi-72b-load-3w" {
		t.Fatalf("state: %d %v", code, m)
	}
	code, m = call(mux, "GET", "/api/bench/aggregates", "")
	if code != 200 || len(m["benchmarks"].([]any)) != 9 || len(m["by_model"].([]any)) == 0 {
		t.Fatalf("aggregates: %d %v", code, m)
	}
	if code, _ = call(mux, "POST", "/api/bench/benchmarks", `{"id":"x","profile":"nope","phases":[{"type":"load"}]}`); code != 422 {
		t.Fatalf("bad benchmark: %d", code)
	}
	if code, _ = call(mux, "DELETE", "/api/bench/benchmarks/gemma-load-1w", ""); code != 409 {
		t.Fatalf("delete of a benchmark a suite uses: %d", code)
	}
	if code, _ = call(mux, "GET", "/api/bench/runs/"+run["id"].(string)+"/export?format=csv", ""); code != 200 {
		t.Fatalf("export: %d", code)
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
