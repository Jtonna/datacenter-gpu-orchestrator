package api

import (
	"encoding/json"
	"testing"
	"time"
)

// One tab stays on the Models page (the models topic) while the memory
// moves on every heartbeat (every 2 s, as the agents send them).
//
//   - A steady minute costs at most one round of plans: the drift rule
//     (catalogDriftGap) recomputes once the last round is a minute old and
//     the free memory has moved; nothing else does.
//   - A second browser's page open in that minute (GET /api/models, which is
//     always fresh) computes ONE round, and that round is the standing tab's
//     catalog too: the hub hands it over (a models message with the open's
//     version) instead of computing it again, and the drift clock starts
//     from it.
func TestStandingSubscriberRounds(t *testing.T) {
	le := newLiveEnv(t, 50*time.Millisecond)
	tab := liveClient(t, le.env, "status", "event", "health", "models")
	tab.until(t, "models", 5*time.Second)
	time.Sleep(500 * time.Millisecond)

	beat := func(i int) {
		le.tick(2 * time.Second)
		le.heartbeat(40 * (i % 5))
		time.Sleep(40 * time.Millisecond) // the status loop and the rebuild loop run for real
	}
	// a steady minute
	r0, d0 := le.rounds(), le.sched.DryPlans()
	for i := 1; i <= 30; i++ {
		beat(i)
	}
	time.Sleep(300 * time.Millisecond)
	steady := le.rounds() - r0
	t.Logf("a steady minute with one models tab: %d plan rounds (%d scheduler plans)", steady, le.sched.DryPlans()-d0)
	if steady > 1 {
		t.Fatalf("a steady minute with one models tab computed %d rounds, want at most 1 (the drift rule)", steady)
	}
	if got := le.sched.DryPlans() - d0; got > steady*livePlanProfiles {
		t.Fatalf("%d scheduler plans for %d rounds of %d profiles", got, steady, livePlanProfiles)
	}

	// a page open by a second browser, half a minute into the next minute
	for i := 1; i <= 15; i++ {
		beat(i)
	}
	tab.collect(100 * time.Millisecond) // drop what came so far
	r1, d1 := le.rounds(), le.sched.DryPlans()
	res := le.get("/api/models")
	var doc struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	time.Sleep(time.Second) // the hub and the rebuild loop settle
	// the standing tab gets the open's catalog
	got := int64(0)
	for _, m := range tab.collect(500 * time.Millisecond) {
		if m.Type != "models" {
			continue
		}
		var d struct {
			Version int64 `json:"version"`
		}
		json.Unmarshal(m.Data, &d)
		if d.Version > got {
			got = d.Version
		}
	}
	// then the rest of that minute, the memory moving all along
	for i := 16; i <= 30; i++ {
		beat(i)
	}
	time.Sleep(300 * time.Millisecond)
	open := le.rounds() - r1
	t.Logf("a page open in the next minute: %d plan rounds (%d scheduler plans), REST version %d, the tab's last version %d", open, le.sched.DryPlans()-d1, doc.Version, got)
	if open != 1 {
		t.Fatalf("a page open with a standing models tab, then the rest of the minute: %d rounds, want 1 (the open's, shared with the tab)", open)
	}
	if got := le.sched.DryPlans() - d1; got != livePlanProfiles {
		t.Fatalf("%d scheduler plans, want %d (one round)", got, livePlanProfiles)
	}
	if got != 0 && got < doc.Version {
		t.Fatalf("the standing tab's catalog (version %d) is older than the open's (%d)", got, doc.Version)
	}
}
