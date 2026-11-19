package scheduler

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// forcedFit is the outcome of defaults.forced_fit_check for one forced
// profile. Two things are compared:
//
//   - the memory need against the sum of the forced nodes' usable memory. On a
//     dedicated-VRAM node that is its free memory with no headroom taken off (a
//     forced profile may eat into it). On a shared-memory node (a PS5: GPU
//     memory is system RAM) it is the smaller of the fleet-based budget (what
//     the automatic placer uses) and pre-flight free minus tier headroom,
//     because the Vulkan pre-flight reports nearly all the RAM as free and a
//     load that exhausts it takes the machine down;
//   - for a placement of two or more nodes, every shared-memory node's
//     expected share against that node's own usable memory (sharedShortfalls):
//     a split model does not land evenly, so one node can be overfilled while
//     the total fits.
//
// The need is the estimate with defaults.estimate_margin when it is an
// estimate, a measurement learned this run when there is one, else needs_mib.
type forcedFit struct {
	mode     string // config.ForcedFitWarn or config.ForcedFitReject
	need     int    // MiB, with the estimate margin where it is an estimate
	free     int    // MiB usable, summed over the forced nodes
	measured bool   // need is measured (needs_mib or learned), not an estimate
	basis    string // plain words: where the need comes from
	total    bool   // the total does not fit
	nodes    []string
	detail   []string    // per node: which figure was used, in plain words
	short    []nodeShort // shared-memory nodes whose own share does not fit
}

// message is the plain-words account used in the 503, the warning and the plan.
func (f *forcedFit) message(profile string) string {
	var parts []string
	if f.total {
		parts = append(parts, fmt.Sprintf("profile %s: need %d MiB (%s), but the nodes it is forced onto have only %d MiB usable in total (%s)",
			profile, f.need, f.basis, f.free, strings.Join(f.detail, "; ")))
	}
	for _, s := range f.short {
		parts = append(parts, fmt.Sprintf("profile %s: need %d MiB (%s) split over the forced nodes, %s", profile, f.need, f.basis, s.text()))
	}
	return strings.Join(parts, "; ")
}

// forcedFitCheck compares p's need with the usable memory of the nodes of the
// forced placement pl as the scheduler holds it: the pre-flight figure while it
// lasts, else the fleet's, on dedicated-VRAM nodes; the smaller of the two,
// minus tier headroom, on shared-memory nodes. It returns nil when the check
// is off, the profile is not forced, the need is unknown, or everything fits.
func (c *schedCore) forcedFitCheck(p *core.Profile, pl core.Placement) *forcedFit {
	mode := c.cfg.Defaults.ForcedFitCheck
	if (mode != config.ForcedFitWarn && mode != config.ForcedFitReject) || len(p.ForceNodes) == 0 || len(pl.Nodes) == 0 {
		return nil
	}
	info, err := c.need(p)
	if err != nil {
		return nil
	}
	f := &forcedFit{mode: mode, need: info.Need.WithMargin(), measured: info.Need.Measured, basis: c.needBasis(info)}
	for _, pn := range pl.Nodes {
		n, ok := c.node(pn.ID)
		if !ok {
			return nil
		}
		label := nodeLabel(pn.ID, n)
		usable, how := c.freeMiB(pn.ID, n), "free memory, no headroom kept"
		if n.SharedMemory {
			usable, how = c.sharedBudget(pn.ID, n)
		}
		f.free += usable
		f.nodes = append(f.nodes, label)
		f.detail = append(f.detail, fmt.Sprintf("usable %d MiB on %s (%s)", usable, label, how))
	}
	f.total = f.need > f.free
	f.short = c.sharedShortfalls(p, info, pl)
	if !f.total && len(f.short) == 0 {
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
