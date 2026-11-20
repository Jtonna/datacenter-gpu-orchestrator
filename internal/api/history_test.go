package api

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

func openStore(t *testing.T, mod ...func(*history.Options)) *history.Store {
	t.Helper()
	o := history.Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, m := range mod {
		m(&o)
	}
	s := history.Open(o)
	t.Cleanup(s.Close)
	return s
}

func histEnv(t *testing.T, st *history.Store) *env {
	t.Helper()
	return newEnv(t, chatOK, func(o *Options) { o.History = st; o.Version = "test-1.2.3" })
}

// seed records a fixed set of events at known times.
func seed(st *history.Store) int64 {
	now := time.Now().UnixMilli()
	st.Record(history.Event{Ts: now - 5000, Type: "loadStarted", Severity: "info", Profile: "gemma", Nodes: []string{"ps5-1", "ps5-2"}, SessionID: "ld-1", Summary: "Loading gemma onto ps5-1, ps5-2."})
	st.Record(history.Event{Ts: now - 4000, Type: "loadFinished", Severity: "info", Profile: "gemma", Nodes: []string{"ps5-1", "ps5-2"}, SessionID: "ld-1",
		Summary: "gemma finished loading in 61.0 s (100.0 MB/s).", Data: map[string]any{"outcome": "ready", "load_s": 61.0, "avg_mb_s": 100.0, "bytes": 6100000000.0,
			"nodes_detail": []any{map[string]any{"node": "ps5-1", "bytes": 3e9, "memory_mib": 4000.0}}}})
	st.Record(history.Event{Ts: now - 3000, Type: "nodeLost", Severity: "error", Profile: "gemma", Nodes: []string{"ps5-2"}, SessionID: "ld-2", Summary: "Node ps5-2 was lost."})
	st.Record(history.Event{Ts: now - 2000, Type: "requestRefused", Severity: "warn", Profile: "devstral", Summary: "Request refused: backoff."})
	return now
}

func getJSONBody(t *testing.T, e *env, path string) map[string]any {
	t.Helper()
	resp := e.get(path)
	b := body(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%s: status %d ct %q body %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	return jsonOf(t, b)
}

func TestHistoryEndpointShape(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	now := seed(st)
	m := getJSONBody(t, e, "/api/history")
	t.Logf("GET /api/history -> %v", m)
	evs := m["events"].([]any)
	if len(evs) != 4 || m["history"] != "on" || m["truncated"] != false || m["total_matching"].(float64) != 4 ||
		m["oldest_ts"].(float64) != float64(now-5000) {
		t.Fatalf("%v", m)
	}
	first := evs[0].(map[string]any) // newest first
	for _, k := range []string{"ts", "seq", "type", "severity", "nodes", "summary", "data"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("event lacks %q: %v", k, first)
		}
	}
	if first["type"] != "requestRefused" {
		t.Fatalf("not newest first: %v", first)
	}
}

func TestHistoryFilters(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	now := seed(st)
	count := func(q string) int { return len(getJSONBody(t, e, "/api/history?"+q)["events"].([]any)) }
	for q, want := range map[string]int{
		"type=loadStarted,nodeLost": 2, "severity=warn": 2, "severity=error": 1, "node=ps5-2": 3, "profile=gemma": 3,
		"session_id=ld-1": 2, "q=LOST": 1, "limit=2": 2, "order=asc&limit=1": 1,
		"since=" + itoa(now-3500): 2, "until=" + itoa(now-3500): 2,
	} {
		if got := count(q); got != want {
			t.Errorf("%s: %d events, want %d", q, got, want)
		}
	}
	asc := getJSONBody(t, e, "/api/history?order=asc")["events"].([]any)
	if asc[0].(map[string]any)["type"] != "loadStarted" {
		t.Fatalf("asc first %v", asc[0])
	}
	if m := getJSONBody(t, e, "/api/history?limit=1"); m["truncated"] != true || m["total_matching"].(float64) != 4 {
		t.Fatalf("%v", m)
	}
	for _, bad := range []string{"since=abc", "until=-5", "severity=loud", "limit=0", "limit=x", "order=sideways"} {
		resp := e.get("/api/history?" + bad)
		b := body(t, resp)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d %s", bad, resp.StatusCode, b)
		}
		errBody(t, b)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestHistoryOffReturnsAnEmptyListAndSaysSo(t *testing.T) {
	e := newEnv(t, chatOK) // no History option
	m := getJSONBody(t, e, "/api/history")
	if m["history"] != "off" || len(m["events"].([]any)) != 0 || m["events"] == nil || m["total_matching"].(float64) != 0 || m["truncated"] != false {
		t.Fatalf("%v", m)
	}
	s := getJSONBody(t, e, "/api/history/summary")
	if s["history"] != "off" || s["incidents"] == nil || s["profiles"] == nil {
		t.Fatalf("%v", s)
	}
	resp := e.get("/api/history/export")
	if b := body(t, resp); resp.StatusCode != 200 || b != "" {
		t.Fatalf("export %d %q", resp.StatusCode, b)
	}
	if a := getJSONBody(t, e, "/api/about"); a["history"] != "off" {
		t.Fatalf("%v", a)
	}
}

func TestHistorySummaryShape(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	seed(st)
	m := getJSONBody(t, e, "/api/history/summary")
	t.Logf("GET /api/history/summary -> %v", m)
	by := m["by_severity"].(map[string]any)
	loads := m["loads"].(map[string]any)
	if m["history"] != "on" || m["events"].(float64) != 4 || by["error"].(float64) != 1 || by["warn"].(float64) != 1 ||
		loads["ready"].(float64) != 1 || loads["failed"].(float64) != 0 {
		t.Fatalf("%v", m)
	}
	p := m["profiles"].([]any)[0].(map[string]any)
	if p["profile"] != "gemma" || p["loads"].(float64) != 1 || p["min_s"].(float64) != 61 || p["median_s"].(float64) != 61 ||
		p["max_s"].(float64) != 61 || p["avg_mb_s"].(float64) != 100 {
		t.Fatalf("%v", p)
	}
	n := m["nodes"].([]any)[0].(map[string]any)
	if n["node"] != "ps5-2" || n["lost"].(float64) != 1 {
		t.Fatalf("%v", n)
	}
	if inc := m["incidents"].([]any); len(inc) != 1 || inc[0].(map[string]any)["type"] != "nodeLost" {
		t.Fatalf("%v", inc)
	}
	if _, ok := m["by_type"].(map[string]any)["loadFinished"]; !ok {
		t.Fatalf("%v", m["by_type"])
	}
}

func TestHistoryExportStreamsMatchingLines(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	now := seed(st)
	resp := e.get("/api/history/export?since=" + itoa(now-4500) + "&until=" + itoa(now-2500))
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" || !strings.Contains(resp.Header.Get("Content-Disposition"), ".jsonl") {
		t.Fatalf("headers %v", resp.Header)
	}
	sc := bufio.NewScanner(resp.Body)
	var types []string
	for sc.Scan() {
		var ev history.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("%q: %v", sc.Text(), err)
		}
		types = append(types, ev.Type)
	}
	resp.Body.Close()
	if strings.Join(types, ",") != "loadFinished,nodeLost" {
		t.Fatalf("exported %v, want the two events in range, oldest first", types)
	}
}

func TestAboutShape(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	seed(st)
	e.advance(90 * time.Second)
	a := getJSONBody(t, e, "/api/about")
	if a["version"] != "test-1.2.3" || a["history"] != "on" || a["uptime_s"].(float64) != 90 || a["oldest_ts"].(float64) == 0 || a["started_at"] == nil {
		t.Fatalf("%v", a)
	}
}

// A real load through the scheduler reaches the history, and the endpoints
// the page uses answer for it.
func TestSchedulerEventsReachTheHistory(t *testing.T) {
	st := openStore(t)
	e := histEnv(t, st)
	if resp := e.post("/v1/chat/completions", chat("alpha")); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	eventually(t, "loadFinished in the history", func() bool {
		m := getJSONBody(t, e, "/api/history?type=loadFinished")
		return len(m["events"].([]any)) == 1
	})
	ev := getJSONBody(t, e, "/api/history?type=loadFinished")["events"].([]any)[0].(map[string]any)
	if ev["profile"] != "alpha" || ev["model_file"] != "a.gguf" || ev["data"].(map[string]any)["outcome"] != "ready" || ev["session_id"] == "" {
		t.Fatalf("%v", ev)
	}
	rel := getJSONBody(t, e, "/api/history?session_id="+ev["session_id"].(string)+"&order=asc")["events"].([]any)
	if len(rel) < 2 || rel[0].(map[string]any)["type"] != "loadStarted" {
		t.Fatalf("related events %v", rel)
	}
	// And it was written to the file.
	st.Flush()
	resp := e.get("/api/history/export")
	if b := body(t, resp); !strings.Contains(b, `"type":"loadFinished"`) {
		t.Fatalf("export %s", b)
	}
}

// ---- the page ----

func TestUIPage(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.get("/ui/")
	b := body(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The page is the shell of the web UI: its stylesheet and its entry module are
	// local files (ui_test.go checks every file, that nothing points at another
	// host and that there is no load or unload control).
	for _, want := range []string{`href="app.css"`, `<script type="module" src="app.js">`, `id="main"`} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if regexp.MustCompile(`(?i)(src|href)=["']?(https?:)?//`).MatchString(b) {
		t.Fatal("the page points at another host")
	}
	if resp := e.get("/ui"); resp.StatusCode != 200 { // redirected to /ui/
		t.Fatalf("/ui: %d", resp.StatusCode)
	}
}

// The JSON the page reads has the keys it uses (see ui/index.html).
func TestUIReadsTheseKeys(t *testing.T) {
	e := newEnv(t, chatOK, func(o *Options) { o.History = openStore(t) })
	st := getJSONBody(t, e, "/api/status")
	for _, k := range []string{"instances", "queue", "view", "shutting_down"} {
		if _, ok := st[k]; !ok {
			t.Errorf("/api/status lacks %q", k)
		}
	}
	nodes := st["view"].(map[string]any)["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("no nodes")
	}
	for _, k := range []string{"id", "hostname", "local", "online", "priority", "excluded", "total_mib", "free_mib", "live_budget_mib"} {
		if _, ok := nodes[0].(map[string]any)[k]; !ok {
			t.Errorf("status node lacks %q", k)
		}
	}
	v := st["view"].(map[string]any)
	for _, k := range []string{"stale", "connected"} {
		if _, ok := v[k]; !ok {
			t.Errorf("status view lacks %q", k)
		}
	}
	for _, p := range []string{"/api/about", "/api/history", "/api/history/summary"} {
		getJSONBody(t, e, p)
	}
	for _, k := range []string{"version", "uptime_s", "history", "oldest_ts"} {
		if _, ok := getJSONBody(t, e, "/api/about")[k]; !ok {
			t.Errorf("/api/about lacks %q", k)
		}
	}
	for _, k := range []string{"events", "total_matching", "oldest_ts", "history", "truncated"} {
		if _, ok := getJSONBody(t, e, "/api/history")[k]; !ok {
			t.Errorf("/api/history lacks %q", k)
		}
	}
	for _, k := range []string{"events", "by_type", "by_severity", "loads", "profiles", "nodes", "incidents"} {
		if _, ok := getJSONBody(t, e, "/api/history/summary")[k]; !ok {
			t.Errorf("/api/history/summary lacks %q", k)
		}
	}
	_ = http.StatusOK
}
