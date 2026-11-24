// Package bench is the benchmark feature (design 9.3): a SQLite store of
// benchmarks, suites and runs, a runner that drives the scheduler through its
// own HTTP API, the statistics and the comparison with a baseline run, and the
// export formats. Nothing here touches scheduler internals.
package bench

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// Item sources: where a benchmark or suite came from.
const (
	SourceConfig = "config"
	SourceAPI    = "api"
)

// Run states.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunPaused    = "paused"
	RunCompleted = "completed"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
	RunSkipped   = "skipped"
)

// IsFinal says whether a run state is an end state.
func IsFinal(state string) bool {
	switch state {
	case RunCompleted, RunFailed, RunCancelled, RunSkipped:
		return true
	}
	return false
}

// Step states.
const (
	StepPending   = "pending"
	StepRunning   = "running"
	StepDone      = "done"
	StepFailed    = "failed"
	StepCancelled = "cancelled"
	StepSkipped   = "skipped"
)

// Step outcomes: what the scheduler did with the benchmark's first request.
const (
	OutcomeReady   = "ready"   // loaded and served
	OutcomeRefused = "refused" // 4xx or 503 before any load started (does not fit, backoff, ...)
	// OutcomeRefusedByExclusion: refused because the profile forces a node
	// that exclude_nodes names (the scheduler's reason "node excluded"). Not
	// judged and not a failure: the configuration keeps the machine out on
	// purpose, and only lifting the exclusion makes the step run again.
	OutcomeRefusedByExclusion = "refused_by_exclusion"
	// ReasonNodeExcluded is the scheduler's reason code for that refusal.
	ReasonNodeExcluded = "node excluded"
	OutcomeFailed      = "failed"    // the load or a request failed (502, child exited, timeout)
	OutcomeError       = "error"     // the runner could not do its job (network, bad answer)
	OutcomeCancelled   = "cancelled" // the run was cancelled during this step
	OutcomeSkipped     = "skipped"   // never started (stop_on_failure, cancel)
)

// Request phases: what a stored request row was for.
const (
	PhaseLoad         = "load" // rows written by the first benchmark runner: the request that triggered the load (it is no longer sent)
	PhaseWarmup       = "warmup"
	PhaseMeasured     = "measured"
	PhaseConversation = "conversation"
	PhaseHold         = "hold"
	PhaseConcurrent   = "concurrent"
)

// Phase states.
const (
	PhaseRunning   = "running"
	PhaseDone      = "done"
	PhaseFailed    = "failed"
	PhaseCancelled = "cancelled"
	PhaseSkipped   = "skipped"
)

// ErrNotFound is returned when an id does not exist.
var ErrNotFound = errors.New("not found")

// ErrBenchOff means the benchmark feature is off.
var ErrBenchOff = errors.New("the benchmark feature is off: set data_dir in the config")

// StoredBenchmark is a benchmark with its bookkeeping.
type StoredBenchmark struct {
	Spec      *benchspec.Benchmark `json:"-"`
	Source    string               `json:"source"`
	CreatedAt int64                `json:"created_at"`
	UpdatedAt int64                `json:"updated_at"`
}

// StoredSuite is a suite with its bookkeeping.
type StoredSuite struct {
	Spec      *benchspec.Suite `json:"-"`
	Source    string           `json:"source"`
	CreatedAt int64            `json:"created_at"`
	UpdatedAt int64            `json:"updated_at"`
}

// Summary is mean, median, min, max and standard deviation of a series.
type Summary struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Stdev  float64 `json:"stdev"`
}

// Stats are the statistics of one step's measured requests (warmup excluded).
type Stats struct {
	Requests int      `json:"requests"` // measured requests that were attempted
	Failed   int      `json:"failed"`   // of those, how many did not return 200
	Decode   *Summary `json:"decode_tps,omitempty"`
	Prefill  *Summary `json:"prefill_tps,omitempty"`
	TTFT     *Summary `json:"ttft_ms,omitempty"`
	WallMS   *Summary `json:"wall_ms,omitempty"`
	// OutputHashes lists the distinct output hashes of the deterministic requests, by request name.
	OutputHashes map[string]string `json:"output_hashes,omitempty"`
}

// NodeLoad is one node's part of a load.
type NodeLoad struct {
	Node      string   `json:"node"`
	Bytes     *int64   `json:"bytes,omitempty"`
	MemoryMiB *float64 `json:"memory_mib,omitempty"`
}

// LoadResult is what the scheduler reported about the step's load.
type LoadResult struct {
	SessionID string     `json:"session_id,omitempty"`
	Outcome   string     `json:"outcome,omitempty"` // the load attempt: ready, failed, cancelled
	Reason    string     `json:"reason,omitempty"`
	LoadS     float64    `json:"load_s"`
	Bytes     int64      `json:"bytes,omitempty"`
	AvgMBs    float64    `json:"avg_mb_s,omitempty"`
	MemoryMiB float64    `json:"memory_mib,omitempty"` // device total from the load log
	Workers   []string   `json:"workers,omitempty"`
	Nodes     []NodeLoad `json:"nodes,omitempty"`
	Source    string     `json:"source,omitempty"` // history, recent_loads or observed
	// LoadMode is how the nodes got the weights: file_read (the workers read
	// the model file), pushed (the host sent them) or local (no remote
	// worker). Empty in rows written before it was recorded.
	LoadMode string `json:"load_mode,omitempty"`
}

// Load modes as the scheduler reports them (scheduler.LoadMode*).
const (
	LoadModeFileRead = "file_read"
	LoadModePushed   = "pushed"
	LoadModeLocal    = "local"
)

// NodeMemLow is the lowest available memory of one node during a step, from
// the scheduler's status sampled about twice a second: the agent's
// mem_available_mib, or the node's free figure when there is no agent (the
// local host).
type NodeMemLow struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	LoadMiB *int64 `json:"load_mib,omitempty"` // lowest while the model was loading
	StepMiB *int64 `json:"step_mib,omitempty"` // lowest during the whole step
	Samples int    `json:"samples"`
	// Kind is which memory the figures are: "system" (available system
	// memory) or "gpu" (the GPU's free memory, on a node whose GPU has memory
	// of its own). Absent on rows stored before it was recorded: system.
	// Note says why a node with a dedicated GPU has a system figure.
	Kind string `json:"kind,omitempty"`
	Note string `json:"note,omitempty"`
}

// Node roles in a step.
const (
	RoleHost   = "host"   // runs llama-server (the scheduler's machine)
	RoleWorker = "worker" // runs rpc-server with a share of the model
)

// NodeRole is one node's role in a step and its share of the model's memory.
type NodeRole struct {
	ID        string   `json:"id,omitempty"`
	Node      string   `json:"node"`
	Role      string   `json:"role"`
	MemoryMiB *float64 `json:"memory_mib,omitempty"`
}

// NodeReload is the scheduler's reload-from-cache figure for one node of the
// step's plan (design, "Reload from cache").
type NodeReload struct {
	ID        string   `json:"id"`
	Node      string   `json:"node"`
	Verdict   string   `json:"verdict"` // full, partial, none, unknown
	CachedPct *float64 `json:"cached_pct,omitempty"`
	Shared    bool     `json:"shared_memory,omitempty"`
	FileMiB   int      `json:"file_mib,omitempty"`
	ModelMiB  int      `json:"model_mib,omitempty"`
	MemoryMiB int      `json:"memory_mib,omitempty"`
	MarginMiB int      `json:"margin_mib,omitempty"`
	// CachedNowMiB is what the node's file cache held when the plan was made,
	// CacheRoomMiB the room for the file once that is counted, CacheOccupied
	// set when that room is below the file share (the first reloads are likely
	// to read part of the file again). Absent when the node reports no cache
	// figure, and on steps stored before they were recorded.
	CachedNowMiB  *int `json:"cached_now_mib,omitempty"`
	CacheRoomMiB  *int `json:"cache_room_mib,omitempty"`
	CacheOccupied bool `json:"cache_occupied,omitempty"`
	// CacheLastFile is the model file of the last load that became ready on
	// the node, and CacheNote why a short room is or is not flagged (the
	// scheduler's CacheFlag).
	CacheLastFile string `json:"cache_last_file,omitempty"`
	CacheNote     string `json:"cache_note,omitempty"`
}

// Check is one expectation and whether it held.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// ExpectResult is the verdict on a benchmark's expect block.
type ExpectResult struct {
	Pass bool `json:"pass"`
	// NotJudged: the step was not judged at all (outcome
	// refused_by_exclusion): Pass is false, and it is neither a pass nor a
	// failure. Every reader that counts or words passes looks at this first.
	NotJudged bool    `json:"not_judged,omitempty"`
	Checks    []Check `json:"checks"`
}

// Comparison is a step against the same benchmark in the baseline run.
type Comparison struct {
	BaselineRunID string   `json:"baseline_run_id"`
	Found         bool     `json:"found"` // the baseline has this benchmark with usable numbers
	LoadPct       *float64 `json:"load_s_pct,omitempty"`
	DecodePct     *float64 `json:"decode_tps_pct,omitempty"`
	PrefillPct    *float64 `json:"prefill_tps_pct,omitempty"`
	TTFTPct       *float64 `json:"ttft_ms_pct,omitempty"`
	HashDiffers   *bool    `json:"output_hash_differs,omitempty"`
	Regression    bool     `json:"regression"`
	Reasons       []string `json:"reasons,omitempty"`
}

// RequestResult is one request of a step.
type RequestResult struct {
	ID            int64           `json:"id,omitempty"`
	StepID        int64           `json:"-"`
	RunID         string          `json:"-"`
	Seq           int             `json:"seq"`
	Name          string          `json:"name"`
	Phase         string          `json:"phase"`
	Status        int             `json:"status"`
	WallMS        float64         `json:"wall_ms"`
	TTFTMS        *float64        `json:"ttft_ms,omitempty"`
	PromptN       int             `json:"prompt_n,omitempty"`
	PromptMS      float64         `json:"prompt_ms,omitempty"`
	PromptTPS     float64         `json:"prompt_tps,omitempty"`
	PredictedN    int             `json:"predicted_n,omitempty"`
	PredictedMS   float64         `json:"predicted_ms,omitempty"`
	DecodeTPS     float64         `json:"decode_tps,omitempty"`
	CacheN        int             `json:"cache_n,omitempty"`
	Timings       json.RawMessage `json:"timings,omitempty"`
	OutputSHA256  string          `json:"output_sha256,omitempty"`
	OutputHead    string          `json:"output_head,omitempty"`
	Deterministic bool            `json:"deterministic"`
	Error         string          `json:"error,omitempty"`
	StartedAt     int64           `json:"started_at"`
	PhaseIdx      int             `json:"phase_idx"`         // the phase of the benchmark this request belongs to
	Turn          int             `json:"turn,omitempty"`    // conversation turn (1-based) or simultaneous request (1-based)
	WaitMS        *float64        `json:"wait_ms,omitempty"` // simultaneous requests: time before the model started on it
}

// TurnResult is one turn of a conversation phase.
type TurnResult struct {
	Turn         int      `json:"turn"`
	Name         string   `json:"name"`
	Status       int      `json:"status"`
	PromptTokens int      `json:"prompt_tokens"` // the whole prompt: processed (prompt_n) plus reused (cache_n)
	PromptN      int      `json:"prompt_n"`      // processed by this request
	CacheN       int      `json:"cache_n"`       // reused from the earlier turns
	PrefillTPS   float64  `json:"prefill_tps"`
	DecodeTPS    float64  `json:"decode_tps"`
	TTFTMS       *float64 `json:"ttft_ms,omitempty"`
	WallMS       float64  `json:"wall_ms"`
	PredictedN   int      `json:"predicted_n"`
}

// ConcurrentItem is one request of a concurrent_requests phase.
type ConcurrentItem struct {
	Index      int      `json:"index"`
	Status     int      `json:"status"`
	WaitMS     *float64 `json:"wait_ms,omitempty"`
	WallMS     float64  `json:"wall_ms"`
	DecodeTPS  float64  `json:"decode_tps,omitempty"`
	PrefillTPS float64  `json:"prefill_tps,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// ConcurrentResult is what a concurrent_requests phase measured.
type ConcurrentResult struct {
	N      int              `json:"n"`
	WallMS float64          `json:"wall_ms"` // from the first send to the last answer
	Errors int              `json:"errors"`
	Items  []ConcurrentItem `json:"items"`
}

// UnloadResult is what an unload phase measured.
type UnloadResult struct {
	Loaded  bool    `json:"was_loaded"` // the profile had an instance when the phase began
	Gone    bool    `json:"gone"`
	UnloadS float64 `json:"unload_s"` // from the request until the instance was gone
}

// PhaseResult is what one phase of a step did and measured.
type PhaseResult struct {
	ID         int64             `json:"-"`
	StepID     int64             `json:"-"`
	RunID      string            `json:"-"`
	Idx        int               `json:"idx"`
	Type       string            `json:"type"`
	Label      string            `json:"label,omitempty"`
	State      string            `json:"state"`
	Outcome    string            `json:"outcome,omitempty"` // ready, refused, failed, error, cancelled; empty while running
	Error      string            `json:"error,omitempty"`
	StartedAt  int64             `json:"started_at,omitempty"`
	FinishedAt int64             `json:"finished_at,omitempty"`
	DurationS  float64           `json:"duration_s"`
	Load       *LoadResult       `json:"load,omitempty"`
	Stats      *Stats            `json:"stats,omitempty"`
	Turns      []TurnResult      `json:"turns,omitempty"`
	Concurrent *ConcurrentResult `json:"concurrent,omitempty"`
	HeldS      *float64          `json:"held_s,omitempty"`
	// LoadedAfterHold says whether the model was still loaded when a hold phase ended (hold phases only).
	LoadedAfterHold *bool         `json:"loaded_after_hold,omitempty"`
	Unload          *UnloadResult `json:"unload,omitempty"`
	// Cold is what a load phase with the cold option did to the file caches.
	Cold *ColdResult `json:"cold,omitempty"`
	// Tasks is what a tasks phase sent and got back (tasks.go), without the
	// answer texts, which are stored apart.
	Tasks *TasksResult `json:"tasks,omitempty"`
}

// WorkerEnv is a worker's identity at the time of a run.
type WorkerEnv struct {
	Node             string `json:"node"`
	RPCServerVersion string `json:"rpc_server_version,omitempty"`
}

// Environment is what a run ran on, so that results of different setups are not mixed up.
type Environment struct {
	SchedulerVersion string      `json:"scheduler_version,omitempty"`
	ConfigRevision   string      `json:"config_revision,omitempty"`
	LlamaBinary      string      `json:"llama_binary,omitempty"`
	Workers          []WorkerEnv `json:"workers,omitempty"`
}

// Key is a canonical string; two environments are the same when their keys are.
func (e *Environment) Key() string {
	if e == nil {
		return ""
	}
	c := *e
	c.Workers = append([]WorkerEnv(nil), e.Workers...)
	sort.Slice(c.Workers, func(i, j int) bool { return c.Workers[i].Node < c.Workers[j].Node })
	b, _ := json.Marshal(c)
	return string(b)
}

// Step is one benchmark execution within a run.
type Step struct {
	ID          int64                `json:"id"`
	RunID       string               `json:"-"`
	Idx         int                  `json:"idx"`
	Group       string               `json:"group,omitempty"` // the parallel step this ran in
	BenchmarkID string               `json:"benchmark_id"`
	Name        string               `json:"name"`
	Profile     string               `json:"profile"`
	State       string               `json:"state"`
	Outcome     string               `json:"outcome,omitempty"`
	Reason      string               `json:"reason,omitempty"` // the scheduler's words for a refusal or failure
	HTTPStatus  int                  `json:"http_status,omitempty"`
	LogFile     string               `json:"log_file,omitempty"`
	StartedAt   int64                `json:"started_at,omitempty"`
	FinishedAt  int64                `json:"finished_at,omitempty"`
	Spec        *benchspec.Benchmark `json:"spec,omitempty"`
	Plan        json.RawMessage      `json:"plan,omitempty"`
	Load        *LoadResult          `json:"load,omitempty"`
	Stats       *Stats               `json:"stats,omitempty"`
	Expect      *ExpectResult        `json:"expect,omitempty"`
	Comparison  *Comparison          `json:"comparison,omitempty"`
	Error       string               `json:"error,omitempty"`
	ModelFile   string               `json:"model_file,omitempty"` // the model file the profile loaded
	NodeCount   int                  `json:"node_count,omitempty"` // worker nodes the model was spread over
	Phases      []PhaseResult        `json:"phases,omitempty"`
	Requests    []RequestResult      `json:"requests,omitempty"`
	// MemLow, Roles and Reload are recorded since the Benchmarks page shows
	// them; a step stored before has none of them ("not recorded").
	MemLow []NodeMemLow `json:"mem_low,omitempty"`
	Roles  []NodeRole   `json:"roles,omitempty"`
	Reload []NodeReload `json:"reload_from_cache,omitempty"`
}

// RunSummary counts a run's steps.
type RunSummary struct {
	Steps int `json:"steps"`
	// Ran counts the steps that were judged (passed or failed).
	// RefusedByExclusion counts the steps with the outcome
	// refused_by_exclusion: not judged, neither passed nor failed. (An
	// ordinary refusal, outcome "refused", is judged against the expectation
	// and counts as passed or failed.) Skipped steps are in none of these.
	Ran                int `json:"ran"`
	Passed             int `json:"passed"`
	Failed             int `json:"failed"`
	RefusedByExclusion int `json:"refused_by_exclusion"`
	Regressions        int `json:"regressions"`
}

// Run kinds: what a run executed.
const (
	RunKindSuite     = "suite"     // a suite (every run before release 0.1.8's data model)
	RunKindBenchmark = "benchmark" // one benchmark, run directly: SuiteID is empty, SuiteName its title
)

// Run is one execution of a suite or of one benchmark.
type Run struct {
	ID string `json:"id"`
	// Kind is suite or benchmark; BenchmarkID names the benchmark of a direct run.
	Kind          string          `json:"kind"`
	BenchmarkID   string          `json:"benchmark_id,omitempty"`
	SuiteID       string          `json:"suite_id"`
	SuiteName     string          `json:"suite_name"`
	State         string          `json:"state"`
	Note          string          `json:"note,omitempty"`
	Trigger       string          `json:"trigger"` // manual or schedule
	BaselineRunID string          `json:"baseline_run_id,omitempty"`
	QueuedAt      int64           `json:"queued_at"`
	StartedAt     int64           `json:"started_at,omitempty"`
	FinishedAt    int64           `json:"finished_at,omitempty"`
	Error         string          `json:"error,omitempty"`
	Version       string          `json:"version,omitempty"`
	Suite         json.RawMessage `json:"suite,omitempty"` // the suite as it was when the run was queued
	Env           *Environment    `json:"env,omitempty"`   // what the run ran on
	Summary       *RunSummary     `json:"summary,omitempty"`
	Steps         []Step          `json:"steps,omitempty"`
	// Pinned: never trimmed by retention (bench_retention_days).
	Pinned bool `json:"pinned"`
	// TrimmedAt (unix ms) is when retention removed the run's requests and
	// transcripts; the run, its steps and its measurements stay.
	TrimmedAt int64 `json:"trimmed_at,omitempty"`
}

// RunFilter selects runs for a listing.
type RunFilter struct {
	Suite string
	State string
	Limit int
	// Kind: only suite runs or only direct benchmark runs. Benchmark: the
	// direct runs of a benchmark and every run with a step of it.
	Kind      string
	Benchmark string
	// Model: the runs with a step on that model file. A step recorded
	// before steps carried their file matches by its profile, when
	// ModelProfiles names it (the profiles of that file now).
	Model         string
	ModelProfiles []string
}

// TrendPoint is one value of a metric over past runs.
type TrendPoint struct {
	RunID     string  `json:"run_id"`
	StartedAt int64   `json:"started_at"`
	Value     float64 `json:"value"`
}
