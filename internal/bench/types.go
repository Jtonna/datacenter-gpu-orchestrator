// Package bench is the benchmark feature (design 9.3): a SQLite store of
// benchmarks, suites and runs, a runner that drives the scheduler through its
// own HTTP API, the statistics and the comparison with a baseline run, and the
// export formats. Nothing here touches scheduler internals.
package bench

import (
	"encoding/json"
	"errors"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
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
	OutcomeReady     = "ready"     // loaded and served
	OutcomeRefused   = "refused"   // 4xx or 503 before any load started (does not fit, backoff, ...)
	OutcomeFailed    = "failed"    // the load or a request failed (502, child exited, timeout)
	OutcomeError     = "error"     // the runner could not do its job (network, bad answer)
	OutcomeCancelled = "cancelled" // the run was cancelled during this step
	OutcomeSkipped   = "skipped"   // never started (stop_on_failure, cancel)
)

// Request phases.
const (
	PhaseLoad     = "load" // the first request of a cold step: it triggers the load, so its time includes it
	PhaseWarmup   = "warmup"
	PhaseMeasured = "measured"
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
}

// Check is one expectation and whether it held.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// ExpectResult is the verdict on a benchmark's expect block.
type ExpectResult struct {
	Pass   bool    `json:"pass"`
	Checks []Check `json:"checks"`
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
	Requests    []RequestResult      `json:"requests,omitempty"`
}

// RunSummary counts a run's steps.
type RunSummary struct {
	Steps       int `json:"steps"`
	Passed      int `json:"passed"`
	Failed      int `json:"failed"`
	Regressions int `json:"regressions"`
}

// Run is one execution of a suite.
type Run struct {
	ID            string          `json:"id"`
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
	Summary       *RunSummary     `json:"summary,omitempty"`
	Steps         []Step          `json:"steps,omitempty"`
}

// RunFilter selects runs for a listing.
type RunFilter struct {
	Suite string
	State string
	Limit int
}

// TrendPoint is one value of a metric over past runs.
type TrendPoint struct {
	RunID     string  `json:"run_id"`
	StartedAt int64   `json:"started_at"`
	Value     float64 `json:"value"`
}
