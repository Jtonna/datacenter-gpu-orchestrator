package scheduler

import (
	"fmt"
	"math"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// The host process's own memory (p25): besides its device share, a model
// server keeps in the RAM of the machine that hosts it
//
//   - context checkpoints: up to ctx_checkpoints per slot, each one
//     recurrent cell for a recurrent or hybrid family, the sliding window's
//     cache for a windowed one (an estimate: the source's bound), none for
//     plain attention (estimate.Checkpoint);
//   - the prompt cache: up to --cache-ram MiB (cache_ram_mib on the
//     scheduler's machine, node_host_cache_ram_mib on a node).
//
// Both are ceilings the server fills as conversations go, not what it takes
// at load. On a node the term joins the host node's reservation beside the
// host overhead when count_host_memory is on (off by default: then it is
// shown and nothing changes). On the scheduler's machine nothing is placed:
// the plan shows it, and the status sums it over the loaded instances.

// HostMemory is the host-memory term of a profile.
type HostMemory struct {
	// Family is plain, recurrent or windowed; CellMiB one checkpoint
	// (Estimate: a windowed bound); Checkpoints ctx_checkpoints × slots.
	Family      string
	CellMiB     float64
	Estimate    bool
	Checkpoints int
	// CheckpointsMiB is Checkpoints × CellMiB, rounded up; PromptCacheMiB
	// the --cache-ram the launch passes where it is hosted.
	CheckpointsMiB int
	PromptCacheMiB int
	// OnNode: hosted on a node (the host node's reservation); otherwise on
	// the scheduler's machine (Machine), where nothing is placed.
	OnNode  bool
	Machine string
	// Counted: TermMiB is in the host node's reservation (count_host_memory).
	Counted bool
}

// TermMiB is the whole term: checkpoints and prompt cache.
func (h HostMemory) TermMiB() int { return h.CheckpointsMiB + h.PromptCacheMiB }

// CheckpointSizer is a NeedProvider that knows a profile's context
// checkpoint (ConfigNeeds, from the GGUF header).
type CheckpointSizer interface {
	CheckpointOf(p *core.Profile) (estimate.Ckpt, error)
}

// CheckpointOf is p's context checkpoint from its model's GGUF header.
func (c *ConfigNeeds) CheckpointOf(p *core.Profile) (estimate.Ckpt, error) {
	if p.Kind != core.KindLlama {
		return estimate.Ckpt{}, nil
	}
	path, err := modelpath.Join(c.Cfg.ModelsDir, p.Model)
	if err != nil {
		return estimate.Ckpt{}, err
	}
	c.mu.Lock()
	defer supervise.Unlocked("scheduler", c.mu.Unlock)
	meta, err := c.metaOf(path)
	if err != nil {
		return estimate.Ckpt{}, err
	}
	q := p.KVQuant
	if q == "" {
		q = "f16"
	}
	return estimate.Checkpoint(meta, q), nil
}

// hostMemoryOf is p's host-memory term hosted on a node (onNode) or on the
// scheduler's machine; false for a command profile or an unreadable header.
func (c *schedCore) hostMemoryOf(p *core.Profile, onNode bool) (HostMemory, bool) {
	if p == nil || p.Kind != core.KindLlama {
		return HostMemory{}, false
	}
	cs, ok := c.needs.(CheckpointSizer)
	if !ok {
		return HostMemory{}, false
	}
	ck, err := cs.CheckpointOf(p)
	if err != nil || ck.Family == "" {
		return HostMemory{}, false
	}
	d := c.cfg.Defaults
	n := max(d.CtxCheckpoints, 0) * max(p.Parallel, 1)
	h := HostMemory{Family: string(ck.Family), CellMiB: ck.CellMiB(), Estimate: ck.Estimate, Checkpoints: n,
		CheckpointsMiB: ck.TotalMiB(n), OnNode: onNode}
	if onNode {
		h.PromptCacheMiB = max(d.NodeHostCacheRAMMiB, 0)
		h.Counted = d.CountHostMemory
	} else {
		h.PromptCacheMiB = max(d.CacheRAMMiB, 0)
		h.Machine = c.serverName()
	}
	return h, true
}

// hostTermOn is what the host-memory term adds to node id's reservation for
// p on pl: the term on the host node when count_host_memory is on, else 0.
func (c *schedCore) hostTermOn(p *core.Profile, pl core.Placement, id core.NodeID) int {
	if !c.cfg.Defaults.CountHostMemory {
		return 0
	}
	if h := hostOf(p, pl); h == "" || h != id {
		return 0
	}
	if hm, ok := c.hostMemoryOf(p, true); ok {
		return hm.TermMiB()
	}
	return 0
}

// serverName is the scheduler's machine as plans name it.
func (c *schedCore) serverName() string {
	for id, n := range c.raw {
		if n.Local {
			return c.nodeName(id)
		}
	}
	return "the scheduler's machine"
}

// planHostMemory fills r.HostMemory and, for the scheduler's machine,
// r.HostMemoryNote.
func (c *schedCore) planHostMemory(r *PlanReport, p *core.Profile) {
	hm, ok := c.hostMemoryOf(p, p.Host == core.HostNode)
	if !ok {
		return
	}
	r.HostMemory = &hm
	if !hm.OnNode {
		r.HostMemoryNote = fmt.Sprintf("on %s: up to %d MiB, not placed (context checkpoints %d MiB, prompt cache %d MiB)",
			hm.Machine, hm.TermMiB(), hm.CheckpointsMiB, hm.PromptCacheMiB)
	}
}

// ServerHostMemory is the host-memory term summed over the instances loaded
// on the scheduler's machine (Status): the most their host processes may
// take there beyond what the load log showed.
type ServerHostMemory struct {
	Machine        string
	CheckpointsMiB int
	PromptCacheMiB int
	Instances      []string
}

// serverHostMemory sums the term over the loaded instances hosted on the
// scheduler's machine.
func (c *schedCore) serverHostMemory() ServerHostMemory {
	s := ServerHostMemory{Machine: c.serverName()}
	for _, in := range c.insts {
		if in.prof == nil || in.prof.Kind != core.KindLlama || hostOf(in.prof, in.placement) != "" {
			continue
		}
		hm, ok := c.hostMemoryOf(in.prof, false)
		if !ok {
			continue
		}
		s.CheckpointsMiB += hm.CheckpointsMiB
		s.PromptCacheMiB += hm.PromptCacheMiB
		s.Instances = append(s.Instances, in.prof.Name)
	}
	sort.Strings(s.Instances)
	return s
}

// ckptDriftPct is how far the logged size may be from the formula before a
// WARN event says so.
const ckptDriftPct = 5.0

// checkHostMemorySizes compares, once per instance and source, what the
// server logged against the formula: the recurrent state at load (one cell
// per slot) and the first checkpoint created. A difference of more than
// ckptDriftPct is a WARN learnedNeed event naming both figures.
func (c *schedCore) checkHostMemorySizes() {
	for _, in := range c.insts {
		if in.child == nil || in.prof == nil || in.prof.Kind != core.KindLlama || in.ckptChecked == 3 {
			continue
		}
		hm := in.child.Load().HostMem
		if hm.RecurrentMiB == 0 && hm.CheckpointMiB == 0 {
			continue
		}
		f, ok := c.hostMemoryOf(in.prof, hostOf(in.prof, in.placement) != "")
		if !ok {
			in.ckptChecked = 3
			continue
		}
		check := func(bit int, what string, logged float64) {
			if in.ckptChecked&bit != 0 || logged == 0 {
				return
			}
			in.ckptChecked |= bit
			if f.CellMiB == 0 {
				return
			}
			if d := 100 * math.Abs(logged-f.CellMiB) / f.CellMiB; d > ckptDriftPct {
				c.recordInst(in, history.TLearnedNeed, history.Warn,
					fmt.Sprintf("The %s of %s is %.2f MiB in the server log; the checkpoint formula says %.2f MiB (%.1f%% apart). The host memory shown for it may be wrong.",
						what, in.prof.Name, logged, f.CellMiB, d),
					map[string]any{"logged_mib": logged, "formula_mib": f.CellMiB, "family": f.Family, "source": what})
			}
		}
		if f.Family == "recurrent" {
			check(1, "recurrent state per slot", hm.RecurrentMiB/float64(max(in.prof.Parallel, 1)))
		} else {
			in.ckptChecked |= 1
		}
		check(2, "context checkpoint", hm.CheckpointMiB)
	}
}
