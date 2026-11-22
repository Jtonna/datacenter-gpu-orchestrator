package api

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// withoutProfiles is a copy of cfg without the named profiles.
func withoutProfiles(cfg *config.Config, drop ...string) *config.Config {
	c := *cfg
	c.Profiles = map[string]*core.Profile{}
	for n, p := range cfg.Profiles {
		c.Profiles[n] = p
	}
	for _, n := range drop {
		delete(c.Profiles, n)
	}
	return &c
}

// Profiles deleted while the listing (GET /api/profiles, the catalog
// rebuild) runs must not crash it: the live scheduler panicked on a nil
// profile here.
func TestProfileListingSurvivesProfilesDeletedMeanwhile(t *testing.T) {
	e := newEnv(t, okUpstream())
	full := e.sched.Config()
	var names []string
	for n := range full.Profiles {
		names = append(names, n)
	}
	less := withoutProfiles(full, names...)
	var stop atomic.Bool
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		for !stop.Load() {
			e.sched.ApplyConfig(less)
			e.sched.ApplyConfig(full)
		}
	}()
	defer func() { stop.Store(true); <-flipped; e.sched.ApplyConfig(full) }()

	deadline := time.Now().Add(time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("listing %d panicked: %v", i, r)
				}
			}()
			for _, p := range e.api.profileEntries(context.Background()) {
				if p["id"] == nil || p["kind"] == nil {
					t.Fatalf("entry without id or kind: %v", p)
				}
			}
		}()
	}
}

// A profile that is in the listing's snapshot but that the planner no longer
// knows (deleted since) is left out; the others are listed.
func TestProfileListingSkipsAProfileThePlannerNoLongerKnows(t *testing.T) {
	e := newEnv(t, okUpstream())
	snap := withoutProfiles(e.sched.Config())
	ghost := *snap.Profiles["alpha"]
	ghost.Name = "ghost"
	snap.Profiles["ghost"] = &ghost
	got := e.api.profileEntriesOf(context.Background(), snap)
	ids := map[string]bool{}
	for _, p := range got {
		ids[fmt.Sprint(p["id"])] = true
	}
	if ids["ghost"] || !ids["alpha"] || len(got) != len(snap.Profiles)-1 {
		t.Fatalf("listed %v", ids)
	}
}
