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

// The host overhead measured per node, shape and device count (phase 6,
// host: node).
//
// What a host process on a node takes there besides its device share is
// host_overhead_mib until it is measured. The first hosted load of a shape
// on a node, on a given device count, plans with the setting; once that load
// is ready and one full free_average_window has passed, the node's measured
// use (its free figure when the instance was created less its free figure
// now, the average when known) less what the load log put on the node's
// own devices is a reading for that shape, node and device count. A
// measurement is never used across device counts: a host holding half the
// model does other work than one holding all of it.
//
// The figure in force follows the keep-the-higher rule (needstore Add): a
// reading replaces it only when higher; it comes down only when the llama
// build or the model file changed, or when three consecutive readings are
// all lower (then to the highest of the three). A cycle that ends
// abnormally gives its highest reading of the cycle, by the same rule. A
// figure restored from another llama build may only raise the setting; one
// measured on a model file of another size is not used. Forgetting the
// shape forgets its host overheads. host_overhead_setting_only plans every
// host with the setting again (the figures are kept).
//
// A row copied from the release before device counts (needstore Pending)
// kept only its latest reading. Once, when its node is known, its readings
// are read back from the event history (the learnedNeed events that carry
// host_overhead_mib for that shape, node and device count, the device count
// being the event's node list) and the figure in force becomes the highest
// of them; a history line says why when that changes the figure.
//
// The agent does not report a process's memory, so the host process is not
// told apart by process but by subtraction, on the host node only. With
// host_no_mmap off, weights mapped from the share are page cache, which
// MemAvailable counts as available: the reading leaves them out, as the
// agent's watchdog does.

// Values of PlanReport.HostOverheadSource and PlanMemoryNode.HostOverheadSource.
const (
	HostOverheadFromSetting = "setting"  // host_overhead_mib
	HostOverheadMeasured    = "measured" // measured on that node for that shape and device count
)

// HostOverheadStore keeps the measured host overheads (needstore.Store).
// A NeedStore without it keeps them for the run only.
type HostOverheadStore interface {
	AllHost() ([]needstore.HostOverhead, error)
	PutHost(needstore.HostOverhead) error
}

// hostKey is a measured host overhead's key.
type hostKey struct {
	shape   string
	node    core.NodeID
	devices int
}

// hostOH is one measured host overhead: the stored row, and whether it was
// restored at start (its llama build then decides whether it is trusted).
type hostOH struct {
	needstore.HostOverhead
	restored bool
}

func (c *schedCore) hostOverheadAt(shape string, id core.NodeID, devices int) (*hostOH, bool) {
	e := c.hostOHs[hostKey{shape, id, devices}]
	return e, e != nil
}

// putHostOverhead keeps e under its key and queues it for the store.
func (c *schedCore) putHostOverhead(e *hostOH) {
	if c.hostOHs == nil {
		c.hostOHs = map[hostKey]*hostOH{}
	}
	c.hostOHs[hostKey{e.Shape, core.NodeID(e.Node), e.Devices}] = e
	c.changed = true
	if c.persist {
		row := e.HostOverhead
		row.Readings = slices.Clone(row.Readings)
		c.needOps = append(c.needOps, needOp{shape: e.Shape, host: &row})
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
			c.hostOHs = map[hostKey]*hostOH{}
		}
		c.hostOHs[hostKey{r.Shape, core.NodeID(r.Node), r.Devices}] = &hostOH{HostOverhead: r, restored: true}
	}
	if len(rows) > 0 {
		c.log.Info("measured host overheads restored", "rows", len(rows))
	}
}

// hostOverheadOf is what hosting p on node id of a placement of devices
// devices takes: the figure, where it comes from (HostOverheadFromSetting or
// HostOverheadMeasured) and, for a measured one, the date of the reading in
// force.
func (c *schedCore) hostOverheadOf(p *core.Profile, id core.NodeID, devices int) (int, string, time.Time) {
	setting := max(c.cfg.Defaults.HostOverheadMiB, 0)
	if c.cfg.Defaults.HostOverheadSettingOnly || id == "" {
		return setting, HostOverheadFromSetting, time.Time{}
	}
	e, ok := c.hostOverheadAt(shapeOf(p), id, devices)
	if !ok {
		return setting, HostOverheadFromSetting, time.Time{}
	}
	if c.otherModelFile(p, e) {
		return setting, HostOverheadFromSetting, time.Time{}
	}
	if e.restored && !c.buildsAgree(e.LlamaBuild) && e.MiB <= setting {
		return setting, HostOverheadFromSetting, time.Time{} // another llama build: it only raises the setting
	}
	return e.MiB, HostOverheadMeasured, e.MeasuredAt
}

// otherModelFile: e was measured on a model file of another size than p's.
func (c *schedCore) otherModelFile(p *core.Profile, e *hostOH) bool {
	base, ok := c.needs_[p.Name]
	return ok && e.ModelBytes > 0 && base.FileBytes > 0 && e.ModelBytes != base.FileBytes
}

// forgetHostOverheads drops shape's figures in memory (the store's rows go
// with the shape's delete); it reports whether there were any.
func (c *schedCore) forgetHostOverheads(shape string) bool {
	had := false
	for k := range c.hostOHs {
		if k.shape == shape {
			delete(c.hostOHs, k)
			had = true
		}
	}
	if had {
		c.changed = true
	}
	return had
}

func (c *schedCore) hasHostOverheads(shape string) bool {
	for k := range c.hostOHs {
		if k.shape == shape {
			return true
		}
	}
	return false
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
// cycle has settled. It also corrects, once, the rows copied from the
// release before device counts (correctPending).
func (c *schedCore) measureHostOverheads() {
	c.correctPending()
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
			c.log.Warn("host overhead reading out of range; the figure in force stays", "profile", in.prof.Name, "node", c.nodeName(id), "mib", mib)
			c.recordInst(in, history.TLearnedNeed, history.Warn,
				fmt.Sprintf("The host overhead of %s on %s read %d MiB, outside 0 to %d MiB: not used.",
					in.prof.Name, c.nodeName(id), mib, needstore.MaxHostOverheadMiB),
				map[string]any{"node": c.nodeName(id), "host_overhead_reading_mib": mib})
			continue
		}
		c.addHostReading(in, mib, logHost, needstore.CauseLoad)
	}
}

// addHostReading applies one reading of in's host overhead by the
// keep-the-higher rule, stores the row and records it.
func (c *schedCore) addHostReading(in *inst, mib, logHost int, cause string) {
	id, k, shape := in.placement.Host, len(in.placement.Nodes), shapeOf(in.prof)
	e, ok := c.hostOverheadAt(shape, id, k)
	if !ok {
		e = &hostOH{HostOverhead: needstore.HostOverhead{Shape: shape, Node: string(id), Devices: k}}
	}
	var bytes int64
	if base, ok := c.needs_[in.prof.Name]; ok {
		bytes = base.FileBytes
	}
	reset := ok && (e.LlamaBuild != c.llamaBuild || (e.ModelBytes > 0 && bytes > 0 && e.ModelBytes != bytes))
	was := e.MiB
	outcome := e.Add(needstore.HostReading{MiB: mib, At: c.now, Cause: cause}, reset)
	e.LogHostMiB, e.Release, e.LlamaBuild, e.ModelBytes, e.Profile, e.restored = logHost, c.buildRelease, c.llamaBuild, bytes, in.prof.Name, false
	c.putHostOverhead(e)
	c.log.Info("measured host overhead", "profile", in.prof.Name, "node", c.nodeName(id), "devices", k, "reading_mib", mib,
		"in_force_mib", e.MiB, "rule", outcome, "log_host_mib", logHost, "cause", cause)
	var why string
	switch outcome {
	case needstore.HostFirst, needstore.HostRaised:
		why = "the figure in force"
	case needstore.HostKept:
		why = fmt.Sprintf("the figure in force stays %d MiB (keep the higher)", was)
	case needstore.HostLowered:
		why = fmt.Sprintf("three consecutive lower readings: the figure in force is now %d MiB, the highest of them", e.MiB)
	case needstore.HostReset:
		why = "the llama build or the model file changed: the figure in force"
	}
	c.recordInst(in, history.TLearnedNeed, history.Info,
		fmt.Sprintf("Measured the host overhead of %s on %s at %d MiB (%s); %s. host_overhead_mib is %d.",
			in.prof.Name, c.nodeName(id), mib, plural(k, "device"), why, c.cfg.Defaults.HostOverheadMiB),
		map[string]any{"node": c.nodeName(id), "host_overhead_mib": mib, "in_force_mib": e.MiB, "rule": outcome, "devices": k,
			"log_host_mib": logHost, "llama_build": c.llamaBuild, "setting_mib": c.cfg.Defaults.HostOverheadMiB, "cause": cause})
}

// endHostCycle: a cycle that ended abnormally gives the most the host took
// in it as a reading (removeInst).
func (c *schedCore) endHostCycle(in *inst) {
	if in.placement.Host == "" || in.readyAt.IsZero() || normalStop(in) || in.hostHigh <= 0 {
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
	c.addHostReading(in, min(in.hostHigh, needstore.MaxHostOverheadMiB), logHost, cause)
}

// HostReadingEvents returns the history's learnedNeed events (Options); the
// rows copied from the release before device counts read their readings
// from it once (correctPending). nil: no history.
type HostReadingEvents func() []history.Event

// correctPending gives every row copied from the release before device
// counts its readings from the event history, once its node is known: the
// readings of host_overhead_mib for the row's shape (from the event's
// profile), node (the first node of the event, the host) and device count
// (the event's node count), on the row's llama build when the event names
// one. The figure in force becomes the highest of them and of the row's
// own; a history line says why when that changes it.
func (c *schedCore) correctPending() {
	for _, e := range c.hostOHs {
		if !e.Pending() {
			continue
		}
		id := core.NodeID(e.Node)
		if _, known := c.raw[id]; !known {
			continue // its name is needed to match the events
		}
		if c.hostEvents == nil && c.hostEventsFn != nil {
			c.hostEvents = c.hostEventsFn()
			if c.hostEvents == nil {
				c.hostEvents = []history.Event{}
			}
		}
		name := c.nodeName(id)
		var rs []needstore.HostReading
		for _, ev := range c.hostEvents {
			mib, ok := numberOf(ev.Data["host_overhead_mib"])
			p := c.cfg.Profiles[ev.Profile]
			if !ok || p == nil || ev.Type != history.TLearnedNeed || len(ev.Nodes) != e.Devices || len(ev.Nodes) == 0 ||
				(ev.Nodes[0] != name && ev.Nodes[0] != e.Node) || shapeOf(p) != e.Shape {
				continue
			}
			if b, _ := ev.Data["llama_build"].(string); b != "" && e.LlamaBuild != "" && b != e.LlamaBuild {
				continue
			}
			cause, _ := ev.Data["cause"].(string)
			rs = append(rs, needstore.HostReading{MiB: mib, At: time.UnixMilli(ev.Ts).UTC(), Cause: cause})
		}
		slices.SortFunc(rs, func(a, b needstore.HostReading) int { return b.At.Compare(a.At) }) // newest first
		was, top := e.MiB, needstore.HostReading{MiB: e.MiB, At: e.MeasuredAt, Cause: e.Cause}
		for _, r := range rs {
			if r.MiB > top.MiB {
				top = r
			}
		}
		if len(rs) == 0 {
			rs = []needstore.HostReading{top}
		}
		if len(rs) > needstore.MaxHostReadings {
			rs = rs[:needstore.MaxHostReadings]
		}
		e.Readings = rs
		e.MiB, e.MeasuredAt, e.Cause = top.MiB, top.At, top.Cause
		c.putHostOverhead(e)
		if top.MiB != was {
			c.log.Info("host overhead corrected from the history: kept the higher of the readings", "node", name, "devices", e.Devices,
				"kept_mib", top.MiB, "was_mib", was)
			c.record(history.TLearnedNeed, history.Info, e.Profile,
				fmt.Sprintf("Kept the higher of the readings of the host overhead on %s (%s, the shape %s measured): %d MiB, not %d MiB. The release before kept only the latest reading.",
					name, plural(e.Devices, "device"), e.Profile, top.MiB, was),
				[]string{name}, "", map[string]any{"node": name, "devices": e.Devices, "kept_mib": top.MiB, "was_mib": was,
					"readings": len(rs)})
		}
	}
}

func numberOf(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	}
	return 0, false
}

// HostOverheadRow is one measured host overhead in GET /api/needs. Used
// says whether plans use it now (not with host_overhead_setting_only, not
// when it was restored from another llama build and is not above the
// setting); Profiles are the profiles of its shape.
type HostOverheadRow struct {
	Shape      string
	Node       core.NodeID
	NodeName   string
	Devices    int
	MiB        int
	LogHostMiB int
	MeasuredBy string
	MeasuredAt time.Time
	Release    string
	LlamaBuild string
	Cause      string
	Restored   bool
	Used       bool
	Readings   []needstore.HostReading
	Profiles   []string
}

// hostOverheadRows lists the measured host overheads by shape, node and
// device count.
func (c *schedCore) hostOverheadRows() []HostOverheadRow {
	users := map[string][]string{}
	for name, p := range c.cfg.Profiles {
		users[shapeOf(p)] = append(users[shapeOf(p)], name)
	}
	usersOf := func(k hostKey) []string {
		out := []string{}
		for _, name := range users[k.shape] {
			if c.usesHostRow(c.cfg.Profiles[name], k) {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}
	setting := max(c.cfg.Defaults.HostOverheadMiB, 0)
	out := []HostOverheadRow{}
	for k, e := range c.hostOHs {
		r := HostOverheadRow{Shape: k.shape, Node: k.node, NodeName: c.nodeName(k.node), Devices: k.devices, MiB: e.MiB, LogHostMiB: e.LogHostMiB,
			MeasuredBy: e.Profile, MeasuredAt: e.MeasuredAt, Release: e.Release, LlamaBuild: e.LlamaBuild, Cause: e.Cause, Restored: e.restored,
			Used:     !c.cfg.Defaults.HostOverheadSettingOnly && (!e.restored || c.buildsAgree(e.LlamaBuild) || e.MiB > setting),
			Readings: slices.Clone(e.Readings), Profiles: usersOf(k)}
		if r.Readings == nil {
			r.Readings = []needstore.HostReading{}
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b HostOverheadRow) int {
		if a.Shape != b.Shape {
			return strings.Compare(a.Shape, b.Shape)
		}
		if a.Node != b.Node {
			return strings.Compare(string(a.Node), string(b.Node))
		}
		return a.Devices - b.Devices
	})
	return out
}

// usesHostRow: p (of the row's shape) can plan with the row of k. A profile
// with forced nodes is hosted on its first one, on as many devices as it
// forces; one without forced nodes may be placed on any node and count.
func (c *schedCore) usesHostRow(p *core.Profile, k hostKey) bool {
	if len(p.ForceNodes) == 0 {
		return true
	}
	first := p.ForceNodes[0]
	return len(p.ForceNodes) == k.devices && (first == string(k.node) || first == c.nodeName(k.node))
}
