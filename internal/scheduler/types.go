package scheduler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
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
	ReasonClientGone        Reason = "client gone"              // 499, the caller's context ended
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
	// RecentLoads: load attempts finished within RecentLoadRetention, newest first.
	RecentLoads []LoadOutcome
	View        ViewStatus
	// LearnedNeeds: per profile, the memory the last successful loads of this
	// run measured and the need fit decisions use now (memfit.go). Empty until
	// a profile has loaded.
	LearnedNeeds map[string]LearnedNeed
}

// InstanceStatus describes one instance.
type InstanceStatus struct {
	Profile        string
	State          string // starting, ready, draining, stopping
	RunningState   RunningState
	Phase          string // starting only: preflight, launching, loading
	Addr           string // 127.0.0.1:port once the child runs
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
	VictimOf       string // profile of the claimed entry this instance drains or stops for
	Nodes          []PlacedNodeStatus
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
}

// PlacedNodeStatus is one node of an instance's placement with the memory
// the load log reported for it (matched by endpoint, design 7.2).
type PlacedNodeStatus struct {
	ID        core.NodeID
	Addr      string
	Local     bool
	BudgetMiB int
	Memory    []DeviceMemory
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
}

// BackoffStatus is one backed-off profile (design 8.4).
type BackoffStatus struct {
	Profile string
	Until   time.Time
	Level   int
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
	ID              core.NodeID
	Hostname        string
	Local           bool
	Online          bool
	Addr            string
	Priority        int
	Eligible        bool // ignoring the profile-specific rules (allow_tiers, min_share)
	Excluded        bool // kept out of the pool by exclude_nodes; never eligible
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
	Profile   string
	Alias     bool
	Action    PlanAction
	Instance  string // state of the profile's instance, if any
	Placement []PlacedNodeStatus
	Victims   []PlanVictim
	Reason    string // why there is no placement (queue, reject, backoff)
	NeedMiB   int    // the profile's need before the estimate margin
	Measured  bool
	// FitNeedMiB is the need the fit rules use (NeedMiB with the estimate
	// margin when it is an estimate), and NeedBasis says where it comes from:
	// "measured, needs_mib", "estimated, +5% margin; the estimate is N MiB" or
	// "measured this run".
	FitNeedMiB int
	NeedBasis  string
	// Warning is set when defaults.forced_fit_check is warn and the forced
	// nodes' free memory is below the need: the load would start anyway.
	Warning string
	// RequiredMiB and CapacityMiB are set for a reject on capacity.
	RequiredMiB, CapacityMiB int
}

// PlanVictim is an instance a plan would stop or drain.
type PlanVictim struct {
	Profile string
	Busy    bool // would drain rather than stop
}
