package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
)

// The /api/events feed (design A.3). Every message is a data-only SSE event
// (no "event:" line, because the hub page reads it with EventSource.onmessage)
// carrying {"type":..., "data":"<JSON string>"}.

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

func frame(typ string, data any) []byte {
	inner, _ := json.Marshal(data)
	env, _ := json.Marshal(map[string]any{"type": typ, "data": string(inner)})
	return append(append([]byte("data: "), env...), '\n', '\n')
}

type inflightReq struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Timestamp string `json:"timestamp"`
}

type client struct{ ch chan []byte }

// hub fans the scheduler's feed out to the connected event streams. It owns
// the in-flight table, so a new connection gets a snapshot that is
// consistent with the upserts and removes that follow it.
type hub struct {
	sched *scheduler.Scheduler
	now   func() time.Time

	mu       sync.Mutex
	clients  map[*client]struct{}
	inflight map[uint64]inflightReq
	closed   bool

	cancel func()
	done   chan struct{}
}

func newHub(s *scheduler.Scheduler, now func() time.Time) *hub {
	h := &hub{sched: s, now: now, clients: map[*client]struct{}{}, inflight: map[uint64]inflightReq{}, done: make(chan struct{})}
	feed, cancel := s.Subscribe(4096)
	h.cancel = cancel
	go h.run(feed)
	return h
}

func (h *hub) run(feed <-chan scheduler.FeedEvent) {
	defer close(h.done)
	var last uint64
	for ev := range feed {
		gap := last != 0 && ev.Seq != last+1
		last = ev.Seq
		h.mu.Lock()
		switch ev.Kind {
		case scheduler.FeedInstance:
			h.broadcastLocked(h.modelStatusLocked())
		case scheduler.FeedAdmitted:
			r := inflightReq{ID: strconv.FormatUint(ev.RequestID, 10), Model: ev.Profile, Timestamp: isoTime(ev.Time)}
			h.inflight[ev.RequestID] = r
			h.broadcastLocked(frame("inflight", map[string]any{"operation": "upsert", "request": r}))
		case scheduler.FeedLoadStarted:
			h.broadcastLocked(frame("loadStarted", jsonView(ev.LoadStarted)))
		case scheduler.FeedLoadFinished:
			h.broadcastLocked(frame("loadFinished", jsonView(ev.Outcome)))
		case scheduler.FeedFinished:
			delete(h.inflight, ev.RequestID)
			h.broadcastLocked(frame("inflight", map[string]any{"operation": "remove", "id": strconv.FormatUint(ev.RequestID, 10)}))
		}
		if gap { // events were dropped for us: resend the whole picture
			h.broadcastLocked(h.modelStatusLocked())
			h.broadcastLocked(h.snapshotLocked())
		}
		h.mu.Unlock()
	}
	h.close()
}

func (h *hub) modelStatusLocked() []byte {
	type entry struct {
		ID         string `json:"id"`
		State      string `json:"state"`
		ReadySince string `json:"readySince,omitempty"`
	}
	ps := h.sched.Profiles()
	out := make([]entry, 0, len(ps))
	for _, p := range ps {
		e := entry{ID: p.Name, State: string(p.State)}
		if p.State == scheduler.RunningReady && !p.ReadySince.IsZero() {
			e.ReadySince = isoTime(p.ReadySince)
		}
		out = append(out, e)
	}
	return frame("modelStatus", out)
}

func (h *hub) snapshotLocked() []byte {
	ids := make([]uint64, 0, len(h.inflight))
	for id := range h.inflight {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	reqs := make([]inflightReq, 0, len(ids))
	for _, id := range ids {
		reqs = append(reqs, h.inflight[id])
	}
	return frame("inflight", map[string]any{"operation": "snapshot", "requests": reqs})
}

// broadcastLocked never blocks: a client that cannot keep up is dropped and
// its browser reconnects to a fresh snapshot.
func (h *hub) broadcastLocked(b []byte) {
	for c := range h.clients {
		select {
		case c.ch <- b:
		default:
			delete(h.clients, c)
			close(c.ch)
		}
	}
}

func (h *hub) activityDone() {
	h.mu.Lock()
	h.broadcastLocked(frame("activity", struct{}{}))
	h.mu.Unlock()
}

// add registers a client whose channel already holds the initial
// modelStatus and inflight snapshot. It returns nil once the hub is closed.
func (h *hub) add() *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	c := &client{ch: make(chan []byte, 256)}
	c.ch <- h.modelStatusLocked()
	c.ch <- h.snapshotLocked()
	h.clients[c] = struct{}{}
	return c
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.ch)
	}
}

func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// close ends every stream and the subscription. Safe to call repeatedly.
func (h *hub) close() {
	h.cancel()
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		for c := range h.clients {
			delete(h.clients, c)
			close(c.ch)
		}
	}
	h.mu.Unlock()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead { // the mux routes HEAD to GET handlers: no stream, no body
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		return
	}
	rc := http.NewResponseController(w)
	c := s.hub.add()
	if c == nil {
		writeSchedError(w, &scheduler.Error{Status: http.StatusServiceUnavailable, Reason: scheduler.ReasonShuttingDown,
			Message: "the scheduler is shutting down", RetryAfter: 30 * time.Second})
		return
	}
	defer s.hub.remove(c)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	ping := time.NewTicker(s.opts.PingInterval)
	defer ping.Stop()
	for {
		select {
		case b, ok := <-c.ch:
			if !ok {
				return
			}
			if _, err := w.Write(b); err != nil {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
