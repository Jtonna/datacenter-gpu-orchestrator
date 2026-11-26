package api

import (
	"context"
	"net/http"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// GET /api/profiles/test (observe): the Test profiles page (profiles plan,
// section 9). One row per test profile: its shown name, id, model, the
// context it really runs at (the plan's: the trained context when the
// written ctx is above it), why it is test, who refers to it and its last
// use in the history kept.

// Why a profile is test.
const (
	testWhyMarked    = "marked"       // stage: test written
	testWhyGenerated = "generated"    // made by a tool (generated, else its id)
	testWhyForced    = "forced zones" // force_nodes
)

// testReasons are every reason that applies to p, the first the one shown.
func testReasons(id string, generated *bool, forced int, stageSet bool) []string {
	var out []string
	if stageSet {
		out = append(out, testWhyMarked)
	}
	if config.IsGenerated(id, generated) {
		out = append(out, testWhyGenerated)
	}
	if forced > 0 {
		out = append(out, testWhyForced)
	}
	return out
}

// profileRefs maps a profile id to the benchmarks that name it and the
// benchmark groups (suites) that list one of those.
func (s *Server) profileRefs() (benches, groups map[string][]string) {
	benches, groups = map[string][]string{}, map[string][]string{}
	st := s.opts.BenchStore
	if st == nil || st.BenchOff() != nil {
		return
	}
	bms, err := st.ListBenchmarks()
	if err != nil {
		return
	}
	byBench := map[string]string{}
	for _, b := range bms {
		if b.Spec != nil && b.Spec.Profile != "" {
			benches[b.Spec.Profile] = append(benches[b.Spec.Profile], b.Spec.ID)
			byBench[b.Spec.ID] = b.Spec.Profile
		}
	}
	sts, err := st.ListSuites()
	if err != nil {
		return
	}
	for _, su := range sts {
		if su.Spec == nil {
			continue
		}
		seen := map[string]bool{}
		for _, it := range su.Spec.Benchmarks {
			p := byBench[it.Ref]
			if it.Inline != nil {
				p = it.Inline.Profile
			}
			if p != "" && !seen[p] {
				seen[p] = true
				groups[p] = append(groups[p], su.Spec.ID)
			}
		}
	}
	return
}

// lastUses is each profile's latest load, unload or benchmark step in the
// events (newest first).
func lastUses(evs []history.Event) map[string]int64 {
	out := map[string]int64{}
	for _, e := range evs {
		switch e.Type {
		case history.TLoadFinished, history.TInstanceUnloaded, history.TBenchStepFinished:
		default:
			continue
		}
		if e.Profile != "" && e.Ts > out[e.Profile] {
			out[e.Profile] = e.Ts
		}
	}
	return out
}

func (s *Server) handleTestProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.testProfiles(r.Context()))
}

func (s *Server) testProfiles(ctx context.Context) map[string]any {
	cfg := s.conf()
	var evs []history.Event
	if snap, err := s.catalogGet(ctx); err == nil && snap != nil {
		evs = snap.in.Events
	}
	used := lastUses(evs)
	benches, groups := s.profileRefs()
	ids := make([]string, 0, len(cfg.Profiles))
	for id := range cfg.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := []map[string]any{}
	byModel, byReason := map[string]int{}, map[string]int{}
	production := 0
	for _, id := range ids {
		p := cfg.Profiles[id]
		stage, set := config.EffectiveStage(p)
		if stage != config.StageTest {
			production++
			continue
		}
		why := testReasons(id, p.Generated, len(p.ForceNodes), set)
		row := map[string]any{"id": id, "shown_name": config.ShownName(p), "model": p.Model, "model_name": s.modelName(p.Model),
			"ctx": p.Ctx, "ctx_real": p.Ctx, "reason": why[0], "reasons": why, "stage_set": set, "aliases": nonNil(p.Aliases),
			"force_nodes": nonNil(p.ForceNodes),
			"refs":        map[string]any{"benchmarks": nonNil(benches[id]), "groups": nonNil(groups[id])}, "last_used": nil}
		if p.DisplayName != "" {
			row["display_name"] = p.DisplayName
		}
		if s.sched != nil {
			if rep, err := s.sched.Plan(ctx, id); err == nil && rep.Ctx > 0 {
				row["ctx_real"] = rep.Ctx // the trained context when the written one is above it
				if rep.CtxNote != "" {
					row["ctx_note"] = rep.CtxNote
				}
			}
		}
		if t := used[id]; t > 0 {
			row["last_used"] = t
		}
		byModel[p.Model]++
		byReason[why[0]]++
		rows = append(rows, row)
	}
	return map[string]any{"profiles": rows, "count": len(rows), "production_count": production,
		"models": byModel, "reasons": byReason, "history": len(evs) > 0}
}
