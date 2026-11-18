package scheduler

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// forcedFit is the outcome of defaults.forced_fit_check for one forced
// profile: its memory need against the sum of the forced nodes' free memory.
// Tier headroom is not taken off (a forced profile may eat into it).
type forcedFit struct {
	mode     string // config.ForcedFitWarn or config.ForcedFitReject
	need     int    // MiB
	free     int    // MiB, summed over the forced nodes
	measured bool   // need is needs_mib, not the estimate
	nodes    []string
}

func (f *forcedFit) basis() string {
	if f.measured {
		return "measured"
	}
	return "estimated"
}

// message is the plain-words account used in the 503, the warning and the plan.
func (f *forcedFit) message(profile string) string {
	return fmt.Sprintf("profile %s needs %d MiB (%s) but the nodes it is forced onto (%s) have only %d MiB free in total",
		profile, f.need, f.basis(), strings.Join(f.nodes, ", "), f.free)
}

// forcedFitCheck compares p's need with the free memory of the given nodes
// (the forced placement) as the scheduler holds it: the pre-flight figure
// while it lasts, else the fleet's. It returns nil when the check is off,
// the profile is not forced, the need is unknown, or the need fits.
func (c *schedCore) forcedFitCheck(p *core.Profile, ids []core.NodeID) *forcedFit {
	mode := c.cfg.Defaults.ForcedFitCheck
	if (mode != config.ForcedFitWarn && mode != config.ForcedFitReject) || len(p.ForceNodes) == 0 || len(ids) == 0 {
		return nil
	}
	info, err := c.need(p)
	if err != nil {
		return nil
	}
	f := &forcedFit{mode: mode, need: info.Need.MiB, measured: info.Need.Measured}
	for _, id := range ids {
		n, ok := c.node(id)
		if !ok {
			return nil
		}
		f.free += c.freeMiB(id, n)
		f.nodes = append(f.nodes, nodeLabel(id, n))
	}
	if f.need <= f.free {
		return nil
	}
	return f
}

// forcedFitErr is the refusal for reject mode: 503, like the other
// placement refusals. Free memory can change, so it carries a Retry-After.
func (f *forcedFit) err(profile string) *Error {
	e := err503(ReasonForcedDoesNotFit, f.message(profile)+" (defaults.forced_fit_check is reject)", queueRetryAfter)
	e.NeedMiB, e.CapacityMiB = f.need, f.free
	return e
}

var _ = http.StatusOK
