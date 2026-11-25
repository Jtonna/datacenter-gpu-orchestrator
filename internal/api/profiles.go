package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
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
			"generated": IsGeneratedProfile(n, p.Generated),
			"ttl_s":     int64(cfg.EffectiveTTL(p) / time.Second)} // unload when idle; 0: never
		if p.TensorSplit == nil {
			e["tensor_split"] = []int{}
		}
		if p.NeedsMiB > 0 {
			e["needs_mib_configured"] = p.NeedsMiB
		}
		if p.Description != "" {
			e["description"] = p.Description
		}
		// The TEST / PRODUCTION marking, read only (config.EffectiveStage).
		e["stage"], e["stage_set"] = config.EffectiveStage(p)
		e["shown_name"] = config.ShownName(p)
		if p.DisplayName != "" {
			e["display_name"] = p.DisplayName
		}
		// It forces a node exclude_nodes names: kept, refused until lifted.
		if b, ok := cfg.BlockedByExclusion(n); ok {
			e["blocked_by_exclusion"] = b.Entry
		} else if err == nil && rep.ExcludedBy != "" {
			e["blocked_by_exclusion"] = rep.ExcludedBy // matched at run time (a hostname glob against an id)
		}
		if err != nil {
			e["plan"] = map[string]any{"action": "unknown", "reason": err.Error()}
		} else {
			if rep.NeedMiB > 0 {
				e["needs_mib"] = rep.NeedMiB
				e["needs_basis"] = rep.NeedBasis
				e["needs_measured"] = rep.Measured
				if rep.DerivedFromCtx > 0 {
					e["derived_from_ctx"] = rep.DerivedFromCtx // the basis says "derived from the measurement at ctx N"
				}
				if sl := rep.DerivedSlope; sl != nil { // the basis says "own slope of this model on N devices"
					e["derived_slope_mib_per_1k_ctx"] = sl.MiBPer1kCtx
					e["derived_slope_from_ctx"] = append(append([]int{}, sl.FromCtx...), sl.ConfirmedBy...)
				}
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
			if rep.TightFit {
				plan["tight_fit"] = true // the reason says where and by how much
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
			hostEntry(e, rep, host)
		}
		out = append(out, e)
	}
	return out
}

// hostEntry adds where the profile's llama-server runs (phase 6): host
// "server" or "node"; for a node, host_node (its name), host_overhead_mib
// and, when it cannot host now, host_problem.
func hostEntry(e map[string]any, rep scheduler.PlanReport, names map[string]string) {
	if rep.Host == "" {
		return
	}
	e["host"] = rep.Host
	if rep.HostMemory != nil {
		// the host process's own memory where it is hosted (scheduler
		// hostmem.go): checkpoints and prompt cache, counted or only shown
		e["host_memory"] = jsonView(*rep.HostMemory)
	}
	if rep.Host != core.HostNode {
		return
	}
	name := names[string(rep.HostNode)]
	if name == "" {
		name = string(rep.HostNode)
	}
	e["host_node"], e["host_overhead_mib"] = name, rep.HostOverheadMiB
	if rep.HostOverheadSource != "" { // which figure: "setting" (host_overhead_mib) or "measured" there
		e["host_overhead_source"] = rep.HostOverheadSource
	}
	if !rep.HostOverheadMeasuredAt.IsZero() {
		e["host_overhead_measured_at"] = isoTime(rep.HostOverheadMeasuredAt)
	}
	if rep.HostProblem != "" {
		e["host_problem"] = rep.HostProblem
	}
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
