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
// and blocked suites, and the agent's cached_bytes figure. 0.1.4 adds the
// refusal at arrival of a forced profile that can never fit, a node taking
// load only after its agent was told it is unpaused, lowest available memory
// by kind (GPU or system) and the file cache figures, the node feed and
// start-up records (section 11.4, GET /api/nodes, /api/startups,
// expect-restart), the local_node setting with "pool" wording in agents mode,
// learned memory needs kept across a restart (POST /api/forget-need), an agent
// that does not remount a share that is mounted but not answering, and stale
// start-up marks ignored after a soft restart. 0.1.5 was a scheduler image
// only: it served and required 0.1.4. 0.1.6 adds, for the agent, the
// boot_origin and boot_origin_partial lines of the start-up marks file
// (section 11.4), the file_cache_evict capability with POST /cache/evict
// (6.13), and the busy file with the busy fields of GET /status (6.14); for
// Potluck, the memory reservation rule, learned needs kept by the shape of a
// load and its device count and trusted only for their llama build (GET
// /api/needs, POST /api/needs/forget), cold-load benchmarks (POST
// /api/cache/evict), excluded nodes shown as excluded, and command profiles
// not shut out by no_auto_nodes. An agent of an older release is refused with
// agent_too_old and updates as section 5.4 says.
const Release = "0.1.6"

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
	// PathCacheEvict drops one model file from the node's page cache (6.13);
	// only an agent that advertises CapFileCacheEvict serves it.
	PathCacheEvict = "/cache/evict"
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
	// Startup is the agent's start-up report (optional; sent in every
	// registration by an agent that has it).
	Startup *Startup `json:"startup,omitempty"`
	// Capabilities names the optional requests this agent serves (6.13), for
	// example CapFileCacheEvict. Optional: absent means none.
	Capabilities []string `json:"capabilities,omitempty"`
}

// Capabilities an agent may advertise in its registration (6.13).
const (
	// CapFileCacheEvict: the agent serves POST /cache/evict.
	CapFileCacheEvict = "file_cache_evict"
	// CapHostLaunch: the agent takes a host launch (role host) with the
	// fields of release 0.1.9 (expect_mib, rpc_file_load, api_key) and names
	// its own device first (6.2). Potluck never sends a host launch to an
	// agent without it.
	CapHostLaunch = "host_launch"
)

// HasCapability reports whether the registration advertises c.
func (r *RegisterRequest) HasCapability(c string) bool {
	for _, x := range r.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// CacheEvictRequest is the body of POST /cache/evict (6.13).
type CacheEvictRequest struct {
	// File is the model file name relative to the node's model source.
	File string `json:"file"`
}

// CacheEvictAnswer is the 200 answer of POST /cache/evict: the file's size and
// how many of its bytes were in the page cache before and after the drop.
type CacheEvictAnswer struct {
	File         string  `json:"file"`
	FileBytes    int64   `json:"file_bytes"`
	CachedBefore int64   `json:"cached_before"`
	CachedAfter  int64   `json:"cached_after"`
	DurationS    float64 `json:"duration_s"`
}

// Refusal words of POST /cache/evict besides the launch ones (6.13).
const (
	ReasonTooSoon     = "too_soon"     // 429
	ReasonUnsupported = "unsupported"  // 501
	ReasonEvictFailed = "evict_failed" // 500
)

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
	// StartupAck is the number of start-up marks of the request's startup
	// object that Potluck has taken (Startup.Count). Absent: Potluck does
	// not take start-up reports, and the agent stops sending them in
	// heartbeats until its next registration.
	StartupAck int `json:"startup_ack,omitempty"`

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
	// HostUptimeS is the machine's own uptime: the seconds since the kernel
	// booted, on the monotonic clock (CLOCK_BOOTTIME), as the agent read it
	// when it built this heartbeat. BootID is the kernel's boot id
	// (/proc/sys/kernel/random/boot_id, 32 hex digits, no dashes). Both
	// optional: an agent built before them (release 0.1.3 as first shipped)
	// sends neither; a Docker install sends the host kernel's, which the
	// container shares.
	HostUptimeS *float64 `json:"host_uptime_s,omitempty"`
	BootID      string   `json:"boot_id,omitempty"`
	// Startup is the agent's start-up report (optional), sent until Potluck
	// has acknowledged every mark (HeartbeatAnswer.StartupAck).
	Startup *Startup `json:"startup,omitempty"`
}

// ---------------------------------------------------------------------------
// Start-up reporting

// Startup is the agent's start-up report, in the registration request and
// in heartbeats until every mark has been sent and acknowledged. Every
// figure is seconds since the kernel booted, on the monotonic clock
// (CLOCK_BOOTTIME), with decimals: never a wall-clock time (rule 2.1).
type Startup struct {
	// BootID is the kernel's boot id; "" when it could not be read.
	BootID string `json:"boot_id"`
	// AgentStartS is when this agent process started.
	AgentStartS float64 `json:"agent_start_s"`
	// UserspaceStartS is systemd's UserspaceTimestampMonotonic: when
	// userspace last started (at boot, or at the last soft restart).
	// SoftReboots is systemd's SoftRebootsCount (systemd 254 or later). Both
	// are absent when systemd could not be asked (a Docker install never
	// asks: it would see the container, not the host).
	UserspaceStartS *float64 `json:"userspace_start_s,omitempty"`
	SoftReboots     *int64   `json:"soft_reboots,omitempty"`
	// Marks are the agent's own marks (StartupMark*), each once, the first
	// time it happened. A mark that has not happened is absent.
	Marks map[string]float64 `json:"marks"`
	// ExternalMarks are the marks of the start-up marks file
	// (StartupMarksFile), passed through unchanged, unknown names included.
	ExternalMarks map[string]float64 `json:"external_marks"`
}

// Count is the number of marks of both kinds: what Potluck acknowledges.
func (s *Startup) Count() int {
	if s == nil {
		return 0
	}
	return len(s.Marks) + len(s.ExternalMarks)
}

// The agent's own start-up marks.
const (
	StartupMarkAgentStart    = "agent_start"
	StartupMarkShareMounted  = "share_mounted"  // the model source was first reachable
	StartupMarkRegistered    = "registered"     // the first accepted registration
	StartupMarkProgramsReady = "programs_ready" // programs verified, exporters started, devices read
	StartupMarkExporterUp    = "exporter_up"    // the exporters were started
	StartupMarkFirstStats    = "first_stats"    // the first successful exporter scrape
)

// The start origin lines of the marks file (agent contract 11.4), seconds
// since kernel start like every mark. boot_origin is where this start began:
// 0 on a full start; after a soft restart the moment the restart began
// (systemd's ShutdownStartTimestampMonotonic), earlier than the new
// userspace start. boot_origin_partial is written instead when the node
// cannot tell that moment: its value is the userspace start, and the figure
// counts from there.
const (
	StartupMarkBootOrigin        = "boot_origin"
	StartupMarkBootOriginPartial = "boot_origin_partial"
)

// StartupMarksFile is written by another component on a console (atomically
// replaced each time a mark is added): one line per mark, "<name> <seconds
// since boot, monotonic, with decimals>". The agent only reads it; it never
// creates or deletes the file or its folder.
const StartupMarksFile = "/run/potluck/startup-marks"

// Bounds of the start-up report, kept by both sides.
const (
	StartupMarksFileMaxBytes = 64 << 10 // a larger file is ignored
	StartupMaxExternalMarks  = 32       // further names are dropped
	StartupMaxNameLen        = 64       // letters, digits, '_', '-', '.'
)

// ValidStartupMarkName: 1 to StartupMaxNameLen letters, digits, '_', '-' or '.'.
func ValidStartupMarkName(s string) bool {
	if s == "" || len(s) > StartupMaxNameLen {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
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
	// StartupAck acknowledges the heartbeat's startup object, as in
	// RegisterAnswer. Absent when the heartbeat had none.
	StartupAck int `json:"startup_ack,omitempty"`
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

// HostSpec is the host part of a launch request (phase 6). Potluck sends a
// host launch only to an agent that advertises CapHostLaunch.
type HostSpec struct {
	Model       string    `json:"model"`
	Ctx         int       `json:"ctx"`
	Parallel    int       `json:"parallel"`
	Workers     []string  `json:"workers"`
	TensorSplit []float64 `json:"tensor_split"`
	// StrictFileLoad is the request of a Potluck that does not send
	// RPCFileLoad: true is "required", false "1". RPCFileLoad wins when set.
	StrictFileLoad bool     `json:"strict_file_load"`
	ExtraArgs      []string `json:"extra_args"`
	// ExpectMiB is the memory Potluck expects the host to take on the node:
	// its device share, the host process and its context (release 0.1.9).
	// The agent makes the same would_not_fit test as for a worker. Optional:
	// 0 makes none.
	ExpectMiB int64 `json:"expect_mib,omitempty"`
	// RPCFileLoad is the host's LLAMA_RPC_FILE_LOAD: RPCFileLoadRequired or
	// RPCFileLoadOff (release 0.1.9). Empty: StrictFileLoad decides.
	RPCFileLoad string `json:"rpc_file_load,omitempty"`
	// APIKey is a per-launch key (release 0.1.9): llama-server then answers
	// only requests that carry "Authorization: Bearer <key>", except /health.
	// The agent hands it to llama-server in a file readable by the launch
	// only, never on the command line, and never reports or logs it. Empty:
	// no key. 16 to 256 letters, digits, '.', '_' or '-'.
	APIKey string `json:"api_key,omitempty"`
}

// Values of HostSpec.RPCFileLoad.
const (
	RPCFileLoadRequired = "required"
	RPCFileLoadOff      = "0"
)

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
	// The busy file's fact (6.14): Busy while a launch exists, Launches how
	// many, BusySince when the node became busy (Unix seconds; null idle).
	Busy      bool   `json:"busy"`
	Launches  int    `json:"launches"`
	BusySince *int64 `json:"busy_since"`
	// AdvertiseAddr is --advertise-addr when set (release 0.1.9): the
	// address the registration names instead of the agent's own.
	AdvertiseAddr string `json:"advertise_addr,omitempty"`
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
