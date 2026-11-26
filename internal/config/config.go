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
	// UsageRequestDays is how long the request log (usage.db in DataDir)
	// keeps request rows, UsageHourlyDays its hourly totals; daily totals
	// are kept for ever.
	UsageRequestDays int
	UsageHourlyDays  int
	// RequestsLiveTimings adds "timings_per_token": true to streamed /v1
	// chat and completion requests that do not set it (benchmark requests
	// excepted), so a running request shows its prompt figures live.
	RequestsLiveTimings bool
	LocalProbeCmd       string
	// LocalNode is the name of the scheduler's own machine as a node (the
	// local_node setting). See LocalNodeName for when it is used.
	LocalNode string
	// DefaultModelSource is the model source a machine made with Create new
	// zone is given when the request names none: share:<host>:/<path>,
	// dir:/<path> or push (ModelSourceRE); empty: none, the create is
	// refused. Never guessed from the registered machines.
	DefaultModelSource string
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

	// LoopStepWarnMs: a core-loop step longer than this is a loopSlowStep
	// history event and turns the health summary's Scheduler row amber.
	LoopStepWarnMs int
	// ModelsDiskBudgetGB, when set, is what the models folder may hold: the
	// health summary's Storage row then also judges the model files against
	// it. 0 (the default) judges the real free space only.
	ModelsDiskBudgetGB int
	// ModelTrashDays is how long a deleted model file is kept in the trash
	// before the clearing may remove it. A file's clearing date is the later
	// of the date set when it was deleted and its deletion time plus this
	// many days: raising it keeps every file longer, lowering it applies to
	// files deleted from then on only. 1 to MaxModelTrashDays, default 30.
	ModelTrashDays int
	// ModelTrashPurge turns the timed clearing of the models trash on
	// (models slice 4). Off (the default): nothing but "Remove for good"
	// removes files from the trash.
	ModelTrashPurge bool
	// modelNote is Options.ModelNote while the configuration is checked.
	modelNote func(string) string
	// keepArgs is Options.KeepArgs while the configuration is checked;
	// lateArgs what the check let through for it (LateManagedArgs).
	keepArgs map[string][]string
	lateArgs []string
	// trainedCtx and keepCtx are Options.TrainedCtx and KeepCtx while the
	// configuration is checked.
	trainedCtx func(string) (int, bool)
	keepCtx    map[string]KeptCtx
	// DefaultUserName is the default user: the name that signs what is done
	// without an access key and without a name of the client's own (the web
	// UI, curl, scripts) in the audit log and the request log. There is no
	// login; everyone using the site is this user.
	DefaultUserName string
}

// Logs are the central logging settings (docs/architecture.md section 6). The
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
//
// LEGACY: used by the fleet membership mode only, kept until phase 7. The
// fleet.* keys stay in the configuration document until then.
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

	// The memory reservation rule (docs/architecture.md, "Reserve"). Every
	// default keeps the placement of the release before it.
	//
	// FillReserveKiBPerToken is the memory a node holding a share of a
	// profile is expected to gain while the conversation fills the context:
	// this x the profile's context (ctx x parallel) on every node of the
	// placement, added to the need share. 0 (the default) turns the reserve
	// off; 8 covers the growth measured on shared-memory nodes. While it is
	// on, a stored peak replaces need share + reserve.
	FillReserveKiBPerToken int
	// PlacementUsesReserved places on min(latest free, average free) and
	// counts what is placed or loading on a node at the larger of its reserved
	// figure and what the node measures as used. Off by default: the measured
	// free figure, as before.
	PlacementUsesReserved bool
	// ReservedDriftWarnMiB: a node whose measured use exceeds its reserved sum
	// by more than this is flagged in the node view (never an error).
	ReservedDriftWarnMiB int
	// MarginMeasuredMiB is the per-node margin on a shared-memory node, on top
	// of the watchdog floor, when every need on it is measured or a stored
	// peak. 0 (unset) keeps the split skew margin.
	MarginMeasuredMiB int
	// MarginEstimatedPercent is the per-node margin on a shared-memory node,
	// in percent of the node's share, when the need is an estimate, at least
	// MarginEstimatedMinMiB. 0 (unset) keeps the split skew margin.
	MarginEstimatedPercent int
	MarginEstimatedMinMiB  int
	// MarginRefuses turns the per-node margin back into a reason to refuse
	// a placement. Off (the default), a placement that holds weights +
	// context + fill reserve with the headroom kept but not the margin
	// starts as a tight fit; the margin then only orders automatic
	// placements (one that meets it everywhere is preferred).
	MarginRefuses bool
	// FreeAverageWindow is the window of the average free memory of a node
	// (from its heartbeats). Default 30 s.
	FreeAverageWindow time.Duration

	// A console as host (phase 6, profile host: node).
	//
	// HostOverheadMiB is what a host on a node takes there besides its
	// device share: the host process with its host buffers, HTTP server,
	// tokenizer and slots. It is added to the host node's reservation and
	// taken off its split weight until it is measured. Default 1024.
	HostOverheadMiB int
	// HostOverheadSettingOnly plans every host on a node with
	// HostOverheadMiB, ignoring the overhead measured per node and shape
	// (the measurements are kept). Off by default: a measured figure is
	// used once there is one.
	HostOverheadSettingOnly bool
	// HostNoMmap starts a host on a node with --no-mmap: its weights are
	// anonymous memory (counted in MemAvailable, watched by the watchdog)
	// instead of pages mapped from the share. Off by default until measured.
	HostNoMmap bool
	// DeviceOverheadSettingOnly plans every estimated need with
	// DeviceOverheadMiB per device, ignoring the per-device overhead measured
	// per shape, node and device count (the measurements are kept). Off by
	// default: a measured figure is used once there is one.
	DeviceOverheadSettingOnly bool

	// PlacementToleranceMiB: nodes whose usable memory differs by less than
	// this are equal for automatic placement. Equal nodes are taken warm
	// first (the last load that became ready there used the profile's model
	// file), then by name, and a model split over them splits by one steady
	// figure, so an idle plan does not follow a few MiB of free-memory noise
	// (idle readings move by 2 to 25 MiB between reads). The figure a node is
	// ordered by moves only when its usable memory moves by more than twice
	// this. Fit is still checked on the exact figures. 0 turns it off (the
	// smallest budget that fits, then the id; the split by exact budget).
	// Default 128.
	PlacementToleranceMiB int

	// The host process's own memory (the model server's prompt cache and
	// context checkpoints), passed explicitly on both launch paths
	// (internal/instance command.go). CacheRAMMiB above is --cache-ram for a
	// host on the scheduler's machine (default 2048); NodeHostCacheRAMMiB is
	// --cache-ram for a host on a node (default 0: no prompt cache there).
	// CtxCheckpoints is --ctx-checkpoints, per slot, on both (default 32, the
	// server's own). CountHostMemory: the host-side need (checkpoints x the
	// family's cell, on a host on a node) enters that node's reservation;
	// off (the default) it is computed and shown only.
	NodeHostCacheRAMMiB int
	CtxCheckpoints      int
	CountHostMemory     bool
}

// Values of defaults.membership. MembershipFleet is the legacy mode, kept
// until phase 7.
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
	// KeepModels maps a profile to the model file it pointed at before the
	// write being checked. A profile that still points at that file is not
	// refused when the file is missing: only a write that adds a profile or
	// points one at a new file is (a load of the profile is refused at plan
	// time instead).
	KeepModels map[string]string
	// ModelNote, when set, adds a sentence to the refusal of a model file
	// that is not there (models slice 3: "it is in the trash since ...").
	ModelNote func(model string) string
	// KeepArgs maps a profile to the extra_args it had before the write being
	// checked (or as stored, for a load). A profile whose extra_args are
	// unchanged is not refused for a flag the scheduler began to manage
	// later (lateManaged): an older configuration stays loadable.
	KeepArgs map[string][]string
	// TrainedCtx reads the trained context of a model file (its GGUF
	// <arch>.context_length); false when unknown. Nil: not checked. A llama
	// profile whose ctx is above it is refused unless it says
	// ctx_above_trained, or it keeps its stored model and ctx (KeepCtx).
	TrainedCtx func(path string) (int, bool)
	// KeepCtx maps a profile to its model and ctx before the write being
	// checked: a stored profile that keeps both still loads.
	KeepCtx map[string]KeptCtx
}

// KeptCtx is a stored profile's model and ctx (Options.KeepCtx).
type KeptCtx struct {
	Model string
	Ctx   int
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
	// MaxRunMinutes stops a run that takes longer (default 720: a Full pass takes 6 to 7 hours).
	MaxRunMinutes int
	// RetentionDays: a finished, unpinned run older than this many days is
	// trimmed (its requests and transcripts removed; the run, its steps and its
	// measurements kept). 0, the default, is off: nothing is ever trimmed.
	RetentionDays int
	// NotStartable (bench_not_startable) lists benchmark and group ids that
	// may not be started: a run of one, or of a group that holds one at any
	// depth, is refused whole before anything starts. Empty by default.
	NotStartable []string
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
	DefaultMaxRunMinutes    = 720
	// DefaultBenchRetentionDays and MaxBenchRetentionDays bound bench_retention_days.
	DefaultBenchRetentionDays = 0 // off: nothing is trimmed until the owner sets it
	MaxBenchRetentionDays     = 36500
)
