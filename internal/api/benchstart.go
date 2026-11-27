package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Run and Cancel from the Benchmarks pages (release v88).
//
//	POST /api/bench/benchmarks/{id}/run[?dry_run=1]
//	POST /api/bench/groups/{id}/run[?dry_run=1]   (and /suites/{id}/run)
//	POST /api/bench/runs/{id}/cancel
//
// Every start is decided on the expanded run before anything is queued:
// a benchmark or group named in bench_not_startable, a load on a zone that is
// excluded, a load on the scheduler's own machine, a profile whose model file
// is in deleted models, or a full queue refuses the whole run, naming the
// entry and the machine. The dry read (dry_run=1) answers what the run would
// do now: its steps, per load the profile, the machines, the memory there and
// whether it fits now, what is loaded now and by whom, what the run would
// unload now, the refusals and warnings. A start needs a comment, and, when
// the run would unload other people's models, the list it was shown
// ("evict"): when that list changed, the start is refused with the new one.

// runStartReq is the body of the run routes.
type runStartReq struct {
	Baseline     string            `json:"baseline"`
	Note         string            `json:"note"`
	EvictOthers  *bool             `json:"evict_others"`
	OnlyWhenIdle *bool             `json:"only_when_idle"`
	IdleWaitS    *int              `json:"idle_wait_s"`
	Inputs       *benchspec.Inputs `json:"inputs"`
	Comment      string            `json:"comment"`
	// Evict is the list of models of other people the run would unload, as
	// the dry read named it (profile ids).
	Evict []string `json:"evict"`
	// AgainOf is the run a "Run again" repeats: the dry read warns when a
	// definition changed since.
	AgainOf string `json:"again_of"`
}

const commentRequired = "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)"

// startPlan is the dry read's answer.
type startPlan struct {
	DryRun        bool                  `json:"dry_run"`
	Startable     bool                  `json:"startable"`
	Kind          string                `json:"kind"`
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	StepCount     int                   `json:"step_count"`
	Steps         []startStep           `json:"steps"`
	MaxRunMinutes int                   `json:"max_run_minutes"`
	WaitsForIdle  bool                  `json:"waits_for_idle"`
	EvictOthers   bool                  `json:"evict_others"`
	Loads         []startLoad           `json:"loads"`
	LoadedNow     []startLoaded         `json:"loaded_now"`
	WouldUnload   []startLoaded         `json:"would_unload"`
	Refusals      []*bench.StartRefusal `json:"refusals"`
	Warnings      []startWarning        `json:"warnings"`
	Queue         startQueue            `json:"queue"`
}

type startStep struct {
	BenchmarkID string `json:"benchmark_id"`
	Name        string `json:"name"`
	Profile     string `json:"profile"`
	ProfileName string `json:"profile_name"`
}

type startMachine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TakesMiB int    `json:"takes_mib,omitempty"`
}

type startLoad struct {
	Profile     string         `json:"profile"`
	ProfileName string         `json:"profile_name"`
	Model       string         `json:"model,omitempty"`
	ModelName   string         `json:"model_name,omitempty"`
	Steps       int            `json:"steps"`
	Machines    []startMachine `json:"machines"`
	NeedMiB     int            `json:"need_mib,omitempty"`
	NeedKnown   bool           `json:"need_known"`
	// Action is the scheduler's plan now: start (fits now), serve or wait
	// (loaded or loading already), evict or queue (waits: in use), reject
	// (would not fit), backoff.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Fits   string `json:"fits"` // fits, waits, no, unknown
}

type startLoaded struct {
	Profile     string   `json:"profile"`
	ProfileName string   `json:"profile_name"`
	Machines    []string `json:"machines"`
	State       string   `json:"state"`
	InFlight    int      `json:"in_flight"`
	IdleS       *int64   `json:"idle_s,omitempty"`
	LastUsedAt  int64    `json:"last_used_at,omitempty"`
	By          string   `json:"by,omitempty"`
	Why         string   `json:"why,omitempty"` // in would_unload: why the run unloads it
}

type startWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type startQueue struct {
	Queued  int    `json:"queued"`
	Max     int    `json:"max"`
	Running string `json:"running,omitempty"`
}

// BenchStartGate is the start gate of every run (bench.Manager.SetStartGate):
// the first refusal of the expanded run, or nil.
func (s *Server) BenchStartGate(g *bench.GateRun) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := s.planStart(ctx, g, nil)
	if len(p.Refusals) > 0 {
		return p.Refusals[0]
	}
	return nil
}

// wireBenchGate plugs BenchStartGate into the manager (once per manager).
func (s *Server) wireBenchGate() {
	if s.opts.Bench != nil {
		s.opts.Bench.SetStartGate(s.BenchStartGate)
		// the progress names the profile in flight as people see it
		s.opts.Bench.SetProfileNamer(func(id string) string {
			if p := s.conf().Profiles[id]; p != nil {
				return config.ShownName(p)
			}
			return ""
		})
	}
}

func dryRun(r *http.Request) bool {
	v := strings.ToLower(r.URL.Query().Get("dry_run"))
	return v == "1" || v == "true" || v == "yes"
}

// startRun is the run routes: kind is bench.RunKindBenchmark or RunKindSuite.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request, kind string) {
	if s.shuttingDown(w) {
		return
	}
	s.wireBenchGate()
	var req runStartReq
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := benchspec.DecodeStrict(body, &req); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be empty or {\"comment\": \"why\", \"note\": \"...\", \"baseline\": \"<run id>\", \"only_when_idle\": true, \"idle_wait_s\": 600, \"evict_others\": false, \"evict\": [\"<profile>\"], \"again_of\": \"<run id>\"%s}: %v",
				map[bool]string{true: ", \"inputs\": {\"model\": \"...\", \"ctx\": 8192, \"placement\": \"any:2\"}", false: ""}[kind == bench.RunKindBenchmark], err)
			return
		}
	}
	if kind != bench.RunKindBenchmark && req.Inputs != nil {
		apiErr(w, http.StatusBadRequest, "inputs are for a run of one benchmark; a group's entries carry their own")
		return
	}
	if req.Inputs != nil {
		// The inputs alone, checked as a recipe's would be.
		probe := benchspec.Benchmark{ID: "inputs", Inputs: req.Inputs, Phases: []benchspec.Phase{{Type: "load"}}}
		if err := probe.Validate(nil); err != nil {
			apiErr(w, http.StatusUnprocessableEntity, "%v", err)
			return
		}
	}
	dry := dryRun(r)
	comment := strings.TrimSpace(req.Comment)
	if comment == "" {
		comment = audit.FromRequest(r).Comment
	}
	if !dry && comment == "" {
		apiErr(w, http.StatusBadRequest, commentRequired)
		return
	}
	id := r.PathValue("id")
	bo := bench.BenchRunOptions{Baseline: req.Baseline, Note: req.Note, EvictOthers: req.EvictOthers, OnlyWhenIdle: req.OnlyWhenIdle, IdleWaitS: req.IdleWaitS, Inputs: req.Inputs}
	so := bench.EnqueueOptions{Baseline: req.Baseline, Note: req.Note, EvictOthers: req.EvictOthers, OnlyWhenIdle: req.OnlyWhenIdle, IdleWaitS: req.IdleWaitS}
	var g *bench.GateRun
	var err error
	if kind == bench.RunKindBenchmark {
		g, err = s.opts.Bench.PreviewBenchmark(id, bo)
	} else {
		g, err = s.opts.Bench.PreviewSuite(id, so)
	}
	if err != nil {
		if errors.Is(err, bench.ErrNotFound) {
			s.storeErr(w, err)
			return
		}
		ref := s.expandRefusal(r.Context(), err)
		if dry {
			writeJSON(w, http.StatusOK, &startPlan{DryRun: true, Kind: kind, ID: id, Refusals: []*bench.StartRefusal{ref},
				Steps: []startStep{}, Loads: []startLoad{}, LoadedNow: []startLoaded{}, WouldUnload: []startLoaded{}, Warnings: []startWarning{}})
			return
		}
		st := http.StatusConflict // not runnable (planned, blocked)
		if errors.Is(err, bench.ErrInvalid) {
			st = http.StatusUnprocessableEntity
		}
		writeRefusals(w, st, []*bench.StartRefusal{ref}, nil)
		return
	}
	plan := s.planStart(r.Context(), g, &req)
	plan.DryRun = dry
	if dry {
		writeJSON(w, http.StatusOK, plan)
		return
	}
	if len(plan.Refusals) > 0 {
		st := http.StatusConflict
		if plan.Refusals[0].Code == "queue_full" {
			w.Header().Set("Retry-After", "60")
			st = http.StatusTooManyRequests
		}
		writeRefusals(w, st, plan.Refusals, nil)
		return
	}
	// Nothing is unloaded that the person was not shown: the list sent must
	// be the list the run would unload now.
	var named []string
	for _, u := range plan.WouldUnload {
		named = append(named, u.Profile)
	}
	if !sameSet(named, req.Evict) {
		msg := "what this run would unload changed since it was shown: nothing was started; look at the new list and start again"
		if len(req.Evict) == 0 {
			msg = "this run would unload models of other people that were not named in the start (\"evict\"): nothing was started; name them to start"
		}
		writeRefusals(w, http.StatusConflict, []*bench.StartRefusal{{Code: "evict_set_changed", Msg: msg}}, map[string]any{"would_unload": plan.WouldUnload})
		return
	}
	start := bench.StartInfo{By: audit.FromRequest(r).Actor, Comment: comment, EvictLimited: plan.EvictOthers, EvictOnly: named}
	r.Header.Set("X-Comment", comment) // the audit entry carries it
	var run *bench.Run
	target := ""
	if kind == bench.RunKindBenchmark {
		bo.Start = start
		run, err = s.opts.Bench.EnqueueBenchmark(id, bo)
		target = "benchmark:" + id
	} else {
		so.Start = start
		run, err = s.opts.Bench.Enqueue(id, so)
		target = "suite:" + id
	}
	if err != nil {
		var ref *bench.StartRefusal
		switch {
		case errors.As(err, &ref):
			writeRefusals(w, http.StatusConflict, []*bench.StartRefusal{ref}, nil)
		case errors.Is(err, bench.ErrQueueFull):
			w.Header().Set("Retry-After", "60")
			writeRefusals(w, http.StatusTooManyRequests, []*bench.StartRefusal{queueFull()}, nil)
		default:
			s.storeErr(w, err)
		}
		return
	}
	w.Header().Set("Location", "/api/bench/runs/"+run.ID)
	after := map[string]any{"run_id": run.ID, "baseline": req.Baseline, "note": req.Note, "comment": comment, "steps": plan.StepCount,
		"only_when_idle": plan.WaitsForIdle, "evict_others": plan.EvictOthers, "may_unload": named}
	if req.Inputs != nil {
		after["inputs"] = req.Inputs
	}
	if req.AgainOf != "" {
		after["again_of"] = req.AgainOf
	}
	s.audit(r, "run_start", target, nil, after)
	writeJSON(w, http.StatusAccepted, run)
}

func queueFull() *bench.StartRefusal {
	return &bench.StartRefusal{Code: "queue_full", Msg: fmt.Sprintf("%d runs are queued already, the most the queue holds: wait until one finishes, or cancel one", bench.MaxQueuedRuns)}
}

// writeRefusals answers a refused start: the first refusal is the error, all
// of them are listed.
func writeRefusals(w http.ResponseWriter, status int, refs []*bench.StartRefusal, extra map[string]any) {
	out := map[string]any{"error": map[string]any{"message": refs[0].Msg, "type": errType(status), "param": nil, "code": status, "reason": refs[0].Code},
		"refusals": refs}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, status, out)
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// expandRefusal says in plain words why a run could not even be expanded: a
// profile that does not exist any more (its model file deleted, when the
// deleted models say so), a group that is planned or blocked, a broken group.
func (s *Server) expandRefusal(ctx context.Context, err error) *bench.StartRefusal {
	var mp *bench.MissingProfile
	if errors.As(err, &mp) {
		if s.store != nil {
			recs, _ := s.store.TrashList(ctx)
			for _, rec := range recs {
				if _, had := rec.Profiles[mp.Profile]; had {
					return &bench.StartRefusal{Code: "model_deleted", Entry: mp.Benchmark,
						Msg: fmt.Sprintf("%s uses the profile %s, which went with its model file %s when that was deleted on %s by %s; the file is in deleted models and can be restored there",
							mp.Benchmark, mp.Profile, rec.File, msText(rec.DeletedAt), orStr(rec.Actor, "someone"))}
				}
			}
		}
		return &bench.StartRefusal{Code: "profile_gone", Entry: mp.Benchmark, Msg: fmt.Sprintf("%s uses the profile %s, which does not exist any more", mp.Benchmark, mp.Profile)}
	}
	msg := err.Error()
	for _, p := range []string{"invalid: ", "not runnable: "} {
		msg = strings.TrimPrefix(msg, p)
	}
	code := "invalid"
	if errors.Is(err, bench.ErrNotRunnable) {
		code = "not_runnable"
	}
	return &bench.StartRefusal{Code: code, Msg: msg}
}

func orStr(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

// planStart judges an expanded run against the fleet now. req is nil for
// the gate (no warnings, no options beyond the run's own).
func (s *Server) planStart(ctx context.Context, g *bench.GateRun, req *runStartReq) *startPlan {
	cfg := s.conf()
	p := &startPlan{Kind: g.Kind, ID: g.ID, Name: g.Suite.Title(), StepCount: len(g.Leaves), MaxRunMinutes: cfg.Bench.MaxRunMinutes,
		WaitsForIdle: g.Suite.WaitsForIdle(), EvictOthers: g.Suite.EvictsOthers(),
		Steps: []startStep{}, Loads: []startLoad{}, LoadedNow: []startLoaded{}, WouldUnload: []startLoaded{}, Refusals: []*bench.StartRefusal{}, Warnings: []startWarning{}}
	if g.Kind == bench.RunKindSuite {
		if su, ok := s.groupLookup(g.ID); ok {
			p.Name = su.Title()
		}
	}
	queued, running := s.opts.Bench.QueueState()
	p.Queue = startQueue{Queued: queued, Max: bench.MaxQueuedRuns, Running: running}

	st, _ := s.sched.Status(ctx)
	nodeName, local := map[string]string{}, map[string]bool{}
	for _, n := range st.View.Nodes {
		nodeName[string(n.ID)] = orStr(n.Hostname, string(n.ID))
		local[string(n.ID)] = n.Local
	}
	name := func(id core.NodeID) string { return orStr(nodeName[string(id)], string(id)) }

	// 1. bench_not_startable: the run itself, a group within it, any entry
	off := map[string]bool{}
	for _, id := range cfg.Bench.NotStartable {
		off[strings.TrimSpace(id)] = true
	}
	if len(off) > 0 {
		what := "benchmark"
		if g.Kind == bench.RunKindSuite {
			what = "group"
		}
		if off[g.ID] {
			p.Refusals = append(p.Refusals, &bench.StartRefusal{Code: "not_startable", Entry: g.ID,
				Msg: fmt.Sprintf("the %s %s is listed in bench_not_startable (Settings)", what, g.ID)})
		}
		if g.Kind == bench.RunKindSuite {
			for _, gid := range s.groupsWithin(g.ID) {
				if off[gid] {
					p.Refusals = append(p.Refusals, &bench.StartRefusal{Code: "not_startable", Entry: gid,
						Msg: fmt.Sprintf("the group %s holds the group %s, which is listed in bench_not_startable (Settings); nothing of it was started", g.ID, gid)})
				}
			}
		}
		seen := map[string]bool{}
		for _, b := range g.Leaves {
			for _, id := range []string{b.ID, b.Recipe} {
				if id != "" && id != g.ID && off[id] && !seen[id] {
					seen[id] = true
					p.Refusals = append(p.Refusals, &bench.StartRefusal{Code: "not_startable", Entry: b.Title(),
						Msg: fmt.Sprintf("%s holds %s, which is listed in bench_not_startable (Settings); nothing of it was started", g.ID, b.Title())})
				}
			}
		}
	}

	// 2. the loads: one per profile, planned on the fleet as it is now
	type loadAt struct {
		l   *startLoad
		rep scheduler.PlanReport
	}
	byProfile := map[string]*loadAt{}
	var order []string
	for _, b := range g.Leaves {
		prof, cand := s.leafProfile(cfg, b)
		p.Steps = append(p.Steps, startStep{BenchmarkID: b.ID, Name: b.Title(), Profile: b.Profile, ProfileName: shownOr(prof, b.Profile)})
		if la := byProfile[b.Profile]; la != nil {
			la.l.Steps++
			s.judgeLeaf(p, b, la.rep, name, local)
			continue
		}
		l := &startLoad{Profile: b.Profile, ProfileName: shownOr(prof, b.Profile), Steps: 1, Machines: []startMachine{}}
		if prof != nil {
			l.Model = prof.Model
			l.ModelName = s.modelName(prof.Model)
		}
		// a model file in deleted models (a recipe's input, or a profile
		// whose file was deleted under it)
		if l.Model != "" && s.store != nil {
			if rec := s.trashedRecordOf(ctx, l.Model); rec != nil {
				p.Refusals = append(p.Refusals, &bench.StartRefusal{Code: "model_deleted", Entry: b.Title(),
					Msg: fmt.Sprintf("%s loads the model file %s, which was deleted on %s by %s; it is in deleted models and can be restored there", b.Title(), l.Model, msText(rec.DeletedAt), orStr(rec.Actor, "someone"))})
			}
		}
		var rep scheduler.PlanReport
		var err error
		switch {
		case prof == nil:
			err = fmt.Errorf("the profile %s does not exist", b.Profile)
		case cand:
			var cps []scheduler.CandidatePlan
			cps, err = s.sched.PlanCandidates(ctx, []*core.Profile{prof})
			if err == nil && len(cps) == 1 {
				rep = cps[0].Report
			}
		default:
			rep, err = s.sched.Plan(ctx, b.Profile)
		}
		if err != nil {
			l.Action, l.Fits, l.Reason = "unknown", "unknown", err.Error()
		} else {
			l.Action, l.Reason = string(rep.Action), rep.Reason
			l.NeedMiB, l.NeedKnown = rep.FitNeedMiB, rep.FitNeedMiB > 0
			l.Machines = machinesOf(rep, name)
			switch rep.Action {
			case scheduler.PlanStart, scheduler.PlanServe, scheduler.PlanWait:
				l.Fits = "fits"
			case scheduler.PlanEvict, scheduler.PlanQueue:
				l.Fits = "waits"
			case scheduler.PlanReject, scheduler.PlanBackoff:
				l.Fits = "no"
			default:
				l.Fits = "unknown"
			}
			if !l.NeedKnown && l.Fits != "no" {
				l.Fits = "unknown"
			}
		}
		byProfile[b.Profile] = &loadAt{l: l, rep: rep}
		order = append(order, b.Profile)
		s.judgeLeaf(p, b, rep, name, local)
	}
	for _, id := range order {
		p.Loads = append(p.Loads, *byProfile[id].l)
	}

	// 3. what is loaded now, by whom; what the run would unload now
	ours := map[string]bool{}
	for _, id := range order {
		ours[id] = true
	}
	now := time.Now()
	mayUnload := !p.WaitsForIdle || p.EvictOthers
	// A model of someone else may be unloaded for this run when it is a
	// victim of a load's plan now, or when it is idle on a machine a load of
	// the run needs: the scheduler evicts it once its hold is over, which a
	// plan made now does not show yet.
	victims := map[string]bool{}
	needs := map[core.NodeID]bool{}
	for _, id := range order {
		for _, v := range byProfile[id].rep.Victims {
			if !v.Busy {
				victims[v.Profile] = true
			}
		}
		for _, n := range nodesOf(byProfile[id].rep) {
			needs[n] = true
		}
	}
	onNeeded := func(in scheduler.InstanceStatus) string {
		for _, n := range in.Nodes {
			if needs[n.ID] {
				return name(n.ID)
			}
		}
		return ""
	}
	for _, in := range st.Instances {
		ld := startLoaded{Profile: in.Profile, ProfileName: in.Profile, State: in.State, InFlight: in.InFlight + in.Waiters, Machines: []string{}}
		if pp, _, ok := cfg.Resolve(in.Profile); ok {
			ld.ProfileName = config.ShownName(pp)
		}
		for _, n := range in.Nodes {
			ld.Machines = append(ld.Machines, name(n.ID))
		}
		if !in.LastUsed.IsZero() {
			ld.LastUsedAt = in.LastUsed.UnixMilli()
			if in.InFlight == 0 {
				idle := int64(now.Sub(in.LastUsed).Seconds())
				ld.IdleS = &idle
			}
		} else if !in.ReadySince.IsZero() && in.InFlight == 0 {
			idle := int64(now.Sub(in.ReadySince).Seconds())
			ld.IdleS = &idle
		}
		ld.By = s.loadedBy(in.Profile)
		p.LoadedNow = append(p.LoadedNow, ld)
		if !mayUnload || in.State != "ready" {
			continue
		}
		if in.InFlight > 0 || in.Waiters > 0 {
			// answering a request: never cut. A run that does not wait
			// until idle leaves it to the scheduler, which unloads it when
			// the request ends if a load of the run needs its machines.
			if !p.WaitsForIdle && !ours[in.Profile] && onNeeded(in) != "" {
				u := ld
				u.Why = "it is answering a request on " + onNeeded(in) + ", which a load of this run needs: it is unloaded when the request ends"
				p.WouldUnload = append(p.WouldUnload, u)
			}
			continue
		}
		switch {
		case ours[in.Profile]:
			u := ld
			u.Why = "this run loads it itself, so it is unloaded first and measured from a fresh load"
			p.WouldUnload = append(p.WouldUnload, u)
		case victims[in.Profile]:
			u := ld
			u.Why = "it is idle and takes the memory a load of this run needs"
			p.WouldUnload = append(p.WouldUnload, u)
		case onNeeded(in) != "":
			u := ld
			u.Why = "it is idle on " + onNeeded(in) + ", which a load of this run needs"
			p.WouldUnload = append(p.WouldUnload, u)
		}
	}

	// 4. the queue
	if queued >= bench.MaxQueuedRuns {
		p.Refusals = append(p.Refusals, queueFull())
	}

	// 5. Run again: a definition that changed since that run is a warning
	if req != nil && req.AgainOf != "" {
		p.Warnings = append(p.Warnings, s.againWarnings(req.AgainOf, g)...)
	}
	p.Startable = len(p.Refusals) == 0
	return p
}

// judgeLeaf adds the refusals of one entry's load: a zone that is excluded,
// or the scheduler's own machine.
func (s *Server) judgeLeaf(p *startPlan, b *benchspec.Benchmark, rep scheduler.PlanReport, name func(core.NodeID) string, local map[string]bool) {
	for _, r := range p.Refusals {
		if r.Entry == b.Title() && (r.Code == "excluded_zone" || r.Code == "scheduler_gpu") {
			return // said once per entry
		}
	}
	if rep.ExcludedBy != "" {
		p.Refusals = append(p.Refusals, &bench.StartRefusal{Code: "excluded_zone", Entry: b.Title(), Machine: rep.ExcludedBy,
			Msg: fmt.Sprintf("%s would load %s on %s, which is excluded as a zone; nothing of the run was started", b.Title(), b.Profile, rep.ExcludedBy)})
		return
	}
	if s.opts.benchLocalOK {
		return
	}
	for _, id := range nodesOf(rep) {
		if local[string(id)] {
			p.Refusals = append(p.Refusals, &bench.StartRefusal{Code: "scheduler_gpu", Entry: b.Title(), Machine: name(id),
				Msg: fmt.Sprintf("%s would load %s on %s, the scheduler's own machine; benchmarks never load there, so nothing of the run was started", b.Title(), b.Profile, name(id))})
			return
		}
	}
}

// nodesOf are the nodes a plan places on: the placement, else the nodes its
// memory rows are for (a queue or a reject).
func nodesOf(rep scheduler.PlanReport) []core.NodeID {
	var out []core.NodeID
	for _, n := range rep.Placement {
		out = append(out, n.ID)
	}
	if len(out) == 0 && rep.Memory != nil {
		for _, n := range rep.Memory.Nodes {
			out = append(out, n.ID)
		}
	}
	if len(out) == 0 && rep.HostNode != "" {
		out = append(out, rep.HostNode)
	}
	return out
}

func machinesOf(rep scheduler.PlanReport, name func(core.NodeID) string) []startMachine {
	takes := map[core.NodeID]int{}
	if rep.Memory != nil {
		for _, n := range rep.Memory.Nodes {
			takes[n.ID] = n.WillTakeMiB
		}
	}
	out := []startMachine{}
	for _, id := range nodesOf(rep) {
		out = append(out, startMachine{ID: string(id), Name: name(id), TakesMiB: takes[id]})
	}
	return out
}

func shownOr(p *core.Profile, id string) string {
	if p == nil {
		return id
	}
	return config.ShownName(p)
}

// leafProfile is the profile an entry loads: the configured one, or for the
// use of a recipe whose profile is not made yet, the profile its inputs
// would make (cand).
func (s *Server) leafProfile(cfg *config.Config, b *benchspec.Benchmark) (p *core.Profile, cand bool) {
	if pp, _, ok := cfg.Resolve(b.Profile); ok {
		return pp, false
	}
	if b.Applied == nil {
		return nil, false
	}
	in := b.Applied
	p = &core.Profile{Name: b.Profile, Kind: core.KindLlama, Model: in.Model, Ctx: in.Ctx, Parallel: 1, KVQuant: orStr(in.KVQuant, "f16"), Stage: config.StageTest}
	if in.FlashAttn != nil {
		p.FlashAttn = *in.FlashAttn
	}
	if in.Thinking != nil && !*in.Thinking {
		p.ExtraArgs = []string{"--reasoning-budget", "0"}
	}
	return p, true
}

// groupsWithin are the groups a group holds, at any depth.
// notStartableWhy is why the lists show a group or benchmark as "Not
// startable here" (the setting bench_not_startable names it, a group within
// it, or an entry of its expanded run; flat is nil for a benchmark), or "".
// The start gate decides the same on the run itself (planStart).
func (s *Server) notStartableWhy(id string, flat *benchspec.Suite) string {
	off := map[string]bool{}
	for _, x := range s.conf().Bench.NotStartable {
		off[strings.TrimSpace(x)] = true
	}
	if len(off) == 0 {
		return ""
	}
	if off[id] {
		return "listed in bench_not_startable (Settings)"
	}
	if flat == nil {
		return ""
	}
	for _, gid := range s.groupsWithin(id) {
		if off[gid] {
			return fmt.Sprintf("it holds the group %s, which is listed in bench_not_startable (Settings)", gid)
		}
	}
	for _, it := range flat.Benchmarks {
		ids := []string{it.Ref}
		if it.Inline != nil {
			ids = append(ids, it.Inline.ID, it.Inline.Recipe)
		}
		for _, x := range ids {
			if x != "" && off[x] {
				return fmt.Sprintf("it holds %s, which is listed in bench_not_startable (Settings)", x)
			}
		}
	}
	return ""
}

func (s *Server) groupsWithin(id string) []string {
	var out []string
	seen := map[string]bool{id: true}
	var walk func(gid string, depth int)
	walk = func(gid string, depth int) {
		su, ok := s.groupLookup(gid)
		if !ok || depth > benchspec.MaxGroupDepth {
			return
		}
		for _, it := range su.Benchmarks {
			if it.Group != "" && !seen[it.Group] {
				seen[it.Group] = true
				out = append(out, it.Group)
				walk(it.Group, depth+1)
			}
		}
	}
	walk(id, 0)
	return out
}

// loadedBy says who uses a loaded model: the caller of its newest request
// ("benchmark run <id>" for a run's).
func (s *Server) loadedBy(profile string) string {
	if s.reqs == nil {
		return ""
	}
	pg, err := s.reqs.List(usage.Filter{Profile: profile, Limit: 1})
	if err != nil || len(pg.Requests) == 0 {
		return ""
	}
	rq := pg.Requests[0]
	if rq.BenchRun != "" {
		return "benchmark run " + rq.BenchRun
	}
	return rq.Caller
}

// againWarnings compares the definitions a run ran with the ones now.
func (s *Server) againWarnings(runID string, g *bench.GateRun) []startWarning {
	old, err := s.opts.BenchStore.GetRun(runID)
	if err != nil {
		return []startWarning{{Code: "again_unknown", Message: fmt.Sprintf("run %s was not found: nothing to compare with", runID)}}
	}
	was := bench.PlanBenchmarks(old)
	if was == nil {
		return []startWarning{{Code: "again_unknown", Message: fmt.Sprintf("run %s does not keep its definitions (an older run): what changed since cannot be told", runID)}}
	}
	var changed, added []string
	now := map[string]bool{}
	for _, b := range g.Leaves {
		now[b.ID] = true
		w := was[b.ID]
		if w == nil {
			added = append(added, b.Title())
			continue
		}
		x, _ := json.Marshal(w)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			changed = append(changed, b.Title())
		}
	}
	var gone []string
	for id, b := range was {
		if !now[id] && !b.IsParallel() {
			gone = append(gone, b.Title())
		}
	}
	sort.Strings(gone)
	var out []startWarning
	if len(changed) > 0 {
		out = append(out, startWarning{Code: "definition_changed", Message: fmt.Sprintf("changed since run %s: %s. This run uses the definitions as they are now", runID, strings.Join(changed, ", "))})
	}
	if len(added) > 0 {
		out = append(out, startWarning{Code: "definition_changed", Message: fmt.Sprintf("new since run %s: %s", runID, strings.Join(added, ", "))})
	}
	if len(gone) > 0 {
		out = append(out, startWarning{Code: "definition_changed", Message: fmt.Sprintf("in run %s but not any more: %s", runID, strings.Join(gone, ", "))})
	}
	return out
}
