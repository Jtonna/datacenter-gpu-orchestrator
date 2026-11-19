package config

import (
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// Config is the parsed, validated configuration file.
type Config struct {
	Listen    string
	Fleet     Fleet
	ModelsDir string
	// ChildLogDir, when set, receives one file per child start holding its
	// complete stdout and stderr; ChildLogKeep is how many such files to keep.
	ChildLogDir  string
	ChildLogKeep int
	// HistoryDir, when set, holds the event history (JSON Lines, rotated by
	// size); empty turns history off. HistoryMaxFileMiB is the size at which a
	// file is rotated, HistoryKeepFiles how many files are kept and
	// HistoryMemoryEvents how many recent events are held in memory for queries.
	HistoryDir          string
	HistoryMaxFileMiB   int
	HistoryKeepFiles    int
	HistoryMemoryEvents int
	LocalProbeCmd       string
	// DataDir holds the scheduler database (benchmarks, runs, results) as
	// scheduler.db; empty turns the benchmark feature off. WSMaxClients is the
	// most concurrent /ws clients.
	DataDir         string
	WSMaxClients    int
	Bench           Bench
	benchEnabledSet *bool // bench.enabled as written, to reject true without data_dir
	Defaults        Defaults
	Tiers           []TierRule
	NodeOverrides   map[string]NodeOverride
	// ExcludeNodes lists nodes that are never used: each entry is a node id or a
	// hostname glob (case-insensitive). See Excluded.
	ExcludeNodes []string
	// Recovery is the optional recovery-request feature (design 8.4b).
	Recovery Recovery
	Profiles map[string]*core.Profile
}

// Recovery actions, in the only order they may be configured.
const (
	RecoveryRestartRPC    = "restart_rpc_server"
	RecoveryRestartDocker = "restart_docker"
	RecoveryReboot        = "reboot"
)

// RecoveryActions lists the allowed actions from least to most disruptive.
var RecoveryActions = []string{RecoveryRestartRPC, RecoveryRestartDocker, RecoveryReboot}

// Recovery configures recovery requests for hung workers (design 8.4b).
type Recovery struct {
	Enabled bool // false: nothing is ever sent
	// Actions is the escalation order; each is allowed once.
	Actions []string
	// MinInterval is the least time between two requests for one node.
	MinInterval time.Duration
	// StepWait is how long a step gets to take effect before the next one.
	StepWait time.Duration
	// RequestTTL is how long a request stays valid (expires_at - issued_at).
	RequestTTL time.Duration
	// HTTPTimeout bounds the POST to the fleet manager.
	HTTPTimeout time.Duration
}

// DefaultRecovery returns the built-in recovery settings (disabled).
func DefaultRecovery() Recovery {
	return Recovery{Actions: []string{RecoveryRestartRPC}, MinInterval: 120 * time.Second,
		StepWait: 90 * time.Second, RequestTTL: 45 * time.Second, HTTPTimeout: 3 * time.Second}
}

// Fleet is the fleet console connection.
type Fleet struct {
	URL        string
	LocalNode  string
	StaleAfter time.Duration
}

// Defaults are the policy values used when a profile does not override them.
type Defaults struct {
	TTL                time.Duration
	MinHold            time.Duration
	MaxHold            time.Duration
	DrainAfter         time.Duration
	QueueTimeout       time.Duration
	StallTimeout       time.Duration
	CommandLoadTimeout time.Duration
	// LoadStallTimeout fails a load whose measured upload acknowledges no new
	// bytes for this long; 0 disables the rule.
	LoadStallTimeout  time.Duration
	ShutdownGrace     time.Duration
	MaxPool           int
	DeviceOverheadMiB int
	MinShareMiB       int
	CacheRAMMiB       int
	ServerBin         string
	LocalDevice       string
	// ForcedNodeGrace is how long a forced node may be absent or offline in a
	// live fleet view before requests for the profile fail fast.
	ForcedNodeGrace time.Duration
	// ForcedFitCheck is what a profile with force_nodes does when its memory
	// need exceeds the forced nodes' free memory at pre-flight: ForcedFitOff
	// (no check, the default), ForcedFitWarn (log and start) or
	// ForcedFitReject (fail the request).
	ForcedFitCheck string
	// WorkerProbeInterval is how often a remote worker that backs an instance
	// gets a plain TCP connect probe (design 8.4a). 0 (the default) means the
	// probe never runs; the rest of worker health detection is always on. The
	// probe is harmful for rpc-server workers (one client, extra connections
	// fill the listen backlog): enable it only for workers that accept
	// parallel connections.
	WorkerProbeInterval time.Duration
	// WorkerSilence is how long a worker's upload progress or fleet heartbeat
	// may stay quiet before that channel counts as silent.
	WorkerSilence time.Duration
	// WorkerProbeDuringInference lets the connect probe run while the instance
	// has a request in flight. Off by default: a worker serves one client at a
	// time, and an extra connection might disturb a live session.
	WorkerProbeDuringInference bool

	// SplitSkewMargin is the extra share a node of a split model may receive
	// beyond its tensor-split fraction (llama.cpp does not land a split
	// evenly: 11% over was measured). Every multi-node placement on
	// shared-memory nodes is fitted per node with share = need x fraction x
	// (1 + this). 0 to 1; default 0.15.
	SplitSkewMargin float64
	// EstimateMargin is added to every memory need that is an estimate (a
	// profile without needs_mib): need x (1 + this). 0 to 1; default 0.05.
	EstimateMargin float64
	// LoadMemoryFloorMiB cancels a load when a shared-memory node's
	// fleet-reported available memory falls below it during the load (a live
	// fleet view only). 0 disables. Default 768.
	LoadMemoryFloorMiB int
}

// Values of defaults.forced_fit_check.
const (
	ForcedFitOff    = "off"
	ForcedFitWarn   = "warn"
	ForcedFitReject = "reject"
)

// Match is a tier's match clause. Empty fields are not checked.
type Match struct {
	Hostname string
	Role     string
	Source   string
	Local    *bool
}

// TierRule is one tiers[] entry: a match clause and the settings it grants.
type TierRule struct {
	Match Match
	core.Tier
}

// NodeOverride replaces the fleet's ip/rpc_port for a node.
type NodeOverride struct {
	RPC string
}

// Options tune parsing; the zero value uses the real filesystem.
type Options struct {
	// FileExists reports whether a model file exists. Nil uses os.Stat.
	FileExists func(path string) bool
}

// Bench is the benchmark feature's settings (design 9.3).
type Bench struct {
	// Enabled is true when data_dir is set and bench.enabled is not false.
	Enabled bool
	// RegressLoadPct: a load this much slower than the baseline's is a regression (default 15).
	RegressLoadPct float64
	// RegressDecodePct: decode tokens per second this much lower is a regression (default 7).
	RegressDecodePct float64
	// MaxRunMinutes stops a run that takes longer (default 240).
	MaxRunMinutes int
	// Benchmarks and Suites are seeded into the database at start (source "config").
	Benchmarks []*benchspec.Benchmark
	Suites     []*benchspec.Suite
}

// Defaults of the websocket and benchmark settings.
const (
	DefaultWSMaxClients     = 32
	MaxWSMaxClients         = 1000
	DefaultRegressLoadPct   = 15
	DefaultRegressDecodePct = 7
	DefaultMaxRunMinutes    = 240
)
