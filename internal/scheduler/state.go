package scheduler

import (
	"log/slog"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
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
	// phWorkers (agents membership): the worker launches are being started on
	// the agents; no child yet. It takes the place of the pre-flight.
	phWorkers
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
	case phWorkers:
		return "workers"
	}
	return ""
}

// inst is one instance (design 8.1). At most one per profile.
type inst struct {
	// tight are the nodes of a tight fit (tight.go): the load fits them
	// without its margin only. Empty for any other load.
	tight     []PlanMemoryNode
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
	logFile     string // full child output on disk; empty when not kept
	loadTimeout time.Duration
	loadDL      time.Time

	readyAt, lastUsed time.Time
	loadDuration      time.Duration
	inFlight          int
	drainInFlight     int // in_flight when draining began; it may only fall
	victimOf          string
	stopMode          instance.StopMode
	// drain: an operator asked it to stop once its runs finish (drain.go);
	// it admits nothing meanwhile, as a draining victim.
	drain bool

	// Load progress (loadprogress.go).
	loadStart   time.Time // the child started; the upload begins about now
	remoteBytes int64     // bytes the remote workers must receive; 0 for a local-only load
	peers       []string  // the workers' rpc addresses
	linkMBs     float64   // slowest remote tier link, MB/s; 0 when unknown
	sentBytes   int64     // largest acknowledged count seen
	measured    bool      // the last count read was good
	lastGrowth  time.Time // first measured reading, then each time the acknowledged total rose
	samples     []byteSample
	nextSample  time.Time
	sampling    bool // a count is being read
	noSample    bool // this platform or child cannot count
	nextLoadLog time.Time

	// The load attempt (loadreport.go): from child start to ready, failure or cancel.
	sessionID   string
	sessionAt   time.Time
	sessionDone bool
	workerEst   []int64              // per worker (in peers order): share of the file by the tensor split
	peerSent    map[string]int64     // per worker: largest acknowledged count seen
	peerKnown   bool                 // the last count carried per-worker figures
	peerGrowth  map[string]time.Time // per worker: when its count was first seen or last rose

	// Agent worker launches (agents.go); zero in fleet membership.
	workers    bool                                // worker launches were requested; they are stopped when the instance goes
	forceNodes []core.NodeID                       // nodes whose launch is stopped with force (force unload)
	launchIDs  map[core.NodeID]string              // the launch on each worker node
	agentLoad  map[string]*agentproto.LoadProgress // per worker rpc address: the agent's own load figures
	// hostTarget is the host launch's memory figures (placement.Host set):
	// expect_mib and min_mem_available_mib, as for a worker (hostnode.go).
	hostTarget WorkerTarget
	// leaseNodes are the placement's machines with their roles, built at the
	// first admission and shared read-only by every lease (leaseNodesOf).
	leaseNodes []LeaseNode
	// gone is closed when the instance is removed: the leases of its
	// requests end with it (a host on a node that went away leaves no
	// connection to break).
	gone chan struct{}

	// baseFree is each node's free figure when the instance was created
	// (reserve.go): what the node gives up for it is measured against it.
	baseFree map[core.NodeID]int
	// baseUnsettled: the nodes an instance that ran left within one
	// free_average_window before this one was created; their base may still
	// show it, so no overhead reading is taken there (unsettledBase).
	baseUnsettled map[core.NodeID]bool
	// cyclePeak is the most each node gave up for this instance in this
	// load-to-unload cycle (reserve.go, endPeakCycle).
	cyclePeak map[core.NodeID]int
	// hostMeasured: the host overhead of this cycle was read (or found out
	// of range); hostHigh is the most the host took on its node in the
	// cycle (hostoverhead.go).
	hostMeasured bool
	hostHigh     int
	// devMeasured: the per-device overheads of this cycle were read
	// (deviceoverhead.go).
	devMeasured bool

	benchRun   string // the bench run whose request started this instance (history records only)
	served     int    // requests admitted (logged when the instance stops)
	stopReason string // why it is being stopped (first reason wins), for the log
	exit       string // how the child ended, for the log
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
	benchRun  string   // X-Bench-Run id of the benchmark run that sent it; only tags history records
	pin       []string // pin.go: load on these nodes only, for this load
}

// grant is a successful admission.
type grant struct {
	ID      uint64
	Addr    string
	APIKey  string // the key requests to Addr carry; "" for none
	Profile string
	Alias   bool
	Gone    <-chan struct{} // closed when the instance is removed
	// Where the request runs, for the request log: the instance, its load
	// session and its machines with their roles (shared, never modified).
	Instance uint64
	Session  string
	Nodes    []LeaseNode
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
	evictHeld  time.Time     // when evictWaits first held its eviction (neverfits.go)
	occ        *occCase      // its only option was an occupied console (occupied.go)
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

	// heldChanged is when what is placed on the node last changed (placed or
	// released): the average free figure is not used for a window after it
	// (reserve.go, avgStale).
	heldChanged time.Time
	// releasedAt is when an instance that ran (the settling kind of
	// release) last left the node: its figure may still show it for a while
	// (neverfits.go, justReleased). A refused or returned placement that
	// never ran does not set it.
	releasedAt time.Time
	// expectStop: the release left worker launches of ours on the node to
	// be stopped, so its memory is back only once the view has seen them
	// end (neverfits.go, justReleased).
	expectStop bool

	emptyPF, emptyClaim int
	hasPF, hasClaim     bool

	quarUntil time.Time
	quarLevel int
	quarOpen  bool          // quarantined and the release not yet logged
	quarWhy   setAsideCause // why and since when (zonefacts.go)

	missingSince time.Time

	// agentHold: a worker launch on the node was refused with
	// model_source_unreachable; the node is not used until then (agents.go).
	agentHold time.Time

	// lastFile is the model file of the last load that became ready on the
	// node; "" when none since the scheduler started. Used only to word the
	// reload-from-cache line (reloadcache.go, CacheFlag).
	lastFile string
}

type backoff struct {
	until time.Time
	level int
	open  bool // backed off and the end not yet logged
}

type lease struct {
	id       uint64
	profile  string
	iid      uint64
	admitted time.Time // drain.go: run lengths for the drain ETA
}

// schedCore is all scheduler state. It is mutated only by handle (step.go),
// on one goroutine, and depends on its collaborators only through
// interfaces. Slow work is emitted as effects for the runner.
type schedCore struct {
	// avgAlways: avgStale is false for the check under way (evictWaits).
	avgAlways bool
	// wantFig: the nodes last given to a FigureWatch view (neverfits.go).
	wantFig map[core.NodeID]time.Time
	// localSysMiB is this machine's system memory (0 when unknown) and
	// envFileLoad the LLAMA_RPC_FILE_LOAD the host child inherits; both are
	// read once, for the reload-from-cache figure and the load mode.
	localSysMiB int
	envFileLoad string

	cfg   *config.Config
	clock Clock
	view  fleet.View
	needs NeedProvider
	build instance.BuildOptions
	log   *slog.Logger
	// agents: membership is agents (a WorkerLauncher is set): worker launches
	// replace the pre-flight, node loss acts at once, recovery requests are off.
	agents   bool
	loadGate func() error

	now, last time.Time
	seq       uint64

	insts        map[string]*inst
	entries      map[string]*entry
	nodes        map[core.NodeID]*nodeState
	backoffs     map[string]*backoff
	waiters      map[uint64]*waiter
	leases       map[uint64]*lease
	runAvg       map[string]time.Duration        // drain.go: average run length per profile
	pins         map[*core.Profile][]string      // pin.go: one-off copies of a profile and their nodes
	needs_       map[string]NeedInfo             // provider results, before the estimate margin and learned figures
	learned      map[string]map[int]*learnedNeed // by shape and device count (needcount.go)
	derive       deriveState                     // what the derivation keeps between calls (derive.go)
	needOps      []needOp                        // learned-need store writes, taken by the runner after each step (needs.go)
	hostOHs      map[hostKey]*hostOH             // measured host overheads by shape, node and device count (hostoverhead.go)
	devOHs       map[hostKey]*hostOH             // measured per-device overheads by shape, node and device count (deviceoverhead.go)
	devEventsFn  DeviceHistory                   // loadFinished and per-device learnedNeed events, read once at start (dropCachedDevice)
	devChecked   bool                            // dropCachedDevice ran
	devCheckFrom time.Time                       // when dropCachedDevice first saw a node (it waits for the rows' nodes)
	hostEventsFn HostReadingEvents               // the history's learnedNeed events, read once for the rows of the release before device counts
	hostEvents   []history.Event                 // what hostEventsFn returned (nil: not read yet)
	persist      bool                            // a need store is attached: learned needs are queued for it
	buildRelease string                          // this build's release, stored with each measurement
	steps        uint64                          // events handled (checkGated)
	llamaBuild   string                          // the llama.cpp commit loads run on; "" when not known (needcount.go)
	releaseBuild func(string) string             // the llama build a release's release.json names; "" when not known (nil: none)
	servedBuild  string                          // the llama build the served release names, read at restore ("" : not known; nothing restored is trusted)
	gated        gateMark                        // the latest gate call (checkGated)
	warnedLow    map[string]bool                 // profiles whose too-low needs_mib was already warned about

	raw           map[core.NodeID]core.Node // view this step
	fresh         bool
	age           time.Duration
	seenReconnect time.Time
	excludedSeen  map[core.NodeID]bool          // excluded nodes already logged
	steady        map[core.NodeID]int           // stable.go: each node's live budget held still
	unplaced      map[core.NodeID]string        // why a listed node takes no new load (NodeGate), read with the view
	unplacedMsg   map[core.NodeID]string        // the message of that reason (NodeGateDetail), when it has one
	forcedGone    map[string]time.Time          // force_nodes name -> when it was first seen absent or offline in a live view
	health        map[core.NodeID]*workerHealth // worker health (workerhealth.go)
	probeGen      uint64
	recov         map[core.NodeID]*recoveryNode // recovery requests (recovery.go)
	nonceFn       func() string                 // nonce source; nil: crypto/rand (tests set it)

	shutdown   bool
	shutdownDL time.Time
	stopAll    bool
	killAll    bool
	finished   bool
	stopLogged bool // "shutdown stopping instances" logged
	doneLogged bool // "shutdown complete" logged

	recent  []LoadOutcome // finished load attempts, newest first (loadreport.go)
	effects []effect
	feed    []FeedEvent
	feedSeq uint64

	// History (history.go).
	hist        []history.Event
	memNext     map[core.NodeID]time.Time // next periodic nodeMemory sample per node that backs an instance
	lastRefusal map[string]RefusalStatus  // newest recorded refusal per profile (status only; not deduplicated)
	refused     map[string]*refusal       // last recorded refusal per profile and reason
	viewSeen    bool
	viewFresh   bool
	viewConn    bool
	staleAt     time.Time
	downAt      time.Time
	changed     bool

	// fitting is the instance whose own placement is being checked at exact
	// figures (fitAndLaunch); its reservation does not count against it.
	fitting *inst

	// preferMargin: the per-node margin counts as part of what a placement
	// must hold, for the try that prefers placements meeting it (tight.go).
	preferMargin bool
}

func newCore(cfg *config.Config, clock Clock, view fleet.View, needs NeedProvider, build instance.BuildOptions, log *slog.Logger) *schedCore {
	if log == nil {
		log = slog.Default()
	}
	return &schedCore{
		cfg: cfg, clock: clock, view: view, needs: needs, build: build, log: log,
		insts: map[string]*inst{}, entries: map[string]*entry{}, nodes: map[core.NodeID]*nodeState{},
		backoffs: map[string]*backoff{}, waiters: map[uint64]*waiter{}, leases: map[uint64]*lease{}, runAvg: map[string]time.Duration{}, pins: map[*core.Profile][]string{},
		needs_: map[string]NeedInfo{}, learned: map[string]map[int]*learnedNeed{}, warnedLow: map[string]bool{}, raw: map[core.NodeID]core.Node{}, excludedSeen: map[core.NodeID]bool{}, steady: map[core.NodeID]int{},
		forcedGone: map[string]time.Time{}, health: map[core.NodeID]*workerHealth{},
		recov: map[core.NodeID]*recoveryNode{}, memNext: map[core.NodeID]time.Time{}, refused: map[string]*refusal{}, lastRefusal: map[string]RefusalStatus{},
		localSysMiB: localSystemMiB(), envFileLoad: os.Getenv(EnvRPCFileLoad),
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
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range c.health { // a worker tracked as hung after node loss may be gone from the view and from c.nodes
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

// liveBudget is the memory the scheduler may use on a free node. On a
// shared-memory node it is the smaller of the fleet-based and the pre-flight
// figure (sharedBudget), because the worker's Vulkan device reports about
// 2.5 GB more free than the machine can give.
func (c *schedCore) liveBudget(id core.NodeID, n core.Node) int {
	old := c.freeMiB(id, n) - n.HeadroomMiB
	if n.SharedMemory {
		old, _ = c.sharedBudget(id, n)
	}
	if !c.cfg.Defaults.PlacementUsesReserved {
		return old
	}
	// reserve.go: the reserved figure (the lower of latest and average, less
	// what is reserved there but not measured yet), never above the old one.
	return min(old, c.reservedBudget(id, n))
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

// excluded: exclude_nodes keeps the node out of the pool (design 6). An
// excluded node is not reachable, so it is never eligible, never counted in
// capacity checks or plans, and never pre-flighted.
func (c *schedCore) excluded(n core.Node) bool { return c.cfg.Excluded(n.ID, n.Hostname) }

// noAuto: no_auto_nodes keeps automatic placement off the node (CR-9 review).
// It stays eligible; only a profile that names it in force_nodes uses it.
func (c *schedCore) noAuto(n core.Node) bool { return c.cfg.NoAuto(n.ID, n.Hostname) }

// autoOff: profile p is placed automatically (no force_nodes) and n is in
// no_auto_nodes, so p may neither be placed on n nor evict what runs there.
// A command profile is never automatic: it runs on the local node alone by
// definition, so no_auto_nodes does not apply to it (exclude_nodes does).
func (c *schedCore) autoOff(p *core.Profile, n core.Node) bool {
	return len(p.ForceNodes) == 0 && !localOnly(p) && c.noAuto(n)
}

// reachable: the local node, or online in a fresh view with an address.
func (c *schedCore) reachable(n core.Node) bool {
	if c.excluded(n) || c.unplaced[n.ID] == UnplacedNoTierMatch || c.unplaced[n.ID] == UnplacedBadFloor {
		return false // agents.go: a node without a tier is outside the pool
	}
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
	if c.noNewLoad(id) {
		return false
	}
	ns := c.nodes[id]
	return ns == nil || (ns.holder == "" && ns.claim == "" && ns.settle == setNone)
}

// noNewLoad: a paused, held or unplaced node takes no new load (agents.go). A
// load already on it continues, but it is neither placed on nor evicted for.
func (c *schedCore) noNewLoad(id core.NodeID) bool {
	n, _ := c.node(id)
	return n.Paused || c.agentHeld(id) || c.unplaced[id] != ""
}

// localOnly restricts kind: command profiles to the local node.
func localOnly(p *core.Profile) bool { return p.Kind == core.KindCommand }

// freeCandidates are the free nodes at live budgets, for Place.
func (c *schedCore) freeCandidates(p *core.Profile) []placer.Candidate {
	var out []placer.Candidate
	for _, id := range c.sortedNodeIDs() {
		n, ok := c.node(id)
		if !ok || !c.isFree(id) || (localOnly(p) && !n.Local) || c.autoOff(p, n) {
			continue
		}
		out = append(out, c.candidate(p, id, n, c.liveBudget(id, n), true))
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
		MaxPool: max(d.MaxPool, 1), MinShareMiB: d.MinShareMiB, DeviceOverheadMiB: d.DeviceOverheadMiB + info.DeviceMiB,
		SkewPermille: permille(d.SplitSkewMargin), EqualMiB: d.PlacementToleranceMiB,
	}
	if len(info.figs) > 0 { // needcount.go: the need of every device count the placer may try
		req.ByCount = make([]placer.CountNeed, max(req.MaxPool, len(p.ForceNodes), 1)+1)
		for k := 1; k < len(req.ByCount); k++ {
			req.ByCount[k] = placer.CountNeed{Need: info.at(k).Need}
		}
	}
	req.NodeDeviceMiB = c.nodeDeviceMiB(p, info) // deviceoverhead.go: nil until a figure is measured for p's shape
	c.applyReserve(&req, p, info)                // reserve.go: no change with the defaults
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

// need is the profile's memory need as every fit decision sees it: the
// provider's figure (cached), with the estimate margin on an estimate, and the
// measurement learned this run or restored at start where there is one
// (effectiveNeed).
func (c *schedCore) need(p *core.Profile) (NeedInfo, error) {
	base, ok := c.needs_[p.Name]
	if !ok {
		var err error
		if base, err = c.needs.Need(p); err != nil {
			return NeedInfo{}, err
		}
		c.checkModelBytes(p, base)
		c.needs_[p.Name] = base // after the check: a panic in it leaves nothing half done (readGuard)
	}
	return c.effectiveNeed(p, base), nil
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
