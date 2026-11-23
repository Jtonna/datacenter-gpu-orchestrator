package api

import (
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
	"github.com/coder/websocket"
)

// The requests topic of /ws. Every frame is {"type":"requests","ts",
// "data"} with data one of:
//
//	{"kind":"snapshot","requests":[row...],"next":<id>,"on":bool,"history_begins_at":<ms>|null}
//	    when the client subscribes: the first page of GET /api/requests
//	    (queued, then running, then the newest 50 finished).
//	{"kind":"queued"|"started"|"ended","request":row}
//	    each change, as it happens; ended carries the final row.
//	{"kind":"tick","requests":[row...]}
//	    once a second while any request runs: the running rows with their
//	    live figures (tokens_out counted from the stream, decode_tps, and,
//	    when the model server says them, tokens_in, tokens_reused,
//	    prefill_tps and prefill_processed).
//
// A row is the shape of GET /api/requests/{id}; the client keys rows by
// id and lets the newest frame win. The snapshot comes before any change
// made after the subscription, and changes follow in order.
//
// It costs nothing while no client listens: the recorder asks Watching()
// and builds no row, and the tick loop waits on a channel.

const reqHoldMax = 512 // frames held for one client while its snapshot is built

type reqTopic struct {
	subs     atomic.Int64  // clients subscribed to the topic
	wake     chan struct{} // the tick loop's wake-up (cap 1)
	every    time.Duration
	running  func() []usage.Request         // the running rows now
	snapshot func() (map[string]any, error) // the first page
	looks    atomic.Int64                   // tick loop looks (tests)
}

// setTopics swaps a client's topics and keeps the requests count; prev is
// the old set, gen the subscription's generation.
func (rt *reqTopic) setTopics(c *wsClient, next map[string]bool) (prev map[string]bool, gen int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev = c.topics
	if c.gone {
		return prev, c.reqGen
	}
	c.topics = next
	switch {
	case next[TopicRequests] && !prev[TopicRequests]:
		c.reqPending, c.reqHeld = true, nil
		c.reqGen++
		if rt.subs.Add(1) == 1 {
			rt.poke()
		}
	case !next[TopicRequests] && prev[TopicRequests]:
		c.reqPending, c.reqHeld = false, nil
		c.reqGen++
		rt.subs.Add(-1)
	}
	return prev, c.reqGen
}

// left is a dropped client: its subscription no longer counts.
func (rt *reqTopic) left(c *wsClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.gone && c.topics[TopicRequests] {
		rt.subs.Add(-1)
	}
	c.gone, c.reqHeld = true, nil
}

func (rt *reqTopic) poke() {
	if rt.wake == nil {
		return
	}
	select {
	case rt.wake <- struct{}{}:
	default:
	}
}

// hold keeps b for a client whose snapshot is being built; overflow when
// too many wait (the client is then dropped as too slow).
func (c *wsClient) hold(b []byte) (held, overflow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.reqPending {
		return false, false
	}
	if len(c.reqHeld) >= reqHoldMax {
		return true, true
	}
	c.reqHeld = append(c.reqHeld, b)
	return true, false
}

// takeHeld takes the held frames of generation gen; when none are left it
// ends the hold (done). A stale generation is done with nothing.
func (c *wsClient) takeHeld(gen int) (frames [][]byte, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reqGen != gen || c.gone {
		return nil, true
	}
	if len(c.reqHeld) == 0 {
		c.reqPending = false
		return nil, true
	}
	frames, c.reqHeld = c.reqHeld, nil
	return frames, false
}

// current is whether gen is still the client's live subscription.
func (c *wsClient) current(gen int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqGen == gen && !c.gone && c.topics[TopicRequests]
}

// Watching is the recorder's question: is anyone subscribed?
func (h *wsHub) Watching() bool { return h.reqs.subs.Load() > 0 }

// Changed sends a row change to the requests topic.
func (h *wsHub) Changed(kind string, row usage.Request) {
	h.pushRequests(wsFrame(TopicRequests, h.now(), map[string]any{"kind": kind, "request": row}))
}

// pushRequests sends a requests frame to every subscriber, holding it for
// one whose snapshot is still being built.
func (h *wsHub) pushRequests(b []byte) {
	var cs []*wsClient
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		cs = make([]*wsClient, 0, len(h.clients))
		for c := range h.clients {
			if c.wants(TopicRequests) {
				cs = append(cs, c)
			}
		}
	}()
	for _, c := range cs {
		switch held, overflow := c.hold(b); {
		case overflow:
			h.log("websocket client dropped: it cannot keep up")
			h.drop(c, websocket.StatusPolicyViolation, "too slow: the send queue is full")
		case !held:
			h.enqueue(c, b)
		}
	}
}

// sendRequests builds the snapshot off the reader's goroutine (the first
// page reads usage.db), queues it, then the frames held meanwhile.
func (h *wsHub) sendRequests(c *wsClient, gen int) {
	if h.reqs.snapshot == nil {
		return
	}
	go supervise.Once("api.ws.requests.snapshot", func() {
		data, err := h.reqs.snapshot()
		if err != nil { // answered from memory alone
			h.log("requests snapshot read failed; sending the live rows only", "err", err)
		}
		if !c.current(gen) { // unsubscribed or dropped meanwhile
			return
		}
		h.enqueue(c, wsFrame(TopicRequests, h.now(), data))
		for done := false; !done; {
			var frames [][]byte
			frames, done = c.takeHeld(gen)
			for _, b := range frames {
				h.enqueue(c, b)
			}
		}
	}, func(any) { h.drop(c, websocket.StatusInternalError, "internal error") }) // the browser reconnects and asks again
}

// requestsLoop sends the running rows once a tick while anyone listens.
// With no subscriber it waits on the wake channel and does nothing.
func (h *wsHub) requestsLoop() {
	for {
		if h.reqs.subs.Load() <= 0 {
			select {
			case <-h.quit:
				return
			case <-h.reqs.wake:
			}
			continue
		}
		t := time.NewTimer(h.reqs.every)
		select {
		case <-h.quit:
			t.Stop()
			return
		case <-t.C:
		}
		if h.reqs.subs.Load() <= 0 {
			continue
		}
		h.reqs.looks.Add(1)
		supervise.Point("api.ws.requests", h)
		rows := h.reqs.running()
		if len(rows) == 0 {
			continue
		}
		h.pushRequests(wsFrame(TopicRequests, h.now(), map[string]any{"kind": "tick", "requests": rows}))
	}
}

// wireRequests gives the hub the request log and makes it the recorder's
// watcher (removed when the hub closes).
func (s *Server) wireRequests(h *wsHub) {
	h.reqs.wake = make(chan struct{}, 1)
	h.reqs.every = orDur(s.opts.WSRequestsTick, time.Second)
	h.reqs.running = func() []usage.Request {
		out := []usage.Request{}
		for _, r := range s.reqs.Live() {
			if r.Status == usage.StatusRunning {
				out = append(out, r)
			}
		}
		return out
	}
	h.reqs.snapshot = func() (map[string]any, error) {
		page, err := s.reqs.List(usage.Filter{})
		st := s.reqs.Store()
		if err != nil { // the live rows and the just-ended ones, from memory
			page, _ = s.reqs.List(usage.Filter{Status: []string{usage.StatusQueued, usage.StatusRunning}})
		}
		return map[string]any{"kind": "snapshot", "requests": page.Requests, "next": page.Next, "on": st.On(), "history_begins_at": beginsAt(st)}, err
	}
	s.reqs.SetWatcher(h)
	h.cancels = append(h.cancels, func() { s.reqs.SetWatcher(nil) })
}
