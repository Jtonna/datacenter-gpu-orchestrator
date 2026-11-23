package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// endless is a model server whose streamed answer starts with the prompt
// (a prompt_progress event: 1000 tokens, 200 reused, read in 200 ms), then
// sends tokens until the client goes, or (dieAfter > 0) drops the
// connection after that many. A request that does not stream is answered
// only after hold.
type endless struct {
	dieAfter int
	hold     time.Duration
	sent     chan int // the tokens sent so far, after the first ten and when it dies
}

func (u *endless) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"stream":true`) {
		if strings.Contains(string(b), "hold") {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(u.hold):
			}
		}
		chatOK(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"choices":[{"delta":{"content":null,"role":"assistant"},"finish_reason":null,"index":0}],"object":"chat.completion.chunk","prompt_progress":{"cache":200,"processed":1000,"time_ms":200,"total":1000}}`+"\n\n")
	for i := 1; ; i++ {
		if _, err := fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":"t%d"},"finish_reason":null,"index":0}],"object":"chat.completion.chunk"}`+"\n\n", i); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		if i == 10 || i == u.dieAfter {
			u.sent <- i
		}
		if i == u.dieAfter {
			conn, _, _ := http.NewResponseController(w).Hijack()
			conn.Close()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Millisecond):
		}
	}
}

func summary(t *testing.T, e *env) usage.Summary {
	t.Helper()
	var sum usage.Summary
	now := time.Now().UnixMilli()
	json.Unmarshal([]byte(body(t, e.get(fmt.Sprintf("/api/requests/summary?from=%d&to=%d&step=hour", now-2*3600000, now+3600000)))), &sum)
	return sum
}

// storedRow reads a row from usage.db itself (a fresh recorder: not the
// in-memory ring of recent rows).
func storedRow(t *testing.T, st *usage.Store, id int64) usage.Request {
	t.Helper()
	flushT(t, st)
	r, err := usage.NewRecorder(st).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sameRow(t *testing.T, what string, framed, stored usage.Request) {
	t.Helper()
	a, _ := json.Marshal(framed)
	b, _ := json.Marshal(stored)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: the ended frame and the stored row differ:\n%s\n%s", what, a, b)
	}
}

// A client that closes its connection in the middle of a streamed answer
// is cancelled (client_left), never failed, on each entry point: the row
// keeps the tokens counted from the stream (tokens_out, decode_tps, the
// prompt the stream had said), the ended frame equals the stored row, and
// the totals count it as cancelled with its tokens.
func TestCancel_ClientLeavingMidStreamIsCancelledWithTheTokensSoFar(t *testing.T) {
	up := &endless{sent: make(chan int, 4)}
	e, st := usageEnv(t, up, nil)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close() // loads alpha, so /upstream is ready
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	c := dialWS(t, e, nil)
	c.until(t, TopicHello, 3*time.Second)
	c.send(t, map[string]any{"type": "subscribe", "topics": []string{"requests"}})
	c.nextReq(t, "snapshot", 3*time.Second)

	for _, x := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"alpha","stream":true,"return_progress":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/completions", `{"model":"alpha","stream":true,"return_progress":true,"prompt":"hi"}`},
		{"/upstream/alpha/v1/chat/completions", `{"stream":true,"return_progress":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", e.front.URL+x.path, strings.NewReader(x.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		<-up.sent
		rd := bufio.NewReader(resp.Body)
		for i := 0; i < 8; i++ { // read a few events, then hang up
			rd.ReadString('\n')
		}
		cancel()
		resp.Body.Close()
		end := c.nextReq(t, usage.ChangeEnded, 5*time.Second).Request
		if end.Status != usage.StatusCancelled || end.Reason != usage.ReasonClientLeft || !strings.Contains(end.Message, usage.MessageTokensCounted) {
			t.Fatalf("%s: %s %s %q", x.path, end.Status, end.Reason, end.Message)
		}
		if end.TokensOut == nil || *end.TokensOut < 10 || end.DecodeTPS == nil || *end.DecodeTPS <= 0 || end.DurationMS <= 0 ||
			end.TokensIn == nil || *end.TokensIn != 1000 || end.TokensReused == nil || *end.TokensReused != 200 ||
			end.PrefillTPS == nil || *end.PrefillTPS != 4000 || end.Live || end.PrefillProcessed != nil {
			b, _ := json.Marshal(end)
			t.Fatalf("%s: %s", x.path, b)
		}
		sameRow(t, x.path, end, storedRow(t, st, end.ID))
	}
	// The totals: three cancelled with their tokens, nothing failed.
	doneIn := 0
	for _, r := range listRequests(t, e, "?status=done").Requests {
		doneIn += *r.TokensIn
	}
	sum := summary(t, e)
	if sum.Totals.Cancelled != 3 || sum.Totals.Failed != 0 || sum.Totals.TokensIn != int64(3000+doneIn) || sum.Totals.TokensReused != 600 || sum.Totals.TokensOut < 30 {
		t.Fatalf("totals %+v", sum.Totals)
	}
}

// A non-streamed request whose client leaves is cancelled; its tokens are
// unknown (null): nothing was counted.
func TestCancel_ClientLeavingANonStreamedRequestIsCancelledWithoutTokens(t *testing.T) {
	up := &endless{hold: 5 * time.Second, sent: make(chan int, 4)}
	e, st := usageEnv(t, up, nil)
	e.post("/v1/chat/completions", chat("alpha")).Body.Close()
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	for i, path := range []string{"/v1/chat/completions", "/upstream/alpha/v1/chat/completions"} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, "POST", e.front.URL+path, strings.NewReader(`{"model":"alpha","messages":[{"role":"user","content":"hold"}]}`))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		cancel()
		eventually(t, "ended", func() bool { return e.api.reqs.Ended() == uint64(i+2) })
	}
	flushT(t, st)
	n := 0
	for _, r := range listRequests(t, e, "?status=cancelled").Requests {
		if r.Reason != usage.ReasonClientLeft || r.TokensOut != nil || r.TokensIn != nil || r.DecodeTPS != nil {
			b, _ := json.Marshal(r)
			t.Fatalf("%s", b)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d cancelled", n)
	}
}

// The model server dropping the connection mid-stream is still a failure
// (aborted), and the row keeps the tokens counted so far.
func TestCancel_ModelServerFailingMidStreamIsFailedWithTheTokensSoFar(t *testing.T) {
	up := &endless{dieAfter: 25, sent: make(chan int, 4)}
	e, st := usageEnv(t, up, nil)
	resp := e.post("/v1/chat/completions", `{"model":"alpha","stream":true,"return_progress":true,"messages":[{"role":"user","content":"hi"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	eventually(t, "ended", func() bool { return e.api.reqs.Ended() == 1 })
	rows := listRequests(t, e, "").Requests
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	r := storedRow(t, st, rows[0].ID)
	if r.Status != usage.StatusFailed || r.Reason != "aborted" || r.TokensOut == nil || *r.TokensOut != 25 ||
		r.TokensIn == nil || *r.TokensIn != 1000 || !strings.Contains(r.Message, usage.MessageTokensCounted) {
		b, _ := json.Marshal(r)
		t.Fatalf("%s", b)
	}
	sum := summary(t, e)
	if sum.Totals.Failed != 1 || sum.Totals.Cancelled != 0 || sum.Totals.TokensOut != 25 {
		t.Fatalf("totals %+v", sum.Totals)
	}
}
