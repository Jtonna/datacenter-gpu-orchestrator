package scheduler

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Load on chosen zones (Zones-Fleet-Detail "Load a model"): one load of a
// profile placed on nodes the operator picked, as force_nodes would place
// it, without changing the profile. The load runs from a copy of the
// profile with ForceNodes set (and a tensor split that does not match
// dropped); c.pins remembers the copies, so the status names the chosen
// nodes and the instance is not reported stale for them. The next load of
// the profile uses the configuration again.

// pinCopy is a one-off copy of p placed on nodes (for a plan: nothing kept).
func pinCopy(p *core.Profile, nodes []string) *core.Profile {
	cp := *p
	cp.ForceNodes = slices.Clone(nodes)
	if len(cp.TensorSplit) != len(nodes) {
		cp.TensorSplit = nil
	}
	return &cp
}

// pinned is pinCopy, remembered for the load it starts.
func (c *schedCore) pinned(p *core.Profile, nodes []string) *core.Profile {
	cp := pinCopy(p, nodes)
	c.pins[cp] = cp.ForceNodes
	return cp
}

// prunePins forgets the pinned copies no entry or instance holds any more
// (a refused or finished entry, a stopped instance).
func (c *schedCore) prunePins() {
	if len(c.pins) == 0 {
		return
	}
	for p := range c.pins {
		if e := c.entries[p.Name]; e != nil && e.prof == p {
			continue
		}
		if in := c.insts[p.Name]; in != nil && in.prof == p {
			continue
		}
		delete(c.pins, p)
	}
}

// pinConflict refuses a pinned load while the profile is loaded, loading,
// stopping or queued: its nodes cannot be chosen then.
func (c *schedCore) pinConflict(p *core.Profile) *Error {
	what := ""
	switch {
	case c.insts[p.Name] != nil:
		what = "is " + c.insts[p.Name].state.String()
	case c.entries[p.Name] != nil:
		what = "is queued"
	default:
		return nil
	}
	return &Error{Status: http.StatusConflict, Reason: ReasonPinConflict,
		Message: fmt.Sprintf("%s %s: unload it (or let the queued load start) before loading it on chosen nodes", p.Name, what)}
}
