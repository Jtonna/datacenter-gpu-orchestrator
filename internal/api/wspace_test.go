package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// The hub's status loop with one client connected (ws.go statusLoop). It
// read the status every 250 ms and encoded it up to three times, whether or
// not anything moved (live, 26 Nov: about 0.07 of a core with one tab open;
// 0.003 with none). Now it reads at the idle pace (2 s) while nothing is
// starting, stopping or queued, and a real change wakes it at once: a
// scheduler feed event (an instance's state, a load, a request), a history
// event (a node lost, joined, excluded) or a write through the API.
//
// Bounds, per minute of a steady fleet with one client: at most 31 status
// reads (one per 2 s, plus the first), each with ONE signature encoding (the
// catalog's signature is computed only for a client of the models topic),
// and at most 13 frames (one per 5 s when nothing changes, plus the first).
// Before: 240 reads, up to 480 encodings beside the frames.
func TestStatusLoopIdlePace(t *testing.T) {
	le := newLiveEnv(t, 0)
	h := le.api.ws
	w := liveClient(t, le.env, "status")
	w.until(t, "status", 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	reads0, frames0, sigs0 := h.statusReads.Load(), h.statusFrames.Load(), h.statusSigs.Load()
	const window = 6 * time.Second
	got := w.collect(window)
	reads, frames, sigs := h.statusReads.Load()-reads0, h.statusFrames.Load()-frames0, h.statusSigs.Load()-sigs0
	perMin := func(n int64) float64 { return float64(n) * float64(time.Minute) / float64(window) }
	t.Logf("steady, one client, %v: %d status reads (%.0f a minute), %d signature encodings, %d frames sent (%d messages seen)", window, reads, perMin(reads), sigs, frames, len(got))
	if reads > 4 { // 6 s at one read per 2 s, plus a tick at either end
		t.Fatalf("steady fleet: %d status reads in %v, want at most 4 (the old loop: 24)", reads, window)
	}
	if sigs > reads {
		t.Fatalf("%d signature encodings for %d reads", sigs, reads)
	}
	if frames > 2 {
		t.Fatalf("steady fleet: %d status frames in %v, want at most 2 (one per 5 s)", frames, window)
	}

	// a real change still reaches the client at once: a load shows within
	// 600 ms (live measured a load on a page within about 2.4 s end to end)
	start := time.Now()
	go le.sched.Load(context.Background(), "p00")
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-w.ch:
			if !ok {
				t.Fatal("the connection closed")
			}
			if m.Type != "status" {
				continue
			}
			var st struct {
				Instances []struct {
					Profile string `json:"profile"`
				} `json:"instances"`
			}
			if json.Unmarshal(m.Data, &st) != nil {
				continue
			}
			for _, i := range st.Instances {
				if i.Profile == "p00" {
					d := time.Since(start)
					t.Logf("the load reached the client in %v", d.Round(time.Millisecond))
					if d > 600*time.Millisecond {
						t.Fatalf("the load reached the client after %v, want within 600 ms", d)
					}
					return
				}
			}
		case <-deadline:
			t.Fatal("the load never reached the client")
		}
	}
}

// A memory drift alone (heartbeats, no event) shows within the idle pace.
func TestStatusLoopDriftWithinIdlePace(t *testing.T) {
	le := newLiveEnv(t, 0)
	w := liveClient(t, le.env, "status")
	w.until(t, "status", 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	le.heartbeat(700)
	for {
		m := w.until(t, "status", 3*time.Second)
		var st struct {
			View struct {
				Nodes []struct {
					FreeMiB int `json:"free_mib"`
				} `json:"nodes"`
			} `json:"view"`
		}
		if json.Unmarshal(m.Data, &st) == nil && len(st.View.Nodes) > 0 && st.View.Nodes[0].FreeMiB == 14700 {
			d := time.Since(start)
			t.Logf("a memory change reached the client in %v", d.Round(time.Millisecond))
			if d > 2500*time.Millisecond {
				t.Fatalf("after %v, want within the 2 s idle pace", d)
			}
			return
		}
	}
}

// statusWithin waits for a status frame that ok accepts and fails when it
// takes longer than within from start (or never comes in 3 s).
// statusSeen is what of a status frame the wake tests read.
type statusSeen struct {
	Instances []struct {
		Profile string `json:"profile"`
		State   string `json:"state"`
	} `json:"instances"`
	Queue []struct {
		Profile string `json:"profile"`
	} `json:"queue"`
	View struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	} `json:"view"`
}

func statusWithin(t *testing.T, w *wsc, start time.Time, within time.Duration, what string, ok func(st statusSeen) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m, open := <-w.ch:
			if !open {
				t.Fatal("the connection closed")
			}
			var st statusSeen
			if m.Type != "status" || json.Unmarshal(m.Data, &st) != nil || !ok(st) {
				continue
			}
			d := time.Since(start)
			t.Logf("%s reached the client in %v", what, d.Round(time.Millisecond))
			if d > within {
				t.Fatalf("%s reached the client after %v, want within %v", what, d, within)
			}
			return
		case <-deadline:
			t.Fatalf("%s never reached the client", what)
		}
	}
}

// idleClient is a status client on le once its loop has gone back to the
// idle pace (the first frame, then activeHold and a little more).
func idleClient(t *testing.T, le *liveEnv) *wsc {
	t.Helper()
	w := liveClient(t, le.env, "status")
	w.until(t, "status", 3*time.Second)
	time.Sleep(activeHold + 700*time.Millisecond)
	for len(w.ch) > 0 {
		<-w.ch
	}
	return w
}

// Each real change wakes the idle status loop within the bound of a load
// (600 ms): an unload, a queue entry (a request that waits for memory), an
// agent change (a node leaving the pool, recorded in the history as the
// pool records it).
func TestStatusLoopWakes(t *testing.T) {
	t.Run("unload", func(t *testing.T) {
		le := newLiveEnv(t, 0)
		if err := le.sched.Load(context.Background(), "p00"); err != nil {
			t.Fatal(err)
		}
		le.waitReady("p00")
		w := idleClient(t, le)
		start := time.Now()
		if err := le.sched.Unload(context.Background(), "p00", false); err != nil {
			t.Fatal(err)
		}
		statusWithin(t, w, start, 600*time.Millisecond, "the unload", func(st statusSeen) bool {
			for _, i := range st.Instances {
				if i.Profile == "p00" && i.State == "ready" {
					return false
				}
			}
			return true
		})
	})
	t.Run("queue entry", func(t *testing.T) {
		le := newLiveEnv(t, 0)
		le.heartbeat(-13500) // 500 MiB free: p23 (3,300 MiB) waits for memory
		w := idleClient(t, le)
		start := time.Now()
		go func() {
			if res, err := http.Post(le.front.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"p23","messages":[{"role":"user","content":"hi"}]}`)); err == nil {
				res.Body.Close()
			}
		}()
		statusWithin(t, w, start, 600*time.Millisecond, "the queue entry", func(st statusSeen) bool {
			return len(st.Queue) == 1 && st.Queue[0].Profile == "p23"
		})
	})
	t.Run("agent change", func(t *testing.T) {
		hist := history.Open(history.Options{Dir: t.TempDir()})
		t.Cleanup(func() { hist.Close() })
		le := newLiveEnvWith(t, 0, func(o *Options) { o.History = hist })
		w := idleClient(t, le)
		start := time.Now()
		le.view.remove("host-2")
		hist.Record(history.Event{Ts: time.Now().UnixMilli(), Type: history.TNodeMembership, Severity: history.Warn,
			Nodes: []string{"host-2"}, Summary: "Node host-2 left the pool."})
		statusWithin(t, w, start, 600*time.Millisecond, "the node leaving", func(st statusSeen) bool {
			return len(st.View.Nodes) == 1
		})
	})
}

// Agents' heartbeats and log uploads are writes through the API too, but
// they carry memory figures (which reach the page at the idle pace) and
// logs: they do not wake the loop. Seven agents every 2 s woke it every
// 0.3 s and kept it at 250 ms for good (the cost on live did not move).
func TestStatusLoopAgentPostsDoNotWake(t *testing.T) {
	le := newLiveEnv(t, 0)
	h := le.api.ws
	w := idleClient(t, le)
	reads0 := h.statusReads.Load()
	stop := time.Now().Add(6 * time.Second)
	for i := 0; time.Now().Before(stop); i++ {
		p := "/api/pool/nodes/02:00:00:00:00:01/heartbeat"
		if i%5 == 4 {
			p = "/api/pool/nodes/02:00:00:00:00:01/logs"
		}
		if res, err := http.Post(le.front.URL+p, "application/json", strings.NewReader(`{}`)); err == nil {
			res.Body.Close()
		}
		time.Sleep(285 * time.Millisecond)
	}
	reads := h.statusReads.Load() - reads0
	t.Logf("6 s of agent posts every 285 ms: %d status reads (%d frames seen)", reads, len(w.ch))
	if reads > 4 {
		t.Fatalf("agent posts woke the status loop: %d reads in 6 s, want at most 4 (the idle pace)", reads)
	}
}

// The health topic judges the status the live channel's loop read a moment
// ago (within healthReuse) instead of asking the scheduler's loop again
// every 2 s: with a page open (status and health topics) health makes no
// status read of its own. A client of health alone still gets fresh
// figures: health then reads the status itself.
func TestHealthReusesTheLoopStatus(t *testing.T) {
	le := newLiveEnv(t, 0)
	h := &le.api.health
	w := liveClient(t, le.env, "status", "health")
	w.until(t, "health", 3*time.Second)
	time.Sleep(time.Second)
	asked0, evals0 := h.asked.Load(), h.evals.Load()
	time.Sleep(6 * time.Second)
	asked, evals := h.asked.Load()-asked0, h.evals.Load()-evals0
	t.Logf("a page open, 6 s: %d health evaluations, %d status reads of its own", evals, asked)
	if evals < 2 || asked != 0 {
		t.Fatalf("%d evaluations, %d status reads of health's own; want 2 or more, and 0", evals, asked)
	}
	w.conn.CloseNow()

	// no page open: GET /api/health reads the status itself when none was
	// read within healthReuse, and reuses one that was
	le2 := newLiveEnv(t, 0)
	h2 := &le2.api.health
	time.Sleep(healthReuse + 200*time.Millisecond) // the catalog's first build read one
	a0 := h2.asked.Load()
	body(t, le2.get("/api/health"))
	if n := h2.asked.Load() - a0; n != 1 {
		t.Fatalf("GET /api/health with no recent status: %d reads of its own, want 1", n)
	}
	body(t, le2.get("/api/status"))
	a0 = h2.asked.Load()
	body(t, le2.get("/api/health"))
	if n := h2.asked.Load() - a0; n != 0 {
		t.Fatalf("GET /api/health right after GET /api/status: %d reads of its own, want 0", n)
	}
}
