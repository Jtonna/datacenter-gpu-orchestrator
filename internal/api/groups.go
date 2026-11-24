package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/groups"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Zone groups (Zones-Fleet-Group, Zones-Fleet-Selected, Zones-Create): a
// named set of zones, each zone in at most one group. A group is a label
// and a way to act on many zones at once; it never changes placement.
//
//	GET    /api/groups                      every group with its figures, and the zones in none
//	POST   /api/groups                      {name, members?, comment?}: create (members move into it)
//	PATCH  /api/groups/{name}               {name, comment?}: rename
//	DELETE /api/groups/{name}               {comment?}: delete (its zones are then in no group)
//	POST   /api/groups/{name}/members       {nodes, comment?}: add or move zones into the group
//	DELETE /api/groups/{name}/members       {nodes, comment?}: take zones out of the group
//	POST   /api/groups/{name}/pause         {comment}: "Accepting models" off: pause every member
//	POST   /api/groups/{name}/unpause       {comment}: "Accepting models" on: unpause every member
//
// Every change is an audit entry (section groups, target group:<name>),
// signed by the request's actor (the default user unless an agent key or
// X-Actor names another). Pause and unpause are one node entry per member,
// as on one zone. The /ws topic groups sends the document of GET
// /api/groups when a client subscribes and again only when a group changes.

// SectionGroups is the audit section of the group changes.
const SectionGroups = "groups"

// TopicGroups is the /ws topic of the groups, asked for by name.
const TopicGroups = "groups"

// Accepting: whether a group's members take new models.
const (
	AcceptingAll   = "all"   // no member is paused
	AcceptingNone  = "none"  // every member is paused
	AcceptingMixed = "mixed" // some are
)

// ZoneCounts are the zones by state, as the health Zones row counts them
// (availability.Classify; removed zones are left out; counts.go): down is
// lost plus set aside; reachable is every zone that is not down,
// switched-off ones included; up is free plus in use plus loading (that
// is, reachable less off).
type ZoneCounts struct {
	Total     int `json:"total"`
	Reachable int `json:"reachable"`
	Up        int `json:"up"`
	Down      int `json:"down"`
	Lost      int `json:"lost"`
	SetAside  int `json:"set_aside"`
	Off       int `json:"off"`
	InUse     int `json:"in_use"`
	Free      int `json:"free"`
	Loading   int `json:"loading"`
}

func (c *ZoneCounts) add(state availability.State) {
	c.Total++
	switch state {
	case availability.Lost:
		c.Lost++
		c.Down++
	case availability.SetAside:
		c.SetAside++
		c.Down++
	case availability.Off:
		c.Off++
	case availability.InUse:
		c.InUse++
	case availability.Loading:
		c.Loading++
	default:
		c.Free++
	}
	c.Reachable = c.Total - c.Down
	c.Up = c.Free + c.InUse + c.Loading
}

// GroupMemory sums the members' memory as the scheduler's view has it.
// EachTotalMiB is set when every member with a figure has the same total
// ("13.5 GB each").
type GroupMemory struct {
	UsedMiB      int64  `json:"used_mib"`
	TotalMiB     int64  `json:"total_mib"`
	EachTotalMiB *int64 `json:"each_total_mib,omitempty"`
}

// GroupTemp is the members' headline temperatures (stats): the average and
// the hottest; absent when no member reports one.
type GroupTemp struct {
	AvgC        *float64 `json:"avg_c,omitempty"`
	HottestC    *float64 `json:"hottest_c,omitempty"`
	HottestID   string   `json:"hottest_id,omitempty"`
	HottestName string   `json:"hottest_name,omitempty"`
}

// ZoneRef is a member that needs attention: down (lost or set aside).
type ZoneRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// GroupModel is a model placed on members of the group, and on how many.
type GroupModel struct {
	Profile string `json:"profile"`
	Zones   int    `json:"zones"`
}

// GroupSummary is one group as GET /api/groups answers it.
type GroupSummary struct {
	Name        string     `json:"name,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	Members     []string   `json:"members"`
	MemberNames []string   `json:"member_names"`
	Count       int        `json:"count"`
	// Chip is the members' common GPU name ("mixed" when they differ or
	// some report none; absent when none reports one).
	Chip           string       `json:"chip,omitempty"`
	Counts         ZoneCounts   `json:"counts"`
	Memory         GroupMemory  `json:"memory"`
	Temp           GroupTemp    `json:"temp"`
	Accepting      string       `json:"accepting,omitempty"`
	NeedsAttention []ZoneRef    `json:"needs_attention"`
	Models         []GroupModel `json:"models"`
	// Uptime7d is the members' uptime over the last 7 days (uptime.go;
	// the document's uptime_window says what it covers). Absent without a
	// usage store.
	Uptime7d *availability.Totals `json:"uptime_7d,omitempty"`
}

// zoneInfo is what the group figures read of one zone.
type zoneInfo struct {
	name   string
	sample *availability.Sample
	fact   *pool.NodeFacts
	view   *scheduler.NodeStatus
	model  string // the profile of the instance placed on it
}

type zoneIndex map[string]*zoneInfo

// indexZones joins the scheduler's status and the pool's facts per zone.
func indexZones(st *scheduler.Status, facts []pool.NodeFacts) zoneIndex {
	idx := zoneIndex{}
	get := func(id string) *zoneInfo {
		z := idx[id]
		if z == nil {
			z = &zoneInfo{name: id}
			idx[id] = z
		}
		return z
	}
	if st == nil {
		st = &scheduler.Status{}
	}
	for _, smp := range availability.Classify(st, facts) {
		smp := smp
		z := get(smp.Node)
		z.sample, z.name = &smp, smp.Name
	}
	for i := range facts {
		get(facts[i].ID).fact = &facts[i]
	}
	for i := range st.View.Nodes {
		get(string(st.View.Nodes[i].ID)).view = &st.View.Nodes[i]
	}
	for _, in := range st.Instances {
		for _, m := range in.Nodes {
			if z := idx[string(m.ID)]; z != nil {
				z.model = in.Profile
			}
		}
	}
	return idx
}

func headlineTemp(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var s struct {
		T *float64 `json:"headline_temp_c"`
	}
	if json.Unmarshal(raw, &s) != nil || s.T == nil {
		return 0, false
	}
	return *s.T, true
}

// summarize works out a group's figures from the zone index.
func summarizeGroup(g groups.Group, idx zoneIndex) GroupSummary {
	out := GroupSummary{Name: g.Name, Members: append([]string{}, g.Members...), MemberNames: []string{}, Count: len(g.Members),
		NeedsAttention: []ZoneRef{}, Models: []GroupModel{}}
	if !g.CreatedAt.IsZero() {
		t := g.CreatedAt.UTC()
		out.CreatedAt = &t
	}
	var tempSum float64
	var temps, paused, unpaused int
	var each int64 = -1
	models := map[string]int{}
	var chips []string
	for _, id := range g.Members {
		z := idx[id]
		if z == nil {
			out.MemberNames = append(out.MemberNames, id)
			continue
		}
		out.MemberNames = append(out.MemberNames, z.name)
		if smp := z.sample; smp != nil && !smp.Removed {
			out.Counts.add(smp.State)
			if smp.State == availability.Lost || smp.State == availability.SetAside {
				out.NeedsAttention = append(out.NeedsAttention, ZoneRef{ID: id, Name: z.name, State: string(smp.State)})
			}
		}
		if v := z.view; v != nil && v.TotalMiB > 0 {
			out.Memory.TotalMiB += int64(v.TotalMiB)
			out.Memory.UsedMiB += int64(max(v.TotalMiB-v.FreeMiB, 0))
			switch {
			case each == -1:
				each = int64(v.TotalMiB)
			case each != int64(v.TotalMiB):
				each = -2
			}
		}
		if f := z.fact; f != nil {
			chips = append(chips, chipOf(f.Stats))
			if t, ok := headlineTemp(f.Stats); ok {
				tempSum += t
				temps++
				if out.Temp.HottestC == nil || t > *out.Temp.HottestC {
					t := t
					out.Temp.HottestC, out.Temp.HottestID, out.Temp.HottestName = &t, id, z.name
				}
			}
			if f.State != pool.StateRemoved {
				if f.Paused {
					paused++
				} else {
					unpaused++
				}
			}
		}
		if z.model != "" {
			models[z.model]++
		}
	}
	if each > 0 {
		out.Memory.EachTotalMiB = &each
	}
	out.Chip = groupChip(chips)
	if temps > 0 {
		avg := float64(int(tempSum/float64(temps)*10+0.5)) / 10
		out.Temp.AvgC = &avg
	}
	switch {
	case paused == 0 && unpaused > 0:
		out.Accepting = AcceptingAll
	case unpaused == 0 && paused > 0:
		out.Accepting = AcceptingNone
	case paused > 0:
		out.Accepting = AcceptingMixed
	}
	sort.SliceStable(out.NeedsAttention, func(i, j int) bool { // the attention order: lost before set aside, then by name
		a, b := out.NeedsAttention[i], out.NeedsAttention[j]
		if a.State != b.State {
			return a.State == string(availability.Lost)
		}
		return a.Name < b.Name
	})
	for p, n := range models {
		out.Models = append(out.Models, GroupModel{Profile: p, Zones: n})
	}
	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Zones != out.Models[j].Zones {
			return out.Models[i].Zones > out.Models[j].Zones
		}
		return out.Models[i].Profile < out.Models[j].Profile
	})
	return out
}

func (s *Server) groupRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/groups", s.handleGroups)
	m.HandleFunc("POST /api/groups", s.groupCreate)
	m.HandleFunc("PATCH /api/groups/{name}", s.groupRename)
	m.HandleFunc("DELETE /api/groups/{name}", s.groupDelete)
	m.HandleFunc("POST /api/groups/{name}/members", s.groupAdd)
	m.HandleFunc("DELETE /api/groups/{name}/members", s.groupRemove)
	m.HandleFunc("POST /api/groups/{name}/pause", func(w http.ResponseWriter, r *http.Request) { s.groupPause(w, r, true) })
	m.HandleFunc("POST /api/groups/{name}/unpause", func(w http.ResponseWriter, r *http.Request) { s.groupPause(w, r, false) })
}

// zoneFactsAll is the agent facts and the scheduler's own machine, as the
// node feed lists them, with the status they were read with.
func (s *Server) zoneSnapshot(ctx context.Context) (*scheduler.Status, []pool.NodeFacts) {
	var stp *scheduler.Status
	c, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if st, err := s.sched.Status(c); err == nil {
		stp = &st
	}
	var facts []pool.NodeFacts
	if s.opts.Pool != nil {
		facts = s.agentFacts(stp)
	}
	return stp, facts
}

// groupsDoc is the document of GET /api/groups and of the groups topic.
func (s *Server) groupsDoc(ctx context.Context) map[string]any {
	st, facts := s.zoneSnapshot(ctx)
	idx := indexZones(st, facts)
	rev := s.groups.Revision()
	list := s.groups.List()
	out := []GroupSummary{}
	grouped := map[string]bool{}
	for _, g := range list {
		out = append(out, summarizeGroup(g, idx))
		for _, m := range g.Members {
			grouped[m] = true
		}
	}
	var rest []string
	for id, z := range idx {
		if !grouped[id] && (z.sample == nil || !z.sample.Removed) {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	ungrouped := summarizeGroup(groups.Group{Members: rest}, idx)
	doc := map[string]any{"revision": rev, "groups": out, "ungrouped": ungrouped}
	if s.opts.Usage.On() {
		up := s.uptimeDoc(ctx, uptimeDefaultDays)
		for i := range out {
			for _, g := range up.Groups {
				if g.Name == out[i].Name {
					t := g.Totals
					out[i].Uptime7d = &t
				}
			}
		}
		ungrouped.Uptime7d = &up.Ungrouped
		doc["ungrouped"] = ungrouped
		doc["uptime_window"] = up.Window
	}
	return doc
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.groupsDoc(r.Context()))
}

// groupsChanged sends the document to the groups topic's subscribers.
func (s *Server) groupsChanged() {
	if s.ws == nil || !s.ws.wantsAny(TopicGroups) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.ws.broadcast(TopicGroups, wsFrame(TopicGroups, s.ws.now(), s.groupsDoc(ctx)))
}

// sendGroups queues the document for one client that just subscribed.
func (s *Server) sendGroups(c *wsClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.ws.enqueue(c, wsFrame(TopicGroups, s.ws.now(), s.groupsDoc(ctx)))
}

func groupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, groups.ErrBadName):
		apiErr(w, http.StatusBadRequest, "%v", err)
	case errors.Is(err, groups.ErrExists):
		apiErr(w, http.StatusConflict, "%v", err)
	case errors.Is(err, groups.ErrNotFound):
		apiErr(w, http.StatusNotFound, "%v", err)
	default:
		apiErr(w, http.StatusInternalServerError, "%v", err)
	}
}

func (s *Server) auditGroup(r *http.Request, action, name, comment string, before, after any) int64 {
	if s.opts.Audit == nil {
		return 0
	}
	e := audit.Entry{Action: action, Section: SectionGroups, Target: "group:" + name, Before: raw(before), After: raw(after),
		TS: time.Now().UnixMilli(), Comment: comment}
	audit.FromRequest(r).Apply(&e)
	id, err := s.opts.Audit.Record(r.Context(), e)
	if err != nil {
		s.log.Warn("audit entry not recorded (the change itself was made)", "action", action, "group", name, "err", err)
		return 0
	}
	s.announce(r, SectionGroups, "group:"+name, id)
	return id
}

// knownZones is the ids a group may hold: every node the pool lists and
// the scheduler's own machine.
func (s *Server) knownZones(ctx context.Context) map[string]bool {
	st, facts := s.zoneSnapshot(ctx)
	out := map[string]bool{}
	for _, f := range facts {
		out[f.ID] = true
	}
	if st != nil {
		for _, v := range st.View.Nodes {
			out[string(v.ID)] = true
		}
	}
	if s.opts.Pool != nil {
		if l, ok := s.opts.Pool.Local(); ok {
			out[l.ID] = true
		}
	}
	return out
}

// groupBody reads {name?, members? | nodes?, comment?}.
type groupBody struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
	Nodes   []string `json:"nodes"`
	Comment string   `json:"comment"`
}

func readGroupBody(w http.ResponseWriter, r *http.Request) (groupBody, bool) {
	var b groupBody
	raw, ok := readBody(w, r)
	if !ok {
		return b, false
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be a JSON object: %v", err)
			return b, false
		}
	}
	b.Comment = strings.TrimSpace(b.Comment)
	if b.Comment == "" {
		b.Comment = audit.FromRequest(r).Comment
	}
	return b, true
}

func (s *Server) checkZones(w http.ResponseWriter, r *http.Request, ids []string) bool {
	known := s.knownZones(r.Context())
	for _, id := range ids {
		if !known[id] {
			apiErr(w, http.StatusBadRequest, "%s is not a zone Potluck knows", id)
			return false
		}
	}
	return true
}

func (s *Server) groupCreate(w http.ResponseWriter, r *http.Request) {
	b, ok := readGroupBody(w, r)
	if !ok || !s.checkZones(w, r, b.Members) {
		return
	}
	moved, err := s.groups.Create(b.Name, b.Members)
	if err != nil {
		groupErr(w, err)
		return
	}
	aid := s.auditGroup(r, "create", b.Name, b.Comment, nil, map[string]any{"name": b.Name, "members": orEmpty(b.Members), "moved_from": moved})
	s.groupsChanged()
	writeJSON(w, http.StatusCreated, map[string]any{"group": b.Name, "moved": moved, "audit_id": aid})
}

func (s *Server) groupRename(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, ok := readGroupBody(w, r)
	if !ok {
		return
	}
	if err := s.groups.Rename(name, b.Name); err != nil {
		groupErr(w, err)
		return
	}
	aid := s.auditGroup(r, "rename", b.Name, b.Comment, map[string]any{"name": name}, map[string]any{"name": b.Name})
	s.groupsChanged()
	writeJSON(w, http.StatusOK, map[string]any{"group": b.Name, "audit_id": aid})
}

func (s *Server) groupDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, ok := readGroupBody(w, r)
	if !ok {
		return
	}
	g, found := s.groups.Get(name)
	if err := s.groups.Delete(name); err != nil || !found {
		groupErr(w, errors.Join(err, groups.ErrNotFound))
		return
	}
	aid := s.auditGroup(r, "delete", g.Name, b.Comment, map[string]any{"name": g.Name, "members": g.Members}, nil)
	s.groupsChanged()
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "ungrouped": g.Members, "audit_id": aid})
}

func (s *Server) groupAdd(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, ok := readGroupBody(w, r)
	if !ok || !s.checkZones(w, r, b.Nodes) {
		return
	}
	before, _ := s.groups.Get(name)
	moved, err := s.groups.AddMembers(name, b.Nodes)
	if err != nil {
		groupErr(w, err)
		return
	}
	after, _ := s.groups.Get(name)
	var aid int64
	if len(after.Members) != len(before.Members) || len(moved) > 0 {
		aid = s.auditGroup(r, "add_members", after.Name, b.Comment, map[string]any{"members": before.Members},
			map[string]any{"members": after.Members, "moved_from": moved})
		s.groupsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": after.Name, "members": after.Members, "moved": moved, "audit_id": aid})
}

func (s *Server) groupRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, ok := readGroupBody(w, r)
	if !ok {
		return
	}
	before, _ := s.groups.Get(name)
	removed, err := s.groups.RemoveMembers(name, b.Nodes)
	if err != nil {
		groupErr(w, err)
		return
	}
	after, _ := s.groups.Get(name)
	var aid int64
	if len(removed) > 0 {
		aid = s.auditGroup(r, "remove_members", after.Name, b.Comment, map[string]any{"members": before.Members},
			map[string]any{"members": after.Members, "removed": removed})
		s.groupsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": after.Name, "members": after.Members, "removed": removed, "audit_id": aid})
}

// groupPause is "Accepting models" on a group: each member is paused or
// unpaused as POST /api/nodes/{id}/pause does it (history event, audit
// entry per member, the comment naming the group).
func (s *Server) groupPause(w http.ResponseWriter, r *http.Request, pause bool) {
	if !s.agentsOn(w) {
		return
	}
	name := r.PathValue("name")
	b, ok := readGroupBody(w, r)
	if !ok {
		return
	}
	g, found := s.groups.Get(name)
	if !found {
		groupErr(w, groups.ErrNotFound)
		return
	}
	if b.Comment == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	action := "pause"
	if !pause {
		action = "unpause"
	}
	c := b.Comment + " (group " + g.Name + ")"
	results := []map[string]any{}
	for _, id := range g.Members {
		prev, err := s.opts.Pool.SetPaused(id, pause)
		res := map[string]any{"node": id, "paused": pause}
		if err != nil {
			res["error"] = err.Error()
			results = append(results, res)
			continue
		}
		s.opts.Pool.Record(id, "Node "+id+" was "+action+"d: "+c, map[string]any{"action": action, "comment": c, "group": g.Name})
		res["audit_id"] = s.auditNode(r, action, id, c, map[string]any{"paused": prev}, map[string]any{"paused": pause})
		s.forgetPauseActor(id)
		results = append(results, res)
	}
	s.log.Info("group "+action+"d", "group", g.Name, "members", len(g.Members), "comment", b.Comment)
	s.groupsChanged()
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "paused": pause, "nodes": results})
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
