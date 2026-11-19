package api

import (
	"context"
	"net/http"
	"sort"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
)

// statusView is the /api/status document, with one addition per instance for
// model-centric clients: last_request (the token rate of the newest finished
// request of that model, when the upstream reported one). Both GET /api/status
// and the status WebSocket message use it.
func (s *Server) statusView(ctx context.Context) (any, error) {
	st, err := s.sched.Status(ctx)
	if err != nil {
		return nil, err
	}
	v := jsonView(st).(map[string]any)
	last := s.activity.lastByModel()
	if insts, ok := v["instances"].([]any); ok {
		for _, i := range insts {
			im, ok := i.(map[string]any)
			if !ok {
				continue
			}
			if r, ok := last[im["profile"].(string)]; ok {
				im["last_request"] = map[string]any{"tokens_per_second": r.TPS, "output_tokens": r.Out, "input_tokens": r.In,
					"status": r.Status, "duration_ms": r.DurationMS, "finished_at": isoTime(r.Timestamp)}
			}
		}
	}
	return v, nil
}

// handleProfiles is GET /api/profiles: every profile with its settings, its
// memory need (and where the figure comes from) and what a request for it
// would do now (the dry-run plan). Read-only.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.conf().Profiles))
	for n := range s.conf().Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	host := map[string]string{}
	if st, err := s.sched.Status(r.Context()); err == nil {
		for _, n := range st.View.Nodes {
			if n.Hostname != "" {
				host[string(n.ID)] = n.Hostname
			}
		}
	}
	out := make([]any, 0, len(names))
	for _, n := range names {
		p := s.conf().Profiles[n]
		e := map[string]any{"id": n, "kind": string(p.Kind), "model": p.Model, "ctx": p.Ctx, "parallel": p.Parallel,
			"aliases": nonNil(p.Aliases), "force_nodes": nonNil(p.ForceNodes), "tensor_split": p.TensorSplit}
		if p.TensorSplit == nil {
			e["tensor_split"] = []int{}
		}
		if p.NeedsMiB > 0 {
			e["needs_mib_configured"] = p.NeedsMiB
		}
		rep, err := s.sched.Plan(r.Context(), n)
		if err != nil {
			e["plan"] = map[string]any{"action": "unknown", "reason": err.Error()}
		} else {
			if rep.NeedMiB > 0 {
				e["needs_mib"] = rep.NeedMiB
				e["needs_basis"] = rep.NeedBasis
				e["needs_measured"] = rep.Measured
			}
			var nodes []string
			for _, pn := range rep.Placement {
				name := host[string(pn.ID)]
				if name == "" {
					name = string(pn.ID)
				}
				nodes = append(nodes, name)
			}
			plan := map[string]any{"action": string(rep.Action), "would_start": rep.Action == scheduler.PlanStart || rep.Action == scheduler.PlanServe,
				"nodes": nonNil(nodes), "reason": rep.Reason, "instance": rep.Instance}
			if rep.Warning != "" {
				plan["warning"] = rep.Warning
			}
			e["plan"] = plan
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
