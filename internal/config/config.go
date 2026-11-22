package config

import (
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
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
	// ProgramsDir holds the program sets handed out to agents (agent contract
	// 5.2): <dir>/<release>/release.json and its folders. Empty: no release
	// is served and every agent is refused.
	ProgramsDir string
	// HistoryDir, when set, holds the event history (JSON Lines, rotated by
	// size); empty turns history off. HistoryMaxFileMiB is the size at which a
	// file is rotated, HistoryKeepFiles how many files are kept and
	// HistoryMemoryEvents how many recent events are held in memory for queries.
	HistoryDir          string
	HistoryMaxFileMiB   int
	HistoryKeepFiles    int
	HistoryMemoryEvents int
	LocalProbeCmd       string
	// LocalNode is the name of the scheduler's own machine as a node (the
	// local_node setting). See LocalNodeName for when it is used.
	LocalNode string
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
	// NoAutoNodes lists nodes that automatic placement never picks (CR-9
	// review): same entries and matching as ExcludeNodes. A profile that names
	// such a node in force_nodes may still use it. ExcludeNodes wins over
	// both. See NoAuto.
	NoAutoNodes []string
	// Recovery is the optional recovery-request feature (design 8.4b).
	Recovery Recovery
	Profiles map[string]*core.Profile

	// Logs are the central logging settings (CR-10).
	Logs Logs

	// AuditRetentionDays is informational (the application never deletes audit entries); 0 keeps everything.
	AuditRetentionDays int
	// Revision is the configuration revision this snapshot was built from and
	// Configured is false for the built-in defaults of an empty database.
	Revision   int64
	Configured bool
}

// Logs are the central logging settings (docs/cr-10-central-logging.md). The
// address of the log store is not here: it is a deployment fact
// (LLM_SCHEDULER_LOGS_STORE_URL).
type Logs struct {
	// Enabled false: no logs object is handed out and the log routes answer 404.
	Enabled bool
	// RetentionDays and MaxDiskGiB are passed to the log store when it starts.
	RetentionDays int
	MaxDiskGiB    int
	// PushURL, when set, is the address senders are told to push to instead
	// of Potluck's own route.
	PushURL string
	// ClientIntake false: POST /api/logs and GET /api/logs/config answer 404.
	ClientIntake bool
}

// DefaultLogs returns the built-in logging settings.
func DefaultLogs() Logs {
	return Logs{Enabled: true, RetentionDays: 14, MaxDiskGiB: 20, ClientIntake: true}
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
	// ForcedFitReject (fail the request). In agents membership the agents'
	// own check (would_not_fit, and a node with no memory figure) refuses a
	// forced placement whatever this says; that cannot be switched off
	// (agent contract 11.2, item 1).
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

	// Membership is where the pool's nodes come from: MembershipFleet (the
	// fleet console feed, the default) or MembershipAgents (registrations and
	// heartbeats of potluck-agent, agent contract 2.7). Read at start.
	Membership string
	// WorkerModelDir is the worker launch's model_dir (agent contract 6.2):
	// true lets a worker read model files itself. Off by default.
	WorkerModelDir bool
	// WatchdogFloorMiB is the agents' memory watchdog floor (agent contract
	// 6.7); for a device that shares system memory the usable figure is
	// min(device free, mem_available_mib - this). Default 1000.
	WatchdogFloorMiB int
	// WorkerStartTimeout is how long the worker launches of a load may stay
	// starting before the load fails with worker_start_timeout. Default 15 s.
	WorkerStartTimeout time.Duration
}

// Values of defaults.membership.
const (
	MembershipFleet  = "fleet"
	MembershipAgents = "agents"
)

// AgentsMode reports whether the pool comes from agents.
func (c *Config) AgentsMode() bool { return c.Defaults.Membership == MembershipAgents }

// LocalNodeName is the node name of the machine the scheduler runs on. With
// membership agents it is the local_node setting, or fleet.local_node while
// local_node is empty, so a database written before local_node existed keeps
// its node name (no_auto_nodes and similar settings match on it). With
// membership fleet it is fleet.local_node, as before.
func (c *Config) LocalNodeName() string {
	if c.AgentsMode() && c.LocalNode != "" {
		return c.LocalNode
	}
	return c.Fleet.LocalNode
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
	// Platform and Device match what an agent node reports (agent contract
	// 11): its platform (exact) and a device name (glob, as Hostname). A node
	// from the fleet feed reports neither, so a tier that sets one never
	// matches it.
	Platform string
	Device   string
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
	// EnabledSetting is the bench_enabled setting (Enabled also needs data_dir).
	EnabledSetting bool
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
