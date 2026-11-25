package scheduler

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/gguf"
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
	// (Estimate: a windowed bound, "at most"); Checkpoints ctx_checkpoints
	// × slots.
	Family      string
	CellMiB     float64
	Estimate    bool
	Checkpoints int
	// SizeUnknown: a windowed model whose window layers are not known from
	// its header and whose checkpoint the server has not logged yet this
	// run: no figure is shown, and the checkpoints add 0 to the term.
	// FromLog: CellMiB is the largest checkpoint the server logged for the
	// model file this run (held in memory only).
	SizeUnknown bool
	FromLog     bool
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
	return estimate.Checkpoint(meta, q, p.Ctx), nil // p.Ctx is per slot
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
		CheckpointsMiB: ck.TotalMiB(n), OnNode: onNode, SizeUnknown: ck.Unknown}
	if ck.Unknown {
		if seen := c.ckptSeen[p.Model]; seen > 0 { // read at a load this run
			h.SizeUnknown, h.FromLog, h.Estimate, h.CellMiB = false, true, true, seen
			h.CheckpointsMiB = int(math.Ceil(float64(n)*seen - 1e-9))
		}
	}
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

// serverName is the scheduler's machine as plans name it: its local node
// (the lowest id when there are several, so the name never moves between
// two reads), else "the scheduler's machine".
func (c *schedCore) serverName() string {
	var ids []core.NodeID
	for id, n := range c.raw {
		if n.Local {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "the scheduler's machine"
	}
	slices.Sort(ids)
	return c.nodeName(ids[0])
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
		if hm.SizeUnknown {
			r.HostMemoryNote = fmt.Sprintf("on %s: up to %d MiB, not placed (context checkpoints: size not yet known, read at the first load; prompt cache %d MiB)",
				hm.Machine, hm.TermMiB(), hm.PromptCacheMiB)
		} else {
			r.HostMemoryNote = fmt.Sprintf("on %s: up to %d MiB, not placed (context checkpoints %d MiB, prompt cache %d MiB)",
				hm.Machine, hm.TermMiB(), hm.CheckpointsMiB, hm.PromptCacheMiB)
		}
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
	// SizeUnknown names the instances whose checkpoint size is not known
	// yet (HostMemory.SizeUnknown): they add their prompt cache only.
	SizeUnknown []string
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
		if hm.SizeUnknown {
			s.SizeUnknown = append(s.SizeUnknown, in.prof.Name)
		}
	}
	sort.Strings(s.Instances)
	sort.Strings(s.SizeUnknown)
	return s
}

// ckptDriftPct is how far the logged size may be from the formula before a
// WARN event says so. The recurrent formula is exact (the server prints
// 50.25 and 50.251 MiB for a 50.25 MiB cell, 0.002% apart), so 1% only
// leaves room for the log's rounding and a few bytes of metadata.
const ckptDriftPct = 1.0

// checkHostMemorySizes compares what the server logged against the
// formula, once per instance and source: the recurrent state at load (one
// cell per slot) and the first checkpoint created, more than ckptDriftPct
// apart either way; for a window model, any checkpoint above the bound by
// more than ckptDriftPct (a smaller one is normal: a conversation shorter
// than the window). Either is a WARN learnedNeed event naming both figures.
// For a window model whose size is not known from its header, the largest
// checkpoint logged is kept for the model file for the run (ckptSeen).
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
		if f.SizeUnknown || f.FromLog {
			if hm.CheckpointMiB > c.ckptSeen[in.prof.Model] {
				if c.ckptSeen == nil {
					c.ckptSeen = map[string]float64{}
				}
				c.ckptSeen[in.prof.Model] = hm.CheckpointMiB
			}
			continue // nothing to compare with: the log is the figure
		}
		if f.Family == string(gguf.FamilyWindowed) {
			if in.ckptChecked&2 == 0 && f.CellMiB > 0 && hm.CheckpointMiB > f.CellMiB*(1+ckptDriftPct/100) {
				in.ckptChecked |= 2
				d := 100 * (hm.CheckpointMiB - f.CellMiB) / f.CellMiB
				c.recordInst(in, history.TLearnedNeed, history.Warn,
					fmt.Sprintf("A context checkpoint of %s is %.2f MiB in the server log; the window bound says at most %.2f MiB (%.1f%% above). The host memory shown for it may be wrong.",
						in.prof.Name, hm.CheckpointMiB, f.CellMiB, d),
					map[string]any{"logged_mib": hm.CheckpointMiB, "formula_mib": f.CellMiB, "family": f.Family, "source": "context checkpoint"})
			}
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
