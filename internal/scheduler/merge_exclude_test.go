package scheduler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The exclusion work and the just-released wait together: a node excluded
// right after something of ours left it (1) refuses a profile that forces it
// at once, without the just-released wait or the eviction hold, and (2) is
// never a node to wait for, nor room an eviction makes.
func TestExclude_AJustReleasedExcludedNodeIsNeitherWaitedForNorEvictedFor(t *testing.T) {
	for _, tight := range []bool{false, true} {
		h := newHarness(t).nodes(ps5("a", 2, 15000), ps5("b", 4, 15000))
		if tight {
			h.cfg.Defaults.PlacementUsesReserved = true
			h.cfg.Defaults.MarginMeasuredMiB = 300
		}
		h.llama("v", 8000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.llama("u", 8000, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
		h.llama("m", 8000, func(p *core.Profile) { p.ForceNodes = []string{"b"} })
		h.llama("n", 12000)
		for _, name := range []string{"v", "u"} {
			w := h.request(name)
			h.healthy(name)
			h.release(h.granted(w))
		}
		h.advance(time.Minute) // past the holds
		h.step(evUnload{profile: "v", force: true})
		h.advance(time.Second)
		if !h.c.justReleased("b") {
			t.Fatalf("tight %v: b is not just released after v left it", tight)
		}
		nc := cloneCfg(h.cfg)
		nc.ExcludeNodes = []string{"b"}
		h.step(evConfig{cfg: nc})
		if h.c.justReleased("b") {
			t.Fatalf("tight %v: an excluded node still counts as just released", tight)
		}

		// (1) refused by the exclusion at once
		h.failed(h.request("m"), http.StatusServiceUnavailable, ReasonNodeExcluded)
		if h.c.entries["m"] != nil {
			t.Fatalf("tight %v: m entered the queue", tight)
		}

		// (2) n is placed on a by evicting u; b is not waited for
		wn := h.request("n")
		if _, ok := h.c.wantFig["b"]; ok {
			t.Fatalf("tight %v: the queue waits for readings of the excluded node b", tight)
		}
		if e := h.c.entries["n"]; e != nil && !e.evictHeld.IsZero() {
			t.Fatalf("tight %v: n's eviction is held", tight)
		}
		if u := h.c.insts["u"]; u != nil && u.state == stReady {
			t.Fatalf("tight %v: u was not evicted for n", tight)
		}
		if e := h.c.entries["n"]; e != nil {
			for _, id := range e.claim {
				if id == "b" {
					t.Fatalf("tight %v: n claimed the excluded node b", tight)
				}
			}
		}
		if a, ok := h.peek(wn); ok && a.err != nil {
			t.Fatalf("tight %v: n refused: %v", tight, a.err)
		}
	}
}
