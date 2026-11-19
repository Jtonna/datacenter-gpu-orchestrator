package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

// GET /api/models in the scripted scenarios: the real catalog code
// (api.BuildModelCatalog) on the scripted status and profile list, the real
// history store, the real benchmark aggregates and the harness's model files
// (which pretend to have the sizes of fixtures.go). In scenario live the real
// server answers.

func (h *harness) writeCatalog(w http.ResponseWriter, in api.CatalogInput, key string) {
	out := api.BuildModelCatalog(in, key)
	if key == "" {
		writeJSON(w, 200, map[string]any{"models": out, "models_dir": "/models", "history": in.HistoryOn})
		return
	}
	if len(out) == 0 {
		writeErr(w, 404, "no model file "+key+": see GET /api/models", "not found")
		return
	}
	writeJSON(w, 200, out[0])
}

func (h *harness) handleModels(w http.ResponseWriter, r *http.Request) bool {
	profiles := h.st.profileEntries()
	if profiles == nil {
		return false
	}
	st, err := h.st.statusSource(r.Context())
	if err != nil {
		return false
	}
	in := api.CatalogInput{Profiles: profiles, HistoryOn: h.c.hist.On()}
	in.Status, _ = st.(map[string]any)
	in.Events = h.c.hist.Query(history.Query{Limit: history.MaxLimit}).Events
	if v, err := h.st.realGet(r.Context(), "/api/bench/aggregates"); err == nil {
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
	key, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/models"), "/"))
	h.writeCatalog(w, in, key)
	return true
}
