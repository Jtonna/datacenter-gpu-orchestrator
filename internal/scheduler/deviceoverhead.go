package scheduler

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/needstore"
)

// The per-device overhead measured per shape, node and device count.
//
// device_overhead_mib is what one device of a placement holds beyond the
// estimator's share of the weights and KV cache and the compute buffer that
// grows with the context (NeedInfo.DeviceMiB): the fixed part of the compute
// buffer, the buffers the estimate leaves out, and the device runtime. It is
// added only to an ESTIMATED need (deviceCostOn); a measured need already
// holds it.
//
// A reading is taken on every worker node of a ready load (never its host
// node, whose own figure is the host overhead), once per cycle, one full
// free_average_window after ready:
//
//	D = (the node's free figure when the instance was created - its average
//	     free figure now) - the node's split fraction x the estimator's need
//	     - the compute buffer
//
// and never below the log's exact part L = (what the load log put on the
// node) - the share - the compute buffer. It is taken only from a COLD load
// on that node (coldLoad: the bytes sent there on the wire, measured, reach
// half of what the log puts on its devices): after a cached reload the
// worker already held the tensors, the node gave up almost nothing, and the
// reading would be the log's part alone. It is not taken from a command
// profile, a configured need, a model with a sliding window (whose estimate
// is far off), a load whose log does not account for every node, or while
// a node's latest and average free figures differ by more than
// deviceMovingMiB (tried again at the next step).
//
// Like every measured constant it is keyed by shape + machine + device
// count and never used across device counts. The figure in force follows
// the keep-the-higher rule (needstore Add). A figure restored from another
// llama build only raises the setting; one measured on a model file of
// another size is not used. device_overhead_setting_only plans with the
// setting again (the figures are kept). Forgetting the shape forgets them.
//
// What the shape key leaves: the figure is used where the shape's need on
// that count is an estimate (the placer's per-node cost, the expected
// shares). The load that gives a reading also stores the shape's own
// measured need, so a shape with a figure normally plans from its
// measurement; the row's Used then says so.

// Values of PlanMemoryNode.DeviceOverheadSource.
const (
	DeviceOverheadFromSetting = "setting"  // device_overhead_mib
	DeviceOverheadMeasured    = "measured" // measured on that node for that shape and device count
)

// deviceMovingMiB: a node whose latest and average free figures differ by
// more than this is moving; its reading waits.
const deviceMovingMiB = 128

// DeviceOverheadStore keeps the measured per-device overheads
// (needstore.Store). A NeedStore without it keeps them for the run only.
type DeviceOverheadStore interface {
	AllDevice() ([]needstore.DeviceOverhead, error)
	PutDevice(needstore.DeviceOverhead) error
	DeleteDeviceRow(shape, node string, devices int) error
}

// coldLoad: the bytes the host sent to a node on the wire (measured) reach
// at least half of what the load log puts on that node's devices (logMiB):
// the load really moved the model there. A cached or partly cached reload
// (the worker already held the tensors: kilobytes cross the wire) is not
// cold, and what the node gave up for it says nothing about the load.
func coldLoad(sentBytes int64, measured bool, logMiB float64) bool {
	return measured && logMiB > 0 && float64(sentBytes) >= logMiB*float64(1<<20)/2
}

func (c *schedCore) deviceOverheadAt(shape string, id core.NodeID, devices int) (*hostOH, bool) {
	e := c.devOHs[hostKey{shape, id, devices}]
	return e, e != nil
}

// putDeviceOverhead keeps e under its key and queues it for the store.
func (c *schedCore) putDeviceOverhead(e *hostOH) {
	if c.devOHs == nil {
		c.devOHs = map[hostKey]*hostOH{}
	}
	c.devOHs[hostKey{e.Shape, core.NodeID(e.Node), e.Devices}] = e
	c.changed = true
	if c.persist {
		row := e.HostOverhead
		row.Readings = slices.Clone(row.Readings)
		c.needOps = append(c.needOps, needOp{shape: e.Shape, dev: &row})
	}
}

// restoreDeviceOverheads reads the stored figures (restoreNeeds).
func (c *schedCore) restoreDeviceOverheads(st NeedStore) {
	ds, ok := st.(DeviceOverheadStore)
	if !ok {
		return
	}
	rows, err := ds.AllDevice()
	if err != nil {
		c.log.Warn("stored per-device overheads cannot be read; estimated needs plan with device_overhead_mib", "err", err)
		return
	}
	for _, r := range rows {
		if c.devOHs == nil {
			c.devOHs = map[hostKey]*hostOH{}
		}
		c.devOHs[hostKey{r.Shape, core.NodeID(r.Node), r.Devices}] = &hostOH{HostOverhead: r, restored: true}
	}
	if len(rows) > 0 {
		c.log.Info("measured per-device overheads restored", "rows", len(rows))
	}
}

// deviceOverheadOf is the per-device overhead of p on node id of a
// placement of devices devices: the figure, where it comes from and, for a
// measured one, the date of the reading in force.
func (c *schedCore) deviceOverheadOf(p *core.Profile, id core.NodeID, devices int) (int, string, time.Time) {
	setting := max(c.cfg.Defaults.DeviceOverheadMiB, 0)
	if c.cfg.Defaults.DeviceOverheadSettingOnly || id == "" || p == nil {
		return setting, DeviceOverheadFromSetting, time.Time{}
	}
	e, ok := c.deviceOverheadAt(shapeOf(p), id, devices)
	if !ok || c.otherModelFile(p, e) {
		return setting, DeviceOverheadFromSetting, time.Time{}
	}
	if e.restored && !c.buildsAgree(e.LlamaBuild) && e.MiB <= setting {
		return setting, DeviceOverheadFromSetting, time.Time{} // another llama build: it only raises the setting
	}
	return e.MiB, DeviceOverheadMeasured, e.MeasuredAt
}

// deviceCostOn is what one device on node id of a placement of k devices: 0 for a
// measured need, else the per-device overhead there plus the compute buffer.
func (c *schedCore) deviceCostOn(p *core.Profile, info NeedInfo, id core.NodeID, k int) int {
	if info.Need.Measured {
		return 0
	}
	mib, _, _ := c.deviceOverheadOf(p, id, k)
	return mib + max(info.DeviceMiB, 0)
}

// nodeDeviceMiB is the placer's per-node cost for p (placer.Request
// NodeDeviceMiB); nil, the setting on every node, when nothing is measured
// for p's shape or the need is measured.
func (c *schedCore) nodeDeviceMiB(p *core.Profile, info NeedInfo) func(core.NodeID, int) int {
	if info.Need.Measured || c.cfg.Defaults.DeviceOverheadSettingOnly || !c.hasDeviceOverheads(shapeOf(p)) {
		return nil
	}
	return func(id core.NodeID, n int) int {
		mib, _, _ := c.deviceOverheadOf(p, id, n)
		return mib + info.DeviceMiB
	}
}

// forgetDeviceOverheads drops shape's figures in memory (the store's rows go
// with the shape's delete); it reports whether there were any.
func (c *schedCore) forgetDeviceOverheads(shape string) bool {
	had := false
	for k := range c.devOHs {
		if k.shape == shape {
			delete(c.devOHs, k)
			had = true
		}
	}
	if had {
		c.changed = true
	}
	return had
}

func (c *schedCore) hasDeviceOverheads(shape string) bool {
	for k := range c.devOHs {
		if k.shape == shape {
			return true
		}
	}
	return false
}

// deviceWorkers are the nodes of in's placement a per-device reading is
// taken on: every node but the host node and the scheduler's own.
func deviceWorkers(in *inst) []int {
	var out []int
	for i, pn := range in.placement.Nodes {
		if pn.Local || pn.ID == in.placement.Host {
			continue
		}
		out = append(out, i)
	}
	return out
}

// deviceSettleAt is when in's per-device overheads are read: one full
// average window after ready; zero when there is nothing to read.
func (c *schedCore) deviceSettleAt(in *inst) time.Time {
	if in.prof.Kind != core.KindLlama || in.child == nil || in.readyAt.IsZero() || in.devMeasured || len(deviceWorkers(in)) == 0 {
		return time.Time{}
	}
	if !c.deviceEstimate(in.prof) {
		return time.Time{}
	}
	return in.readyAt.Add(max(c.cfg.Defaults.FreeAverageWindow, 0))
}

// deviceEstimate: p's need is the estimator's, so a reading can be taken
// (not a configured need, and not a model with a sliding window, whose
// estimate is far off).
func (c *schedCore) deviceEstimate(p *core.Profile) bool {
	base, ok := c.needs_[p.Name]
	return ok && !base.Need.Measured && base.window == 0 && base.Need.MiB > 0
}

// deviceReading is one worker node's reading.
type deviceReading struct {
	id      core.NodeID
	mib     int
	logPart int
}

// deviceReadings reads every worker node of in. wait is true when a figure
// is not known yet or a node is moving (tried again at the next step); a
// nil result with wait false means this cycle gives no reading.
func (c *schedCore) deviceReadings(in *inst) (out []deviceReading, wait bool) {
	if !c.deviceEstimate(in.prof) {
		return nil, false
	}
	base := c.needs_[in.prof.Name]
	k := len(in.placement.Nodes)
	fracs := []float64{1}
	if k > 1 {
		fracs = instance.SplitFractions(*in.prof, in.placement)
		if len(fracs) != k {
			return nil, false
		}
	}
	for i := range in.placement.Nodes {
		if _, ok := c.reportedOn(in, in.placement.Nodes[i].ID); !ok {
			return nil, false // the log does not account for every node
		}
	}
	if !c.fresh {
		return nil, true
	}
	for _, i := range deviceWorkers(in) {
		id := in.placement.Nodes[i].ID
		start, ok := in.baseFree[id]
		if !ok {
			return nil, false
		}
		n, ok := c.raw[id]
		if !ok || n.FreeUnknown || !n.Online || !n.FreeAvgKnown {
			return nil, true
		}
		if d := n.FreeMiB - n.FreeAvgMiB; d > deviceMovingMiB || d < -deviceMovingMiB {
			return nil, true
		}
		logMiB, _ := c.reportedOn(in, id)
		if !coldLoad(in.peerSent[in.placement.Nodes[i].Addr], in.sentBytes > 0, float64(logMiB)) {
			c.log.Debug("no per-device overhead reading: not a cold load on this node", "profile", in.prof.Name, "node", c.nodeName(id),
				"bytes_sent", in.peerSent[in.placement.Nodes[i].Addr], "log_mib", logMiB)
			continue
		}
		share := int(math.Ceil(float64(base.Need.MiB)*fracs[i] - 1e-6))
		comp := max(base.DeviceMiB, 0)
		logPart := logMiB - share - comp
		used := start - n.FreeAvgMiB - share - comp
		out = append(out, deviceReading{id: id, mib: max(used, logPart), logPart: logPart})
	}
	return out, false
}

// measureDeviceOverheads runs every step: it reads every settled load's
// per-device overheads once per cycle.
func (c *schedCore) measureDeviceOverheads() {
	c.dropCachedDevice()
	for _, in := range c.insts {
		if in.state != stReady && in.state != stDraining {
			continue
		}
		at := c.deviceSettleAt(in)
		if at.IsZero() || c.now.Before(at) {
			continue
		}
		rs, wait := c.deviceReadings(in)
		if wait {
			continue
		}
		in.devMeasured = true
		for _, r := range rs {
			if r.mib < 0 || r.mib > needstore.MaxDeviceOverheadMiB {
				c.log.Warn("per-device overhead reading out of range; the figure in force stays", "profile", in.prof.Name, "node", c.nodeName(r.id), "mib", r.mib)
				c.recordInst(in, history.TLearnedNeed, history.Warn,
					fmt.Sprintf("The per-device overhead of %s on %s read %d MiB, outside 0 to %d MiB: not used.",
						in.prof.Name, c.nodeName(r.id), r.mib, needstore.MaxDeviceOverheadMiB),
					map[string]any{"node": c.nodeName(r.id), "device_overhead_reading_mib": r.mib})
				continue
			}
			c.addDeviceReading(in, r)
		}
	}
}

// addDeviceReading applies one reading by the keep-the-higher rule, stores
// the row and records it.
func (c *schedCore) addDeviceReading(in *inst, r deviceReading) {
	k, shape := len(in.placement.Nodes), shapeOf(in.prof)
	e, ok := c.deviceOverheadAt(shape, r.id, k)
	if !ok {
		e = &hostOH{HostOverhead: needstore.DeviceOverhead{Shape: shape, Node: string(r.id), Devices: k}}
	}
	var bytes int64
	if base, ok := c.needs_[in.prof.Name]; ok {
		bytes = base.FileBytes
	}
	reset := ok && (e.LlamaBuild != c.llamaBuild || (e.ModelBytes > 0 && bytes > 0 && e.ModelBytes != bytes))
	was := e.MiB
	outcome := e.Add(needstore.HostReading{MiB: r.mib, At: c.now, Cause: needstore.CauseLoad}, reset)
	e.LogHostMiB, e.Release, e.LlamaBuild, e.ModelBytes, e.Profile, e.restored = r.logPart, c.buildRelease, c.llamaBuild, bytes, in.prof.Name, false
	c.putDeviceOverhead(e)
	c.log.Info("measured per-device overhead", "profile", in.prof.Name, "node", c.nodeName(r.id), "devices", k, "reading_mib", r.mib,
		"in_force_mib", e.MiB, "rule", outcome, "log_part_mib", r.logPart)
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
		fmt.Sprintf("Measured the per-device overhead of %s on %s at %d MiB (%s); %s. device_overhead_mib is %d.",
			in.prof.Name, c.nodeName(r.id), r.mib, plural(k, "device"), why, c.cfg.Defaults.DeviceOverheadMiB),
		map[string]any{"node": c.nodeName(r.id), "device_overhead_mib": r.mib, "in_force_mib": e.MiB, "rule": outcome, "devices": k,
			"log_part_mib": r.logPart, "llama_build": c.llamaBuild, "setting_mib": c.cfg.Defaults.DeviceOverheadMiB})
}

// usesDeviceRow: p (of the row's shape) can plan with the row of k: a
// profile with forced nodes when k's node is one of them and it forces k's
// device count; one without forced nodes on any row of its shape.
func (c *schedCore) usesDeviceRow(p *core.Profile, k hostKey) bool {
	if len(p.ForceNodes) == 0 {
		return true
	}
	return len(p.ForceNodes) == k.devices && slices.ContainsFunc(p.ForceNodes, func(n string) bool {
		return n == string(k.node) || n == c.nodeName(k.node)
	})
}

// deviceOverheadRows lists the measured per-device overheads for GET
// /api/needs. Used says whether a plan reads the row now: not with
// device_overhead_setting_only, not for a lower figure of another llama
// build, and not while the shape plans from its own measured need (it then
// holds the device's overhead already).
func (c *schedCore) deviceOverheadRows() []HostOverheadRow {
	users := map[string][]string{}
	for name, p := range c.cfg.Profiles {
		users[shapeOf(p)] = append(users[shapeOf(p)], name)
	}
	setting := max(c.cfg.Defaults.DeviceOverheadMiB, 0)
	out := []HostOverheadRow{}
	for k, e := range c.devOHs {
		profiles := []string{}
		for _, name := range users[k.shape] {
			if c.usesDeviceRow(c.cfg.Profiles[name], k) {
				profiles = append(profiles, name)
			}
		}
		slices.Sort(profiles)
		r := HostOverheadRow{Shape: k.shape, Node: k.node, NodeName: c.nodeName(k.node), Devices: k.devices, MiB: e.MiB, LogHostMiB: e.LogHostMiB,
			MeasuredBy: e.Profile, MeasuredAt: e.MeasuredAt, Release: e.Release, LlamaBuild: e.LlamaBuild, Cause: e.Cause, Restored: e.restored,
			Used: !c.cfg.Defaults.DeviceOverheadSettingOnly && (!e.restored || c.buildsAgree(e.LlamaBuild) || e.MiB > setting) &&
				len(c.learned[k.shape]) == 0,
			Readings: slices.Clone(e.Readings), Profiles: profiles}
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

// dropCachedDevice drops, once at start, every restored row whose reading
// came from a cached reload (the release before took readings from any
// load). For each row it finds in the event history the learnedNeed event
// that recorded the reading (the row's profile shape, node, device count and
// figure, at the reading's date), then the loadFinished event of that load:
// the newest one before it for the same profile and node list. The row is
// dropped when that load's measured bytes to the node are below the cold
// floor (coldLoad), with a history line. A row whose load cannot be found
// is kept.
func (c *schedCore) dropCachedDevice() {
	if c.devChecked || c.devEventsFn == nil || len(c.raw) == 0 {
		return
	}
	c.devChecked = true
	restored := false
	for _, e := range c.devOHs {
		restored = restored || e.restored
	}
	if !restored {
		return
	}
	evs := c.devEventsFn()
	slices.SortStableFunc(evs, func(a, b history.Event) int { return int(a.Ts - b.Ts) })
	for k, e := range c.devOHs {
		if !e.restored || len(e.Readings) == 0 {
			continue
		}
		at := e.Readings[0].At.UnixMilli()
		name := c.nodeName(k.node)
		read := -1
		for i, ev := range evs {
			if ev.Type != history.TLearnedNeed || len(ev.Nodes) != k.devices || ev.Ts < at-2000 || ev.Ts > at+2000 {
				continue
			}
			p := c.cfg.Profiles[ev.Profile]
			if p == nil || shapeOf(p) != k.shape {
				continue
			}
			if n, _ := ev.Data["node"].(string); n != name && n != string(k.node) {
				continue
			}
			if v, ok := numberOf(ev.Data["device_overhead_mib"]); !ok || v != e.Readings[0].MiB {
				continue
			}
			read = i
		}
		if read < 0 {
			continue
		}
		var load *history.Event
		for i := read - 1; i >= 0; i-- {
			ev := evs[i]
			if ev.Type == history.TLoadFinished && ev.Profile == evs[read].Profile && slices.Equal(ev.Nodes, evs[read].Nodes) {
				load = &evs[i]
				break
			}
		}
		if load == nil {
			continue
		}
		measured, _ := load.Data["bytes_measured"].(bool)
		var sent, logMiB float64
		found := false
		detail, _ := load.Data["nodes_detail"].([]any)
		for _, d := range detail {
			m, _ := d.(map[string]any)
			if n, _ := m["node"].(string); n == name || n == string(k.node) {
				sent, _ = m["bytes"].(float64)
				logMiB, _ = m["memory_mib"].(float64)
				found = true
			}
		}
		if !found || logMiB <= 0 || coldLoad(int64(sent), measured, logMiB) {
			continue
		}
		delete(c.devOHs, k)
		c.changed = true
		if c.persist {
			row := e.HostOverhead
			c.needOps = append(c.needOps, needOp{shape: k.shape, dev: &row, drop: true})
		}
		c.log.Info("per-device overhead from a cached load dropped", "node", name, "devices", k.devices, "mib", e.MiB, "bytes_sent", int64(sent))
		c.record(history.TLearnedNeed, history.Info, evs[read].Profile,
			fmt.Sprintf("Dropped the per-device overhead of %s on %s (%s, %d MiB): the load it was read from was a cached reload (%d bytes sent to %s, under half of the %.0f MiB the log put there), so the reading measured nothing of the load.",
				evs[read].Profile, name, plural(k.devices, "device"), e.MiB, int64(sent), name, logMiB),
			evs[read].Nodes, "", map[string]any{"node": name, "devices": k.devices, "dropped_device_overhead_mib": e.MiB, "bytes_sent": int64(sent), "log_mib": logMiB})
	}
}
