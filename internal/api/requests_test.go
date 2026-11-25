package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// diskGate is a usage store's BeforeBatch: while closed it parks the writer,
// like a disk that does not answer.
type diskGate struct {
	mu      sync.Mutex
	blocked chan struct{}
	entered chan struct{}
}

func (g *diskGate) block() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocked, g.entered = make(chan struct{}), make(chan struct{})
}

func (g *diskGate) unblock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked != nil {
		close(g.blocked)
		g.blocked = nil
	}
}

func (g *diskGate) pass() {
	g.mu.Lock()
	b, ent := g.blocked, g.entered
	g.mu.Unlock()
	if b == nil {
		return
	}
	select {
	case <-ent:
	default:
		close(ent)
	}
	<-b
}

// usageEnv is an env with a request log in a temporary usage.db.
func usageEnv(t *testing.T, upstream http.Handler, gate *diskGate) (*env, *usage.Store) {
	t.Helper()
	o := usage.Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if gate != nil {
		o.BeforeBatch = gate.pass
	}
	st := usage.Open(o)
	t.Cleanup(func() {
		if gate != nil {
			gate.unblock()
		}
		st.Close()
	})
	e := newEnv(t, upstream, func(o *Options) { o.Requests = usage.NewRecorder(st) })
	return e, st
}

func flushT(t *testing.T, st *usage.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

type reqPage struct {
	Requests        []usage.Request `json:"requests"`
	Next            int64           `json:"next"`
	On              bool            `json:"on"`
	HistoryBeginsAt int64           `json:"history_begins_at"`
}

func listRequests(t *testing.T, e *env, query string) reqPage {
	t.Helper()
	var p reqPage
	if err := json.Unmarshal([]byte(body(t, e.get("/api/requests"+query))), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every /v1 and /upstream request is a row: done with its tokens, refused
// with the scheduler's reason, under the profile even when sent with an
// alias; the in-memory activity record is unchanged beside it.
func TestRequests_RecordedListedAndSummarized(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	e.post("/v1/chat/completions", chat("alpha-old")).Body.Close()
	e.post("/v1/chat/completions", chat("nope")).Body.Close()
	resp, err := http.Post(e.front.URL+"/upstream/alpha/v1/chat/completions", "application/json", strings.NewReader(chat("alpha")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	eventually(t, "four ended", func() bool { return e.api.reqs.Ended() == 4 })
	flushT(t, st)

	p := listRequests(t, e, "")
	if len(p.Requests) != 4 || !p.On || p.HistoryBeginsAt == 0 {
		t.Fatalf("%+v", p)
	}
	byKind := map[string]int{}
	for _, r := range p.Requests {
		byKind[r.Kind+"/"+r.Status]++
		if r.Caller != "anonymous" || r.CallerKind != "default" || r.SourceAddr == "" {
			t.Fatalf("caller %+v", r)
		}
	}
	if byKind["v1/done"] != 2 || byKind["v1/refused"] != 1 || byKind["upstream/done"] != 1 {
		t.Fatalf("%v", byKind)
	}
	done := listRequests(t, e, "?status=done&profile=alpha").Requests
	if len(done) != 3 { // two /v1 and the /upstream one
		t.Fatalf("%d done rows", len(done))
	}
	var v1, up usage.Request
	for _, r := range done {
		if r.Kind == usage.KindV1 && (r.TokensIn == nil || *r.TokensIn != 5 || *r.TokensOut != 7 || *r.DecodeTPS != 12.5 ||
			r.Instance == 0 || r.SchedID == 0 || r.StartedAt == 0 || r.Profile != "alpha") {
			t.Fatalf("done row %+v", r)
		}
		if r.Kind == usage.KindV1 {
			v1 = r
		} else {
			up = r
		}
	}
	// The passthrough holds no lease, yet says the instance and machines
	// that served it, as the /v1 rows do.
	if up.Instance != v1.Instance || up.Session != v1.Session || up.SchedID != 0 || len(up.Nodes) != len(v1.Nodes) || len(up.Nodes) == 0 ||
		up.Nodes[0] != v1.Nodes[0] || up.HostNode != v1.HostNode {
		t.Fatalf("upstream %+v, v1 %+v", up, v1)
	}
	ref := listRequests(t, e, "?status=refused").Requests[0]
	if ref.Reason != "unknown model" || ref.HTTPStatus != 404 || ref.Requested != "nope" {
		t.Fatalf("refused %+v", ref)
	}
	var one usage.Request
	json.Unmarshal([]byte(body(t, e.get(fmt.Sprintf("/api/requests/%d", ref.ID)))), &one)
	if one.ID != ref.ID || one.Status != usage.StatusRefused {
		t.Fatalf("by id %+v", one)
	}
	if r := e.get("/api/requests/42"); r.StatusCode != 404 {
		t.Fatalf("missing id: %d", r.StatusCode)
	}
	if r := e.get("/api/requests?status=bogus"); r.StatusCode != 400 {
		t.Fatalf("bad status: %d", r.StatusCode)
	}

	var sum usage.Summary
	now := time.Now().UnixMilli()
	json.Unmarshal([]byte(body(t, e.get(fmt.Sprintf("/api/requests/summary?from=%d&to=%d&step=hour", now-2*3600000, now+3600000)))), &sum)
	if sum.Totals.Requests != 4 || sum.Totals.Done != 3 || sum.Totals.Refused != 1 || sum.Totals.TokensIn != 15 || sum.Totals.TokensOut != 21 ||
		len(sum.Series) != 3 || sum.OldestTS == 0 || !sum.On {
		t.Fatalf("summary %+v", sum)
	}
	if r := e.get("/api/requests/summary?step=week"); r.StatusCode != 400 {
		t.Fatalf("bad step: %d", r.StatusCode)
	}

	// The old in-memory record still sees the two /v1 requests that ran.
	data := jsonOf(t, body(t, e.get("/api/metrics/activity")))["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("activity %v", data)
	}
}

// A client that leaves while queued (B7) is a row: cancelled,
// client_left_queued, never started.
func TestRequests_ClientLeftWhileQueuedIsRecorded(t *testing.T) {
	o := usage.Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st := usage.Open(o)
	t.Cleanup(func() { st.Close() })
	q := newQueuedEnv(t, func(o *Options) { o.Requests = usage.NewRecorder(st) })
	q.busyAlpha()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "POST", q.front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"beta:64k"}`))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	eventually(t, "beta queued", func() bool { return q.waiters() == 1 })
	live := listRequests(t, q.env, "?status=queued").Requests
	if len(live) != 1 || live[0].Profile != "beta:64k" || live[0].StartedAt != 0 {
		t.Fatalf("live %+v", live)
	}
	running := listRequests(t, q.env, "?status=running").Requests
	if len(running) != 1 || running[0].Profile != "alpha" || running[0].StartedAt == 0 {
		t.Fatalf("running %+v", running)
	}
	cancel()
	eventually(t, "the queued row ended", func() bool {
		r, err := q.api.reqs.Get(live[0].ID)
		return err == nil && r.Status == usage.StatusCancelled
	})
	flushT(t, st)
	r, _ := q.api.reqs.Get(live[0].ID)
	if r.Reason != usage.ReasonClientLeftQueued || r.StartedAt != 0 || r.WaitMS <= 0 && r.EndedAt < r.QueuedAt {
		t.Fatalf("%+v", r)
	}
}

// A blocked disk never delays a request or the scheduler's core loop: with
// the usage writer parked, N requests complete, the loop's longest event
// (stepMax, as the scheduler's own stall tests read it) stays under its
// bound, and not one transaction reaches the disk until the writer is let
// go: the request path itself does no disk work.
func TestRequests_BlockedDiskNeverDelaysRequestsOrTheCoreLoop(t *testing.T) {
	gate := &diskGate{}
	e, st := usageEnv(t, chatOK, gate)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close() // load alpha first
	eventually(t, "first ended", func() bool { return e.api.reqs.Ended() == 1 })
	flushT(t, st)
	gate.block()
	e.post("/v1/chat/completions", chat("alpha")).Body.Close() // parks the writer on its batch
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the disk")
	}
	before := st.Stats()
	base, _ := e.sched.LongestStep() // the loading so far; this test's events must stay under the bound
	const n = 60
	t0 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.post("/v1/chat/completions", chat("alpha")).Body.Close()
		}()
	}
	wg.Wait()
	took := time.Since(t0)
	longest, ev := e.sched.LongestStep()
	bound := 50 * time.Millisecond
	eventually(t, "all ended", func() bool { return e.api.reqs.Ended() == n+2 })
	after := st.Stats()
	t.Logf("%d requests with the disk blocked: %v; longest core event %v; queued ops %d", n, took, longest, after.Queued)
	if longest > max(base, bound) {
		t.Fatalf("the core loop stalled %v on one event (%s), bound %v", longest, ev, bound)
	}
	if took > 10*time.Second {
		t.Fatalf("requests took %v with the disk blocked", took)
	}
	if after.Batches != before.Batches || after.Written != before.Written {
		t.Fatalf("disk work while blocked: %+v -> %+v", before, after)
	}
	// Each request queued its rows for the writer: arrival, start, end.
	if after.Queued < 3*n || after.Dropped != 0 {
		t.Fatalf("queue %+v", after)
	}
	gate.unblock()
	flushT(t, st)
	if p := listRequests(t, e, "?limit=500"); len(p.Requests) != n+2 {
		t.Fatalf("%d rows after the disk came back, want %d", len(p.Requests), n+2)
	}
}

// With no usage store (or an unusable one) requests run as before and the
// routes answer from memory: the live rows and the last finished ones.
func TestRequests_NoStoreStillServes(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	p := listRequests(t, e, "")
	if p.On || len(p.Requests) != 1 || p.Requests[0].Status != usage.StatusDone {
		t.Fatalf("%+v", p)
	}
	// An off store has no history start: null, not the zero time.
	for _, path := range []string{"/api/requests", "/api/requests/summary"} {
		if raw := body(t, e.get(path)); !strings.Contains(raw, `"history_begins_at":null`) {
			t.Fatalf("%s: %s", path, raw)
		}
	}
	if r := e.get("/api/requests/summary"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
}

// A full disk under the request log: every request is served as before,
// /health stays 200, the failures are counted in storage.usage with the
// error and one WARN, and the store stays on.
func TestRequests_FullDiskNeverStopsTheScheduler(t *testing.T) {
	dir := t.TempDir()
	probe := usage.Open(usage.Options{Dir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	var pages int
	probe.DB().QueryRow(`PRAGMA page_count`).Scan(&pages)
	probe.Close()
	logs := &syncBuf{}
	st := usage.Open(usage.Options{Dir: dir, Logger: slog.New(slog.NewTextHandler(logs, nil)), MaxPages: pages + 2})
	t.Cleanup(func() { st.Close() })
	e := newEnv(t, chatOK, func(o *Options) { o.Requests, o.Usage = usage.NewRecorder(st), st })
	for i := 0; i < 200; i++ {
		resp := e.post("/v1/chat/completions", chat("alpha"))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	flushT(t, st)
	if r := e.get("/health"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	var sys struct {
		Storage struct {
			Usage usage.Stats `json:"usage"`
		} `json:"storage"`
	}
	json.Unmarshal([]byte(body(t, e.get("/api/system"))), &sys)
	u := sys.Storage.Usage
	if !u.On || u.Failed == 0 || !strings.Contains(u.LastError, "full") || u.SizeBytes == 0 {
		t.Fatalf("storage.usage %+v", u)
	}
	if n := strings.Count(logs.String(), "a write failed"); n != 1 {
		t.Fatalf("%d WARN lines", n)
	}
	if p := listRequests(t, e, "?limit=500"); len(p.Requests) == 0 {
		t.Fatal("the routes answer nothing")
	}
}

// requests_live_timings: a streamed chat or completion request that does
// not set timings_per_token gets it (with the right Content-Length); off,
// non-streamed, already set, a benchmark's request, or another path: the
// body arrives byte for byte as the client sent it.
func TestRequests_LiveTimingsRewritesOnlyWhenOnAndUnset(t *testing.T) {
	var mu sync.Mutex
	var got []string
	var lengths []int64
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if notUnderTest(w, r) { // the scheduler's own GET /props after the load races the first request
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, lengths = append(got, string(b)), append(lengths, r.ContentLength)
		mu.Unlock()
		chatOK(w, r)
	})
	e := newEnv(t, up)
	send := func(path, body string, bench bool) string {
		t.Helper()
		req, _ := http.NewRequest("POST", e.front.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bench {
			req.Header.Set("X-Bench-Run", "run-1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", body, resp.StatusCode)
		}
		mu.Lock()
		defer mu.Unlock()
		last := got[len(got)-1]
		if lengths[len(lengths)-1] != int64(len(last)) {
			t.Fatalf("Content-Length %d for %d bytes", lengths[len(lengths)-1], len(last))
		}
		return last
	}
	stream := `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	if b := send("/v1/chat/completions", stream, false); b != stream {
		t.Fatalf("rewritten while off: %s", b)
	}
	e.cfg.RequestsLiveTimings = true
	if b := send("/v1/chat/completions", stream, false); b != `{"timings_per_token":true,`+stream[1:] {
		t.Fatalf("chat stream: %s", b)
	}
	completion := `{"model":"alpha","stream":true,"prompt":"hi"}`
	if b := send("/v1/completions", completion, false); b != `{"timings_per_token":true,`+completion[1:] {
		t.Fatalf("completion stream: %s", b)
	}
	for _, c := range []struct {
		path, body string
		bench      bool
	}{
		{"/v1/chat/completions", `{"model":"alpha","messages":[{"role":"user","content":"hi"}]}`, false},
		{"/v1/chat/completions", `{"model":"alpha","stream":true,"timings_per_token":false,"messages":[]}`, false},
		{"/v1/chat/completions", stream, true},
		{"/v1/embeddings", `{"model":"alpha","stream":true,"input":"hi"}`, false},
	} {
		if b := send(c.path, c.body, c.bench); b != c.body {
			t.Fatalf("%s %s (bench %v) rewritten: %s", c.path, c.body, c.bench, b)
		}
	}
	// A body that is not JSON never reaches the model server (400, as before).
	if r := e.post("/v1/chat/completions", `not json`); r.StatusCode != 400 {
		t.Fatalf("not JSON: %d", r.StatusCode)
	}
}

// Only inference is a row: a table of methods and paths through /v1 and
// /upstream, each answered as before, recorded or not.
func TestRequests_OnlyInferenceIsRecorded(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close() // loads alpha, so /upstream is ready
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	for _, c := range []struct {
		method, path string
		recorded     bool
	}{
		{"POST", "/v1/chat/completions", true},
		{"POST", "/v1/completions", true},
		{"POST", "/v1/embeddings", true},
		{"POST", "/v1/rerank", true},
		{"GET", "/v1/models", false},
		{"POST", "/v1/tokenize", false},
		{"POST", "/v1/chat/completions/input_tokens", false},
		{"POST", "/upstream/alpha/completion", true},
		{"POST", "/upstream/alpha/v1/chat/completions", true},
		{"POST", "/upstream/alpha/embedding", true},
		{"POST", "/upstream/alpha/infill", true},
		{"POST", "/upstream/alpha/v1/messages", true},
		{"GET", "/upstream/alpha/slots", false},
		{"GET", "/upstream/alpha/props", false},
		{"POST", "/upstream/alpha/props", false},
		{"GET", "/upstream/alpha/health", false},
		{"GET", "/upstream/alpha/metrics", false},
		{"GET", "/upstream/alpha/v1/models", false},
		{"POST", "/upstream/alpha/tokenize", false},
		{"POST", "/upstream/alpha/detokenize", false},
		{"POST", "/upstream/alpha/apply-template", false},
		{"GET", "/upstream/alpha/lora-adapters", false},
		{"GET", "/upstream/alpha", false},
		{"GET", "/upstream/alpha/v1/chat/completions", false},
	} {
		before := e.api.reqs.Ended()
		req, _ := http.NewRequest(c.method, e.front.URL+c.path, strings.NewReader(chat("alpha")))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d", c.method, c.path, resp.StatusCode)
		}
		if c.recorded {
			eventually(t, c.method+" "+c.path+" recorded", func() bool { return e.api.reqs.Ended() == before+1 })
		} else if e.api.reqs.Ended() != before {
			t.Fatalf("%s %s recorded", c.method, c.path)
		}
	}
	flushT(t, st)
	for _, r := range listRequests(t, e, "?limit=500").Requests {
		if strings.Contains(r.Path, "slots") || strings.Contains(r.Path, "props") || strings.Contains(r.Path, "tokenize") || strings.Contains(r.Path, "models") {
			t.Fatalf("a monitoring call is a row: %+v", r)
		}
	}
}

// 1,000 polls of /upstream/<model>/slots cost the request log nothing: no
// live tracker at any time, no write, no row.
func TestRequests_MonitoringPollsCostNoRecorderWork(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	flushT(t, st)
	s0 := st.Stats()
	for i := 0; i < 1000; i++ {
		resp := e.get("/upstream/alpha/slots")
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("poll %d: %d", i, resp.StatusCode)
		}
		if i%100 == 0 && len(e.api.reqs.Live()) != 0 {
			t.Fatal("a poll made a live tracker")
		}
	}
	flushT(t, st)
	s1 := st.Stats()
	if s1.Written != s0.Written || s1.Batches != s0.Batches || s1.Dropped != 0 || s1.Failed != 0 || e.api.reqs.Ended() != 1 {
		t.Fatalf("before %+v after %+v, ended %d", s0, s1, e.api.reqs.Ended())
	}
	if n := len(listRequests(t, e, "?limit=500").Requests); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// The stored row keeps the machines as the lease gives them, host first,
// each with its share, in the three shapes: hosted on the scheduler's
// machine with helpers (that machine first, no share); a console alone
// (share 1); a console host with helpers (shares add up to 1).
func TestRequests_StoredRowNodesHostFirstWithShares(t *testing.T) {
	e, st := usageEnv(t, chatOK, nil)
	shapes := []struct {
		name  string
		nodes []scheduler.LeaseNode
		json  string
	}{
		{"server host and helpers", []scheduler.LeaseNode{{ID: "host-1", Name: "scheduler-host", Role: "host"},
			{ID: "a", Name: "zone-a", Role: "helper", Share: .6}, {ID: "b", Name: "zone-b", Role: "helper", Share: .4}},
			`[{"id":"host-1","name":"scheduler-host","role":"host"},{"id":"a","name":"zone-a","role":"helper","share":0.6},{"id":"b","name":"zone-b","role":"helper","share":0.4}]`},
		{"console alone", []scheduler.LeaseNode{{ID: "a", Name: "zone-a", Role: "host", Share: 1}},
			`[{"id":"a","name":"zone-a","role":"host","share":1}]`},
		{"console host and helpers", []scheduler.LeaseNode{{ID: "a", Name: "zone-a", Role: "host", Share: .5},
			{ID: "b", Name: "zone-b", Role: "helper", Share: .3}, {ID: "c", Name: "zone-c", Role: "helper", Share: .2}},
			`[{"id":"a","name":"zone-a","role":"host","share":0.5},{"id":"b","name":"zone-b","role":"helper","share":0.3},{"id":"c","name":"zone-c","role":"helper","share":0.2}]`},
	}
	for _, s := range shapes {
		tr := e.api.reqs.Queued(usage.Begin{Kind: usage.KindV1, Path: "/v1/chat/completions", Profile: s.name, Requested: s.name})
		tr.Started(leasePlacement(&scheduler.Lease{Nodes: s.nodes}), nil)
		tr.Ended(usage.End{Status: usage.StatusDone, TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1})
	}
	flushT(t, st)
	var page struct {
		Requests []struct {
			Profile  string          `json:"profile"`
			HostNode string          `json:"host_node"`
			Nodes    json.RawMessage `json:"nodes"`
		} `json:"requests"`
	}
	resp := e.get("/api/requests?limit=10")
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := map[string]string{}
	host := map[string]string{}
	for _, r := range page.Requests {
		got[r.Profile], host[r.Profile] = string(r.Nodes), r.HostNode
	}
	for _, s := range shapes {
		if got[s.name] != s.json {
			t.Errorf("%s: nodes %s, want %s", s.name, got[s.name], s.json)
		}
		if host[s.name] != s.nodes[0].ID {
			t.Errorf("%s: host_node %q", s.name, host[s.name])
		}
	}
	// And straight from the file, not the in-memory ring.
	var stored string
	st.DB().QueryRow(`SELECT nodes FROM client_requests WHERE profile = ?`, "console host and helpers").Scan(&stored)
	if !strings.Contains(stored, `"share":0.5`) || strings.Index(stored, `"a"`) > strings.Index(stored, `"b"`) {
		t.Fatalf("stored %s", stored)
	}
}
