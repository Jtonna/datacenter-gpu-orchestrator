// Package startups turns the agents' start-up reports into one figure per
// start of a node: seconds from boot start to ready for model loads
// (design "Start-up figures").
//
//   - Ready for model loads: the agent is registered, its programs are
//     ready, the model source is reachable (a share node), and the node has
//     no unplaced reason other than a pause someone set.
//   - Boot start: the start of the Linux kernel for a full start; the start
//     of userspace for a soft restart (systemctl soft-reboot). An agent that
//     restarts while the machine keeps running is an agent restart and gets
//     no figure.
//
// Every figure is on the node's own axis: seconds since its kernel booted,
// monotonic. Potluck's clock is used only to map the moment an unplaced
// reason cleared onto that axis and for the times shown with a record.
// Never a node's wall clock.
package startups

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The kinds of start.
const (
	KindFull         = "full"          // the kernel started
	KindSoft         = "soft"          // userspace restarted, the kernel kept running
	KindAgentRestart = "agent_restart" // only the agent restarted
)

// Where a figure counts from (Record.Origin). Records stored before the
// origin existed have none.
const (
	// OriginBootOrigin: the marks file's boot_origin, 0 on a full start and
	// the moment the restart began on a soft restart (the shutdown included).
	OriginBootOrigin = "boot_origin"
	// OriginKernel: kernel start, a full start without boot_origin.
	OriginKernel = "kernel"
	// OriginUserspace: the userspace start, a soft restart without
	// boot_origin (boot_origin_partial, or neither line): the shutdown
	// before it is not included.
	OriginUserspace = "userspace"
)

// What a figure ends at (Record.HeadlineEnd), when the console's boot_ready
// takes part (with boot_origin).
const (
	// HeadlineEndBootReady: the console's boot screen finished after Potluck
	// found the node placeable; the figure is the console's own.
	HeadlineEndBootReady = "boot_ready"
	// HeadlineEndPlaceable: Potluck found the node placeable after (or when)
	// the boot screen finished.
	HeadlineEndPlaceable = "placeable"
)

// MarkBootReady is the external mark the console's boot screen writes when
// all its steps have passed; with boot_origin, the figure ends no earlier.
const MarkBootReady = "boot_ready"

// InferWindow is the "short time" of the rule for a node's first record:
// userspace within this of kernel boot, and the agent within this of
// userspace, make a full start.
const InferWindow = 5 * time.Minute

// Lifetime: a record that has no figure this long after its registration is
// closed with the condition that is still missing.
const Lifetime = 30 * time.Minute

// KeepPerNode is how many records the store keeps per node.
const KeepPerNode = 500

// Record is one start of a node, as Potluck stores and shows it.
type Record struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	Release  string `json:"release"`
	Install  string `json:"install,omitempty"`
	// AgentStartID is the agent process the record belongs to (its
	// registration's agent_start_id): a re-registration of the same process
	// is not a new start.
	AgentStartID string `json:"agent_start_id"`
	// Kind is full, soft or agent_restart. Inferred is true when there was
	// no earlier record to compare with (or its facts were missing) and the
	// kind comes from the rule for a first record; KindNote says why.
	Kind     string `json:"kind"`
	Inferred bool   `json:"inferred"`
	KindNote string `json:"kind_note,omitempty"`
	// RegisteredAt is Potluck's clock when the registration arrived.
	RegisteredAt time.Time `json:"registered_at"`
	// The agent's report: the node's own time axis.
	BootID          string             `json:"boot_id"`
	AgentStartS     float64            `json:"agent_start_s"`
	UserspaceStartS *float64           `json:"userspace_start_s,omitempty"`
	SoftReboots     *int64             `json:"soft_reboots,omitempty"`
	Marks           map[string]float64 `json:"marks"`
	ExternalMarks   map[string]float64 `json:"external_marks"`
	// NeedShare: the node's model source is a share, so share_mounted is a
	// condition of ready.
	NeedShare bool `json:"need_share"`
	// HeadlineS is the figure: seconds from boot start to ready for model
	// loads. Absent until it is known, and for good when a condition never
	// became true (Missing or BlockedBy then says which).
	HeadlineS *float64 `json:"headline_s,omitempty"`
	// Origin says where HeadlineS counts from (Origin*); set with the figure.
	Origin string `json:"origin,omitempty"`
	// PlaceableS is Potluck's own readiness point (ReadyS) minus the base,
	// and HeadlineEnd says what the figure ended at (HeadlineEnd*). Both are
	// set only when the figure counts from boot_origin and the console's
	// boot_ready is in: the figure then ends at the later of boot_ready and
	// ReadyS. Absent otherwise, and on records stored before.
	PlaceableS  *float64 `json:"placeable_s,omitempty"`
	HeadlineEnd string   `json:"headline_end,omitempty"`
	// ReadyS is Potluck's own ready moment on the node's axis (the node
	// placeable); ReadyAt the same moment on Potluck's clock.
	ReadyS  *float64   `json:"ready_s,omitempty"`
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	// Missing names the marks of ready that have not happened
	// (registered, programs_ready, share_mounted), or userspace_start for a
	// soft restart whose userspace start is not known.
	Missing string `json:"missing,omitempty"`
	// BlockedBy is the node's unplaced reason while the three marks are in
	// and the node still cannot take a load. WasBlockedBy keeps it once the
	// reason cleared and the figure was taken at that moment.
	BlockedBy    string `json:"blocked_by,omitempty"`
	WasBlockedBy string `json:"was_blocked_by,omitempty"`
	// Open: the record may still get its figure.
	Open bool `json:"open"`
	// Requested: the start was announced with expect-restart.
	Requested      bool       `json:"requested"`
	RequestedKind  string     `json:"requested_kind,omitempty"`
	RequestComment string     `json:"request_comment,omitempty"`
	RequestedAt    *time.Time `json:"requested_at,omitempty"`
	// AwayS: from the expect-restart call to the registration; AwayToReadyS:
	// from the call to ready for model loads. Both on Potluck's clock.
	AwayS        *float64 `json:"away_s,omitempty"`
	AwayToReadyS *float64 `json:"away_to_ready_s,omitempty"`
}

// Condition is what the record says in place of a figure: the missing
// condition or the reason that blocks; "" when it has its figure, or for an
// agent restart.
func (r *Record) Condition() string {
	switch {
	case r.HeadlineS != nil || r.Kind == KindAgentRestart:
		return ""
	case r.BlockedBy != "":
		return "blocked_by " + r.BlockedBy
	case r.Missing != "":
		return "missing " + r.Missing
	}
	return ""
}

// Facts are the start facts a record is classified on.
type Facts struct {
	BootID          string
	AgentStartS     float64
	UserspaceStartS *float64
	SoftReboots     *int64
}

// FactsOf returns the facts of a record.
func FactsOf(r *Record) Facts {
	return Facts{BootID: r.BootID, AgentStartS: r.AgentStartS, UserspaceStartS: r.UserspaceStartS, SoftReboots: r.SoftReboots}
}

// Classify says what kind of start cur is, given the node's previous record
// (nil when it has none), from the facts only:
//
//  1. Both boot ids known and different: full start.
//  2. Same boot id, and a higher soft-restart count or a later userspace
//     start (by more than a second): soft restart.
//  3. Same boot id and the same userspace (both systemd figures, where both
//     sides have them, unchanged): agent restart.
//  4. Same boot id but no systemd figure on one side to compare: agent
//     restart, marked inferred (the kernel is the same; whether userspace
//     restarted cannot be told).
//  5. No previous record, or a boot id missing on either side: the rule for
//     a first record, marked inferred: a soft restart when the soft-restart
//     count is above 0 and the agent started within InferWindow of the
//     userspace start; a full start when the userspace start (if known) is
//     within InferWindow of kernel boot and the agent started within
//     InferWindow of it (of kernel boot when the userspace start is not
//     known); otherwise an agent restart.
func Classify(prev *Facts, cur Facts) (kind string, inferred bool, note string) {
	if prev != nil && prev.BootID != "" && cur.BootID != "" {
		if prev.BootID != cur.BootID {
			return KindFull, false, "the boot id changed"
		}
		if cur.SoftReboots != nil && prev.SoftReboots != nil && *cur.SoftReboots > *prev.SoftReboots {
			return KindSoft, false, "same boot id, higher soft-restart count"
		}
		if cur.UserspaceStartS != nil && prev.UserspaceStartS != nil && *cur.UserspaceStartS > *prev.UserspaceStartS+1 {
			return KindSoft, false, "same boot id, later userspace start"
		}
		comparable := (cur.SoftReboots != nil && prev.SoftReboots != nil) || (cur.UserspaceStartS != nil && prev.UserspaceStartS != nil)
		if comparable {
			return KindAgentRestart, false, "same boot id, same userspace"
		}
		return KindAgentRestart, true, "same boot id; systemd's figures are missing, so a soft restart cannot be told apart: taken as an agent restart"
	}
	why := "first record of this node"
	if prev != nil {
		why = "the boot id is missing"
	}
	win := InferWindow.Seconds()
	if cur.SoftReboots != nil && *cur.SoftReboots > 0 && cur.UserspaceStartS != nil && cur.AgentStartS-*cur.UserspaceStartS <= win {
		return KindSoft, true, why + "; soft-restart count above 0 and the agent started within 5 minutes of userspace"
	}
	if cur.UserspaceStartS != nil {
		if *cur.UserspaceStartS <= win && cur.AgentStartS-*cur.UserspaceStartS <= win {
			return KindFull, true, why + "; userspace started within 5 minutes of kernel boot and the agent within 5 minutes of userspace"
		}
		return KindAgentRestart, true, why + "; the agent started long after userspace"
	}
	if cur.AgentStartS <= win {
		return KindFull, true, why + "; no userspace start known, the agent started within 5 minutes of kernel boot"
	}
	return KindAgentRestart, true, why + "; no userspace start known, the agent started more than 5 minutes after kernel boot"
}

// New builds the record of a registration that is a new start.
func New(nodeID, name, release, install, startID string, needShare bool, at time.Time, rep *agentproto.Startup, prev *Record) *Record {
	r := &Record{NodeID: nodeID, NodeName: name, Release: release, Install: install, AgentStartID: startID, RegisteredAt: at,
		BootID: rep.BootID, AgentStartS: rep.AgentStartS, Marks: map[string]float64{}, ExternalMarks: map[string]float64{},
		NeedShare: needShare, Open: true}
	r.Merge(rep)
	var pf *Facts
	if prev != nil {
		f := FactsOf(prev)
		pf = &f
	}
	r.Kind, r.Inferred, r.KindNote = Classify(pf, FactsOf(r))
	if r.Kind == KindAgentRestart {
		r.Open = false
	}
	return r
}

// Merge takes the marks of a later report of the same agent process (a mark
// keeps its first value; external marks take the newest). It reports whether
// anything changed. Names that are not valid are dropped; external marks are
// capped as on the agent.
func (r *Record) Merge(rep *agentproto.Startup) bool {
	changed, late := false, false
	if r.UserspaceStartS == nil && rep.UserspaceStartS != nil && finite(*rep.UserspaceStartS) {
		v := *rep.UserspaceStartS
		r.UserspaceStartS, changed = &v, true
	}
	if r.SoftReboots == nil && rep.SoftReboots != nil && *rep.SoftReboots >= 0 {
		v := *rep.SoftReboots
		r.SoftReboots, changed = &v, true
	}
	for k, v := range rep.Marks {
		if _, ok := r.Marks[k]; ok || !agentproto.ValidStartupMarkName(k) || !finite(v) {
			continue
		}
		r.Marks[k], changed = v, true
	}
	for k, v := range rep.ExternalMarks {
		if !agentproto.ValidStartupMarkName(k) || !finite(v) {
			continue
		}
		if old, ok := r.ExternalMarks[k]; ok && old == v {
			continue
		} else if !ok && len(r.ExternalMarks) >= agentproto.StartupMaxExternalMarks {
			continue
		}
		r.ExternalMarks[k], changed = v, true
		if k == MarkBootReady || k == agentproto.StartupMarkBootOrigin {
			late = true
		}
	}
	// boot_ready is written late, often after the figure was taken from
	// Potluck's own marks: the figure follows it.
	if late && !r.Open && r.HeadlineS != nil {
		r.endAtBootReady()
	}
	return changed
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// readyMarks are the marks of "ready for model loads", in order.
func (r *Record) readyMarks() []string {
	m := []string{agentproto.StartupMarkRegistered, agentproto.StartupMarkProgramsReady}
	if r.NeedShare {
		m = append(m, agentproto.StartupMarkShareMounted)
	}
	return m
}

// Evaluate decides the figure of an open record at Potluck's time now, with
// the node's unplaced reason now ("" when it could take a load; "paused"
// counts as none: a pause someone set does not hold the figure back). It
// reports whether the record changed.
//
// When the three marks are in and nothing blocks, ready is the latest of
// them. When a reason blocks, the record waits with blocked_by; when the
// reason clears, ready is that moment mapped onto the node's axis: the
// registered mark plus Potluck's time since the registration (both are the
// moment of the first accepted registration, seen from either side; the
// mapping is off by the registration's network time, well under a second).
// The figure is ready minus boot start: the marks file's boot_origin when
// it has one (at or before ready, and for a soft restart at or before the
// userspace start when that is known); else 0 for a full start and the
// userspace start for a soft restart. Origin says which.
func (r *Record) Evaluate(now time.Time, unplaced string) bool {
	if !r.Open {
		return false
	}
	before := r.snapshot()
	var missing []string
	ready := 0.0
	for _, m := range r.readyMarks() {
		v, ok := r.Marks[m]
		if !ok {
			missing = append(missing, m)
			continue
		}
		ready = math.Max(ready, v)
	}
	if unplaced == "paused" {
		unplaced = ""
	}
	switch {
	case len(missing) > 0:
		r.Missing, r.BlockedBy = strings.Join(missing, ", "), ""
	case r.Kind == KindSoft && r.UserspaceStartS == nil && !r.hasBootOrigin(ready):
		r.Missing, r.BlockedBy = "userspace_start", ""
	case unplaced != "":
		r.Missing, r.BlockedBy = "", unplaced
	default:
		if r.BlockedBy != "" {
			mapped := r.Marks[agentproto.StartupMarkRegistered] + now.Sub(r.RegisteredAt).Seconds()
			ready = math.Max(ready, mapped)
			r.WasBlockedBy = r.BlockedBy
		}
		base, origin := 0.0, OriginKernel
		switch {
		case r.hasBootOrigin(ready):
			base, origin = r.ExternalMarks[agentproto.StartupMarkBootOrigin], OriginBootOrigin
		case r.Kind == KindSoft:
			base, origin = *r.UserspaceStartS, OriginUserspace
		}
		r.Origin = origin
		h := round3(ready - base)
		rd := round3(ready)
		at := r.RegisteredAt.Add(time.Duration((ready - r.Marks[agentproto.StartupMarkRegistered]) * float64(time.Second)))
		r.HeadlineS, r.ReadyS, r.ReadyAt = &h, &rd, &at
		r.Missing, r.BlockedBy, r.Open = "", "", false
		if r.RequestedAt != nil {
			a := round3(at.Sub(*r.RequestedAt).Seconds())
			r.AwayToReadyS = &a
		}
		r.endAtBootReady()
	}
	return r.snapshot() != before
}

// Expire closes an open record whose lifetime is over; it keeps saying what
// is missing or what blocks. It reports whether the record changed.
func (r *Record) Expire(now time.Time) bool {
	if !r.Open || now.Sub(r.RegisteredAt) < Lifetime {
		return false
	}
	r.Open = false
	if r.Missing == "" && r.BlockedBy == "" {
		r.Missing = "registered"
	}
	return true
}

// Close ends an open record without a figure (the node started again).
func (r *Record) Close() bool {
	if !r.Open {
		return false
	}
	r.Open = false
	if r.Missing == "" && r.BlockedBy == "" {
		r.Missing = "registered"
	}
	return true
}

// MarkRequested ties the record to an expect-restart call.
func (r *Record) MarkRequested(kind, comment string, at, registered time.Time) {
	r.Requested, r.RequestedKind, r.RequestComment = true, kind, comment
	t := at
	r.RequestedAt = &t
	a := round3(registered.Sub(at).Seconds())
	r.AwayS = &a
}

// endAtBootReady ends a figure that counts from boot_origin at the later of
// the console's boot_ready and Potluck's own point (ReadyS), so that it is
// the console's own figure whenever the boot screen finishes last and never
// claims ready before the node was placeable. Without boot_ready (or with
// one before boot_origin) the figure is ReadyS minus the base, as it always
// was. away_to_ready_s follows the end. It reports whether anything changed.
func (r *Record) endAtBootReady() bool {
	if r.HeadlineS == nil || r.ReadyS == nil || r.Origin != OriginBootOrigin {
		return false
	}
	o, okO := r.ExternalMarks[agentproto.StartupMarkBootOrigin]
	b, okB := r.ExternalMarks[MarkBootReady]
	if !okO || !okB || b < o {
		return false
	}
	before := r.snapshot() + fmt.Sprint(r.PlaceableS != nil, r.HeadlineEnd)
	own := *r.ReadyS
	end, why := own, HeadlineEndPlaceable
	if b > own {
		end, why = b, HeadlineEndBootReady
	}
	h, pl := round3(end-o), round3(own-o)
	r.HeadlineS, r.PlaceableS, r.HeadlineEnd = &h, &pl, why
	if r.RequestedAt != nil && r.ReadyAt != nil {
		a := round3(r.ReadyAt.Sub(*r.RequestedAt).Seconds() + end - own)
		r.AwayToReadyS = &a
	}
	return r.snapshot()+fmt.Sprint(r.PlaceableS != nil, r.HeadlineEnd) != before
}

// hasBootOrigin: the record has a boot_origin usable as the base of a
// figure whose ready moment is ready: not after ready, and for a soft
// restart not after its userspace start (when known).
func (r *Record) hasBootOrigin(ready float64) bool {
	o, ok := r.ExternalMarks[agentproto.StartupMarkBootOrigin]
	if !ok || o > ready {
		return false
	}
	return r.Kind != KindSoft || r.UserspaceStartS == nil || o <= *r.UserspaceStartS
}

func (r *Record) snapshot() string {
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprint(f(r.HeadlineS), r.Missing, r.BlockedBy, r.Open, r.WasBlockedBy, f(r.AwayToReadyS))
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// MarksInOrder lists every mark of a record (agent's and external), sorted
// by time, then name: for display.
func (r *Record) MarksInOrder() []NamedMark {
	var out []NamedMark
	for k, v := range r.Marks {
		out = append(out, NamedMark{Name: k, S: v})
	}
	for k, v := range r.ExternalMarks {
		out = append(out, NamedMark{Name: k, S: v, External: true})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].S != out[j].S {
			return out[i].S < out[j].S
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// NamedMark is one mark.
type NamedMark struct {
	Name     string  `json:"name"`
	S        float64 `json:"s"`
	External bool    `json:"external,omitempty"`
}

// Last is what a node's entry in status says about its last start.
type Last struct {
	Kind      string   `json:"kind"`
	Inferred  bool     `json:"inferred,omitempty"`
	HeadlineS *float64 `json:"headline_s,omitempty"`
	Origin    string   `json:"origin,omitempty"`
	// PlaceableS and HeadlineEnd: as in Record.
	PlaceableS  *float64  `json:"placeable_s,omitempty"`
	HeadlineEnd string    `json:"headline_end,omitempty"`
	Missing     string    `json:"missing,omitempty"`
	BlockedBy   string    `json:"blocked_by,omitempty"`
	Open        bool      `json:"open,omitempty"`
	Requested   bool      `json:"requested,omitempty"`
	At          time.Time `json:"at"`
}

// LastOf summarises a record for status.
func LastOf(r *Record) *Last {
	if r == nil {
		return nil
	}
	l := &Last{Kind: r.Kind, Inferred: r.Inferred, Origin: r.Origin, Missing: r.Missing, BlockedBy: r.BlockedBy, Open: r.Open,
		Requested: r.Requested, At: r.RegisteredAt}
	if r.HeadlineS != nil {
		h := *r.HeadlineS
		l.HeadlineS = &h
	}
	if r.PlaceableS != nil {
		v := *r.PlaceableS
		l.PlaceableS, l.HeadlineEnd = &v, r.HeadlineEnd
	}
	return l
}

// Aggregate is the figures of one node and one kind. Kinds are never mixed.
type Aggregate struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	Kind     string `json:"kind"`
	// Count is every start of this kind; WithFigure those with a figure.
	Count      int `json:"count"`
	WithFigure int `json:"with_figure"`
	// Requested counts the figures announced with expect-restart.
	Requested int      `json:"requested"`
	LastS     *float64 `json:"last_s,omitempty"`
	MedianS   *float64 `json:"median_s,omitempty"`
	WorstS    *float64 `json:"worst_s,omitempty"`
	// Origin is where the newest figure counts from (Record.Origin);
	// MixedOrigins: the group's figures do not all count from one origin
	// (a soft restart counted from boot_origin includes the shutdown, one
	// counted from the userspace start does not).
	Origin       string `json:"origin,omitempty"`
	MixedOrigins bool   `json:"mixed_origins,omitempty"`
	// Last is the newest record of this node and kind, with its marks.
	Last *Record `json:"last,omitempty"`
}

// Aggregates groups records (any order) by node and kind. Within a group,
// last is the newest figure, median and worst are over every figure. The
// groups are sorted by node name, then id, then kind (full, soft, agent
// restart).
func Aggregates(recs []Record) []Aggregate {
	type key struct{ node, kind string }
	groups := map[key][]Record{}
	for _, r := range recs {
		k := key{r.NodeID, r.Kind}
		groups[k] = append(groups[k], r)
	}
	var out []Aggregate
	for k, rs := range groups {
		sort.Slice(rs, func(i, j int) bool { return newer(rs[i], rs[j]) })
		a := Aggregate{NodeID: k.node, NodeName: rs[0].NodeName, Kind: k.kind, Count: len(rs)}
		last := rs[0]
		a.Last = &last
		var figs []float64
		for _, r := range rs {
			if r.HeadlineS == nil {
				continue
			}
			if a.LastS == nil {
				v := *r.HeadlineS
				a.LastS, a.Origin = &v, r.Origin
			} else if base(r.Kind, r.Origin) != base(a.Kind, a.Origin) {
				a.MixedOrigins = true
			}
			figs = append(figs, *r.HeadlineS)
			if r.Requested {
				a.Requested++
			}
		}
		a.WithFigure = len(figs)
		if len(figs) > 0 {
			sort.Float64s(figs)
			med := figs[len(figs)/2]
			if len(figs)%2 == 0 {
				med = (figs[len(figs)/2-1] + figs[len(figs)/2]) / 2
			}
			med, worst := round3(med), figs[len(figs)-1]
			a.MedianS, a.WorstS = &med, &worst
		}
		out = append(out, a)
	}
	rank := map[string]int{KindFull: 0, KindSoft: 1, KindAgentRestart: 2}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return rank[out[i].Kind] < rank[out[j].Kind]
	})
	return out
}

// base is what an origin means for a kind, to compare figures: every full
// start counts from kernel start (boot_origin is 0 there), and a soft
// restart stored before the origin existed counted from userspace.
func base(kind, origin string) string {
	switch {
	case kind == KindFull:
		return OriginKernel
	case origin == "":
		return OriginUserspace
	}
	return origin
}

// newer orders records newest first: by registration, then id.
func newer(a, b Record) bool {
	if !a.RegisteredAt.Equal(b.RegisteredAt) {
		return a.RegisteredAt.After(b.RegisteredAt)
	}
	return a.ID > b.ID
}
