package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"
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
