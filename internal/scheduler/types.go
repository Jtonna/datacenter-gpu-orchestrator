package scheduler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Reason says why a request was not served. It is the "reason" named in the
// JSON error body (design 8.3, 8.4, 8.5).
type Reason string

const (
	ReasonUnknownModel      Reason = "unknown model"            // 404
	ReasonLoadFailed        Reason = "load failed"              // 502, with the child's output tail
	ReasonClusterBusy       Reason = "cluster busy"             // 503, queue_timeout
	ReasonForcedUnavailable Reason = "forced nodes unavailable" // 503, queue_timeout
	ReasonFleetStale        Reason = "fleet view stale"         // 503, queue_timeout
	ReasonBackoff           Reason = "backoff"                  // 503, profile backed off after a failed load
	ReasonShuttingDown      Reason = "shutting down"            // 503
	ReasonNodesQuarantined  Reason = "nodes quarantined"        // 503, queue_timeout: the nodes that would fit are quarantined
	ReasonNodeNoAddress     Reason = "node has no address"      // 503, queue_timeout: a node that would fit has no known rpc address
	ReasonDoesNotFit        Reason = "does not fit"             // 503, need exceeds cluster capacity (7.2)
	ReasonForcedDoesNotFit  Reason = "forced nodes do not fit"  // 503, forced_fit_check: reject and the need exceeds the forced nodes' free memory
	ReasonNodeExcluded      Reason = "node excluded"            // 503, exclude_nodes keeps out the nodes this needs (permanent for a forced profile)
	ReasonUnloaded          Reason = "unloaded"                 // 503, a loading instance was force-unloaded
	ReasonPinConflict       Reason = "already loaded or queued" // 409, a load on chosen nodes while the model is loaded or queued (pin.go)
	ReasonPinUnusable       Reason = "chosen zone unusable"     // 409, a chosen node is lost, offline, excluded, paused, set aside or outside the pool (pin.go)
	ReasonClientGone        Reason = "client gone"              // 499, the caller's context ended
	ReasonInternal          Reason = "internal error"           // 500, a read-only query panicked (recovered, counted in /api/system)
)

// Error is the error type of every failed Acquire or Load. Status is the
// HTTP status the API layer should send.
type Error struct {
	Status  int
	Reason  Reason
	Message string
	// RetryAfter is set for every 503 (whole seconds, at least 1 s) except
	// ReasonDoesNotFit, which is permanent and carries none (zero).
	RetryAfter time.Duration
	// Tail (502) holds the last lines of the child's output.
	Tail []string
	// NeedMiB, RequiredMiB and CapacityMiB are set for ReasonDoesNotFit.
	NeedMiB, RequiredMiB, CapacityMiB int
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%d %s", e.Status, e.Reason)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if len(e.Tail) > 0 {
		msg += "\n" + strings.Join(e.Tail, "\n")
	}
	return msg
}

// StatusClientClosed is the status recorded for a request whose client left
// while it waited (nginx's 499). It is never sent: the client is gone.
const StatusClientClosed = 499

func errUnknown(model string) *Error {
	return &Error{Status: http.StatusNotFound, Reason: ReasonUnknownModel, Message: fmt.Sprintf("model %q not found", model)}
}

func err503(r Reason, msg string, retry time.Duration) *Error {
	if retry < time.Second {
		retry = time.Second
	}
	retry = retry.Round(time.Second)
	return &Error{Status: http.StatusServiceUnavailable, Reason: r, Message: msg, RetryAfter: retry}
}

func err502(msg string, tail []string) *Error {
	return &Error{Status: http.StatusBadGateway, Reason: ReasonLoadFailed, Message: msg, Tail: tail}
}

// queueRetryAfter is the Retry-After of a queue_timeout answer (design 8.3).
const queueRetryAfter = 30 * time.Second

// RunningState is the state shown by /running and modelStatus (design 8.1
// and A.1, A.3): starting, ready (also for draining), stopping, stopped.
type RunningState string

const (
	RunningStarting RunningState = "starting"
	RunningReady    RunningState = "ready"
	RunningStopping RunningState = "stopping"
	RunningStopped  RunningState = "stopped"
)

// RunningInstance is one /running entry (design A.1).
type RunningInstance struct {
	Profile    string
	State      RunningState
	Cmd        string    // the command line as one string
	ReadySince time.Time // zero until ready
	// LoadProgress is 0 to 1 while a remote load is under way (never 1: the
	// instance is ready when it is done), else nil. See InstanceStatus.
	LoadProgress *float64
	// Load is the structured load report while the child loads (loadreport.go).
	Load *LoadReport
}

// ProfileState is one modelStatus entry (design A.3).
type ProfileState struct {
	Name       string
	State      RunningState
	ReadySince time.Time // zero unless ready
}

// FeedKind says what a FeedEvent reports.
type FeedKind int

const (
	// FeedInstance: an instance changed state (Profile, State, ReadySince).
	// Re-read Profiles for the full modelStatus list.
	FeedInstance FeedKind = iota + 1
	// FeedAdmitted: a request was admitted to an instance (RequestID,
	// Profile, Time). It is the inflight "upsert".
	FeedAdmitted
	// FeedFinished: an admitted request ended (its lease was released).
	// The HTTP layer adds the path and status it knows.
	FeedFinished
	// FeedLoadStarted: a load attempt began (LoadStarted).
	FeedLoadStarted
	// FeedLoadFinished: a load attempt ended (Outcome).
	FeedLoadFinished
	// FeedWorkerHealth: a watched worker's health state changed (Worker).
	FeedWorkerHealth
	// FeedRecovery: a recovery request was sent, or a node's outcome was seen (Recovery).
	FeedRecovery
)

// FeedEvent is one entry of the subscription feed (design A.3, A.4).
type FeedEvent struct {
	// Seq increases by one per event across the whole scheduler; a gap
	// seen by a subscriber means events were dropped for it.
	Seq         uint64
	Kind        FeedKind
	Time        time.Time
	Profile     string
	State       RunningState       // FeedInstance
	ReadySince  time.Time          // FeedInstance, when ready
	RequestID   uint64             // FeedAdmitted, FeedFinished
	LoadStarted *LoadStart         // FeedLoadStarted
	Outcome     *LoadOutcome       // FeedLoadFinished
	Worker      *WorkerHealthEvent // FeedWorkerHealth
	Recovery    *RecoveryEvent     // FeedRecovery
}

// Status is the /api/status document.
type Status struct {
	Time         time.Time
	ShuttingDown bool
	Instances    []InstanceStatus
	Queue        []EntryStatus // oldest first; claims are shown on their entries
	Quarantines  []QuarantineStatus
	Backoffs     []BackoffStatus
	// LastRefusals: per profile, the newest answer that refused it without
	// serving it (does not fit, backoff, forced nodes unavailable, ...), so a
	// client that started a load can see why it never began. Not cleared when
	// the profile later loads; compare At with the time of your own request.
	LastRefusals []RefusalStatus
	// RecentLoads: load attempts finished within RecentLoadRetention, newest first.
	RecentLoads []LoadOutcome
	View        ViewStatus
	// LearnedNeeds: per profile, the memory the last successful loads of this
	// run measured and the need fit decisions use now (memfit.go). Empty until
	// a profile has loaded.
	LearnedNeeds map[string]LearnedNeed
	// ConfigRevision is the configuration revision in force; Configured is false while the built-in defaults run (nothing imported yet).
	ConfigRevision int64
	Configured     bool
	// ServerHostMemory sums, over the instances loaded on the scheduler's
	// machine ONLY, the host-memory term nothing places there (hostmem.go):
	// context checkpoints and prompt cache. NodeHostMemory is the same term
	// per console that hosts a loaded model (empty when none does).
	ServerHostMemory ServerHostMemory
	NodeHostMemory   []NodeHostMemory
}

// InstanceStatus describes one instance.
type InstanceStatus struct {
	Profile string
	// Ctx is the context per slot the instance was planned and launched with
	// (llama); CtxServed the server's own figure (/props), nil until read or
	// when it could not be; CtxNote the trained-context cap, "runs at 2,048,
	// profile says 8,192", or "context not confirmed: ..." (realctx.go).
	Ctx            int `view:"omitempty"`
	CtxServed      *int
	CtxNote        string `view:"omitempty"`
	State          string // starting, ready, draining, stopping
	RunningState   RunningState
	Phase          string // starting only: preflight, launching, loading
	Addr           string // 127.0.0.1:port, or the host node's address, once the child runs
	Cmd            string
	LogFile        string // file holding the child's full output; empty when not kept
	Created        time.Time
	ReadySince     time.Time
	LastUsed       time.Time
	ModelFile      string // the profile model file (kind llama)
	RequestsServed int    // requests admitted since the instance started
	LoadDuration   time.Duration
	LoadDeadline   time.Time // starting, child running
	InFlight       int
	Waiters        int
	// StaleProfile: the profile was changed or deleted after this instance started; it keeps the definition it started with until its next load.
	StaleProfile bool
	VictimOf     string // profile of the claimed entry this instance drains or stops for
	// Drain: an operator asked it to stop once its runs finish (drain.go);
	// DrainEtaS estimates when, while it drains (nil otherwise, or when no
	// run of the profile has finished yet). Both are left out of the status
	// of an instance not drained on request.
	Drain     bool `view:"omitempty"`
	DrainEtaS *float64
	// RunsEtaS is the same estimate for any ready or draining instance with
	// runs in flight: what a drain would wait (the zone action's dry run).
	// Internal: never in the status document (view:"-").
	RunsEtaS *float64 `view:"-"`
	// PinnedNodes: loaded on these nodes by a one-off load on chosen nodes
	// (pin.go), not by its profile's force_nodes; absent otherwise.
	PinnedNodes []string `view:"omitempty"`
	// Production and Scope: a run-scoped instance's production profile and
	// scope (scoped.go), so a page counts it for itself and not as its
	// production profile; absent on any other instance.
	Production string `view:"omitempty"`
	Scope      string `view:"omitempty"`
	// LoadMode is how the instance's nodes get the weights: file_read (the
	// workers read the model file), pushed (the host sends them), local (no
	// remote worker); empty for a command profile or when not known.
	LoadMode string
	Nodes    []PlacedNodeStatus
	// HostMemory lists host-side buffers from the load log (CPU, *_Host);
	// they are not part of the per-device sums.
	HostMemory []DeviceMemory
	// Unmatched lists device buffers whose name matched no placed node.
	Unmatched []DeviceMemory
	// DeviceTotalMiB is the sum of every device buffer from the load log:
	// the figure to write into needs_mib (design 7.2).
	DeviceTotalMiB float64

	// Load progress, set only while the instance is starting; nil fields are
	// left out of the JSON. A load with no remote worker has no upload, so it
	// has only LoadElapsedS. The fields are plain numbers in seconds and bytes.
	LoadElapsedS       *float64 // since the instance was created
	LoadBytesTotal     *int64   // what must be uploaded to the remote workers
	LoadBytesSent      *int64   // bytes the workers have acknowledged; only when measured
	LoadProgress       *float64 // 0 to 0.99 (1 only by becoming ready)
	LoadEtaS           *float64 // seconds left at the current rate
	LoadProgressSource *string  // "measured" or "estimated"
	// Load is the structured version of the above plus per-worker progress;
	// set while the child loads.
	Load *LoadReport
	// Host is where llama-server runs: "server" (the scheduler's machine)
	// or "node" (HostNode, phase 6); empty for a command profile. Parallel
	// is the profile's parallel slots (at least 1): busy = InFlight/Parallel.
	// Additive (zonefacts.go).
	// None is said while zero (view:"omitempty").
	Host     string      `view:"omitempty"`
	HostNode core.NodeID `view:"omitempty"`
	Parallel int         `view:"omitempty"`
}

// PlacedNodeStatus is one node of an instance's placement with the memory
// the load log reported for it (matched by endpoint, design 7.2).
type PlacedNodeStatus struct {
	ID        core.NodeID
	Addr      string
	Local     bool
	BudgetMiB int
	Memory    []DeviceMemory
	// Role is host (the member that runs llama-server) or helper (an RPC
	// worker); Share is its fraction of the model (0 to 1): its tensor split
	// weight over the sum, exactly the split the launch was given. Shares
	// of an instance's members add up to 1. Additive (zonefacts.go).
	Role  string  `view:"omitempty"`
	Share float64 `view:"omitempty"`
}

// DeviceMemory is one buffer name's sizes from the load log, in MiB.
type DeviceMemory struct {
	Name       string
	ModelMiB   float64
	KVMiB      float64
	ComputeMiB float64
	TotalMiB   float64
}

// EntryStatus is one queue entry.
type EntryStatus struct {
	Profile        string
	Created        time.Time
	Age            time.Duration
	Clock          time.Duration // accumulated unclaimed time (queue_timeout clock)
	ClockRunning   bool
	Waiters        int
	Claimed        bool
	Claim          []core.NodeID
	Victims        []string
	VictimsStopped bool
	// WaitReason says why an unclaimed entry has not been placed: the placer's
	// reason, or a quarantined / no-address / forced-node-absent cause.
	WaitReason string
}

// QuarantineStatus is one quarantined node (design 8.4).
type QuarantineStatus struct {
	Node  core.NodeID
	Until time.Time
	Level int // number of consecutive failed pre-flights
	// Reason is why the node was set aside (preflight_failed,
	// agent_unreachable, worker_hung, node_lost), Message the cause in
	// words, Profile the load it happened in (when one), Since when.
	// Additive (zonefacts.go).
	Reason  string    `view:"omitempty"`
	Message string    `view:"omitempty"`
	Profile string    `view:"omitempty"`
	Since   time.Time `view:"omitempty"`
}

// BackoffStatus is one backed-off profile (design 8.4).
type BackoffStatus struct {
	Profile string
	Until   time.Time
	Level   int
}

// RefusalStatus is the newest refusal of one profile.
type RefusalStatus struct {
	Profile  string
	At       time.Time
	Status   int
	Reason   string
	Message  string
	BenchRun string // the X-Bench-Run id of the request refused, when it had one
}

// ViewStatus is the node view as the scheduler uses it.
type ViewStatus struct {
	Age       time.Duration // -1 before the first byte
	Connected bool
	Stale     bool
	Nodes     []NodeStatus
}

// NodeStatus is one node with its budgets and bookkeeping (design 7.1).
type NodeStatus struct {
	ID       core.NodeID
	Hostname string
	Local    bool
	Online   bool
	Addr     string
	Priority int
	// TierIndex is the 1-based position of the tier the node view matched
	// (core.Node.TierIndex); not shown in the status (the Tiers page reads
	// GET /api/config/tiers tier_zones).
	TierIndex int  `view:"-"`
	Eligible  bool // ignoring the profile-specific rules (allow_tiers, min_share)
	Excluded  bool // kept out of the pool by exclude_nodes; never eligible
	// ExcludedBy is the exclude_nodes entry that matches the node ("" when
	// not excluded).
	ExcludedBy string
	// Auto is false for a node in no_auto_nodes (CR-9 review): automatic
	// placement never picks it; a profile that forces it still may.
	Auto bool
	// Unplaced is why the node takes no new load although it is listed
	// (agents membership): no_tier_match, programs_not_ready,
	// model_source_unreachable, orphan_launch, silent or agent_unreachable.
	// Empty when it may take one. UnplacedMessage says more when the reason
	// has more to say (agent_unreachable: the address, the port, what to check).
	Unplaced        string
	UnplacedMessage string
	TotalMiB        int
	FreeMiB         int // figure used for the live budget (fleet, fetch, or pre-flight override)
	LiveBudgetMiB   int
	EmptyMiB        int
	PlanBudgetMiB   int
	OverrideFreeMiB int       // pre-flight override, when OverrideUntil is set
	OverrideUntil   time.Time // zero when no override
	Holder          string    // profile of the instance holding it
	ClaimedBy       string    // profile of the entry claiming it
	Settling        string    // "", "settling", "fetching", "fetch-failed"
	SettleUntil     time.Time
	Quarantined     time.Time // zero unless quarantined
	MissingSince    time.Time // node-loss counter start (design 8.4), zero if not counting
	// Health is set for a remote worker an instance uses, while worker health
	// detection is on (design 8.4a); nil fields are left out of the JSON.
	Health *WorkerHealthStatus
	// Recovery is set once a recovery request was sent for the node or the node
	// reported an outcome (design 8.4b); nil is left out of the JSON.
	Recovery *RecoveryStatus
	// RPCServerMemMiB and RPCServerVersion are what the fleet reports about the
	// node's rpc-server; informational only, left out when not reported.
	RPCServerMemMiB  *float64
	RPCServerVersion *string

	// The memory reservation rule (reserve.go). Reserved lists what is
	// placed, loading or loaded on the node with its reserved figure;
	// MeasuredUsedMiB is what the node gave up for it since it was placed
	// (nil when not measurable) and DriftMiB that less ReservedMiB.
	// DriftWarning is set when the drift exceeds reserved_drift_warn_mib.
	// FreeForNewLoadMiB is the node's budget for a new load now (before the
	// profile's own margin). FreeAvgMiB is the short average of the free
	// figure (nil when not known).
	Reserved          []NodeReservation
	ReservedMiB       int
	MeasuredUsedMiB   *int
	DriftMiB          *int
	DriftWarning      string
	FreeForNewLoadMiB int
	FreeAvgMiB        *int

	// QuarantineReason is why the node is set aside while Quarantined is
	// set (the reason word of its QuarantineStatus). FreeAt is when a node
	// that is claimed or settling is expected to be free, FreeBasis how
	// that was worked out: settling (the settle end), fetching (now: its
	// free figure is being read), stopping (the victim stops; now plus the
	// node's settle), draining (the victim still serves; no FreeAt) or
	// claimed (nothing left on it: now). Additive (zonefacts.go).
	// None is said while zero (view:"omitempty").
	QuarantineReason string    `view:"omitempty"`
	FreeAt           time.Time `view:"omitempty"`
	FreeBasis        string    `view:"omitempty"`
}

// RecoveryStatus is status.recovery of a node (design 8.4b).
type RecoveryStatus struct {
	LastRequest *RecoveryRequestStatus
	// LastOutcome is the node's last_recovery_action as the fleet reports it.
	LastOutcome *RecoveryOutcomeStatus
	// Step is how many steps of the actions list the current hung episode has sent.
	Step int
	// EpisodeStarted is unix ms; set only while the node is in a hung episode.
	EpisodeStarted *int64
}

// RecoveryRequestStatus is the last request the scheduler sent for a node.
type RecoveryRequestStatus struct {
	Action     string
	Nonce      string
	SentAt     int64  // unix ms
	Status     string // pending, stored, refused or error
	HTTPStatus int    // 0 until an answer arrives (or on a timeout)
}

// RecoveryOutcomeStatus is a node's reported recovery outcome.
type RecoveryOutcomeStatus struct {
	Action     string
	At         int64 // unix ms
	Outcome    string
	Reason     string
	Nonce      string
	DurationMs int64
}

// RecoveryEvent is the payload of the recoveryRequest feed event. Phase is
// "sent" (a request was posted) or "outcome" (the node's outcome was seen).
type RecoveryEvent struct {
	Node     core.NodeID
	Phase    string
	Recovery *RecoveryStatus
}

// WorkerHealthStatus is the health of one worker (design 8.4a).
type WorkerHealthStatus struct {
	State      string // ok, suspect, hung
	Since      int64  // unix ms: when the state was entered
	Channels   WorkerChannels
	SilentForS map[string]float64 // only the silent channels: seconds since each became silent
}

// WorkerChannels are the three health channels of a worker: ok, silent or unknown.
type WorkerChannels struct {
	Upload    string
	Heartbeat string
	Probe     string
}

// WorkerHealthEvent is the payload of the workerHealth feed event. Health is
// nil when the worker is no longer tracked (no instance uses it).
type WorkerHealthEvent struct {
	Node   core.NodeID
	Addr   string
	Health *WorkerHealthStatus
}

// PlanAction is the outcome of a dry-run Plan.
type PlanAction string

const (
	PlanServe   PlanAction = "serve"   // a ready instance would take the request
	PlanWait    PlanAction = "wait"    // the profile is starting; the request would wait for it
	PlanStart   PlanAction = "start"   // free nodes fit: an instance would start (8.3 step 1)
	PlanEvict   PlanAction = "evict"   // a plan with victims would claim nodes (step 2)
	PlanQueue   PlanAction = "queue"   // the request would wait in the queue (step 3, or old instance still present)
	PlanBackoff PlanAction = "backoff" // 503 at once (8.4)
	PlanReject  PlanAction = "reject"  // 503 at once: does not fit (7.2), or shutting down
)

// PlanReport is the answer to POST /api/plan. Nothing is changed by it.
type PlanReport struct {
	Profile string
	// Ctx is the context per slot the plan is for (llama), and CtxNote says
	// when that is not the configured ctx: "ctx 8,192 is above this model's
	// trained context of 2,048; the server runs it at 2,048" (realctx.go).
	Ctx       int
	CtxNote   string `view:"omitempty"`
	Alias     bool
	Action    PlanAction
	Instance  string // state of the profile's instance, if any
	Placement []PlacedNodeStatus
	Victims   []PlanVictim
	Reason    string // why there is no placement (queue, reject, backoff)
	// ExcludedBy is the exclude_nodes entry that refuses this profile: it
	// forces a node the entry names (or is a command profile and the local
	// node is excluded). Empty otherwise.
	ExcludedBy string
	NeedMiB    int // the profile's need before the estimate margin
	Measured   bool
	// FitNeedMiB is the need the fit rules use (NeedMiB with the estimate
	// margin when it is an estimate), and NeedBasis says where it comes from:
	// "measured, needs_mib", "estimated, +5% margin; the estimate is N MiB" or
	// "measured this run".
	FitNeedMiB int
	NeedBasis  string
	// DerivedFromCtx: the need is derived from a measurement of the same
	// model at this other context (NeedBasis says so); 0 otherwise.
	DerivedFromCtx int
	// DerivedSlope: the need is read from the model's own line on one device
	// count (NeedBasis says so); nil otherwise.
	DerivedSlope *DerivedSlope
	// Warning is set when defaults.forced_fit_check is warn and the forced
	// nodes' free memory is below the need: the load would start anyway.
	Warning string
	// RequiredMiB and CapacityMiB are set for a reject on capacity.
	RequiredMiB, CapacityMiB int
	// LoadMode is how the placement's nodes would get the weights
	// (file_read, pushed, local; empty when not known), and ReloadFromCache
	// the reload rule for each placed node. Both are set only with a
	// placement.
	LoadMode        string
	ReloadFromCache []NodeReload
	// Memory is the pre-calculation of the memory reservation rule: per
	// node, what the profile will take assuming its context is filled
	// completely, what the node has free, what is reserved there and what
	// remains; and whether it fits. Nil when the need is unknown.
	Memory *PlanMemory
	// TightFit: the plan starts (or evicts for) a tight fit: on at least
	// one node the load fits only without its margin (Memory says where;
	// Reason says "tight fit: N MiB spare on <node>, margin M MiB not met").
	TightFit bool
	// Host is where the profile's llama-server runs: core.HostServer (the
	// scheduler's machine) or core.HostNode (phase 6). HostNode is the node
	// that hosts (the first forced node), HostOverheadMiB what hosting adds
	// to its reservation (host_overhead_mib), HostProblem why it cannot host
	// now (for example host_not_capable); all empty or 0 for the server.
	Host            string
	HostNode        core.NodeID
	HostOverheadMiB int
	// HostOverheadSource says which figure HostOverheadMiB is:
	// HostOverheadFromSetting (host_overhead_mib) or HostOverheadMeasured
	// (measured on the host node for this shape, at
	// HostOverheadMeasuredAt; hostoverhead.go). Empty for the server.
	HostOverheadSource     string    `view:"omitempty"`
	HostOverheadMeasuredAt time.Time `view:"omitempty"`
	HostProblem            string
	// HostMemory is the host process's own memory where it is hosted
	// (hostmem.go): in the host node's reservation when Counted, never
	// placed on the scheduler's machine, where HostMemoryNote says so.
	// Nil for a command profile or an unreadable header.
	HostMemory     *HostMemory `view:"omitempty"`
	HostMemoryNote string      `view:"omitempty"`
	// HostRoom is the scheduler machine's budget for a profile hosted
	// there (hostbudget.go), when count_host_memory is on and the plan
	// would start or evict.
	HostRoom *HostRoom `view:"omitempty"`
}

// PlanMemory is the memory section of a plan (reserve.go).
type PlanMemory struct {
	Nodes []PlanMemoryNode
	// Fits: every node of the placement keeps remains_mib >= 0. With
	// Hypothetical (no placement was found) it is false.
	Fits bool
	// ShortNode and ShortfallMiB name the node that falls shortest and by
	// how much, when it does not fit.
	ShortNode    string
	ShortfallMiB int
	// Hypothetical: there is no placement; the nodes are the ones it would
	// be spread over (the forced nodes, or the best max_pool nodes).
	Hypothetical bool
	// PlacementUsesReserved is the setting the available figures follow.
	PlacementUsesReserved bool
	// TightFit: it fits, and on at least one node only without the margin
	// (tight.go): the load starts with less than the margin spare there.
	TightFit bool
	// Decides: the decision holds every placement to Fits (any reservation
	// setting is on). With the defaults Fits is shown only; the old rules
	// decide.
	Decides bool
	Note    string
}

// PlanMemoryNode is one node of PlanMemory. AvailableMiB is the figure the
// placement is held to (the node's budget): with placement_uses_reserved,
// min(free now, free average) less what is reserved there and not measured
// yet, less headroom; else free now less headroom. RemainsMiB = AvailableMiB
// - WillTakeMiB - MarginMiB.
type PlanMemoryNode struct {
	ID             core.NodeID
	Node           string
	Shared         bool
	TotalMiB       int
	WeightsMiB     int
	ContextMiB     int
	FillReserveMiB int
	WillTakeMiB    int
	Basis          string // measured, peak or estimated
	FreeNowMiB     int
	FreeAvgMiB     int
	FreeAvgKnown   bool
	ReservedBy     []PlanReservedBy
	HeadroomMiB    int
	AvailableMiB   int
	MarginMiB      int
	MarginRule     string
	RemainsMiB     int
	// HostOverheadMiB is host_overhead_mib on the node that hosts the load
	// (in ContextMiB and WillTakeMiB already); 0 on any other node.
	HostOverheadMiB int
	// HostMemoryMiB is the host process's checkpoints and prompt cache on
	// the host node when count_host_memory counts them (hostmem.go; in
	// ContextMiB and WillTakeMiB already); 0 otherwise.
	HostMemoryMiB int `view:"omitempty"`
	// HostOverheadSource and HostOverheadMeasuredAt say, on the host row,
	// which figure HostOverheadMiB is (as PlanReport's); empty elsewhere.
	HostOverheadSource     string    `view:"omitempty"`
	HostOverheadMeasuredAt time.Time `view:"omitempty"`
	// DeviceOverheadMiB is, for an estimated need, the per-device overhead
	// in WillTakeMiB on this node (device_overhead_mib, or the figure
	// measured on this node for this shape and device count), with its
	// source and date; zero and empty for a measured need.
	DeviceOverheadMiB        int       `view:"omitempty"`
	DeviceOverheadSource     string    `view:"omitempty"`
	DeviceOverheadMeasuredAt time.Time `view:"omitempty"`
	// SpareMiB is AvailableMiB - WillTakeMiB: what the node keeps beyond
	// the load before the margin. Tight: the node holds the load without
	// its margin only (0 <= spare < margin; tight.go), short of the margin
	// by MarginShortMiB.
	SpareMiB       int
	Tight          bool
	MarginShortMiB int

	// What the row's figures are built from, so none is taken off twice
	// (planfigures.go). FreeNowMiB is after the watchdog floor and
	// AvailableMiB after the floor and the headroom; PlaceableMiB is
	// AvailableMiB under its plain name. On a shared-memory node:
	// MemAvailableMiB is the console's reported MemAvailable (the current
	// reading), MemAvailableAvgMiB its average when known, and
	// MemAvailableReading which one the row is built from ("current" or
	// "average"); WatchdogFloorMiB the floor. Then
	//
	//	reading - floor - headroom (+ PlaceableAdjustMiB) = PlaceableMiB
	//
	// where PlaceableAdjustMiB (absent when 0) is whatever else the rules
	// changed, said in PlaceableAdjustWhy. PredictedLowestAvailableMiB is
	// MemAvailableMiB - WillTakeMiB: the figure to compare with the
	// console's own available memory during the load. All absent on a node
	// with memory of its own.
	PlaceableMiB                int
	MemAvailableMiB             *int
	MemAvailableAvgMiB          *int
	MemAvailableReading         *string
	WatchdogFloorMiB            *int
	PlaceableAdjustMiB          *int
	PlaceableAdjustWhy          *string
	PredictedLowestAvailableMiB *int
}

// PlanReservedBy is what another (or the same) profile holds on a node.
type PlanReservedBy struct {
	Profile string
	MiB     int
	State   string
}

// NodeReservation is one entry of a node's reserved list.
type NodeReservation struct {
	Profile        string
	State          string
	Basis          string
	Ctx            int
	WeightsMiB     int
	ContextMiB     int
	FillReserveMiB int
	TotalMiB       int
}

// PlanVictim is an instance a plan would stop or drain.
type PlanVictim struct {
	Profile string
	Busy    bool // would drain rather than stop
}
