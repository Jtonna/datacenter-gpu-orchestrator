package scheduler

import "testing"

// A description is words for people: changing only it never makes a loaded
// instance stale (replaced at its next load); changing the load does.
func TestDescriptionChangeIsNotStale(t *testing.T) {
	h := newHarness(t).nodes(hostNode(16000))
	h.llama("m", 4000)
	h.ready("m")
	in := h.inst("m")
	p := *h.cfg.Profiles["m"]
	p.Description = "now with words"
	h.cfg.Profiles["m"] = &p
	if h.c.staleProfile(in) {
		t.Fatal("stale after a description change")
	}
	q := p
	q.Ctx++
	h.cfg.Profiles["m"] = &q
	if !h.c.staleProfile(in) {
		t.Fatal("not stale after a context change")
	}
}
