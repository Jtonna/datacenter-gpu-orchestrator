package placer

import (
	"fmt"
	"strings"
)

// Reason says why there is no placement.
type Reason int

const (
	// ForcedUnavailable: a force_nodes entry is not in the usable set.
	// NoPlacement.Missing names them. 8.3: "forced nodes unavailable".
	ForcedUnavailable Reason = iota + 1
	// NoFit: no node or pool of at most max_pool usable nodes fits the
	// need (Place: the free nodes; Plan: free plus evictable nodes).
	NoFit
	// ExceedsCapacity: the need does not fit the best max_pool eligible
	// nodes at plan budgets (7.2); the request fails at once with 503.
	ExceedsCapacity
	// InvalidInput: the caller broke a documented precondition.
	InvalidInput
)

func (r Reason) String() string {
	switch r {
	case ForcedUnavailable:
		return "forced nodes unavailable"
	case NoFit:
		return "does not fit"
	case ExceedsCapacity:
		return "exceeds cluster capacity"
	case InvalidInput:
		return "invalid input"
	}
	return fmt.Sprintf("Reason(%d)", int(r))
}

// NoPlacement is the error every function in this package returns when
// there is no placement. Use errors.As to read it.
type NoPlacement struct {
	Reason Reason
	// Missing (ForcedUnavailable): force_nodes names not usable, in config order.
	Missing []string
	// NeedMiB is the need as given. RequiredMiB is what the fit rule asks
	// the summed budgets to reach: NeedMiB for a measured need,
	// ceil(NeedMiB × 1.05) for an estimated one.
	NeedMiB, RequiredMiB int
	// CapacityMiB (ExceedsCapacity) is the best max_pool nodes' summed
	// budgets as the fit rule counts them: Σ budget (measured) or
	// Σ max(budget − device_overhead, 0) (estimated).
	CapacityMiB int
	// Detail (InvalidInput) describes the broken precondition.
	Detail string
}

func (e *NoPlacement) Error() string {
	switch e.Reason {
	case ForcedUnavailable:
		return "placer: forced nodes unavailable: " + strings.Join(e.Missing, ", ")
	case NoFit:
		return fmt.Sprintf("placer: need %d MiB (requires %d MiB) does not fit the available nodes", e.NeedMiB, e.RequiredMiB)
	case ExceedsCapacity:
		return fmt.Sprintf("placer: need %d MiB (requires %d MiB) exceeds cluster capacity %d MiB", e.NeedMiB, e.RequiredMiB, e.CapacityMiB)
	case InvalidInput:
		return "placer: invalid input: " + e.Detail
	}
	return "placer: no placement"
}
