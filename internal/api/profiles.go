package api

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
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
	s.agentStatus(v, &st)
	s.logStatus(v)
	return v, nil
}

// handleProfiles is GET /api/profiles: every profile with its settings, its
// memory need (and where the figure comes from) and what a request for it
// would do now (the dry-run plan). Read-only.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": s.profileEntries(r.Context())})
}

// profileEntries builds the entries of GET /api/profiles (the model catalog
// reads them too) from one configuration snapshot.
func (s *Server) profileEntries(ctx context.Context) []map[string]any {
	return s.profileEntriesOf(ctx, s.conf())
}

// profileEntriesOf lists the profiles of cfg. A profile the planner no
// longer knows (deleted since cfg was taken) is left out.
func (s *Server) profileEntriesOf(ctx context.Context, cfg *config.Config) []map[string]any {
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	host := map[string]string{}
	learned := map[string]scheduler.LearnedNeed{}
	if st, err := s.sched.Status(ctx); err == nil {
		for _, n := range st.View.Nodes {
			if n.Hostname != "" {
				host[string(n.ID)] = n.Hostname
			}
		}
		learned = st.LearnedNeeds
	}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		p := cfg.Profiles[n]
		rep, err := s.sched.Plan(ctx, n)
		var se *scheduler.Error
		if errors.As(err, &se) && se.Reason == scheduler.ReasonUnknownModel {
			continue // deleted since the snapshot
		}
		e := map[string]any{"id": n, "kind": string(p.Kind), "model": p.Model, "ctx": p.Ctx, "parallel": p.Parallel,
			"aliases": nonNil(p.Aliases), "force_nodes": nonNil(p.ForceNodes), "tensor_split": p.TensorSplit,
			"generated": IsGeneratedProfile(n, p.Generated)}
		if p.TensorSplit == nil {
			e["tensor_split"] = []int{}
		}
		if p.NeedsMiB > 0 {
			e["needs_mib_configured"] = p.NeedsMiB
		}
		if err != nil {
			e["plan"] = map[string]any{"action": "unknown", "reason": err.Error()}
		} else {
			if rep.NeedMiB > 0 {
				e["needs_mib"] = rep.NeedMiB
				e["needs_basis"] = rep.NeedBasis
				e["needs_measured"] = rep.Measured
			}
			if l, ok := learned[n]; ok && l.PeakMiB > 0 {
				e["peak_mib"], e["peak_basis"] = l.PeakMiB, l.PeakBasis // the memory reservation rule's stored peak
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
			if rep.LoadMode != "" {
				plan["load_mode"] = rep.LoadMode
			}
			if len(rep.ReloadFromCache) > 0 {
				plan["reload_from_cache"] = reloadEntries(rep.ReloadFromCache, host)
			}
			if rep.Memory != nil {
				plan["memory"] = jsonView(rep.Memory)
			}
			e["plan"] = plan
		}
		out = append(out, e)
	}
	return out
}

// reloadEntries is the reload rule of a plan's nodes with their host names
// (design, "Reload from cache").
func reloadEntries(rs []scheduler.NodeReload, host map[string]string) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		name := host[string(r.ID)]
		if name == "" {
			name = string(r.ID)
		}
		e := map[string]any{"id": string(r.ID), "node": name, "verdict": r.Verdict, "shared_memory": r.Shared,
			"file_mib": r.FileMiB, "model_mib": r.ModelMiB, "memory_mib": r.MemoryMiB, "margin_mib": r.MarginMiB}
		if r.CachedPct != nil {
			e["cached_pct"] = *r.CachedPct
		}
		if r.CacheRoomMiB != nil {
			e["cache_room_mib"], e["cache_occupied"] = *r.CacheRoomMiB, r.CacheOccupied
			if r.CacheLastFile != "" {
				e["cache_last_file"] = r.CacheLastFile
			}
			if r.CacheNote != "" {
				e["cache_note"] = r.CacheNote
			}
		}
		if r.CachedNowMiB != nil {
			e["cached_now_mib"] = *r.CachedNowMiB
		}
		out = append(out, e)
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
