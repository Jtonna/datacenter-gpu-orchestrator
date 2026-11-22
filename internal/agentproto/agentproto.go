// Package agentproto holds the wire types of the agent contract
// (docs/agent-contract.md, DRAFT 4) and of the stats contract
// (docs/stats-contract.md, stats_version 1).
//
// It is shared by potluck-agent and by the scheduler side. It has no
// dependencies outside the standard library and holds no behaviour beyond
// JSON encoding rules, so both sides can import it freely.
//
// Naming: every JSON tag is exactly the field name the contract uses.
// Fields the contract shows as JSON null (for example "launch_id": null) are
// pointers without omitempty; optional fields the contract says are omitted
// when unknown are pointers with omitempty.
//
// No field here is a node wall-clock timestamp (agent contract 2.1): every
// time value is an age or a duration in seconds or milliseconds, measured on
// the node's CLOCK_BOOTTIME.
package agentproto

// Release is the release this code is: Potluck serves it and requires it of
// agents, and the agent reports it (agent contract 5.1). The two commands
// take it as the default of their -ldflags -X variables. 0.1.1 was the first
// release with CR-9 and CR-10 batch 1; 0.1.2 adds the rest of CR-10 (the
// agent's log shipper, Potluck's own lines, the read side). 0.1.3 adds the
// forced-fit check before any eviction, paused and unplaced nodes never
// evicted and a pause winning after a claim, the benchmark step facts (lowest
// available memory, load mode, roles, the reload-from-cache verdict), planned
// and blocked suites, and the agent's cached_bytes figure. An agent of an
// older release is refused with agent_too_old and updates as section 5.4 says.
const Release = "0.1.3"

// Ports, all fixed (agent contract section 3).
const (
	PortPotluck          = 3122
	PortNFS              = 2049
	PortAgent            = 50060
	PortWorker           = 50052 // rpc-server during a worker launch
	PortHost             = 50061 // llama-server during a host launch (phase 6)
	PortNodeExporter     = 50070
	PortNvidiaExporter   = 50071
	NodeExporterAddr     = "127.0.0.1:50070"
	NvidiaExporterAddr   = "127.0.0.1:50071"
	LaunchUnitPrefix     = "potluck-launch-" // systemd unit prefix, part of the contract (6.1)
	AgentUnitName        = "potluck-agent.service"
	NodeExporterUnit     = "potluck-node-exporter"
	NvidiaExporterUnit   = "potluck-nvidia-exporter"
	ExitCodeDockerUpdate = 75 // Docker install on agent_too_old (5.4)
)

// Program names (section 1 and 5.2). File names are exactly these, with no
// version; a later windows platform adds ".exe".
const (
	ProgramLlamaServer       = "llama-server"
	ProgramRPCServer         = "rpc-server"
	ProgramNodeExporter      = "node_exporter"
	ProgramNvidiaGPUExporter = "nvidia_gpu_exporter"
)

// Install kinds (4.2 `install`).
const (
	InstallBare   = "bare"
	InstallDocker = "docker"
)

// Roles (section 1, 4.3 `role`, 6.2 `role`).
const (
	RoleIdle   = "idle"
	RoleWorker = "worker"
	RoleHost   = "host"
)

// Model source kinds (section 8).
const (
	ModelSourceShare = "share"
	ModelSourcePush  = "push"
)

// GPU vendors (4.2 `gpus[].vendor`) and backends (4.2 `backends`).
const (
	VendorAMD    = "amd"
	VendorNvidia = "nvidia"
	VendorIntel  = "intel"
	VendorOther  = "other"

	BackendVulkan = "vulkan"
	BackendCUDA   = "cuda"
)

// Registration refusal reasons (4.2).
const (
	ReasonAgentTooOld         = "agent_too_old"
	ReasonPotluckTooOld       = "potluck_too_old"
	ReasonUnsupportedHardware = "unsupported_hardware"
	ReasonDuplicateID         = "duplicate_id"
	ReasonForeignProcess      = "foreign_process"
	ReasonRemoved             = "removed"
)

// Launch refusal reasons (6.2), with their HTTP status in LaunchRefusalStatus.
const (
	ReasonBusy                   = "busy"
	ReasonPaused                 = "paused"
	ReasonReleaseMismatch        = "release_mismatch"
	ReasonModelSourceUnreachable = "model_source_unreachable"
	ReasonModelNotFound          = "model_not_found"
	ReasonLowMemory              = "low_memory"
	ReasonWorkerUnreachable      = "worker_unreachable"
	ReasonPortInUse              = "port_in_use"
	// ReasonWouldNotFit (CR-9): the launch's expect_mib does not fit the
	// node's memory; nothing was started.
	ReasonWouldNotFit = "would_not_fit"

	// ReasonInvalidRequest is not a contract word.
	// CONTRACT-QUESTION: section 6.2 says the agent "refuses" a host model
	// name that leaves the model folder, extra_args it sets itself, a missing
	// tensor_split and model_dir on a push node, but gives no reason word or
	// HTTP code for these. The agent answers 400 with this word.
	ReasonInvalidRequest = "invalid_request"
)

// LaunchRefusalStatus maps each launch refusal reason to its HTTP status (6.2).
var LaunchRefusalStatus = map[string]int{
	ReasonBusy:                   409,
	ReasonPaused:                 409,
	ReasonReleaseMismatch:        409,
	ReasonModelSourceUnreachable: 503,
	ReasonModelNotFound:          404,
	ReasonLowMemory:              507,
	ReasonWorkerUnreachable:      502,
	ReasonPortInUse:              409,
	ReasonWouldNotFit:            409,
	ReasonInvalidRequest:         400,
}

// Launch states (6.3).
const (
	StateNone     = "none"
	StateStarting = "starting"
	StateReady    = "ready"
	StateStopping = "stopping"
	StateExited   = "exited"
)

// Load progress states (6.3 `load.state`), the words Potluck uses today.
const (
	LoadWaiting   = "waiting"
	LoadReceiving = "receiving"
	LoadDone      = "done"
)

// Exit reasons (6.3), in the order the agent decides them.
const (
	ExitStopped          = "stopped"
	ExitForced           = "forced"
	ExitLeaseExpired     = "lease_expired"
	ExitWatchdog         = "watchdog"
	ExitReset            = "reset"
	ExitFileLoadTimeout  = "file_load_timeout"
	ExitFileLoadRequired = "file_load_required"
	ExitStartFailed      = "start_failed"
	ExitOOMKill          = "oom_kill"
	ExitCrashed          = "crashed"
)

// Potluck-side reason used when a Docker agent restarts during a launch (6.1).
const InstanceReasonAgentRestarted = "agent_restarted"

// Log tokens of the fork (6.3), final as quoted in the contract.
const (
	TokenFileLoadTimeout  = "rpc_file_load_timeout:"
	TokenFileLoadRequired = "rpc_file_load_required:"
	TokenHostLoadProgress = "load_progress: fraction="
	TokenWorkerFileLoad   = "[file_load] progress bytes="
)

// Paths on Potluck (agent to Potluck). {id} is the node id.
const (
	PathRegister       = "/api/pool/register"
	PathHeartbeatFmt   = "/api/pool/nodes/%s/heartbeat"  // fmt with id
	PathDeregisterFmt  = "/api/pool/nodes/%s/deregister" // fmt with id
	PathRelease        = "/api/pool/release"
	PathProgramsPrefix = "/api/pool/programs/"
	// Central logging (CR-10).
	PathNodeLogsFmt      = "/api/pool/nodes/%s/logs" // fmt with id: GET the logs object, POST lines
	PathClientLogs       = "/api/logs"               // POST lines (clients)
	PathClientLogsConfig = "/api/logs/config"        // GET the clients' logs object
	PathLogsQuery        = "/api/logs/query"         // GET: a read-only LogsQL query (readers, scripts, MCP)
	// PathLogsPages is where Potluck serves the store's own pages and query
	// API, read only. The store runs with -http.pathPrefix=/logs, so paths
	// pass through unchanged; its search page is PathLogsPages+"select/vmui/".
	PathLogsPages = "/logs/"
	// HeaderAgentStartID carries agent_start_id on the node log routes, whose
	// body is not a JSON object (CR-10).
	HeaderAgentStartID = "X-Agent-Start-Id"
)

// Central logging (CR-10): the format word of the logs object, the sources
// of a line and the figures of the limits object.
const (
	LogsFormatJSONLine = "jsonline"

	LogSourceScheduler = "scheduler"
	LogSourceHost      = "host"
	LogSourceAgent     = "agent"
	LogSourceLaunch    = "launch"
	LogSourceJournal   = "journal"
	LogSourceClient    = "client"

	LogsMaxBatchBytes = 1 << 20   // one request body
	LogsMaxLineBytes  = 8 << 10   // the sender cuts its lines to this
	LogsMaxBytesPerS  = 256 << 10 // per node
	LogsEveryS        = 2         // a sender pushes at most this often
)

// Paths on the agent (Potluck to agent, port 50060).
const (
	PathLaunch    = "/launch"     // POST start, GET state
	PathLaunchLog = "/launch/log" // GET ?since=
	PathReset     = "/reset"
	PathStatus    = "/status" // 127.0.0.1 only
	// POST /launch/{launch_id}/lease and DELETE /launch/{launch_id}[?force=1]
	HeaderNextOffset   = "X-Next-Offset"
	HeaderDroppedBytes = "X-Dropped-Bytes"
)

// ---------------------------------------------------------------------------
// Registration (4.2)

// GPU is one entry of RegisterRequest.GPUs.
type GPU struct {
	Vendor     string `json:"vendor"` // amd, nvidia, intel, other
	Model      string `json:"model"`
	Integrated bool   `json:"integrated"`
}

// ProgramHash is a program the agent already holds and has just re-hashed.
type ProgramHash struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// ForeignProcess is an rpc-server or llama-server the agent did not start.
type ForeignProcess struct {
	PID int    `json:"pid"`
	Exe string `json:"exe"`
}

// ModelSource is the registration's model_source (section 8).
type ModelSource struct {
	Kind   string `json:"kind"`             // share or push
	Target string `json:"target,omitempty"` // share: the export or folder; absent for push
}

// RegisterRequest is the body of POST /api/pool/register.
// Slices are always sent as arrays, never null: use NewRegisterRequest or
// set them to empty slices.
type RegisterRequest struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	AgentRelease     string           `json:"agent_release"`
	AgentStartID     string           `json:"agent_start_id"`
	OS               string           `json:"os"`
	Arch             string           `json:"arch"`
	Install          string           `json:"install"`
	Addr             string           `json:"addr"`
	GPUs             []GPU            `json:"gpus"`
	Backends         []string         `json:"backends"`
	MemTotalMiB      int64            `json:"mem_total_mib"`
	RAMOnly          bool             `json:"ram_only"`
	ModelSource      ModelSource      `json:"model_source"`
	Programs         []ProgramHash    `json:"programs"`
	LaunchID         *string          `json:"launch_id"`
	ForeignProcesses []ForeignProcess `json:"foreign_processes"`
	// WatchdogFloorMiB is the agent's memory watchdog floor (CR-9). Optional:
	// an agent of release 0.1.0 does not send it.
	WatchdogFloorMiB int64 `json:"watchdog_floor_mib,omitempty"`
}

// FillEmpty replaces nil slices by empty ones so they encode as [].
func (r *RegisterRequest) FillEmpty() {
	if r.GPUs == nil {
		r.GPUs = []GPU{}
	}
	if r.Backends == nil {
		r.Backends = []string{}
	}
	if r.Programs == nil {
		r.Programs = []ProgramHash{}
	}
	if r.ForeignProcesses == nil {
		r.ForeignProcesses = []ForeignProcess{}
	}
}

// ProgramEntry is one program Potluck hands out for the node's platform.
// Path is relative to the configured Potluck address.
type ProgramEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Build  string `json:"build"`
}

// ExporterSpec is one exporter to run, with its arguments from the stats
// contract. The agent adds nothing and refuses a listen address that is not
// on 127.0.0.1.
type ExporterSpec struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

// RegisterAnswer is the 200 answer to a registration. When Accepted is false,
// Reason and Message are set (and RequiredRelease for agent_too_old).
type RegisterAnswer struct {
	Accepted        bool           `json:"accepted"`
	Release         string         `json:"release,omitempty"`
	Platform        string         `json:"platform,omitempty"`
	HeartbeatEveryS float64        `json:"heartbeat_every_s,omitempty"`
	Programs        []ProgramEntry `json:"programs,omitempty"`
	Exporters       []ExporterSpec `json:"exporters,omitempty"`
	Paused          bool           `json:"paused"`
	StopLaunch      *string        `json:"stop_launch"`
	// Logs says where and how to push log lines (CR-10). Absent or null: the
	// agent ships nothing. Release 0.1.0 ignores it.
	Logs *Logs `json:"logs,omitempty"`
	// ServerTimeUnixMs is Potluck's own clock when it answered, in Unix
	// milliseconds (CR-10). The agent stamps its log lines from it: it is
	// Potluck's time, never the node's. Optional: 0 and absent mean none;
	// release 0.1.0 ignores it.
	ServerTimeUnixMs int64 `json:"server_time_unix_ms,omitempty"`

	Reason          string `json:"reason,omitempty"`
	Message         string `json:"message,omitempty"`
	RequiredRelease string `json:"required_release,omitempty"`
}

// Logs is the logs object (CR-10): where a sender pushes its log lines, in
// which format and within which limits. It is part of the registration answer
// and the answer to GET /api/pool/nodes/{id}/logs and GET /api/logs/config.
type Logs struct {
	// Revision changes whenever a logging setting changes.
	Revision int64 `json:"revision"`
	// URL without a host is a path on the Potluck the sender talks to; with a
	// host it is a full address.
	URL    string `json:"url"`
	Format string `json:"format"` // jsonline
	// Auth is null in the default setup: no credential is handed out.
	Auth      *LogsAuth  `json:"auth"`
	Limits    LogsLimits `json:"limits"`
	DenyUnits []string   `json:"deny_units"` // system log units the sender drops
}

// LogsAuth is a credential for the push address: one request header.
type LogsAuth struct {
	Header string `json:"header"`
	Value  string `json:"value"`
}

// LogsLimits are the limits a sender keeps to.
type LogsLimits struct {
	MaxBatchBytes int64   `json:"max_batch_bytes"`
	MaxLineBytes  int64   `json:"max_line_bytes"`
	MaxBytesPerS  int64   `json:"max_bytes_per_s"`
	EveryS        float64 `json:"every_s"`
}

// LogShipping is what an agent reports about its log shipping in a heartbeat
// (CR-10). The counts are since the agent started.
type LogShipping struct {
	LinesSent    uint64 `json:"lines_sent"`
	LinesDropped uint64 `json:"lines_dropped"`
	// LastError is the last push error; empty when there was none.
	LastError     string   `json:"last_error,omitempty"`
	LastErrorAgeS *float64 `json:"last_error_age_s,omitempty"`
}

// ---------------------------------------------------------------------------
// Heartbeat (4.3)

// ModelSourceState is the heartbeat's model_source.
type ModelSourceState struct {
	Kind string `json:"kind"`
	// Reachable is the result of the last finished share probe (section 8).
	// CONTRACT-QUESTION: the contract does not say what is sent before the
	// first probe has finished, nor for a push node. The agent sends
	// reachable=false and omits checked_age_s until a probe has finished,
	// and omits both for push.
	Reachable   *bool    `json:"reachable,omitempty"`
	CheckedAgeS *float64 `json:"checked_age_s,omitempty"`
}

// Device is one device of the node's llama program (--list-devices).
type Device struct {
	Name               string  `json:"name"`
	Backend            string  `json:"backend"`
	TotalMiB           int64   `json:"total_mib"`
	FreeMiB            int64   `json:"free_mib"`
	SharesSystemMemory bool    `json:"shares_system_memory"`
	ReadAgeS           float64 `json:"read_age_s"`
}

// Heartbeat is the body of POST /api/pool/nodes/{id}/heartbeat.
type Heartbeat struct {
	AgentStartID    string           `json:"agent_start_id"`
	Seq             uint64           `json:"seq"`
	UptimeS         float64          `json:"uptime_s"`
	Role            string           `json:"role"`
	LaunchID        *string          `json:"launch_id"`
	ProgramsReady   bool             `json:"programs_ready"`
	ProgramsError   *string          `json:"programs_error"`
	ModelSource     ModelSourceState `json:"model_source"`
	Devices         []Device         `json:"devices"`
	MemAvailableMiB int64            `json:"mem_available_mib"`
	// Stats is the stats contract object. A heartbeat without it is valid.
	Stats *Stats `json:"stats,omitempty"`
	// WatchdogFloorMiB is the agent's memory watchdog floor (CR-9, optional).
	WatchdogFloorMiB int64 `json:"watchdog_floor_mib,omitempty"`
	// StopReason and StopMessage: why the agent is stopping its launch, sent
	// while the launch is stopping (CR-9, optional; see LaunchState).
	StopReason  string `json:"stop_reason,omitempty"`
	StopMessage string `json:"stop_message,omitempty"`
	// Logs are the agent's log shipping figures (CR-10, optional).
	Logs *LogShipping `json:"logs,omitempty"`
}

// HeartbeatAnswer is the 200 answer to a heartbeat. 410 means "register again".
type HeartbeatAnswer struct {
	Release string `json:"release"`
	Paused  bool   `json:"paused"`
	// LogsRevision is the revision of the node's logs object (CR-10). Absent:
	// there is no logs object and the agent ships nothing.
	LogsRevision int64 `json:"logs_revision,omitempty"`
	// ServerTimeUnixMs is Potluck's clock when it answered (CR-10, as in
	// RegisterAnswer). Optional.
	ServerTimeUnixMs int64 `json:"server_time_unix_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// Agent releases (5.4)

// AgentBinary is one agent binary in ReleaseInfo.
type AgentBinary struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ReleaseInfo is the answer to GET /api/pool/release. Agents is keyed by
// "<os>-<arch>", for example "linux-amd64".
//
// LAYOUT RULE: deploy/agent/potluck-starter.sh reads this answer with sed
// (no jq or python on a console), so the layout must stay parseable that
// way. The starter relies on exactly this:
//
//  1. The whole entry of one platform, from its key to the closing brace of
//     its object, is on ONE line: "linux-amd64":{"path":"...","sha256":"..."}
//     (json.Marshal of this type, which puts the whole answer on one line,
//     satisfies it; so does one entry per line).
//  2. That object is flat: no nested object, so no "}" before its end.
//  3. path and sha256 are plain strings without '"' or backslash escapes;
//     path is absolute ("/..."); sha256 is 64 lower-case hex digits.
//  4. Whitespace around ':' and ',' is allowed; key order inside the object
//     is free; unknown fields may be added to the object if they are plain
//     strings or numbers.
//
// The starter's patterns (with P the platform key):
//
//	sed -n 's/.*"P"[[:space:]]*:[[:space:]]*{[^}]*"path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
//	sed -n 's/.*"P"[[:space:]]*:[[:space:]]*{[^}]*"sha256"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/\1/p'
//
// Both take the first matching line (the starter pipes through "head -n 1").
type ReleaseInfo struct {
	Release string                 `json:"release"`
	Agents  map[string]AgentBinary `json:"agents"`
}

// ---------------------------------------------------------------------------
// Launches (6.2 to 6.8)

// WorkerSpec is the worker part of a launch request.
type WorkerSpec struct {
	ModelDir  bool     `json:"model_dir"`
	ExtraArgs []string `json:"extra_args"`
	// ExpectMiB is the memory Potluck expects this launch to take on the node
	// (CR-9). Optional: without it the agent makes no would_not_fit check.
	ExpectMiB int64 `json:"expect_mib,omitempty"`
}

// HostSpec is the host part of a launch request (phase 6).
type HostSpec struct {
	Model          string    `json:"model"`
	Ctx            int       `json:"ctx"`
	Parallel       int       `json:"parallel"`
	Workers        []string  `json:"workers"`
	TensorSplit    []float64 `json:"tensor_split"`
	StrictFileLoad bool      `json:"strict_file_load"`
	ExtraArgs      []string  `json:"extra_args"`
}

// LaunchRequest is the body of POST /launch.
type LaunchRequest struct {
	LaunchID           string      `json:"launch_id"`
	Role               string      `json:"role"`
	Release            string      `json:"release"`
	Label              string      `json:"label"`
	LeaseTTLS          float64     `json:"lease_ttl_s"`
	MinMemAvailableMiB int64       `json:"min_mem_available_mib"`
	Worker             *WorkerSpec `json:"worker,omitempty"`
	Host               *HostSpec   `json:"host,omitempty"`
}

// LoadProgress is this node's part of a load (6.3 `load`).
type LoadProgress struct {
	Fraction    *float64 `json:"fraction"`
	BytesTotal  *int64   `json:"bytes_total"`
	BytesLoaded *int64   `json:"bytes_loaded"`
	State       string   `json:"state"` // waiting, receiving, done
}

// ExitInfo is the exit object of an exited launch (6.3).
type ExitInfo struct {
	Code    *int     `json:"code"`   // exit code; 128+signal when killed, as a shell reports it
	Signal  *string  `json:"signal"` // signal name without "SIG", e.g. "KILL"; null when none
	Reason  string   `json:"reason"`
	Message string   `json:"message,omitempty"`
	AgeS    float64  `json:"age_s"`
	LogTail []string `json:"log_tail"` // last 40 log lines
}

// LaunchState is the answer to GET /launch and the 202 answer to POST /launch.
// When State is "none" it encodes as {"state":"none"} only.
type LaunchState struct {
	LaunchID        string        `json:"launch_id"`
	Role            string        `json:"role"`
	State           string        `json:"state"`
	StateAgeS       float64       `json:"state_age_s"`
	Port            int           `json:"port"`
	Command         []string      `json:"command"`
	Build           string        `json:"build"`
	UptimeS         float64       `json:"uptime_s"`
	ClientConnected *bool         `json:"client_connected,omitempty"` // worker only
	Load            *LoadProgress `json:"load"`
	MemAvailableMiB int64         `json:"mem_available_mib"`
	MemLowWaterMiB  int64         `json:"mem_low_water_mib"`
	Exit            *ExitInfo     `json:"exit"`
	// LeaseExpiresInS is null once the launch has exited.
	LeaseExpiresInS *float64 `json:"lease_expires_in_s"`
	// StopReason is set as soon as the agent has decided to stop the launch
	// (watchdog, lease_expired, forced, stopped, reset), while State is still
	// "stopping"; StopMessage says more when there is more (CR-9, optional).
	// The final word is exit.reason: it can differ (oom_kill, crashed).
	StopReason  string `json:"stop_reason,omitempty"`
	StopMessage string `json:"stop_message,omitempty"`
}

// LaunchRefusal is the body of every refused launch call (6.2) and of a stop
// with a wrong id (6.5).
type LaunchRefusal struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	// LaunchID is the existing launch on busy.
	LaunchID string `json:"launch_id,omitempty"`
	// FailedWorkers lists the workers that did not accept a connection
	// (worker_unreachable).
	// CONTRACT-QUESTION: 6.2 says "the body lists the ones that failed"
	// without naming the field; the agent uses "failed_workers".
	FailedWorkers []string `json:"failed_workers,omitempty"`
}

// KilledProcess is one entry of ResetAnswer.
type KilledProcess struct {
	PID      int    `json:"pid,omitempty"`
	Exe      string `json:"exe,omitempty"`
	Unit     string `json:"unit,omitempty"`
	LaunchID string `json:"launch_id,omitempty"`
}

// ResetAnswer is the answer to POST /reset.
// CONTRACT-QUESTION: 6.8 says "answers with what it killed" without a shape.
// The agent answers 200 {"killed":[...]} with one entry per unit or process.
type ResetAnswer struct {
	Killed []KilledProcess `json:"killed"`
}

// ---------------------------------------------------------------------------
// Local status (6.9)

// Status is the answer to GET /status on 127.0.0.1:50060.
type Status struct {
	Registered bool          `json:"registered"`
	Potluck    string        `json:"potluck"`
	Release    string        `json:"release"`
	Paused     bool          `json:"paused"`
	Role       string        `json:"role"`
	Model      *string       `json:"model"`
	State      string        `json:"state"`
	Load       *LoadProgress `json:"load"`
	// LastLaunch is the launch that has exited (state "exited"), else null.
	// Model and Load are null then: the node runs no model and loads nothing.
	LastLaunch    *LastLaunch `json:"last_launch"`
	LastError     *string     `json:"last_error"`
	LastErrorAgeS *float64    `json:"last_error_age_s"`
}

// LastLaunch is the exited launch in GET /status (6.9).
type LastLaunch struct {
	ID        string  `json:"id"`
	Model     *string `json:"model"`
	Reason    string  `json:"reason"`
	EndedAgeS float64 `json:"ended_age_s"`
	// Load is the launch's last load progress, null when it had none.
	Load *LoadProgress `json:"load"`
}
