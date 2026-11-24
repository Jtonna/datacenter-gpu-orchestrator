package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Actions on a selection of zones (Zones-Fleet-Selected, Zones-Confirm):
//
//	POST /api/zone-actions/{action}   {nodes, comment, dry_run?}   action: pause, unpause, force-unload, drain
//
// Each action is its own route (scopes.go): pause and unpause need the
// zones permission, force-unload and drain the models permission.
//
// drain is "Wait for the runs, then unload": every model on the zones
// admits nothing new and unloads once its runs finish (eta_s from the
// profile's average run length); new requests queue until it loads again.
//
// With dry_run (body or ?dry_run=1) nothing is done: the answer says, per
// zone, what the action would do, which models would unload (and every
// zone each leaves, inside the selection or not) and how many runs would
// be cut. That is what the confirm dialog states, so the page and an MCP
// client read the same consequences. Without dry_run each zone is acted on
// as its own route does it (POST /api/nodes/{id}/pause, ...): one history
// event and one audit entry per zone; the answer has the consequences
// worked out before acting and a result per zone. A comment is required
// unless it is a dry run.

// Zone actions.
const (
	ZoneActionPause       = "pause"
	ZoneActionUnpause     = "unpause"
	ZoneActionForceUnload = "force-unload"
	ZoneActionDrain       = "drain"
)

// ZoneEffect is what an action does to one zone.
type ZoneEffect struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ZoneState string   `json:"zone_state"`
	Changes   bool     `json:"changes"` // false: the action leaves this zone as it is
	Effect    string   `json:"effect"`  // plain words for the dialog
	Models    []string `json:"models"`  // models that unload because of this zone
	RunsCut   int      `json:"runs_cut"`
}

// ModelEffect is one model the action unloads.
type ModelEffect struct {
	Profile  string   `json:"profile"`
	State    string   `json:"state"` // starting (its load is cancelled), ready, draining, stopping
	InFlight int      `json:"in_flight"`
	Host     string   `json:"host,omitempty"` // the zone that runs llama-server, when it is a zone
	Zones    []string `json:"zones"`          // every zone it leaves, by name
	Outside  []string `json:"outside"`        // of those, the zones not in the selection
	// EtaS (drain): seconds until its runs should end; absent when no run
	// of it has finished yet (unknown) or nothing is in flight.
	EtaS *float64 `json:"eta_s,omitempty"`
}

// ZoneActionPlan is the dry-run answer, and the consequences part of a real one.
type ZoneActionPlan struct {
	Action  string        `json:"action"`
	DryRun  bool          `json:"dry_run"`
	Zones   []ZoneEffect  `json:"zones"`
	Models  []ModelEffect `json:"models"`
	RunsCut int           `json:"runs_cut"`
	// RunsWait (drain): the runs waited for; EtaS the longest model's ETA.
	RunsWait int      `json:"runs_wait"`
	EtaS     *float64 `json:"eta_s,omitempty"`
	Summary  string   `json:"summary"`
	// NextLoad (drain): NextLoadMayMove, for the Confirm dialog.
	NextLoad string `json:"next_load,omitempty"`
}

// NextLoadMayMove is said with every drain: the load that serves the
// requests queued meanwhile is placed afresh once the model has stopped
// and may start on another zone (the drained one may still be releasing
// its memory; a pin is not kept).
const NextLoadMayMove = "The next load may start on another zone."

func (s *Server) zoneActionRoutes(m *http.ServeMux) {
	act := func(action string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { s.handleZoneAction(w, r, action) }
	}
	m.HandleFunc("POST /api/zone-actions/pause", act(ZoneActionPause))
	m.HandleFunc("POST /api/zone-actions/unpause", act(ZoneActionUnpause))
	m.HandleFunc("POST /api/zone-actions/force-unload", act(ZoneActionForceUnload))
	m.HandleFunc("POST /api/zone-actions/drain", act(ZoneActionDrain))
}

// planZoneAction works out the consequences of action on ids.
func planZoneAction(action string, ids []string, st *scheduler.Status, idx zoneIndex) ZoneActionPlan {
	if st == nil {
		st = &scheduler.Status{}
	}
	plan := ZoneActionPlan{Action: action, Zones: []ZoneEffect{}, Models: []ModelEffect{}}
	name := func(id string) string {
		if z := idx[id]; z != nil && z.name != "" {
			return z.name
		}
		return id
	}
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	touching := map[string][]int{} // zone -> indexes of the instances placed on it
	for i, in := range st.Instances {
		for _, n := range in.Nodes {
			touching[string(n.ID)] = append(touching[string(n.ID)], i)
		}
	}
	counted := map[int]bool{}
	for _, id := range ids {
		z := idx[id]
		e := ZoneEffect{ID: id, Name: name(id), Models: []string{}}
		paused := false
		if z != nil {
			if z.sample != nil {
				e.ZoneState = string(z.sample.State)
			}
			if z.fact != nil {
				paused = z.fact.Paused
			}
		}
		var here []string
		for _, i := range touching[id] {
			here = append(here, st.Instances[i].Profile)
		}
		switch action {
		case ZoneActionPause:
			e.Changes = !paused
			switch {
			case paused:
				e.Effect = "already takes no new model: nothing changes"
			case len(here) > 0:
				e.Effect = "takes no new model; " + strings.Join(here, ", ") + " keeps running"
			default:
				e.Effect = "takes no new model"
			}
		case ZoneActionUnpause:
			e.Changes = paused
			if paused {
				e.Effect = "takes models again"
			} else {
				e.Effect = "already takes models: nothing changes"
			}
		case ZoneActionForceUnload, ZoneActionDrain:
			drain := action == ZoneActionDrain
			var parts []string
			for _, i := range touching[id] {
				in := st.Instances[i]
				e.Models = append(e.Models, in.Profile)
				e.Changes = true
				switch {
				case counted[i]:
					parts = append(parts, in.Profile+" unloads (counted with another zone)")
					continue
				case drain && in.State == "starting":
					parts = append(parts, in.Profile+" is loading: a drain refuses it (force unload cancels the load)")
				case drain && in.State == "stopping":
					parts = append(parts, in.Profile+" is already stopping")
				case drain && in.InFlight > 0:
					parts = append(parts, fmt.Sprintf("%s unloads after its %s%s; new runs queue until it loads again",
						in.Profile, plural(in.InFlight, "run", "runs"), etaWords(in.RunsEtaS)))
				case drain:
					parts = append(parts, in.Profile+" unloads now; nothing in flight")
				case in.State == "starting":
					parts = append(parts, "the load of "+in.Profile+" is cancelled")
				case in.InFlight > 0:
					parts = append(parts, fmt.Sprintf("%s unloads; %s fail", in.Profile, plural(in.InFlight, "run", "runs")))
				default:
					parts = append(parts, in.Profile+" unloads; nothing in flight")
				}
				counted[i] = true
				if !drain {
					e.RunsCut += in.InFlight
				}
			}
			if len(parts) == 0 {
				parts = append(parts, "nothing loaded: nothing interrupted")
			}
			e.Effect = strings.Join(parts, "; ")
		}
		plan.RunsCut += e.RunsCut
		plan.Zones = append(plan.Zones, e)
	}
	if action == ZoneActionForceUnload || action == ZoneActionDrain {
		var order []int
		for i := range counted {
			order = append(order, i)
		}
		sort.Ints(order)
		for _, i := range order {
			in := st.Instances[i]
			m := ModelEffect{Profile: in.Profile, State: in.State, InFlight: in.InFlight, Zones: []string{}, Outside: []string{}}
			if action == ZoneActionDrain && in.State != "starting" {
				m.EtaS = in.RunsEtaS
				plan.RunsWait += in.InFlight
				if m.EtaS != nil && (plan.EtaS == nil || *m.EtaS > *plan.EtaS) {
					plan.EtaS = m.EtaS
				}
			}
			if in.HostNode != "" {
				m.Host = name(string(in.HostNode))
			}
			for _, n := range in.Nodes {
				m.Zones = append(m.Zones, name(string(n.ID)))
				if !selected[string(n.ID)] {
					m.Outside = append(m.Outside, name(string(n.ID)))
				}
			}
			plan.Models = append(plan.Models, m)
		}
	}
	plan.Summary = planSummary(plan)
	if action == ZoneActionDrain && len(plan.Models) > 0 {
		plan.NextLoad = NextLoadMayMove
		plan.Summary += " " + NextLoadMayMove
	}
	return plan
}

func planSummary(p ZoneActionPlan) string {
	n := 0
	for _, z := range p.Zones {
		if z.Changes {
			n++
		}
	}
	zones := plural(len(p.Zones), "zone", "zones")
	switch p.Action {
	case ZoneActionPause:
		return fmt.Sprintf("%d of %s stop taking new models; nothing running is interrupted.", n, zones)
	case ZoneActionUnpause:
		return fmt.Sprintf("%d of %s take models again.", n, zones)
	}
	if len(p.Models) == 0 {
		return fmt.Sprintf("Nothing is loaded on the %s: nothing is interrupted.", zones)
	}
	var outside []string
	for _, m := range p.Models {
		outside = append(outside, m.Outside...)
	}
	s := fmt.Sprintf("%s unload; %s fail straight away.", plural(len(p.Models), "model", "models"), plural(p.RunsCut, "run", "runs"))
	if p.Action == ZoneActionDrain {
		s = fmt.Sprintf("%s unload once their %s finish%s; nothing is cut and new runs queue until they load again.",
			plural(len(p.Models), "model", "models"), plural(p.RunsWait, "run", "runs"), etaWords(p.EtaS))
	}
	if len(outside) > 0 {
		s += " They also leave " + strings.Join(outside, ", ") + "."
	}
	return s
}

// etaWords is " (about 40 s)", " (about 3 min)", or "" when unknown.
func etaWords(eta *float64) string {
	if eta == nil {
		return ""
	}
	switch v := *eta; {
	case v < 1:
		return " (any moment now)"
	case v < 90:
		return fmt.Sprintf(" (about %.0f s)", v)
	default:
		return fmt.Sprintf(" (about %.0f min)", v/60)
	}
}

func (s *Server) handleZoneAction(w http.ResponseWriter, r *http.Request, action string) {
	if !s.agentsOn(w) {
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var b struct {
		Nodes   []string `json:"nodes"`
		Comment string   `json:"comment"`
		DryRun  bool     `json:"dry_run"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be a JSON object: %v", err)
			return
		}
	}
	if b.Comment = strings.TrimSpace(b.Comment); b.Comment == "" {
		b.Comment = audit.FromRequest(r).Comment
	}
	dry := b.DryRun || r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	if len(b.Nodes) == 0 {
		apiErr(w, http.StatusBadRequest, "nodes is required: the ids or names of the zones")
		return
	}
	if b.Nodes, ok = s.resolveZones(w, r, b.Nodes); !ok {
		return
	}
	if !dry && b.Comment == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	st, facts := s.zoneSnapshot(r.Context())
	plan := planZoneAction(action, b.Nodes, st, indexZones(st, facts))
	plan.DryRun = dry
	if dry {
		writeJSON(w, http.StatusOK, plan)
		return
	}
	results := []map[string]any{}
	drained := map[string]bool{}
	for _, id := range b.Nodes {
		res := map[string]any{"node": id}
		switch action {
		case ZoneActionPause, ZoneActionUnpause:
			aid, err := s.pauseZone(r, id, action == ZoneActionPause, b.Comment)
			if err != nil {
				res["error"] = err.Error()
			} else {
				res["audit_id"] = aid
			}
		case ZoneActionForceUnload:
			prof, aid, err := s.forceUnloadZone(r, id, b.Comment)
			if err != nil {
				res["error"] = err.Error()
			} else {
				res["stopped_profile"], res["audit_id"] = prof, aid
			}
		case ZoneActionDrain:
			var profs, errs []string
			for _, z := range plan.Zones {
				if z.ID != id {
					continue
				}
				for _, prof := range z.Models {
					if drained[prof] {
						continue
					}
					drained[prof] = true
					if _, err := s.sched.Drain(r.Context(), prof); err != nil && !errors.Is(err, scheduler.ErrNotLoaded) {
						errs = append(errs, err.Error())
						continue
					}
					profs = append(profs, prof)
				}
			}
			res["drained"] = append([]string{}, profs...)
			if len(errs) > 0 {
				res["error"] = strings.Join(errs, "; ")
			}
			if len(profs) > 0 {
				res["audit_id"] = s.auditNode(r, "drain", id, b.Comment, nil, map[string]any{"drained": profs})
			}
		}
		results = append(results, res)
	}
	s.groupsChanged()
	writeJSON(w, http.StatusOK, map[string]any{"plan": plan, "results": results})
}

// resolveZones turns a selection of zone ids or names into ids, as
// /api/load resolves its nodes (pool.Resolve: an id, the scheduler's own
// machine by its name, or the name of one live zone, any case). A known id
// is taken as is (a lost zone included). A name two zones share is
// refused; so is one Potluck does not know. The same zone named twice
// (say by name and by id) counts once, in the order first given.
func (s *Server) resolveZones(w http.ResponseWriter, r *http.Request, entries []string) ([]string, bool) {
	known := s.knownZones(r.Context())
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		id := e
		if !known[e] {
			rid, err := s.opts.Pool.Resolve(e)
			var amb *fleet.AmbiguousError
			switch {
			case errors.As(err, &amb):
				ids := make([]string, len(amb.IDs))
				for i, x := range amb.IDs {
					ids[i] = string(x)
				}
				apiErr(w, http.StatusBadRequest, "%s is the name of %d zones (%s): name the zone by its id", e, len(ids), strings.Join(ids, ", "))
				return nil, false
			case err != nil || !known[string(rid)]:
				apiErr(w, http.StatusBadRequest, "%s is not a zone Potluck knows", e)
				return nil, false
			}
			id = string(rid)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, true
}
