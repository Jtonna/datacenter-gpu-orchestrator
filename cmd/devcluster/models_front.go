package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// The model catalog in the scripted and recorded scenarios. GET /api/models,
// GET /api/models/<key>, the icon endpoints and the "models" topic of /ws are
// the real server's own code and its in-memory catalog; only what the catalog
// is joined from is replaced (api.Options.WithCatalogSource, DEV-ONLY): the
// scripted or recorded status and profile list, the real history store, the
// benchmark aggregates and the harness's model files (which pretend to have
// the sizes of fixtures.go). In scenario live the real server joins its own.
//
// The harness says with a signature when its picture changed: the scenario,
// the scripted status (the recorded one is frozen) and how many history and
// benchmark events it has seen. While the signature is the one the server
// holds, nothing is gathered and the server answers from memory, as in
// production.

// catalogSeen counts the events that can change the catalog (watchCatalog).
var catalogSeen atomic.Int64

func (h *harness) watchCatalog() {
	if h.c.hist.On() {
		h.c.hist.Subscribe(func(history.Event) { catalogSeen.Add(1) })
	}
	h.c.benchMgr.Subscribe(func(ev map[string]any) {
		if k := ev["kind"]; k == "aggregate_updated" || k == "run" {
			catalogSeen.Add(1)
		}
	})
}

func (h *harness) catalogSource(ctx context.Context, have string) (api.CatalogInput, string, bool) {
	none := api.CatalogInput{}
	if h.c == nil || h.st == nil {
		return none, "", false
	}
	sc, _ := h.st.get()
	if h.real {
		sig := fmt.Sprintf("real|%s|%d", sc.Name, catalogSeen.Load())
		if sig == have {
			return none, sig, true
		}
		v, err := h.st.statusSource(ctx)
		if err != nil {
			return none, "", false
		}
		m, _ := v.(map[string]any)
		return h.realCatalogInput(m), sig, true
	}
	profiles := h.st.profileEntries()
	if profiles == nil {
		return none, "", false // scenario live: the real server's own
	}
	st, err := h.st.statusSource(ctx)
	if err != nil {
		return none, "", false
	}
	sig := fmt.Sprintf("%s|%d|%d|%s", sc.Name, len(profiles), catalogSeen.Load(), api.CatalogStatusSig(st))
	if sig == have {
		return none, sig, true
	}
	in := api.CatalogInput{Profiles: profiles, HistoryOn: h.c.hist.On()}
	in.Status, _ = st.(map[string]any)
	in.Events = h.c.hist.Query(history.Query{Limit: history.MaxLimit}).Events
	if v, err := h.st.realGet(ctx, "/api/bench/aggregates"); err == nil {
		in.Aggregates, _ = v.(map[string]any)
	}
	var named []string
	for _, p := range profiles {
		if m, _ := p["model"].(string); m != "" {
			named = append(named, m)
		}
	}
	in.Files = api.ListModelFiles(h.c.svc.Config().ModelsDir, named)
	for i := range in.Files {
		if n, ok := modelBytes[in.Files[i].File]; ok && in.Files[i].Exists {
			in.Files[i].SizeBytes = n
		}
	}
	return in, sig, true
}
