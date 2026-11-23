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
	resp, err := http.Get(e.front.URL + "/upstream/alpha/props")
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
	for _, r := range done {
		if r.Kind == usage.KindV1 && (r.TokensIn == nil || *r.TokensIn != 5 || *r.TokensOut != 7 || *r.DecodeTPS != 12.5 ||
			r.Instance == 0 || r.SchedID == 0 || r.StartedAt == 0 || r.Profile != "alpha") {
			t.Fatalf("done row %+v", r)
		}
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
	if sum.Totals.Requests != 4 || sum.Totals.Done != 3 || sum.Totals.Refused != 1 || sum.Totals.TokensIn != 10 || sum.Totals.TokensOut != 14 ||
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
// routes answer from memory.
func TestRequests_NoStoreStillServes(t *testing.T) {
	e := newEnv(t, chatOK)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	p := listRequests(t, e, "")
	if p.On || len(p.Requests) != 0 {
		t.Fatalf("%+v", p)
	}
	if r := e.get("/api/requests/summary"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
}
