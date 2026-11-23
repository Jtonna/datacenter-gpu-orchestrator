package scheduler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// deriveStore: a store the size of a live one (about 80 measurements of
// a dozen models at several contexts and device counts) and 300 profiles
// at contexts with no measurement of their own, most of them derived. It
// counts the provider calls the derivation makes (the estimates of other
// contexts: a GGUF header read each in production when not cached).
func deriveStore(t *testing.T) (h *harness, calls *int) {
	h = deriveHarness(t)
	calls = new(int)
	inner := h.c.needs
	h.c.needs = NeedFunc(func(p *core.Profile) (NeedInfo, error) {
		if strings.HasPrefix(p.Name, deriveName) {
			*calls++
		}
		return inner.Need(p)
	})
	rows := 0
	for m := range 12 {
		model := fmt.Sprintf("model%02d.gguf", m)
		for i, ctx := range []int{2048, 8192, 32768} {
			for d := 1; d <= 3; d++ {
				if (m+i+d)%4 == 0 {
					continue
				}
				h.measure(model, ctx, d, derivedFrom(5000+m*100, 2048, ctx, d)+d*600)
				rows++
			}
		}
	}
	if rows < 75 {
		t.Fatalf("store of %d rows", rows)
	}
	for i := range 300 {
		ctx := []int{4096, 16384, 65536, 1024, 24576}[i%5]
		h.llama(fmt.Sprintf("p%03d", i), 0, func(p *core.Profile) { p.Model, p.Ctx = fmt.Sprintf("model%02d.gguf", i%12), ctx })
	}
	return h, calls
}

// what GET /api/profiles and GET /api/needs compute of the needs
func listNeeds(t *testing.T, h *harness) (derived int) {
	t.Helper()
	for name, p := range h.cfg.Profiles {
		info, err := h.c.need(p)
		if err != nil {
			t.Fatal(name, err)
		}
		if info.Source == NeedDerived {
			derived++
		}
	}
	_ = h.c.derivePairs(nil)
	return derived
}

// The derivation's work is done once per state of the store: listing again,
// or after a configuration write, asks the provider for nothing and checks
// no pair again; a new measurement re-checks only its own model.
func TestDerivedNeed_WorkIsDoneOncePerStore(t *testing.T) {
	h, calls := deriveStore(t)
	if d := listNeeds(t, h); d < 100 {
		t.Fatalf("only %d derived needs: the test does not exercise the derivation", d)
	}
	first, builds := *calls, h.c.derive.pairBuilds
	if first == 0 || builds == 0 {
		t.Fatalf("calls %d builds %d", first, builds)
	}
	listNeeds(t, h)
	if *calls != first || h.c.derive.pairBuilds != builds {
		t.Fatalf("a second listing: provider calls %d -> %d, pair checks %d -> %d", first, *calls, builds, h.c.derive.pairBuilds)
	}

	// A configuration write: one profile added, one changed, one deleted.
	nc := cloneCfg(h.cfg)
	nc.Profiles["extra"] = &core.Profile{Name: "extra", Kind: core.KindLlama, Model: "model03.gguf", Ctx: 16384, Parallel: 1}
	nc.Profiles["p001"].Ctx = 65536
	delete(nc.Profiles, "p002")
	nc.Revision = 2
	h.step(evConfig{cfg: nc})
	h.cfg = nc
	listNeeds(t, h)
	if *calls != first || h.c.derive.pairBuilds != builds {
		t.Fatalf("after a configuration write: provider calls %d -> %d, pair checks %d -> %d", first, *calls, builds, h.c.derive.pairBuilds)
	}

	// A new measurement re-checks its model's pairs only.
	h.measure("model05.gguf", 4096, 1, derivedFrom(5500, 2048, 4096, 1)+600)
	listNeeds(t, h)
	if got := h.c.derive.pairBuilds - builds; got != 1 {
		t.Fatalf("a new measurement re-checked %d models' pairs, want 1", got)
	}
	if got := *calls - first; got > 2 {
		t.Fatalf("a new measurement cost %d provider calls", got)
	}
}
