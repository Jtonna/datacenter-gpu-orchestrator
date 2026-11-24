package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

// The host overhead measured per node and shape (phase 6, host: node).
//
// What a host process on a node takes there besides its device share is
// host_overhead_mib until it is measured. The first hosted load of a shape
// on a node plans with the setting; once that load is ready and one full
// free_average_window has passed, the node's measured use (its free figure
// when the instance was created less its free figure now, the average when
// known) less what the load log put on the node's own devices is that
// node's host overhead for the shape. Later plans of that shape hosted on
// that node use it, with the margin rules as they are; every other node
// and shape keeps the setting. A normal cycle replaces the figure with its
// own reading; a cycle that ends abnormally (the watchdog, an OOM kill, a
// failure) can only raise it, to the most the host was seen to take in the
// cycle. A figure measured on another llama build, restored at start, may
// only raise the setting; one measured on a model file of another size is
// not used. Forgetting the shape forgets its host overheads.
// host_overhead_setting_only plans every host with the setting again (the
// figures are kept).
//
// The agent does not report a process's memory, so the host process is not
// told apart by process but by subtraction, on the host node only. With
// host_no_mmap off, weights mapped from the share are page cache, which
// MemAvailable counts as available: the reading leaves them out, as the
// agent's watchdog does.

// Values of PlanReport.HostOverheadSource and PlanMemoryNode.HostOverheadSource.
const (
	HostOverheadFromSetting = "setting"  // host_overhead_mib
	HostOverheadMeasured    = "measured" // measured on that node for that shape
)

// HostOverheadStore keeps the measured host overheads (needstore.Store).
// A NeedStore without it keeps them for the run only.
type HostOverheadStore interface {
	AllHost() ([]needstore.HostOverhead, error)
	PutHost(needstore.HostOverhead) error
}

// hostOH is one node's measured host overhead for one shape.
type hostOH struct {
	mib, logHost int
	at           time.Time
	release      string
	build        string
	modelBytes   int64
	profile      string
	cause        string
	restored     bool
}

func (c *schedCore) hostOverheadAt(shape string, id core.NodeID) (*hostOH, bool) {
	e := c.hostOHs[shape][id]
	return e, e != nil
}

// putHostOverhead keeps e as shape's figure on node id and queues it for the
// store.
func (c *schedCore) putHostOverhead(shape string, id core.NodeID, e *hostOH) {
	if c.hostOHs == nil {
		c.hostOHs = map[string]map[core.NodeID]*hostOH{}
	}
	if c.hostOHs[shape] == nil {
		c.hostOHs[shape] = map[core.NodeID]*hostOH{}
	}
	c.hostOHs[shape][id] = e
	c.changed = true
	if c.persist {
		c.needOps = append(c.needOps, needOp{shape: shape, host: &needstore.HostOverhead{Shape: shape, Node: string(id), MiB: e.mib,
			LogHostMiB: e.logHost, MeasuredAt: e.at, Release: e.release, LlamaBuild: e.build, ModelBytes: e.modelBytes,
			Profile: e.profile, Cause: e.cause}})
	}
}

// restoreHostOverheads reads the stored figures (restoreNeeds).
func (c *schedCore) restoreHostOverheads(st NeedStore) {
	hs, ok := st.(HostOverheadStore)
	if !ok {
		return
	}
	rows, err := hs.AllHost()
	if err != nil {
		c.log.Warn("stored host overheads cannot be read; hosts on a node plan with host_overhead_mib", "err", err)
		return
	}
	for _, r := range rows {
		if c.hostOHs == nil {
			c.hostOHs = map[string]map[core.NodeID]*hostOH{}
		}
		if c.hostOHs[r.Shape] == nil {
			c.hostOHs[r.Shape] = map[core.NodeID]*hostOH{}
		}
		c.hostOHs[r.Shape][core.NodeID(r.Node)] = &hostOH{mib: r.MiB, logHost: r.LogHostMiB, at: r.MeasuredAt, release: r.Release,
			build: r.LlamaBuild, modelBytes: r.ModelBytes, profile: r.Profile, cause: r.Cause, restored: true}
	}
	if len(rows) > 0 {
		c.log.Info("measured host overheads restored", "rows", len(rows))
	}
}

// hostOverheadOf is what hosting p takes on node id: the figure, where it
// comes from (HostOverheadFromSetting or HostOverheadMeasured) and, for a
// measured one, when it was measured.
func (c *schedCore) hostOverheadOf(p *core.Profile, id core.NodeID) (int, string, time.Time) {
	setting := max(c.cfg.Defaults.HostOverheadMiB, 0)
	if c.cfg.Defaults.HostOverheadSettingOnly || id == "" {
		return setting, HostOverheadFromSetting, time.Time{}
	}
	e, ok := c.hostOverheadAt(shapeOf(p), id)
	if !ok {
		return setting, HostOverheadFromSetting, time.Time{}
	}
	if base, ok := c.needs_[p.Name]; ok && e.modelBytes > 0 && base.FileBytes > 0 && e.modelBytes != base.FileBytes {
		return setting, HostOverheadFromSetting, time.Time{} // measured on another model file
	}
	if e.restored && !c.buildsAgree(e.build) && e.mib <= setting {
		return setting, HostOverheadFromSetting, time.Time{} // another llama build: it only raises the setting
	}
	return e.mib, HostOverheadMeasured, e.at
}

// forgetHostOverheads drops shape's figures in memory (the store's rows go
// with the shape's delete); it reports whether there were any.
func (c *schedCore) forgetHostOverheads(shape string) bool {
	had := len(c.hostOHs[shape]) > 0
	delete(c.hostOHs, shape)
	if had {
		c.changed = true
	}
	return had
}

// hostReading is what the host took on its node now: the node's measured
// use less what the load log put on the node's own devices. avg uses the
// node's average free figure when known. ok is false when a figure is not
// known.
func (c *schedCore) hostReading(in *inst, avg bool) (mib, logHost int, ok bool) {
	id := in.placement.Host
	base, ok := in.baseFree[id]
	if !ok || !c.fresh {
		return 0, 0, false
	}
	n, ok := c.raw[id]
	if !ok || n.FreeUnknown || !n.Online {
		return 0, 0, false
	}
	free := n.FreeMiB
	if avg && n.FreeAvgKnown {
		free = n.FreeAvgMiB
	}
	dev, ok := c.reportedOn(in, id)
	if !ok {
		return 0, 0, false
	}
	return base - free - dev, int(in.child.Load().HostMiB() + 0.999), true
}

// hostSettleAt is when in's host overhead is read: one full average window
// after ready; zero when there is nothing to read.
func (c *schedCore) hostSettleAt(in *inst) time.Time {
	if in.placement.Host == "" || in.prof.Kind != core.KindLlama || in.child == nil || in.readyAt.IsZero() || in.hostMeasured {
		return time.Time{}
	}
	return in.readyAt.Add(max(c.cfg.Defaults.FreeAverageWindow, 0))
}

// measureHostOverheads runs every step: for every ready host on a node, it
// tracks the most the host took in the cycle, and reads its figure once the
// cycle has settled.
func (c *schedCore) measureHostOverheads() {
	for _, in := range c.insts {
		if in.placement.Host == "" || in.child == nil || in.readyAt.IsZero() || (in.state != stReady && in.state != stDraining) {
			continue
		}
		if mib, _, ok := c.hostReading(in, false); ok {
			in.hostHigh = max(in.hostHigh, mib)
		}
		at := c.hostSettleAt(in)
		if at.IsZero() || c.now.Before(at) {
			continue
		}
		mib, logHost, ok := c.hostReading(in, true)
		if !ok {
			continue // tried again at the next step
		}
		in.hostMeasured = true
		id := in.placement.Host
		if mib < 0 || mib > needstore.MaxHostOverheadMiB {
			c.log.Warn("host overhead reading out of range; host_overhead_mib stays", "profile", in.prof.Name, "node", c.nodeName(id), "mib", mib)
			c.recordInst(in, history.TLearnedNeed, history.Warn,
				fmt.Sprintf("The host overhead of %s on %s read %d MiB, outside 0 to %d MiB: not used; plans keep %d MiB.",
					in.prof.Name, c.nodeName(id), mib, needstore.MaxHostOverheadMiB, max(c.cfg.Defaults.HostOverheadMiB, 0)),
				map[string]any{"node": c.nodeName(id), "host_overhead_reading_mib": mib})
			continue
		}
		c.storeHostOverhead(in, mib, logHost, needstore.CauseLoad)
	}
}

func (c *schedCore) storeHostOverhead(in *inst, mib, logHost int, cause string) {
	id := in.placement.Host
	e := &hostOH{mib: mib, logHost: logHost, at: c.now, release: c.buildRelease, build: c.llamaBuild, profile: in.prof.Name, cause: cause}
	if base, ok := c.needs_[in.prof.Name]; ok {
		e.modelBytes = base.FileBytes
	}
	c.putHostOverhead(shapeOf(in.prof), id, e)
	c.log.Info("measured host overhead", "profile", in.prof.Name, "node", c.nodeName(id), "mib", mib, "log_host_mib", logHost,
		"setting_mib", c.cfg.Defaults.HostOverheadMiB, "cause", cause)
	c.recordInst(in, history.TLearnedNeed, history.Info,
		fmt.Sprintf("Measured the host overhead of %s on %s at %d MiB (host_overhead_mib is %d); plans of this shape hosted there use it.",
			in.prof.Name, c.nodeName(id), mib, c.cfg.Defaults.HostOverheadMiB),
		map[string]any{"node": c.nodeName(id), "host_overhead_mib": mib, "log_host_mib": logHost,
			"setting_mib": c.cfg.Defaults.HostOverheadMiB, "cause": cause})
}

// endHostCycle: a cycle that ended abnormally raises the stored figure to
// the most the host took in it (removeInst).
func (c *schedCore) endHostCycle(in *inst) {
	if in.placement.Host == "" || in.readyAt.IsZero() || normalStop(in) || in.hostHigh <= 0 {
		return
	}
	e, ok := c.hostOverheadAt(shapeOf(in.prof), in.placement.Host)
	if ok && e.mib >= in.hostHigh {
		return
	}
	cause := in.stopReason
	if cause == "" {
		cause = "failed"
	}
	logHost := 0
	if in.child != nil {
		logHost = int(in.child.Load().HostMiB() + 0.999)
	}
	c.storeHostOverhead(in, min(in.hostHigh, needstore.MaxHostOverheadMiB), logHost, cause)
}

// HostOverheadRow is one measured host overhead in GET /api/needs. Used
// says whether plans use it now (not with host_overhead_setting_only, not
// when it was restored from another llama build and is not above the
// setting); Profiles are the profiles of its shape.
type HostOverheadRow struct {
	Shape      string
	Node       core.NodeID
	NodeName   string
	MiB        int
	LogHostMiB int
	MeasuredBy string
	MeasuredAt time.Time
	Release    string
	LlamaBuild string
	Cause      string
	Restored   bool
	Used       bool
	Profiles   []string
}

// hostOverheadRows lists the measured host overheads by shape and node.
func (c *schedCore) hostOverheadRows() []HostOverheadRow {
	users := map[string][]string{}
	for name, p := range c.cfg.Profiles {
		users[shapeOf(p)] = append(users[shapeOf(p)], name)
	}
	setting := max(c.cfg.Defaults.HostOverheadMiB, 0)
	out := []HostOverheadRow{}
	for shape, byNode := range c.hostOHs {
		for id, e := range byNode {
			r := HostOverheadRow{Shape: shape, Node: id, NodeName: c.nodeName(id), MiB: e.mib, LogHostMiB: e.logHost, MeasuredBy: e.profile,
				MeasuredAt: e.at, Release: e.release, LlamaBuild: e.build, Cause: e.cause, Restored: e.restored,
				Used:     !c.cfg.Defaults.HostOverheadSettingOnly && (!e.restored || c.buildsAgree(e.build) || e.mib > setting),
				Profiles: slices.Sorted(slices.Values(users[shape]))}
			if r.Profiles == nil {
				r.Profiles = []string{}
			}
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b HostOverheadRow) int {
		if a.Shape != b.Shape {
			return strings.Compare(a.Shape, b.Shape)
		}
		return strings.Compare(string(a.Node), string(b.Node))
	})
	return out
}
