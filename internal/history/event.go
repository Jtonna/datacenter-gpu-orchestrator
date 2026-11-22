// Package history keeps the scheduler's safety and lifecycle events: a JSON
// Lines file set on disk (rotated by size, oldest deleted), the newest events
// in memory for queries, and the query and summary functions behind
// GET /api/history (design 9.1).
//
// Recording never blocks and never fails: Record only appends to a bounded
// in-memory ring and hands the event to a buffered channel that a writer
// goroutine drains; when the channel is full the event is counted as dropped.
// A bad directory or a write error warns once and history continues without
// the file part.
package history

import "strings"

// Severities, in increasing order.
const (
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// SeverityRank orders the severities; an unknown one ranks as info.
func SeverityRank(s string) int {
	switch s {
	case Warn:
		return 1
	case Error:
		return 2
	}
	return 0
}

// Event types.
const (
	TSchedulerStarted   = "schedulerStarted"
	TSchedulerStopping  = "schedulerStopping"
	TLoadStarted        = "loadStarted"
	TLoadFinished       = "loadFinished"
	TRequestRefused     = "requestRefused"
	TLoadStalled        = "loadStalled"
	TLoadLowMemory      = "loadCancelledLowMemory"
	TNodeLost           = "nodeLost"
	TEviction           = "eviction"
	TInstanceUnloaded   = "instanceUnloaded"
	TChildExited        = "childExited"
	TWorkerHealth       = "workerHealth"
	TNodeQuarantined    = "nodeQuarantined"
	TQuarantineEnded    = "quarantineEnded"
	TProfileBackedOff   = "profileBackedOff"
	TRecoveryRequest    = "recoveryRequest"
	TRecoveryResponse   = "recoveryResponse"
	TRecoveryOutcome    = "recoveryOutcome"
	TRecoveryStopped    = "recoverySequenceStopped"
	TFleetViewStale     = "fleetViewStale"
	TFleetViewRecovered = "fleetViewRecovered"
	TFleetFeedDown      = "fleetFeedDisconnected"
	TFleetFeedRecovered = "fleetFeedRecovered"
	TPreflightFailed    = "preflightFailed"
	TLearnedNeed        = "learnedNeed"
	TNeedsMiBTooLow     = "needsMibTooLow"
	TNodeMemory         = "nodeMemory"
	TBenchRunStarted    = "benchRunStarted"
	TBenchStepFinished  = "benchStepFinished"
	TBenchRunFinished   = "benchRunFinished"

	// Agents membership (agent contract): a worker launch refused or
	// failed, a launch that exited, node membership changes and the
	// operator's node actions.
	TWorkerLaunchFailed = "workerLaunchFailed"
	TWorkerExited       = "workerExited"
	TNodeMembership     = "nodeMembership"
	TNodeAdmin          = "nodeAdmin"
)

// Event is one history record. Ts is unix milliseconds; Seq is monotonic per
// process (events loaded from an earlier run keep theirs).
type Event struct {
	Ts        int64          `json:"ts"`
	Seq       uint64         `json:"seq"`
	Type      string         `json:"type"`
	Severity  string         `json:"severity"`
	Profile   string         `json:"profile,omitempty"`
	ModelFile string         `json:"model_file,omitempty"`
	Nodes     []string       `json:"nodes"`
	SessionID string         `json:"session_id,omitempty"`
	Summary   string         `json:"summary"`
	Data      map[string]any `json:"data"`
}

// Query selects events. Zero fields do not filter.
type Query struct {
	Since, Until int64    // unix ms, inclusive
	Types        []string // any of
	MinSeverity  string
	Node         string
	Profile      string
	SessionID    string
	Text         string // substring of the summary, case-insensitive
	Limit        int    // default 200, at most MaxLimit
	Asc          bool   // oldest first; default newest first
}

// Default and maximum query limits.
const (
	DefaultLimit = 200
	MaxLimit     = 2000
)

func (q Query) limit() int {
	switch {
	case q.Limit <= 0:
		return DefaultLimit
	case q.Limit > MaxLimit:
		return MaxLimit
	}
	return q.Limit
}

// Match reports whether e passes the filters (not Limit or order).
func (q Query) Match(e *Event) bool {
	if q.Since > 0 && e.Ts < q.Since {
		return false
	}
	if q.Until > 0 && e.Ts > q.Until {
		return false
	}
	if len(q.Types) > 0 {
		ok := false
		for _, t := range q.Types {
			if t == e.Type {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q.MinSeverity != "" && SeverityRank(e.Severity) < SeverityRank(q.MinSeverity) {
		return false
	}
	if q.Profile != "" && e.Profile != q.Profile {
		return false
	}
	if q.SessionID != "" && e.SessionID != q.SessionID {
		return false
	}
	if q.Node != "" {
		ok := false
		for _, n := range e.Nodes {
			if n == q.Node {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q.Text != "" && !strings.Contains(strings.ToLower(e.Summary), strings.ToLower(q.Text)) {
		return false
	}
	return true
}

// Result is the answer to a query.
type Result struct {
	Events []Event `json:"events"`
	// TotalMatching counts the matching events inside the in-memory window
	// plus those found in any file part that had to be read; when Truncated is
	// true there may be more.
	TotalMatching int    `json:"total_matching"`
	OldestTs      int64  `json:"oldest_ts"`
	History       string `json:"history"` // "on" or "off"
	Truncated     bool   `json:"truncated"`
}
