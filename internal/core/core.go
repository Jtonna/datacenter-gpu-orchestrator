// Package core holds the small plain data types shared across packages
// (config, placer, scheduler, instance). It has no logic beyond trivial
// helpers and imports no other internal package.
package core

import "time"

// NodeID is the fleet manager's member id (its MAC when known).
type NodeID string

// Node is one cluster node as the placer and scheduler see it. All memory
// is in MiB.
type Node struct {
	ID       NodeID // fleet id
	Hostname string // fleet hostname (may be shared by two consoles)
	Local    bool   // true for the host the scheduler runs on (the host machine)
	Online   bool   // currently online in a fresh enough view
	// Paused: the node takes no new load (agents membership, agent contract
	// 7.2). A load already on it continues; it stays counted in capacity.
	Paused bool
	// UnpausePending: Paused only because the operator's unpause has not
	// reached the node's agent yet (it takes load again within a heartbeat).
	UnpausePending bool
	Addr           string // rpc ip:port; empty for the local node
	// TierIndex is the 1-based position of the tier the node was matched to
	// (core.Tier.Index); 0 when it matches none.
	TierIndex int

	TotalMiB int // total device memory
	// SystemMiB is the machine's system memory (RAM) when known, 0 otherwise:
	// where the file cache lives. Used only for the reload-from-cache figure.
	SystemMiB int
	// CachedMiB is what the machine's file cache holds now (the agent's
	// stats cached_bytes), valid when CachedKnown. Used only for the
	// reload-from-cache line ("the file cache ... holds other files now").
	CachedMiB   int
	CachedKnown bool
	FreeMiB     int // free memory, already reduced per the shared-memory rule (design 7.1)
	// FreeUnknown: the fleet did not report the figures FreeMiB comes from, so
	// it is 0 only because "unknown is not free". The load memory watch must
	// not read such a zero as a real reading.
	FreeUnknown bool
	// FigureAt is when the memory reading behind FreeMiB was taken (agents
	// membership: the heartbeat that carried it); zero when not known. A
	// node something of ours just left is decided on a reading taken after
	// the release (scheduler, justReleased).
	FigureAt time.Time
	// StopSeenAt is when the scheduler's host last saw a worker launch on
	// the node end (its stop confirmed, or its exit); FiguresSinceStop
	// counts the memory readings received after that. A reading measured
	// after the workers freed their memory is certain from the second one
	// (agents membership; zero otherwise).
	StopSeenAt       time.Time
	FiguresSinceStop int
	// FreeAvgMiB is FreeMiB averaged over defaults.free_average_window, valid
	// when FreeAvgKnown (agents membership: from the heartbeats the pool
	// receives). placement_uses_reserved places on min(FreeMiB, FreeAvgMiB).
	FreeAvgMiB   int
	FreeAvgKnown bool
	// WatchdogFloorMiB is the memory watchdog floor of an agent node: the one
	// its agent reports, else the scheduler's setting (agent contract CR-9).
	// It is already taken off FreeMiB when the node shares system memory. 0
	// for a node that is not an agent's.
	WatchdogFloorMiB int
	// MemAvailableMiB is the machine's MemAvailable as its agent last
	// reported it, before the floor is taken off (shared-memory agent nodes;
	// 0 otherwise); MemAvailableAvgMiB its average over
	// defaults.free_average_window, valid when MemAvailableAvgKnown. Shown
	// in plans only: FreeMiB and FreeAvgMiB are what placement uses.
	MemAvailableMiB      int
	MemAvailableAvgMiB   int
	MemAvailableAvgKnown bool

	// Settings of the matched tier.
	Priority     int           // lower is preferred
	HeadroomMiB  int           // kept free, subtracted from the budget
	SharedMemory bool          // GPU memory is system RAM
	LinkMBs      float64       // link speed in MB/s, used to size load timeouts
	Settle       time.Duration // unusable for this long after an instance releases it

	// Recovery is the node's last reported recovery outcome (the fleet's
	// last_recovery_action); Present is false when there is none or the fleet
	// manager is too old to report it. Comparable, so Node stays comparable.
	Recovery RecoveryOutcome

	// Informational figures from the fleet (never used for decisions).
	RPCServerMemMiB   float64 // the rpc-server process's memory; valid when RPCServerMemKnown
	RPCServerMemKnown bool
	RPCServerVersion  string // rpc-server build id; empty when not reported
}

// RecoveryOutcome is what a node reported after acting on a recovery request.
type RecoveryOutcome struct {
	Present    bool
	Action     string
	At         int64  // unix ms
	Outcome    string // ok or failed
	Reason     string
	Nonce      string
	DurationMs int64
}

// LiveBudgetMiB is the memory the scheduler may use on a node nobody
// holds: free minus headroom. It can be negative.
func (n Node) LiveBudgetMiB() int { return n.FreeMiB - n.HeadroomMiB }

// Tier holds the settings of one matched tier (config tiers[]).
type Tier struct {
	Priority     int           // lower is preferred; at least 1
	HeadroomMiB  int           // kept free on the node
	SharedMemory bool          // GPU memory is system RAM
	LinkMBs      float64       // link speed in MB/s
	Settle       time.Duration // settle time after a release
	// Index is the tier's 1-based position in the configured list, set by
	// the matcher on the tier it returns (config.MatchTierNode); 0 in the
	// configuration itself.
	Index int
}

// Kind says how a profile is started.
type Kind string

const (
	// KindLlama is a llama-server-rpc instance whose placement the scheduler chooses. The default.
	KindLlama Kind = "llama"
	// KindCommand is a fixed command, always placed on the local node alone.
	KindCommand Kind = "command"
)

// Profile is one model-plus-context entry from config. Fields marked
// llama-only are zero for KindCommand and the reverse for command-only.
type Profile struct {
	Name string // profile name, never an alias
	Kind Kind   // KindLlama or KindCommand

	Model       string   // llama: file under models_dir
	Ctx         int      // llama: context per slot
	Parallel    int      // llama: slots; 1 when not configured
	KVQuant     string   // llama: f16, q8_0 or q4_0; f16 when not configured
	FlashAttn   bool     // llama: adds --flash-attn on
	AllowTiers  []int    // llama: tier priorities this profile may use; empty means all
	ForceNodes  []string // llama: node ids or hostnames, in device order; skips the fit check
	TensorSplit []int    // llama: explicit split, same length as ForceNodes
	ExtraArgs   []string // llama: appended last to the command line

	Cmd           string // command: command line containing ${PORT}
	CheckEndpoint string // command: readiness path

	NeedsMiB int      // measured memory need; 0 means estimate (required for command)
	Aliases  []string // old names that resolve to this profile
	// Generated marks a profile a tool made (a benchmark matrix, a sweep), as
	// opposed to one a person wrote. Nil means "not said": clients then fall
	// back to the naming rule (see api.IsGeneratedProfile). It changes nothing
	// about how the profile is placed or loaded.
	Generated *bool

	// TTL and StallTimeout use nil for "not set, use the default", because
	// an explicit 0 means "never unload" and "disabled" respectively.
	TTL          *time.Duration // idle unload time
	StallTimeout *time.Duration // max gap between streamed chunks
	// LoadTimeout of 0 means "use the default" (design 7.6 formula for
	// llama, command_load_timeout for command).
	LoadTimeout time.Duration
	// LoadStallTimeout is nil for "use the default"; an explicit 0 disables
	// the load stall rule for this profile.
	LoadStallTimeout *time.Duration
	// Host is where the llama-server host process runs: "" on the
	// scheduler's machine (profile host: server, the default), HostNode on
	// the first forced node, started there by its agent (phase 6).
	Host string
	// Description is free text for people (one line, at most 500
	// characters). It changes nothing in scheduling: a loaded instance is
	// not stale when only its description changed.
	Description string
}

// HostNode is Profile.Host for a host on a node; HostServer is the profile
// word for the default.
const (
	HostNode   = "node"
	HostServer = "server"
)

// PlacedNode is one node chosen for an instance.
type PlacedNode struct {
	ID        NodeID
	Addr      string // rpc ip:port; empty for the local node
	Local     bool
	BudgetMiB int // budget used for the tensor split
}

// Placement is where an instance will run.
type Placement struct {
	Nodes       []PlacedNode // in device order
	TensorSplit []int        // explicit split (from the profile); nil means derive from BudgetMiB
	// Host is the node that runs the host process (phase 6); it is Nodes[0].
	// Empty: the scheduler's machine, as before.
	Host NodeID
}
