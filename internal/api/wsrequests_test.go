package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

type reqFrame struct {
	Kind            string          `json:"kind"`
	Request         usage.Request   `json:"request"`
	Requests        []usage.Request `json:"requests"`
	Next            int64           `json:"next"`
	On              bool            `json:"on"`
	HistoryBeginsAt *int64          `json:"history_begins_at"`
}

// nextReq reads until a requests frame of the kind arrives.
func (w *wsc) nextReq(t *testing.T, kind string, within time.Duration) reqFrame {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			t.Fatalf("no requests %s frame within %v", kind, within)
		}
		m := w.until(t, TopicRequests, left)
		var f reqFrame
		if err := json.Unmarshal(m.Data, &f); err != nil {
			t.Fatal(err)
		}
		if f.Kind == kind {
			return f
		}
	}
}

// streamGate is a model server that streams: two prompt_progress events
// (the client asked return_progress), then waits for release, then three
// tokens and the final event with timings.
type streamGate struct {
	once    sync.Once
	release chan struct{}
	started chan struct{}
}

func newStreamGate() *streamGate {
	return &streamGate{release: make(chan struct{}), started: make(chan struct{}, 8)}
}

func (g *streamGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *streamGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/props" {
		chatOK(w, r)
		return
	}
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"stream":true`) {
		chatOK(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	prog := `data: {"choices":[{"delta":{"content":null,"role":"assistant"},"finish_reason":null,"index":0}],"object":"chat.completion.chunk","prompt_progress":{"cache":%d,"processed":%d,"time_ms":%d,"total":%d}}` + "\n\n"
	fmt.Fprintf(w, prog, 200, 200, 0, 1000)
	fmt.Fprintf(w, prog, 200, 600, 100, 1000)
	w.(http.Flusher).Flush()
	g.started <- struct{}{}
	<-g.release
	for i := 0; i < 3; i++ {
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":"t%d"},"finish_reason":null,"index":0}],"object":"chat.completion.chunk"}`+"\n\n", i)
	}
	fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"object":"chat.completion.chunk","timings":{"cache_n":200,"prompt_n":800,"prompt_ms":200,"prompt_per_second":4000,"predicted_n":3,"predicted_ms":30,"predicted_per_second":100}}`+"\n\ndata: [DONE]\n\n")
}

func streamChat(model string) string {
	return `{"model":"` + model + `","stream":true,"return_progress":true,"messages":[{"role":"user","content":"hi"}]}`
}

// A subscriber gets the snapshot, then queued, started, a tick with the
// live figures while the request runs, and the final row when it ends.
func TestWSRequests_SnapshotChangesAndTicks(t *testing.T) {
	g := newStreamGate()
	t.Cleanup(g.open)
	e := newEnv(t, g, func(o *Options) { o.WSRequestsTick = 30 * time.Millisecond })
	e.post("/v1/chat/completions", chat("alpha")).Body.Close() // one finished row before anyone listens

	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"requests"}})
	snap := c.nextReq(t, "snapshot", 3*time.Second)
	if len(snap.Requests) != 1 || snap.Requests[0].Status != usage.StatusDone || snap.On || snap.HistoryBeginsAt != nil {
		t.Fatalf("snapshot %+v", snap)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamChat("alpha")))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	q := c.nextReq(t, usage.ChangeQueued, 3*time.Second)
	s := c.nextReq(t, usage.ChangeStarted, 3*time.Second)
	if q.Request.ID == 0 || s.Request.ID != q.Request.ID || s.Request.Status != usage.StatusRunning || len(s.Request.Nodes) == 0 {
		t.Fatalf("queued %+v started %+v", q.Request, s.Request)
	}
	<-g.started
	var row usage.Request
	eventually(t, "a tick with the prompt progress", func() bool {
		f := c.nextReq(t, "tick", 3*time.Second)
		if len(f.Requests) != 1 {
			return false
		}
		row = f.Requests[0]
		return row.PrefillProcessed != nil
	})
	if row.ID != q.Request.ID || *row.TokensIn != 1000 || *row.TokensReused != 200 || *row.PrefillProcessed != 600 ||
		*row.PrefillTPS != 4000 || !row.Live || row.FirstTokenAt != 0 || row.TokensOut != nil {
		t.Fatalf("tick row %+v", row)
	}
	g.open()
	end := c.nextReq(t, usage.ChangeEnded, 3*time.Second)
	if end.Request.ID != q.Request.ID || end.Request.Status != usage.StatusDone || *end.Request.TokensIn != 1000 ||
		*end.Request.TokensReused != 200 || *end.Request.TokensOut != 3 || end.Request.FirstTokenAt == 0 || end.Request.Live {
		t.Fatalf("ended %+v", end.Request)
	}
	<-done
	// Nothing runs: no more ticks.
	for _, m := range c.collect(150 * time.Millisecond) {
		var f reqFrame
		json.Unmarshal(m.Data, &f)
		if m.Type == TopicRequests && f.Kind == "tick" {
			t.Fatalf("a tick with nothing running: %s", m.Data)
		}
	}
}

// With no subscriber the topic does no work: the recorder builds no row
// for it and the tick loop never looks, even while a request runs.
// Unsubscribing and leaving both stop it again.
func TestWSRequests_NoSubscriberNoWork(t *testing.T) {
	g := newStreamGate()
	t.Cleanup(g.open)
	e := newEnv(t, g, func(o *Options) { o.WSRequestsTick = 20 * time.Millisecond })
	h := e.api.ws
	c := dialWS(t, e, nil) // default topics: not requests
	c.until(t, TopicHello, 3*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamChat("alpha"))); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-g.started
	time.Sleep(150 * time.Millisecond)
	if n := h.reqs.looks.Load(); n != 0 || h.Watching() {
		t.Fatalf("%d looks with no subscriber", n)
	}
	for _, m := range c.collect(50 * time.Millisecond) {
		if m.Type == TopicRequests {
			t.Fatalf("an unsubscribed client got %s", m.Data)
		}
	}

	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"requests"}})
	c.nextReq(t, "snapshot", 3*time.Second)
	c.nextReq(t, "tick", 3*time.Second)
	if !h.Watching() {
		t.Fatal("not watching with a subscriber")
	}

	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"status"}})
	eventually(t, "unsubscribed", func() bool { return !h.Watching() })
	time.Sleep(60 * time.Millisecond)
	n := h.reqs.looks.Load()
	time.Sleep(150 * time.Millisecond)
	if h.reqs.looks.Load() != n {
		t.Fatal("the tick loop looked after the last subscriber left the topic")
	}

	c2 := dialWS(t, e, nil)
	c2.until(t, TopicHello, 3*time.Second)
	c2.send(t, map[string]any{"type": "subscribe", "topics": []string{"requests"}})
	c2.nextReq(t, "snapshot", 3*time.Second)
	c2.conn.CloseNow()
	eventually(t, "a closed subscriber no longer counts", func() bool { return h.reqs.subs.Load() == 0 })
	g.open()
	<-done
}

// The snapshot comes first: changes made while it is built wait and follow
// it, in order.
func TestWSRequests_ChangesDuringTheSnapshotFollowIt(t *testing.T) {
	e := newEnv(t, chatOK)
	h := e.api.ws
	inner := h.reqs.snapshot
	building, finish := make(chan struct{}), make(chan struct{})
	h.reqs.snapshot = func() (map[string]any, error) {
		close(building)
		<-finish
		return inner()
	}
	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"requests"}})
	<-building
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	close(finish)
	var kinds []string
	for len(kinds) < 4 {
		m := c.until(t, TopicRequests, 3*time.Second)
		var f reqFrame
		json.Unmarshal(m.Data, &f)
		kinds = append(kinds, f.Kind)
	}
	if strings.Join(kinds, ",") != "snapshot,queued,started,ended" {
		t.Fatalf("%v", kinds)
	}
}
