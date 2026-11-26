package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// One tab stays on the Models page (the models topic) while the memory
// moves on every heartbeat (every 2 s, as the agents send them).
//
//   - A steady minute costs at most one round of plans: the drift rule
//     (catalogDriftGap) recomputes once the last round is a minute old;
//     nothing else does.
//   - A second browser's page open (GET /api/models, always fresh) at a
//     status no round was computed under costs ONE round, and that round is
//     the standing tab's catalog too: the tab holds the open's version (it
//     is sent to it, or it had it already) without a second computation, and
//     the drift clock starts from it.
//
// The gate's race run (26 Nov) failed the first form of this test, which
// opened the page after a fixed sleep at delta 0: a status the subscriber's
// first round was computed under, so the open rightly reused it
// (TestRESTReusesPlansOfTheSameStatus) and nothing was sent. The open is
// now at a status never planned (delta 777, below catalogFreeStep), every
// wait is on a condition (the hub's catalog caught up with the status; the
// tab's version), and it runs with the status loop at its pace and with a
// slow one (20 s: the timing that reproduced the race run's failure).
func TestStandingSubscriberRounds(t *testing.T) {
	for _, pace := range []time.Duration{0, 20 * time.Second} {
		t.Run(fmt.Sprintf("status loop pace %v", pace), func(t *testing.T) {
			le := newLiveEnvWith(t, 50*time.Millisecond, func(o *Options) {
				if pace > 0 {
					o.WSIdlePace = pace
				}
			})
			tab := liveClient(t, le.env, "status", "event", "health", "models")
			tabVer := int64(0)
			seen := func(m wsMsg) {
				if m.Type != "models" {
					return
				}
				var d struct {
					Version int64 `json:"version"`
				}
				json.Unmarshal(m.Data, &d)
				tabVer = max(tabVer, d.Version)
			}
			seen(tab.until(t, "models", 5*time.Second))
			// settled: the hub's catalog was built under the status as it is now
			// (the status loop is woken: with a slow pace it would read it late)
			settled := func() {
				t.Helper()
				eventually(t, "the hub's catalog under the current status", func() bool {
					for _, m := range tab.collect(20 * time.Millisecond) {
						seen(m)
					}
					le.api.ws.poke()
					st, err := le.api.statusView(context.Background())
					if err != nil {
						return false
					}
					snap, dirty := le.api.catalog.current()
					return snap != nil && !dirty && snap.sig == catalogSig(st)
				})
			}
			beat := func(i int) {
				le.tick(2 * time.Second)
				le.heartbeat(40 * (i % 5))
				time.Sleep(20 * time.Millisecond)
			}
			settled()
			// a steady minute
			r0, d0 := le.rounds(), le.sched.DryPlans()
			for i := 1; i <= 30; i++ {
				beat(i)
			}
			settled()
			steady := le.rounds() - r0
			t.Logf("a steady minute with one models tab: %d plan rounds (%d scheduler plans)", steady, le.sched.DryPlans()-d0)
			if steady > 1 {
				t.Fatalf("a steady minute with one models tab computed %d rounds, want at most 1 (the drift rule)", steady)
			}
			if got := le.sched.DryPlans() - d0; got != steady*livePlanProfiles {
				t.Fatalf("%d scheduler plans for %d rounds of %d profiles", got, steady, livePlanProfiles)
			}

			// half a minute more, then a page open by a second browser at a
			// status no round was computed under
			for i := 1; i <= 15; i++ {
				beat(i)
			}
			settled() // a drift round the loop owes is counted before the open
			r1, d1 := le.rounds(), le.sched.DryPlans()
			le.tick(2 * time.Second)
			le.heartbeat(777)
			res := le.get("/api/models")
			var doc struct {
				Version int64 `json:"version"`
			}
			if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			eventually(t, "the standing tab holds the open's catalog", func() bool {
				for _, m := range tab.collect(20 * time.Millisecond) {
					seen(m)
				}
				return tabVer >= doc.Version
			})
			// then the rest of that minute, the memory moving all along
			for i := 16; i <= 30; i++ {
				beat(i)
			}
			settled()
			open := le.rounds() - r1
			t.Logf("a page open, then the rest of the minute: %d plan rounds (%d scheduler plans), REST version %d, the tab's version %d", open, le.sched.DryPlans()-d1, doc.Version, tabVer)
			if open != 1 {
				t.Fatalf("a page open with a standing models tab, then the rest of the minute: %d rounds, want 1 (the open's, shared with the tab)", open)
			}
			if got := le.sched.DryPlans() - d1; got != livePlanProfiles {
				t.Fatalf("%d scheduler plans, want %d (one round)", got, livePlanProfiles)
			}
		})
	}
}

// What decided "0 rounds" in the gate's race run (catalogplans.go:22, the
// reuse when the plans were computed under this very status): the memory
// went back to a state the last round was computed under (the drift
// pattern 40*(i%5) returns to 0 every fifth heartbeat), so the page open's
// fresh read reused that round. Not stale: the status, and with it every
// plan input, is identical. Deterministic, with no hub: a round at delta 0,
// a live rebuild at delta 40 (reuses it), back at delta 0 a REST read
// computes nothing and serves plans equal to that round; at a state never
// planned (even after the live rebuild reached it) the REST read computes
// one round.
func TestRESTReusesPlansOfTheSameStatus(t *testing.T) {
	le := newLiveEnv(t, 50*time.Millisecond)
	plans := func(snap *catalogSnap) string {
		var doc struct {
			Models []struct {
				Profiles []struct {
					ID   string         `json:"id"`
					Plan map[string]any `json:"plan"`
				} `json:"profiles"`
			} `json:"models"`
		}
		if err := json.Unmarshal(snap.list, &doc); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(doc)
		return string(b)
	}
	le.heartbeat(0)
	first := le.build()
	if r := le.rounds(); r != 1 {
		t.Fatalf("first build: %d rounds", r)
	}
	le.tick(2 * time.Second)
	le.heartbeat(40)
	if s := le.build(); s.plansSig == s.sig {
		t.Fatal("the live rebuild at delta 40 computed plans: want them reused (drift under catalogFreeStep)")
	}
	le.tick(2 * time.Second)
	le.heartbeat(0)
	r0 := le.rounds()
	snap, err := le.api.catalogGet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := le.rounds() - r0; got != 0 {
		t.Fatalf("REST back at the planned status: %d rounds, want 0 (the plans of this very status)", got)
	}
	if snap.plansSig != snap.sig {
		t.Fatalf("REST served plans of another status (%q, status %q): stale", snap.plansSig, snap.sig)
	}
	if plans(snap) != plans(first) {
		t.Fatal("REST back at the planned status served plans that differ from that round's")
	}
	// a status never planned, which the live rebuild reached first (it
	// reuses the plans of delta 0): the REST read computes one round, it
	// never serves the hub's catalog with plans of another status
	le.tick(2 * time.Second)
	le.heartbeat(777)
	r0 = le.rounds()
	if s := le.build(); s.plansSig == s.sig || le.rounds() != r0 {
		t.Fatal("the live rebuild at delta 777 computed plans: want them reused")
	}
	if snap, err = le.api.catalogGet(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := le.rounds() - r0; got != 1 || snap.plansSig != snap.sig {
		t.Fatalf("REST at a status never planned: %d rounds (plans of %q, status %q), want 1 fresh", got, snap.plansSig, snap.sig)
	}
}
