package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/api"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// scenarioState is the current scenario and when it was selected (animated
// figures such as a creeping load progress count from then).
type scenarioState struct {
	mu    sync.Mutex
	name  string
	since time.Time
	c     *cluster
	// configState says what the status documents carry next to the status:
	// config_revision and configured (false only when started bare).
	configState func() (revision int, configured bool)
}

// decorate adds config_revision and configured to a status document.
func (s *scenarioState) decorate(v any) any {
	m, ok := v.(map[string]any)
	if !ok || s.configState == nil {
		return v
	}
	m["config_revision"], m["configured"] = s.configState()
	return m
}

func (s *scenarioState) get() (scenario, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, _ := scenarioByName(s.name)
	return sc, time.Since(s.since)
}

func (s *scenarioState) set(name string) {
	s.mu.Lock()
	s.name, s.since = name, time.Now()
	s.mu.Unlock()
}

func (s *scenarioState) live() bool {
	sc, _ := s.get()
	return sc.Status == nil
}

// scripted builds the scripted status now; nil in scenario live.
func (s *scenarioState) scripted() *statusBuilder {
	sc, t := s.get()
	if sc.Status == nil {
		return nil
	}
	return sc.Status(time.Now(), t)
}

// realGet runs a GET on the real server in process and decodes the JSON answer.
func (s *scenarioState) realGet(ctx context.Context, path string) (any, error) {
	rec := httptest.NewRecorder()
	s.c.api.ServeHTTP(rec, httptest.NewRequest("GET", path, nil).WithContext(ctx))
	if rec.Code != 200 {
		return nil, fmt.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	var v any
	err := json.Unmarshal(rec.Body.Bytes(), &v)
	return v, err
}

// statusSource is the /ws status for the real server's hub: the scripted
// status, or in scenario live the real GET /api/status document.
func (s *scenarioState) statusSource(ctx context.Context) (any, error) {
	if sc, _ := s.get(); sc.Raw != nil { // a recorded status, as it was
		return sc.Raw()
	}
	if b := s.scripted(); b != nil {
		v := b.finish()
		if sc, _ := s.get(); sc.Decorate != nil {
			sc.Decorate(v)
		}
		return s.decorate(v), nil
	}
	v, err := s.realGet(ctx, "/api/status")
	return s.decorate(v), err
}

// ---- scripted /api/status, /api/profiles, /api/plan ----

// handleStatus answers GET /api/status: scripted, or in live the real
// document; both with config_revision and configured added.
func (s *scenarioState) handleStatus(w http.ResponseWriter, r *http.Request) bool {
	v, err := s.statusSource(r.Context())
	if err != nil {
		return false
	}
	writeJSON(w, 200, v)
	return true
}

func hostOf(id string) string {
	for _, n := range allNodes {
		if n.ID == id {
			return n.Host
		}
	}
	return id
}

const pool = 4 // defaults.max_pool

// planFor is the scripted dry run, following the order of the real dryPlan.
func planFor(profile string, st scheduler.Status, now time.Time) scheduler.PlanReport {
	p, _ := profileByID(profile)
	r := scheduler.PlanReport{Profile: p.ID, Alias: p.ID != profile, NeedMiB: p.NeedsMiB, Measured: true, FitNeedMiB: p.NeedsMiB, NeedBasis: "measured, needs_mib",
		Placement: []scheduler.PlacedNodeStatus{}, Victims: []scheduler.PlanVictim{}}
	for _, b := range st.Backoffs {
		if b.Profile == p.ID && b.Until.After(now) {
			r.Action, r.Reason = scheduler.PlanBackoff, "backed off until "+b.Until.Local().Format("15:04:05")
			return r
		}
	}
	for _, in := range st.Instances {
		if in.Profile != p.ID {
			continue
		}
		r.Instance = in.State
		r.Placement = nil
		for _, pn := range in.Nodes {
			pn.Memory = nil
			r.Placement = append(r.Placement, pn)
		}
		switch in.State {
		case "ready":
			r.Action = scheduler.PlanServe
		case "starting":
			r.Action = scheduler.PlanWait
		default:
			r.Action, r.Reason = scheduler.PlanQueue, fmt.Sprintf("waits for the %s instance to go", in.State)
		}
		return r
	}
	// capacity: the best max_pool eligible nodes at plan budgets
	var budgets []int
	for _, n := range st.View.Nodes {
		if !n.Excluded {
			budgets = append(budgets, n.PlanBudgetMiB)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(budgets)))
	capacity := 0
	for i := 0; i < len(budgets) && i < pool; i++ {
		capacity += budgets[i]
	}
	if req := int(math.Ceil(float64(p.NeedsMiB) * 1.15)); p.NeedsMiB > capacity || req > capacity {
		r.Action, r.RequiredMiB, r.CapacityMiB = scheduler.PlanReject, req, capacity
		r.Reason = fmt.Sprintf("placer: need %d MiB (requires %d MiB) exceeds cluster capacity %d MiB; nodes excluded by config and not counted: %s", p.NeedsMiB, req, capacity, laptop.Host)
		return r
	}
	for _, e := range st.Queue {
		if e.Profile == p.ID {
			r.Action = scheduler.PlanQueue
			r.Reason = fmt.Sprintf("joins the queued entry (%d waiting, claimed %v)", e.Waiters, e.Claimed)
			return r
		}
	}
	free := func(n scheduler.NodeStatus, victims map[string]bool) bool {
		return n.Online && !n.Excluded && n.Quarantined.IsZero() && n.ClaimedBy == "" && (n.Holder == "" || victims[n.Holder])
	}
	budget := func(n scheduler.NodeStatus, victims map[string]bool) int {
		if n.Holder != "" && victims[n.Holder] {
			return n.PlanBudgetMiB
		}
		return n.LiveBudgetMiB
	}
	try := func(victims map[string]bool) ([]scheduler.NodeStatus, string) {
		if len(p.ForceNodes) > 0 {
			var out []scheduler.NodeStatus
			var busy []string
			for _, h := range p.ForceNodes {
				for _, n := range st.View.Nodes {
					if n.Hostname == h {
						if free(n, victims) {
							out = append(out, n)
						} else {
							busy = append(busy, h)
						}
					}
				}
			}
			if len(busy) > 0 {
				return nil, "forced nodes in use: " + strings.Join(busy, ", ")
			}
			return out, ""
		}
		var cands []scheduler.NodeStatus
		for _, n := range st.View.Nodes {
			if free(n, victims) {
				cands = append(cands, n)
			}
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Priority != cands[j].Priority {
				return cands[i].Priority < cands[j].Priority
			}
			return budget(cands[i], victims) > budget(cands[j], victims)
		})
		for _, n := range cands {
			if budget(n, victims) >= p.NeedsMiB {
				return []scheduler.NodeStatus{n}, ""
			}
		}
		for k := 2; k <= pool && k <= len(cands); k++ {
			share := float64(p.NeedsMiB) / float64(k) * 1.15
			ok := true
			for _, n := range cands[:k] {
				if float64(budget(n, victims)) < share {
					ok = false
				}
			}
			if ok {
				return cands[:k], ""
			}
		}
		return nil, fmt.Sprintf("placer: need %d MiB (requires %d MiB) does not fit the available nodes", p.NeedsMiB, p.NeedsMiB)
	}
	placed := func(ns []scheduler.NodeStatus, victims map[string]bool) []scheduler.PlacedNodeStatus {
		var out []scheduler.PlacedNodeStatus
		for _, n := range ns {
			out = append(out, scheduler.PlacedNodeStatus{ID: n.ID, Addr: n.Addr, Local: n.Local, BudgetMiB: budget(n, victims)})
		}
		return out
	}
	if ns, why := try(nil); ns != nil {
		r.Action, r.Placement = scheduler.PlanStart, placed(ns, nil)
		return r
	} else {
		victims := map[string]bool{}
		for _, in := range st.Instances {
			if in.State == "ready" {
				victims[in.Profile] = true
			}
		}
		if len(victims) > 0 {
			if ns, _ := try(victims); ns != nil {
				r.Action, r.Placement = scheduler.PlanEvict, placed(ns, victims)
				seen := map[string]bool{}
				for _, n := range ns {
					if n.Holder != "" && !seen[n.Holder] {
						seen[n.Holder] = true
						busy := false
						for _, in := range st.Instances {
							if in.Profile == n.Holder && in.InFlight > 0 {
								busy = true
							}
						}
						r.Victims = append(r.Victims, scheduler.PlanVictim{Profile: n.Holder, Busy: busy})
					}
				}
				return r
			}
		}
		r.Action, r.Reason = scheduler.PlanQueue, why
		if !st.View.Connected || st.View.Stale {
			r.Reason = "fleet view stale; " + why
		}
		return r
	}
}

func planJSON(rep scheduler.PlanReport) map[string]any {
	v := api.StatusJSON(rep).(map[string]any)
	v["model"] = v["profile"]
	delete(v, "profile")
	return v
}

func writeUnknownModel(w http.ResponseWriter, model string) {
	writeJSON(w, 404, map[string]any{"error": map[string]any{"message": fmt.Sprintf("unknown model: model %q not found", model),
		"type": "invalid_request_error", "param": nil, "code": "model_not_found", "reason": "unknown model"}})
}

func (s *scenarioState) handlePlan(w http.ResponseWriter, r *http.Request) bool {
	b := s.scripted()
	if b == nil {
		return false
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Model == "" {
		writeErr(w, 400, "body must be JSON with a \"model\" field", "bad request")
		return true
	}
	if _, ok := profileByID(req.Model); !ok {
		writeUnknownModel(w, req.Model)
		return true
	}
	b.finish()
	writeJSON(w, 200, planJSON(planFor(req.Model, b.st, b.now)))
	return true
}

func (s *scenarioState) handleProfiles(w http.ResponseWriter, r *http.Request) bool {
	out := s.profileEntries()
	if out == nil {
		return false
	}
	writeJSON(w, 200, map[string]any{"profiles": out})
	return true
}

// profileEntries is the scripted profile list (the entries of GET
// /api/profiles); nil in scenario live.
func (s *scenarioState) profileEntries() []map[string]any {
	b := s.scripted()
	if b == nil {
		return nil
	}
	b.finish()
	ps := currentProfiles() // the real configuration's (none when nothing is configured)
	out := []map[string]any{}
	for _, p := range ps {
		e := map[string]any{"id": p.ID, "kind": "llama", "model": p.Model, "ctx": p.Ctx, "parallel": max(p.Parallel, 1),
			"aliases": nonNil(p.Aliases), "force_nodes": nonNil(p.ForceNodes), "tensor_split": p.TensorSplit, "needs_mib_configured": p.NeedsMiB,
			"generated": api.IsGeneratedProfile(p.ID, p.Generated)}
		if p.TensorSplit == nil {
			e["tensor_split"] = []int{}
		}
		rep := planFor(p.ID, b.st, b.now)
		e["needs_mib"], e["needs_basis"], e["needs_measured"] = rep.NeedMiB, rep.NeedBasis, rep.Measured
		nodes := []string{}
		for _, pn := range rep.Placement {
			nodes = append(nodes, hostOf(string(pn.ID)))
		}
		plan := map[string]any{"action": string(rep.Action), "would_start": rep.Action == scheduler.PlanStart || rep.Action == scheduler.PlanServe,
			"nodes": nodes, "reason": rep.Reason, "instance": rep.Instance}
		if rep.Warning != "" {
			plan["warning"] = rep.Warning
		}
		e["plan"] = plan
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
