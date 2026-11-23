package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func errBody(t *testing.T, s string) map[string]any {
	t.Helper()
	m := jsonOf(t, s)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an OpenAI error body: %s", s)
	}
	return e
}

func chat(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
}

// ---- /v1 routing ----

func TestChatRoutesByModelAndLoads(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.post("/v1/chat/completions", chat("alpha"))
	b := body(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `"chat.completion"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	if r := body(t, e.get("/running")); !strings.Contains(r, `"alpha"`) {
		t.Fatalf("alpha not running after a request: %s", r)
	}
}

func TestUnknownModelIs404OpenAIFormat(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.post("/v1/chat/completions", chat("nope"))
	b := body(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	er := errBody(t, b)
	if er["code"] != "model_not_found" || er["type"] != "invalid_request_error" || !strings.Contains(er["message"].(string), "nope") {
		t.Fatalf("error = %v", er)
	}
	if r := body(t, e.get("/running")); strings.Contains(r, "nope") {
		t.Fatal("unknown model started something")
	}
}

func TestMissingModelIs400AndBigJSONIs413(t *testing.T) {
	e := newEnv(t, chatOK, func(o *Options) { o.Limits.MaxJSON = 100 })
	if resp := e.post("/v1/chat/completions", `{"messages":[]}`); resp.StatusCode != 400 {
		t.Fatalf("missing model: %d", resp.StatusCode)
	}
	big := `{"model":"alpha","x":"` + strings.Repeat("y", 500) + `"}`
	resp := e.post("/v1/chat/completions", big)
	if resp.StatusCode != 413 {
		t.Fatalf("big body: %d", resp.StatusCode)
	}
	errBody(t, body(t, resp))
}

func TestMultipartRoutesByModelField(t *testing.T) {
	var gotCT, gotBody string
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"text":"ok"}`)
	}))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "alpha-old") // an alias
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write(bytes.Repeat([]byte{1}, 50000))
	mw.Close()
	resp, err := http.Post(e.front.URL+"/v1/audio/transcriptions", mw.FormDataContentType(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "ok") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	if gotBody != buf.String() || gotCT != mw.FormDataContentType() {
		t.Fatalf("upstream got a different request (%d vs %d bytes)", len(gotBody), buf.Len())
	}
}

func TestAliasResolvesAndIsLoggedWithCallerRateLimited(t *testing.T) {
	e := newEnv(t, chatOK)
	for i := 0; i < 3; i++ {
		resp := e.post("/v1/chat/completions", chat("alpha-old"))
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
		}
		resp.Body.Close()
	}
	logs := e.logs.String()
	if n := strings.Count(logs, "alpha-old"); n != 1 {
		t.Fatalf("alias logged %d times within a minute, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs, "127.0.0.1") || !strings.Contains(logs, "alpha") {
		t.Fatalf("log lacks caller or profile:\n%s", logs)
	}
	e.advance(61 * time.Second)
	e.post("/v1/chat/completions", chat("alpha-old")).Body.Close()
	if n := strings.Count(e.logs.String(), "alpha-old"); n != 2 {
		t.Fatalf("after a minute the alias should log again; count = %d", n)
	}
	// A profile name is never logged as an alias.
	before := e.logs.String()
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	if e.logs.String() != before {
		t.Fatal("a plain profile name produced an alias log")
	}
}

func TestSchedulerErrorsMapToStatusRetryAfterAndReason(t *testing.T) {
	e := newEnv(t, chatOK)
	// beta needs 7000 MiB; the node has 8000-256 free, so it fits. Make a
	// request that cannot: a forced shutdown is simpler to provoke.
	e.api.BeginShutdown()
	resp := e.post("/v1/chat/completions", chat("alpha"))
	b := body(t, resp)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	er := errBody(t, b)
	if er["reason"] != "shutting down" || er["type"] != "server_error" {
		t.Fatalf("error = %v", er)
	}
}

func TestWriteSchedErrorShapes(t *testing.T) {
	w := newRecorder()
	writeSchedError(w, &schedErr502)
	if w.Code != 502 {
		t.Fatalf("code %d", w.Code)
	}
	er := errBody(t, w.Body.String())
	tail, _ := er["tail"].([]any)
	if len(tail) != 2 || er["reason"] != "load failed" || !strings.Contains(er["message"].(string), "boom line 2") {
		t.Fatalf("error = %v", er)
	}
	w = newRecorder()
	writeSchedError(w, &scheduler.Error{Status: 503, Reason: scheduler.ReasonDoesNotFit, Message: "needs 99999 MiB"})
	if w.Code != 503 || w.Header().Get("Retry-After") != "" {
		t.Fatalf("does-not-fit: code %d retry-after %q, want none", w.Code, w.Header().Get("Retry-After"))
	}
	w = newRecorder()
	writeSchedError(w, &schedErr503)
	if w.Code != 503 || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("code %d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
	if er := errBody(t, w.Body.String()); er["reason"] != "cluster busy" {
		t.Fatalf("error = %v", er)
	}
}

// ---- model listing, health, running ----

func TestModelsListsProfilesNotAliases(t *testing.T) {
	e := newEnv(t, chatOK)
	m := jsonOf(t, body(t, e.get("/v1/models")))
	if m["object"] != "list" {
		t.Fatalf("object = %v", m["object"])
	}
	var ids []string
	for _, d := range m["data"].([]any) {
		d := d.(map[string]any)
		if d["object"] != "model" {
			t.Fatalf("entry = %v", d)
		}
		ids = append(ids, d["id"].(string))
	}
	if strings.Join(ids, ",") != "alpha,beta:64k,gamma" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestHealthIs200WithNothingLoaded(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.get("/health")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRunningEmptyIsEmptyArray(t *testing.T) {
	e := newEnv(t, chatOK)
	if b := runningOnly(t, e); b != `{"running":[]}` {
		t.Fatalf("running = %s", b)
	}
}

func TestRunningShapeAndHubRegexes(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("beta:64k")).Body.Close()
	resp := e.get("/running")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type %q", ct)
	}
	m := jsonOf(t, body(t, resp))
	list := m["running"].([]any)
	if len(list) != 1 {
		t.Fatalf("running = %v", list)
	}
	r := list[0].(map[string]any)
	if r["model"] != "beta:64k" || r["name"] != "beta:64k" || r["state"] != "ready" {
		t.Fatalf("entry = %v", r)
	}
	cmd := r["cmd"].(string)
	ctx := regexp.MustCompile(`(?:--ctx-size|-c)[= ]+(\d+)`).FindStringSubmatch(cmd)
	if ctx == nil || ctx[1] != "65536" {
		t.Fatalf("hub ctx regex on %q = %v", cmd, ctx)
	}
}

// ---- /upstream ----

func TestUpstreamPassesThroughForReadyModelByNameAndAlias(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	for _, name := range []string{"alpha", "alpha-old"} {
		resp := e.get("/upstream/" + name + "/props")
		if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, `"from":"upstream"`) {
			t.Fatalf("%s: status %d body %s", name, resp.StatusCode, b)
		}
	}
}

func TestUpstreamURLEncodedColonName(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("beta:64k")).Body.Close()
	// The hub quotes with safe='' so ':' arrives as %3A.
	resp := e.get("/upstream/beta%3A64k/props")
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "upstream") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	resp = e.get("/upstream/beta:64k/props")
	if resp.StatusCode != 200 {
		t.Fatalf("unencoded colon: status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUpstreamUnloadedIs503AtOnceAndStartsNothing(t *testing.T) {
	e := newEnv(t, chatOK)
	start := time.Now()
	resp := e.get("/upstream/alpha/props")
	b := body(t, resp)
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if time.Since(start) > time.Second {
		t.Fatal("upstream waited")
	}
	errBody(t, b)
	if r := runningOnly(t, e); r != `{"running":[]}` {
		t.Fatalf("/upstream started something: %s", r)
	}
}

func TestUpstreamStripsPrefixAndKeepsMethodAndQuery(t *testing.T) {
	var got string
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.RequestURI() + " " + string(b)
		io.WriteString(w, "ok")
	}))
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	resp, err := http.Post(e.front.URL+"/upstream/alpha/slots/0?action=erase", "text/plain", strings.NewReader("zz"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "POST /slots/0?action=erase zz" {
		t.Fatalf("upstream saw %q", got)
	}
}

// ---- unload, load, plan, status ----

// requestsEnded waits until no instance counts a request in flight: the
// scheduler counts a request's end a moment after the client has read the
// whole answer, so an unload sent at once may still find it busy (409).
func requestsEnded(t *testing.T, e *env) {
	t.Helper()
	eventually(t, "no request in flight", func() bool {
		for _, in := range jsonOf(t, body(t, e.get("/api/status")))["instances"].([]any) {
			if n, _ := in.(map[string]any)["in_flight"].(float64); n != 0 {
				return false
			}
		}
		return true
	})
}

func TestUnloadEndpoints(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	requestsEnded(t, e)
	resp := e.post("/api/models/unload/alpha", "")
	if resp.StatusCode != 200 {
		t.Fatalf("unload by name: %d %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	eventually(t, "alpha stopped", func() bool { return runningOnly(t, e) == `{"running":[]}` })

	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	e.post("/v1/chat/completions", chat("beta:64k")).Body.Close() // evicts or loads next to it
	requestsEnded(t, e)
	resp = e.get("/unload")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /unload: %d", resp.StatusCode)
	}
	resp.Body.Close()
	eventually(t, "everything idle stopped", func() bool { return runningOnly(t, e) == `{"running":[]}` })

	if resp := e.post("/api/models/unload/nope", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown model: %d", resp.StatusCode)
	}
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	requestsEnded(t, e)
	resp = e.post("/api/models/unload", "")
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/models/unload: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUnloadBusyIs409AndForceStopsIt(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	go func() {
		resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(chat("alpha")))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	resp := e.post("/api/unload", `{"model":"alpha"}`)
	if resp.StatusCode != 409 {
		t.Fatalf("busy unload: %d %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	resp = e.post("/api/unload", `{"model":"alpha","force":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("forced unload: %d %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	close(release)
	eventually(t, "alpha gone", func() bool { return runningOnly(t, e) == `{"running":[]}` })
}

func TestLoadIs202AndPlanChangesNothing(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.post("/api/plan", `{"model":"alpha"}`)
	b := body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("plan: %d %s", resp.StatusCode, b)
	}
	p := jsonOf(t, b)
	if p["action"] != "start" || p["model"] != "alpha" {
		t.Fatalf("plan = %s", b)
	}
	if pl, _ := p["placement"].([]any); len(pl) != 1 {
		t.Fatalf("placement = %v", p["placement"])
	}
	if r := runningOnly(t, e); r != `{"running":[]}` {
		t.Fatalf("plan changed state: %s", r)
	}
	if resp := e.post("/api/plan", `{"model":"nope"}`); resp.StatusCode != 404 {
		t.Fatalf("plan unknown: %d", resp.StatusCode)
	}
	if resp := e.post("/api/plan", `{}`); resp.StatusCode != 400 {
		t.Fatalf("plan without model: %d", resp.StatusCode)
	}

	resp = e.post("/api/load", `{"model":"alpha"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("load: %d %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	eventually(t, "alpha ready", func() bool { return strings.Contains(body(t, e.get("/running")), `"ready"`) })
	if resp := e.post("/api/load", `{"model":"nope"}`); resp.StatusCode != 404 {
		t.Fatalf("load unknown: %d", resp.StatusCode)
	}
}

func TestStatusDocumentShape(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	m := jsonOf(t, body(t, e.get("/api/status")))
	for _, k := range []string{"instances", "queue", "quarantines", "backoffs", "view", "shutting_down"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status lacks %q: %v", k, m)
		}
	}
	inst := m["instances"].([]any)
	if len(inst) != 1 {
		t.Fatalf("instances = %v", inst)
	}
	i0 := inst[0].(map[string]any)
	if i0["profile"] != "alpha" || i0["state"] != "ready" {
		t.Fatalf("instance = %v", i0)
	}
	if _, ok := i0["in_flight"]; !ok {
		t.Fatalf("instance lacks in_flight: %v", i0)
	}
	nodes := m["view"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["live_budget_mib"] == nil {
		t.Fatalf("nodes = %v", nodes)
	}
	if q := m["queue"]; q == nil {
		t.Fatal("queue must be [], not null")
	}
}

func TestUIPageLinksToStatus(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.get("/ui/")
	b := body(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `href="../api/status"`) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d ct %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
}

func TestUnknownPathIs404JSON(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.get("/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	errBody(t, body(t, resp))
}

// ---- /api/metrics/activity ----

func TestActivityRecordsFinishedRequestsWithTokens(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	e.post("/v1/embeddings", chat("alpha-old")).Body.Close()
	// A request's record is written after its answer has gone out, so the
	// second one can arrive a moment after the client has its response:
	// wait for both, finished, before reading them.
	var data []any
	eventually(t, "both records", func() bool {
		data = jsonOf(t, body(t, e.get("/api/metrics/activity")))["data"].([]any)
		if len(data) != 2 {
			return false
		}
		for _, d := range data {
			if tk, _ := d.(map[string]any)["tokens"].(map[string]any); tk == nil || tk["output_tokens"] != float64(7) {
				return false
			}
		}
		return true
	})
	if len(data) != 2 {
		t.Fatalf("data = %v", data)
	}
	r0, r1 := data[0].(map[string]any), data[1].(map[string]any)
	if r0["id"].(float64) >= r1["id"].(float64) {
		t.Fatalf("ids not increasing: %v %v", r0["id"], r1["id"])
	}
	if r0["model"] != "alpha" || r1["model"] != "alpha" { // profile name even for an alias
		t.Fatalf("models: %v %v", r0["model"], r1["model"])
	}
	if r0["req_path"] != "/v1/chat/completions" || r0["resp_status_code"].(float64) != 200 {
		t.Fatalf("record = %v", r0)
	}
	if _, err := time.Parse(time.RFC3339, r0["timestamp"].(string)); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
	if _, ok := r0["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms: %v", r0["duration_ms"])
	}
	tk := r0["tokens"].(map[string]any)
	if tk["input_tokens"].(float64) != 5 || tk["output_tokens"].(float64) != 7 || tk["tokens_per_second"].(float64) != 12.5 {
		t.Fatalf("tokens = %v", tk)
	}
}

func TestActivityKeepsLast200(t *testing.T) {
	e := newEnv(t, chatOK)
	for i := 0; i < 205; i++ {
		e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	}
	// A request's record is written after its answer has gone out, so the
	// last one can arrive a moment after the client has its response.
	var data []any
	eventually(t, "the last record", func() bool {
		data = jsonOf(t, body(t, e.get("/api/metrics/activity")))["data"].([]any)
		return len(data) == 200 && data[0].(map[string]any)["id"].(float64) == 6
	})
	if len(data) != 200 {
		t.Fatalf("len = %d", len(data))
	}
	if first := data[0].(map[string]any)["id"].(float64); first != 6 {
		t.Fatalf("oldest id = %v, want 6", first)
	}
}

func TestActivityEmptyIsEmptyArray(t *testing.T) {
	e := newEnv(t, chatOK)
	if b := strings.TrimSpace(body(t, e.get("/api/metrics/activity"))); b != `{"data":[]}` {
		t.Fatalf("body = %s", b)
	}
}

// ---- /api/events, parsed the way llama.ts does ----

type sseConn struct {
	t     *testing.T
	resp  *http.Response
	lines chan string
}

func openEvents(t *testing.T, e *env) *sseConn {
	t.Helper()
	resp, err := http.Get(e.front.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	c := &sseConn{t: t, resp: resp, lines: make(chan string, 1024)}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return c
}

// message returns the next SSE message the way EventSource.onmessage sees
// it: the data field. Comment lines are returned as ("", true). A message
// with an event: line is a test failure, since onmessage would not see it.
func (c *sseConn) next(timeout time.Duration) (data string, comment bool) {
	c.t.Helper()
	deadline := time.After(timeout)
	var cur []string
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				c.t.Fatal("event stream closed")
			}
			switch {
			case strings.HasPrefix(l, ":"):
				return "", true
			case strings.HasPrefix(l, "event:"):
				c.t.Fatalf("named event %q: EventSource.onmessage would never see it", l)
			case strings.HasPrefix(l, "data:"):
				cur = append(cur, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
			case l == "":
				if len(cur) > 0 {
					return strings.Join(cur, "\n"), false
				}
			}
		case <-deadline:
			c.t.Fatal("timed out waiting for an event")
		}
	}
}

// parsed applies llama.ts's parseJson twice: the envelope, then its data string.
func (c *sseConn) parsed(timeout time.Duration) (typ string, data any) {
	c.t.Helper()
	for {
		raw, comment := c.next(timeout)
		if comment {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			c.t.Fatalf("envelope not JSON: %v: %s", err, raw)
		}
		s, ok := env["data"].(string)
		if !ok {
			c.t.Fatalf("envelope data must be a JSON string: %s", raw)
		}
		var d any
		if err := json.Unmarshal([]byte(s), &d); err != nil {
			c.t.Fatalf("data string not JSON: %v: %s", err, s)
		}
		return env["type"].(string), d
	}
}

func TestEventsInitialModelStatusAndInflightSnapshot(t *testing.T) {
	e := newEnv(t, chatOK)
	c := openEvents(t, e)
	typ, d := c.parsed(3 * time.Second)
	if typ != "modelStatus" {
		t.Fatalf("first event %q", typ)
	}
	list := d.([]any)
	if len(list) != 3 {
		t.Fatalf("modelStatus lists %d profiles, want every profile: %v", len(list), list)
	}
	for _, m := range list {
		m := m.(map[string]any)
		if m["state"] != "stopped" || m["id"] == nil {
			t.Fatalf("entry = %v", m)
		}
		if _, has := m["readySince"]; has {
			t.Fatalf("stopped profile with readySince: %v", m)
		}
	}
	typ, d = c.parsed(3 * time.Second)
	if typ != "inflight" {
		t.Fatalf("second event %q", typ)
	}
	in := d.(map[string]any)
	if in["operation"] != "snapshot" {
		t.Fatalf("inflight = %v", in)
	}
	if reqs, ok := in["requests"].([]any); !ok || len(reqs) != 0 {
		t.Fatalf("requests = %v", in["requests"])
	}
}

func TestEventsStateChangesInflightPairAndActivity(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		io.WriteString(w, `{}`)
	}))
	c := openEvents(t, e)
	c.parsed(3 * time.Second) // modelStatus
	c.parsed(3 * time.Second) // inflight snapshot

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(chat("alpha")))
		if err == nil {
			resp.Body.Close()
		}
	}()
	var sawStarting, sawReady bool
	var upsertID string
	for upsertID == "" {
		typ, d := c.parsed(5 * time.Second)
		switch typ {
		case "modelStatus":
			for _, m := range d.([]any) {
				m := m.(map[string]any)
				if m["id"] == "alpha" && m["state"] == "starting" {
					sawStarting = true
				}
				if m["id"] == "alpha" && m["state"] == "ready" {
					sawReady = true
					rs, _ := m["readySince"].(string)
					if _, err := time.Parse(time.RFC3339, rs); err != nil {
						t.Fatalf("readySince %q: %v", rs, err)
					}
				}
			}
		case "inflight":
			in := d.(map[string]any)
			if in["operation"] != "upsert" {
				t.Fatalf("inflight = %v", in)
			}
			r := in["request"].(map[string]any)
			id, ok := r["id"].(string)
			if !ok || id == "" || r["model"] != "alpha" {
				t.Fatalf("request = %v", r)
			}
			if _, err := time.Parse(time.RFC3339, r["timestamp"].(string)); err != nil {
				t.Fatalf("timestamp: %v", err)
			}
			upsertID = id
		}
	}
	if !sawStarting || !sawReady {
		t.Fatalf("expected starting then ready before the upsert: starting=%v ready=%v", sawStarting, sawReady)
	}
	<-started
	close(release)
	<-done
	var removed, activity bool
	for !(removed && activity) {
		typ, d := c.parsed(5 * time.Second)
		switch typ {
		case "inflight":
			in := d.(map[string]any)
			if in["operation"] == "remove" {
				if in["id"] != upsertID {
					t.Fatalf("remove id %v != upsert id %q", in["id"], upsertID)
				}
				removed = true
			}
		case "activity":
			activity = true
		}
	}
}

func TestEventsPingAndCleanupOnDisconnect(t *testing.T) {
	e := newEnv(t, chatOK)
	c := openEvents(t, e)
	c.parsed(3 * time.Second)
	c.parsed(3 * time.Second)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-c.lines:
			if strings.HasPrefix(l, ": ping") {
				goto pinged
			}
		case <-deadline:
			t.Fatal("no ': ping' comment")
		}
	}
pinged:
	if n := e.api.hub.clientCount(); n != 1 {
		t.Fatalf("clients = %d", n)
	}
	c.resp.Body.Close()
	eventually(t, "client cleaned up", func() bool { return e.api.hub.clientCount() == 0 })
}

func TestEventsFlushedImmediately(t *testing.T) {
	e := newEnv(t, chatOK)
	c := openEvents(t, e)
	// If the first event were buffered it would not arrive within the timeout.
	if _, comment := c.next(2 * time.Second); comment {
		t.Fatal("expected data first")
	}
}

// ---- stall wiring and shutdown ----

func TestStallTimeoutFromProfileEndsStreamWithErrorEvent(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	resp := e.post("/v1/chat/completions", chat("gamma"))
	b := body(t, resp)
	if !strings.HasPrefix(b, "data: one\n\n") || !strings.Contains(b, "event: error") {
		t.Fatalf("body = %q", b)
	}
	// The instance is not touched.
	if r := body(t, e.get("/running")); !strings.Contains(r, `"gamma"`) || !strings.Contains(r, `"ready"`) {
		t.Fatalf("running = %s", r)
	}
}

func TestShutdownStopsAcceptingThenStopsChildren(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	c := openEvents(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.api.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	resp := e.post("/v1/chat/completions", chat("alpha"))
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("after shutdown: %d", resp.StatusCode)
	}
	resp.Body.Close()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-c.lines:
			if !ok {
				return // event stream was closed by the shutdown
			}
		case <-deadline:
			t.Fatal("event stream still open after shutdown")
		}
	}
}
