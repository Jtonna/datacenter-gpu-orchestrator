package scheduler

import (
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/instance"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/placer"
)

// Fixed policy values from the design that are not configurable.
const (
	overrideTTL        = 10 * time.Minute // pre-flight override lifetime (7.1)
	quarantineBase     = 30 * time.Second // 8.4
	quarantineMax      = 10 * time.Minute
	backoffBase        = 60 * time.Second // 8.4
	backoffMax         = 10 * time.Minute
	nodeLossAfter      = 30 * time.Second // 8.4
	staleRecheck       = time.Second      // re-read the view this often while it is stale
	fetchRetryMin      = time.Second      // least delay before re-fetching a node whose fetch failed
	failureTailLines   = 40               // child output lines in a 502
	maxPassesPerStep   = 16
	defaultHealthCheck = "/health"
)

type instState int

const (
	stStarting instState = iota + 1
	stReady
	stDraining
	stStopping
)

func (s instState) String() string {
	switch s {
	case stStarting:
		return "starting"
	case stReady:
		return "ready"
	case stDraining:
		return "draining"
	case stStopping:
		return "stopping"
	}
	return "absent"
}

// running is the /running column of design 8.1.
func (s instState) running() RunningState {
	switch s {
	case stStarting:
		return RunningStarting
	case stReady, stDraining:
		return RunningReady
	case stStopping:
		return RunningStopping
	}
	return RunningStopped
}

// phase subdivides starting (and records how far a stopping instance got).
type phase int

const (
	phPreflight phase = iota + 1 // probes running; no child
	phLaunching                  // Launcher.Start called; result pending
	phLoading                    // child running, waiting for health
	phRunning                    // child healthy (ready or later)
)

func (p phase) String() string {
	switch p {
	case phPreflight:
		return "preflight"
	case phLaunching:
		return "launching"
	case phLoading:
		return "loading"
	case phRunning:
		return "running"
	}
	return ""
}

// inst is one instance (design 8.1). At most one per profile.
type inst struct {
	iid       uint64
	prof      *core.Profile
	state     instState
	phase     phase
	placement core.Placement
	created   time.Time

	// The queue entry this instance was started for, kept so a returned
	// instance (7.5) restores its place and accumulated clock.
	entryCreated time.Time
	entrySeq     uint64
	entryClock   time.Duration

	waiters []*waiter // starting only

	child       instance.Child
	childRan    bool
	argv        []string
	loadTimeout time.Duration
	loadDL      time.Time

	readyAt, lastUsed time.Time
	loadDuration      time.Duration
	inFlight          int
	drainInFlight     int // in_flight when draining began; it may only fall
	victimOf          string
	stopMode          instance.StopMode
}

func (in *inst) nodeIDs() []core.NodeID {
	out := make([]core.NodeID, len(in.placement.Nodes))
	for i, n := range in.placement.Nodes {
		out[i] = n.ID
	}
	return out
}

func (in *inst) hasRemote() bool {
	for _, n := range in.placement.Nodes {
		if !n.Local {
			return true
		}
	}
	return false
}

// waiter is one request waiting for an instance: in a queue entry or on a
// starting instance. It is answered exactly once.
type waiter struct {
	id        uint64
	profile   string
	alias     bool
	synthetic bool // POST /api/load: never disconnects, removed at ready
	arrived   time.Time
	reply     chan answer // nil for synthetic waiters
	answered  bool
}

// grant is a successful admission.
type grant struct {
	ID      uint64
	Addr    string
	Profile string
	Alias   bool
}

type answer struct {
	grant *grant
	err   *Error
}

// entry is a profile's queue entry (design 8.3).
type entry struct {
	prof    *core.Profile
	seq     uint64
	created time.Time
	clock   time.Duration // accumulated unclaimed time
	waiters []*waiter

	claim      []core.NodeID // nil when unclaimed
	victims    []string      // profiles of victim instances
	stoppedAny bool          // a victim has been stopped (or exited)
}

func (e *entry) claimed() bool { return e.claim != nil }

type settlePhase int

const (
	setNone settlePhase = iota
	setWaiting
	setFetching
	setFailed
)

func (s settlePhase) String() string {
	switch s {
	case setWaiting:
		return "settling"
	case setFetching:
		return "fetching"
	case setFailed:
		return "fetch-failed"
	}
	return ""
}

// nodeState is the scheduler's bookkeeping for one node.
type nodeState struct {
	holder string // profile holding it
	claim  string // profile of the entry claiming it

	settle   settlePhase
	settleAt time.Time // end of settle wait, or next fetch retry
	fetchGen uint64
	fetched  *core.Node // figures from the post-settle fetch
	fetchKey core.Node  // the view entry when the fetch was done
	fetchHad bool       // whether the view had the node then
	failLog  bool       // the fetch failure of this outage is already logged

	ovFree  int
	ovUntil time.Time // pre-flight override; zero when none

	emptyPF, emptyClaim int
	hasPF, hasClaim     bool

	quarUntil time.Time
	quarLevel int

	missingSince time.Time
}

type backoff struct {
	until time.Time
	level int
}

type lease struct {
	id      uint64
	profile string
	iid     uint64
}

// schedCore is all scheduler state. It is mutated only by handle (step.go),
// on one goroutine, and depends on its collaborators only through
// interfaces. Slow work is emitted as effects for the runner.
type schedCore struct {
	cfg   *config.Config
	clock Clock
	view  fleet.View
	needs NeedProvider
	build instance.BuildOptions
	log   *slog.Logger

	now, last time.Time
	seq       uint64

	insts    map[string]*inst
	entries  map[string]*entry
	nodes    map[core.NodeID]*nodeState
	backoffs map[string]*backoff
	waiters  map[uint64]*waiter
	leases   map[uint64]*lease
	needs_   map[string]NeedInfo

	raw           map[core.NodeID]core.Node // view this step
	fresh         bool
	age           time.Duration
	seenReconnect time.Time

	shutdown   bool
	shutdownDL time.Time
	stopAll    bool
	killAll    bool
	finished   bool

	effects []effect
	feed    []FeedEvent
	feedSeq uint64
	changed bool
}

func newCore(cfg *config.Config, clock Clock, view fleet.View, needs NeedProvider, build instance.BuildOptions, log *slog.Logger) *schedCore {
	if log == nil {
		log = slog.Default()
	}
	return &schedCore{
		cfg: cfg, clock: clock, view: view, needs: needs, build: build, log: log,
		insts: map[string]*inst{}, entries: map[string]*entry{}, nodes: map[core.NodeID]*nodeState{},
		backoffs: map[string]*backoff{}, waiters: map[uint64]*waiter{}, leases: map[uint64]*lease{},
		needs_: map[string]NeedInfo{}, raw: map[core.NodeID]core.Node{},
	}
}

func (c *schedCore) next() uint64 { c.seq++; return c.seq }

func (c *schedCore) ns(id core.NodeID) *nodeState {
	n := c.nodes[id]
	if n == nil {
		n = &nodeState{}
		c.nodes[id] = n
	}
	return n
}

func (c *schedCore) instByIID(iid uint64) *inst {
	for _, in := range c.insts {
		if in.iid == iid {
			return in
		}
	}
	return nil
}

func (c *schedCore) anyStarting() bool {
	for _, in := range c.insts {
		if in.state == stStarting {
			return true
		}
	}
	return false
}

func (c *schedCore) orderedEntries() []*entry {
	out := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.Before(out[j].created)
		}
		return out[i].seq < out[j].seq
	})
	return out
}

func (c *schedCore) sortedInsts() []*inst {
	out := make([]*inst, 0, len(c.insts))
	for _, in := range c.insts {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].prof.Name < out[j].prof.Name })
	return out
}

func (c *schedCore) sortedNodeIDs() []core.NodeID {
	seen := map[core.NodeID]bool{}
	var ids []core.NodeID
	for id := range c.raw {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range c.nodes {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// ---- node figures and eligibility (design 7.1) ----

// node returns the node's effective figures: the view entry, replaced by
// the post-settle fetch while the view has not changed since.
func (c *schedCore) node(id core.NodeID) (core.Node, bool) {
	if ns := c.nodes[id]; ns != nil && ns.fetched != nil {
		return *ns.fetched, true
	}
	n, ok := c.raw[id]
	return n, ok
}

// freeMiB is the free figure used for the live budget: the pre-flight
// override while it lasts, else the effective view figure.
func (c *schedCore) freeMiB(id core.NodeID, n core.Node) int {
	if ns := c.nodes[id]; ns != nil && !ns.ovUntil.IsZero() && c.now.Before(ns.ovUntil) {
		return ns.ovFree
	}
	return n.FreeMiB
}

func (c *schedCore) liveBudget(id core.NodeID, n core.Node) int {
	return c.freeMiB(id, n) - n.HeadroomMiB
}

// emptyMiB is the best known figure for the node with nothing of ours on
// it: min(total, last pre-flight free, free when a claim last failed).
func (c *schedCore) emptyMiB(id core.NodeID, n core.Node) int {
	e := n.TotalMiB
	if ns := c.nodes[id]; ns != nil {
		if ns.hasPF && ns.emptyPF < e {
			e = ns.emptyPF
		}
		if ns.hasClaim && ns.emptyClaim < e {
			e = ns.emptyClaim
		}
	}
	return e
}

func (c *schedCore) planBudget(id core.NodeID, n core.Node) int {
	return c.emptyMiB(id, n) - n.HeadroomMiB
}

// reachable: the local node, or online in a fresh view with an address.
func (c *schedCore) reachable(n core.Node) bool {
	if n.Local {
		return true
	}
	return c.fresh && n.Online && n.Addr != ""
}

func (c *schedCore) quarantined(id core.NodeID) bool {
	ns := c.nodes[id]
	return ns != nil && c.now.Before(ns.quarUntil)
}

// eligible is design 7.1's eligibility without the two profile-specific
// rules (allow_tiers, budget ≥ min_share) that the placer applies. A node
// whose post-settle fetch failed is treated as offline.
func (c *schedCore) eligible(id core.NodeID) bool {
	n, ok := c.node(id)
	if !ok || !c.reachable(n) || c.quarantined(id) {
		return false
	}
	if ns := c.nodes[id]; ns != nil && ns.settle == setFailed {
		return false
	}
	return true
}

// isFree: eligible, not held, not claimed, not settling.
func (c *schedCore) isFree(id core.NodeID) bool {
	if !c.eligible(id) {
		return false
	}
	ns := c.nodes[id]
	return ns == nil || (ns.holder == "" && ns.claim == "" && ns.settle == setNone)
}

// localOnly restricts kind: command profiles to the local node.
func localOnly(p *core.Profile) bool { return p.Kind == core.KindCommand }

// freeCandidates are the free nodes at live budgets, for Place.
func (c *schedCore) freeCandidates(p *core.Profile) []placer.Candidate {
	var out []placer.Candidate
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || !c.isFree(id) || (localOnly(p) && !n.Local) {
			continue
		}
		out = append(out, placer.FromNode(n, c.liveBudget(id, n)))
	}
	return out
}

// request builds the placer request for a profile. The need comes from
// the provider (cached on success); an error is a load failure.
func (c *schedCore) request(p *core.Profile) (placer.Request, NeedInfo, error) {
	info, err := c.need(p)
	if err != nil {
		return placer.Request{}, NeedInfo{}, err
	}
	d := c.cfg.Defaults
	req := placer.Request{
		Need: info.Need, AllowTiers: p.AllowTiers, TensorSplit: p.TensorSplit,
		MaxPool: max(d.MaxPool, 1), MinShareMiB: d.MinShareMiB, DeviceOverheadMiB: d.DeviceOverheadMiB,
	}
	if localOnly(p) {
		req.MaxPool = 1
		req.AllowTiers = nil
	}
	for _, name := range p.ForceNodes {
		id, err := c.view.Resolve(name)
		if err != nil {
			id = "" // unresolved or ambiguous: the node is unavailable
		}
		req.Force = append(req.Force, placer.ForcedNode{Name: name, ID: id})
	}
	return req, info, nil
}

func (c *schedCore) need(p *core.Profile) (NeedInfo, error) {
	if info, ok := c.needs_[p.Name]; ok {
		return info, nil
	}
	info, err := c.needs.Need(p)
	if err != nil {
		return NeedInfo{}, err
	}
	c.needs_[p.Name] = info
	return info, nil
}

func (c *schedCore) backoffActive(p string) (*backoff, bool) {
	b := c.backoffs[p]
	if b == nil || !c.now.Before(b.until) {
		return b, false
	}
	return b, true
}

// hold is clamp(load_duration, min_hold, max_hold) (design 8.3).
func (c *schedCore) hold(in *inst) time.Duration {
	h := in.loadDuration
	if h < c.cfg.Defaults.MinHold {
		h = c.cfg.Defaults.MinHold
	}
	if h > c.cfg.Defaults.MaxHold {
		h = c.cfg.Defaults.MaxHold
	}
	return h
}

func (c *schedCore) emit(k FeedKind, profile string, f func(*FeedEvent)) {
	c.feedSeq++
	ev := FeedEvent{Seq: c.feedSeq, Kind: k, Time: c.now, Profile: profile}
	if f != nil {
		f(&ev)
	}
	c.feed = append(c.feed, ev)
}

func (c *schedCore) emitState(in *inst, s RunningState) {
	c.emit(FeedInstance, in.prof.Name, func(e *FeedEvent) {
		e.State = s
		if s == RunningReady {
			e.ReadySince = in.readyAt
		}
	})
}
