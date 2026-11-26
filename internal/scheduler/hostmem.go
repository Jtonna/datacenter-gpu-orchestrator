package scheduler

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
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
	// SizeUnknown: a model the code has no exact checkpoint rule for (every
	// window model; a recurrent architecture not proven against a server
	// log) whose checkpoint the server has not logged yet, for this file
	// version at this context, this run: no figure is shown, and the
	// checkpoints add 0 to the term.
	// FromLog: CellMiB is the largest checkpoint the server logged at a
	// load of LoggedFile (LoggedFileBytes: its version) at LoggedCtx per
	// slot, at LoggedAt (held in memory for the run only: no table).
	SizeUnknown     bool
	FromLog         bool
	LoggedFile      string    `view:"omitempty"`
	LoggedFileBytes int64     `view:"omitempty"`
	LoggedCtx       int       `view:"omitempty"`
	LoggedAt        time.Time `view:"omitempty"`
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
	// PerSlot is the restore points per slot the launch passes
	// (--ctx-checkpoints). CheckpointsSet and PromptCacheSet: the value is
	// the profile's own field (ctx_checkpoints, cache_ram_mib), not the
	// cluster's; SetNote says so in words ("" when neither is set).
	PerSlot        int
	CheckpointsSet bool
	PromptCacheSet bool
	SetNote        string `view:"omitempty"`
}

// setNote: "set on this profile: 3 restore points per slot, prompt cache
// 0 MiB", naming only what the profile sets.
func (h HostMemory) setNote() string {
	var parts []string
	if h.CheckpointsSet {
		parts = append(parts, fmt.Sprintf("%d restore points per slot", h.PerSlot))
	}
	if h.PromptCacheSet {
		parts = append(parts, fmt.Sprintf("prompt cache %d MiB", h.PromptCacheMiB))
	}
	if len(parts) == 0 {
		return ""
	}
	return "set on this profile: " + strings.Join(parts, ", ")
}

// LoggedNote says where a FromLog checkpoint size was read: "checkpoint
// size read at a load of <file> (<bytes> bytes) at ctx <n> per slot".
func (h HostMemory) LoggedNote() string {
	if !h.FromLog {
		return ""
	}
	return fmt.Sprintf("checkpoint size %.2f MiB read at a load of %s (%d bytes) at ctx %d per slot", h.CellMiB, h.LoggedFile, h.LoggedFileBytes, h.LoggedCtx)
}

// ckptKey is a model file version at a context per slot: a checkpoint size
// read from a load's log holds for that file and context only.
type ckptKey struct {
	Model     string
	FileBytes int64
	Ctx       int
}

// ckptLogged is the largest checkpoint a load's log showed for a ckptKey.
type ckptLogged struct {
	MiB float64
	At  time.Time
}

// TermMiB is the whole term: checkpoints and prompt cache.
func (h HostMemory) TermMiB() int { return h.CheckpointsMiB + h.PromptCacheMiB }

// msgCkptFlagDropped is the one launch line for a profile whose own
// extra_args set a checkpoint flag the scheduler passes from the setting.
const msgCkptFlagDropped = "profile's own checkpoint flag dropped at launch"

// noteDroppedCkptFlag logs, in one line, a stored profile's own checkpoint
// flag that the launch drops (config.StripLateManaged, on the scheduler's
// machine and on a host node alike) with the value used instead, and
// keeps the same sentence on the instance (InstanceStatus.LaunchNote).
func (c *schedCore) noteDroppedCkptFlag(in *inst) {
	in.launchNote = ""
	dropped := config.LateManagedIn(in.prof.ExtraArgs)
	if len(dropped) == 0 {
		return
	}
	d := strings.Join(dropped, ", ")
	n, set := c.cfg.EffectiveCtxCheckpoints(in.prof)
	used := fmt.Sprintf("--ctx-checkpoints %d (setting ctx_checkpoints)", max(n, 0))
	if set {
		used = fmt.Sprintf("--ctx-checkpoints %d (set on this profile)", max(n, 0))
	}
	in.launchNote = fmt.Sprintf("the profile's own %s is not passed: the launch uses %s", d, used)
	c.log.Warn(msgCkptFlagDropped, "profile", in.prof.Name, "dropped", d, "used", used)
}

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
	perSlot, ckSet := c.cfg.EffectiveCtxCheckpoints(p)
	perSlot = max(perSlot, 0)
	n := perSlot * max(p.Parallel, 1)
	h := HostMemory{Family: string(ck.Family), CellMiB: ck.CellMiB(), Estimate: ck.Estimate, Checkpoints: n,
		CheckpointsMiB: ck.TotalMiB(n), OnNode: onNode, SizeUnknown: ck.Unknown, PerSlot: perSlot, CheckpointsSet: ckSet}
	if ck.Unknown {
		// read at a load this run, of this file version at this context
		if seen, ok := c.ckptSeen[ckptKey{p.Model, ck.FileBytes, p.Ctx}]; ok && seen.MiB > 0 {
			h.SizeUnknown, h.FromLog, h.CellMiB = false, true, seen.MiB
			h.LoggedFile, h.LoggedFileBytes, h.LoggedCtx, h.LoggedAt = p.Model, ck.FileBytes, p.Ctx, seen.At
			h.CheckpointsMiB = int(math.Ceil(float64(n)*seen.MiB - 1e-9))
		}
	}
	if onNode {
		h.PromptCacheMiB = max(d.NodeHostCacheRAMMiB, 0)
		h.Counted = d.CountHostMemory
	} else {
		h.PromptCacheMiB = max(d.CacheRAMMiB, 0)
		h.Machine = c.serverName()
	}
	if p.CacheRAMMiB != nil { // the profile's own, on either path (config EffectiveCacheRAMMiB)
		h.PromptCacheMiB, h.PromptCacheSet = max(*p.CacheRAMMiB, 0), true
	}
	h.SetNote = h.setNote()
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
	// an unknown checkpoint size makes the figure a floor, never a ceiling
	const notIncluded = "context checkpoints NOT included: size not yet known, read at the first load"
	if hm.OnNode && hm.SizeUnknown {
		counted := "not yet counted"
		if hm.Counted {
			counted = "counted in what it needs"
		}
		r.HostMemoryNote = fmt.Sprintf("on the host node: at least %d MiB, %s (prompt cache %d MiB; %s)",
			hm.TermMiB(), counted, hm.PromptCacheMiB, notIncluded)
	}
	if !hm.OnNode {
		if hm.SizeUnknown {
			r.HostMemoryNote = fmt.Sprintf("on %s: at least %d MiB, not placed (prompt cache %d MiB; %s)",
				hm.Machine, hm.TermMiB(), hm.PromptCacheMiB, notIncluded)
		} else {
			r.HostMemoryNote = fmt.Sprintf("on %s: up to %d MiB, not placed (context checkpoints %d MiB, prompt cache %d MiB)",
				hm.Machine, hm.TermMiB(), hm.CheckpointsMiB, hm.PromptCacheMiB)
		}
		if hm.FromLog {
			r.HostMemoryNote += "; " + hm.LoggedNote()
		}
	}
	if r.HostMemoryNote != "" && hm.SetNote != "" {
		r.HostMemoryNote += "; " + hm.SetNote
	}
}

// ServerHostMemory is the host-memory term summed over the instances loaded
// on the scheduler's machine (Status): the most their host processes may
// take there beyond what the load log showed. It is that machine only
// (Scope); the consoles that host a model are in Status.NodeHostMemory.
type ServerHostMemory struct {
	// Scope labels the sum: "on the scheduler's machine only".
	Scope          string
	Machine        string
	CheckpointsMiB int
	PromptCacheMiB int
	Instances      []string
	// SizeUnknown names the instances whose checkpoint size is not known
	// yet (HostMemory.SizeUnknown): they add their prompt cache only.
	SizeUnknown []string
	// Note reads the sum: "up to N MiB" when every size is known, else
	// "at least N MiB; K loaded model(s) with checkpoints of unknown size
	// not included" (a floor, never a ceiling); empty with nothing loaded.
	Note string `view:"omitempty"`
}

// serverScope labels ServerHostMemory.
const serverScope = "on the scheduler's machine only"

// NodeHostMemory is the host-memory term of one console that hosts loaded
// models (a host on a node), summed over them: checkpoints and prompt
// cache, the instances, those whose checkpoint size is unknown, and
// whether the console's reservation counts it (count_host_memory).
type NodeHostMemory struct {
	Node           core.NodeID
	Name           string
	CheckpointsMiB int
	PromptCacheMiB int
	Instances      []string
	SizeUnknown    []string `view:"omitempty"`
	Counted        bool
	// Note reads the figure as ServerHostMemory.Note does.
	Note string
}

// sumNote reads a host-memory sum: "up to N MiB" when every size is known,
// else "at least N MiB; K loaded model(s) with checkpoints of unknown size
// not included"; empty with nothing loaded.
func sumNote(total, instances, unknown int) string {
	switch {
	case instances == 0:
		return ""
	case unknown == 0:
		return fmt.Sprintf("up to %d MiB", total)
	case unknown == 1:
		return fmt.Sprintf("at least %d MiB; 1 loaded model with checkpoints of unknown size not included", total)
	}
	return fmt.Sprintf("at least %d MiB; %d loaded models with checkpoints of unknown size not included", total, unknown)
}

// nodeHostMemory is, per console that hosts a loaded model, its term; in
// node id order.
func (c *schedCore) nodeHostMemory() []NodeHostMemory {
	by := map[core.NodeID]*NodeHostMemory{}
	for _, in := range c.insts {
		if in.prof == nil || in.prof.Kind != core.KindLlama {
			continue
		}
		id := hostOf(in.prof, in.placement)
		if id == "" {
			continue
		}
		hm, ok := c.hostMemoryOf(in.prof, true)
		if !ok {
			continue
		}
		n := by[id]
		if n == nil {
			n = &NodeHostMemory{Node: id, Name: c.nodeName(id), Counted: hm.Counted}
			by[id] = n
		}
		n.CheckpointsMiB += hm.CheckpointsMiB
		n.PromptCacheMiB += hm.PromptCacheMiB
		n.Instances = append(n.Instances, in.prof.Name)
		if hm.SizeUnknown {
			n.SizeUnknown = append(n.SizeUnknown, in.prof.Name)
		}
	}
	out := []NodeHostMemory{} // [] in the status, never null: no console holds any
	for _, n := range by {
		sort.Strings(n.Instances)
		sort.Strings(n.SizeUnknown)
		n.Note = sumNote(n.CheckpointsMiB+n.PromptCacheMiB, len(n.Instances), len(n.SizeUnknown))
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// serverHostMemory sums the term over the loaded instances hosted on the
// scheduler's machine.
func (c *schedCore) serverHostMemory() ServerHostMemory {
	s := ServerHostMemory{Scope: serverScope, Machine: c.serverName()}
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
	s.Note = sumNote(s.CheckpointsMiB+s.PromptCacheMiB, len(s.Instances), len(s.SizeUnknown))
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
// For a model without an exact rule (SizeUnknown), the largest checkpoint
// logged is kept for the file version and context for the run (ckptSeen):
// nothing is compared, the log is the figure.
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
		largest := max(hm.LargestCheckpointMiB, hm.CheckpointMiB)
		if f.SizeUnknown || f.FromLog {
			if cs, ok := c.needs.(CheckpointSizer); ok {
				if ck, err := cs.CheckpointOf(in.prof); err == nil {
					k := ckptKey{in.prof.Model, ck.FileBytes, in.prof.Ctx}
					if largest > c.ckptSeen[k].MiB {
						if c.ckptSeen == nil {
							c.ckptSeen = map[ckptKey]ckptLogged{}
						}
						c.ckptSeen[k] = ckptLogged{MiB: largest, At: c.now}
					}
				}
			}
			continue // nothing to compare with: the log is the figure
		}
		if f.Family == string(gguf.FamilyWindowed) {
			// a bound: the largest checkpoint is compared, and only above it
			// (a conversation shorter than the window keeps less)
			if in.ckptChecked&2 == 0 && f.CellMiB > 0 && largest > f.CellMiB*(1+ckptDriftPct/100) {
				in.ckptChecked |= 2
				d := 100 * (largest - f.CellMiB) / f.CellMiB
				c.recordInst(in, history.TLearnedNeed, history.Warn,
					fmt.Sprintf("The largest context checkpoint of %s is %.2f MiB in the server log; the window bound says at most %.2f MiB (%.1f%% above). The host memory shown for it may be wrong.",
						in.prof.Name, largest, f.CellMiB, d),
					map[string]any{"logged_mib": largest, "formula_mib": f.CellMiB, "family": f.Family, "source": "largest context checkpoint"})
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
