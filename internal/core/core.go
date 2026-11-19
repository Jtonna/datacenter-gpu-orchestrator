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
	Local    bool   // true for the host the scheduler runs on (the OMEN)
	Online   bool   // currently online in a fresh enough view
	Addr     string // rpc ip:port; empty for the local node

	TotalMiB int // total device memory
	FreeMiB  int // free memory, already reduced per the shared-memory rule (design 7.1)
	// FreeUnknown: the fleet did not report the figures FreeMiB comes from, so
	// it is 0 only because "unknown is not free". The load memory watch must
	// not read such a zero as a real reading.
	FreeUnknown bool

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
}

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
}
