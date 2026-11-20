package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Live configuration apply: the core swaps to a new snapshot as an event.

func cloneCfg(c *config.Config) *config.Config {
	nc := *c
	nc.Profiles = map[string]*core.Profile{}
	for k, v := range c.Profiles {
		p := *v
		nc.Profiles[k] = &p
	}
	return &nc
}

func instStale(h *harness, profile string) bool {
	h.t.Helper()
	st := h.c.statusDoc()
	for _, in := range st.Instances {
		if in.Profile == profile {
			return in.StaleProfile
		}
	}
	h.t.Fatalf("no instance %s", profile)
	return false
}

func TestConfigApply_LoadedInstanceKeepsDefinitionAndIsStale(t *testing.T) {
	h := twoBig(t)
	h.ready("big")
	if instStale(h, "big") {
		t.Fatal("stale before any change")
	}
	nc := cloneCfg(h.cfg)
	nc.Profiles["big"].Ctx = 2048
	nc.Revision = 2
	h.step(evConfig{cfg: nc})
	if !instStale(h, "big") {
		t.Fatal("changed profile: instance not flagged stale")
	}
	if got := h.inst("big").prof.Ctx; got != 4096 {
		t.Fatalf("running instance changed its definition: ctx %d", got)
	}
	if h.c.statusDoc().ConfigRevision != 2 {
		t.Fatal("status does not carry the revision")
	}
	// An unrelated change does not flag it again.
	nc2 := cloneCfg(h.cfg)
	h.step(evConfig{cfg: nc2})
	if instStale(h, "big") {
		t.Fatal("identical profile flagged stale")
	}
}

func TestConfigApply_DeletedProfileIsStaleAndItsQueuedRequestsFail(t *testing.T) {
	h := twoBig(t)
	h.ready("big") // busy, so "other" waits in the queue
	w := h.request("other")
	h.pending(w)
	nc := cloneCfg(h.cfg)
	delete(nc.Profiles, "other")
	delete(nc.Profiles, "big")
	h.step(evConfig{cfg: nc})
	e := h.failed(w, 404, ReasonUnknownModel)
	if !strings.Contains(e.Message, "deleted") {
		t.Fatalf("reason not clear: %q", e.Message)
	}
	h.noEntry("other")
	if !instStale(h, "big") {
		t.Fatal("deleted profile's instance not flagged stale")
	}
	h.wantState("big", "ready") // it keeps serving until its ttl
}

func TestConfigApply_QueuedEntryTakesNewDefinitionAndTimingApplies(t *testing.T) {
	h := twoBig(t)
	h.ready("big")
	w := h.request("other")
	h.pending(w)
	nc := cloneCfg(h.cfg)
	nc.Profiles["other"].NeedsMiB = 5000
	nc.Defaults.QueueTimeout = 10 * time.Second
	h.step(evConfig{cfg: nc})
	if got := h.entry("other").prof.NeedsMiB; got != 5000 {
		t.Fatalf("queued entry kept the old definition: %d", got)
	}
	h.advance(15 * time.Second) // the new, shorter queue_timeout applies from the next evaluation
	h.failed(w, 503, ReasonClusterBusy)
}
