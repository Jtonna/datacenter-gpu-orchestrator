package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/fleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/startups"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// The node view (fleet.View) built from the agents. Remote nodes are the
// registered agents; the local node comes from local_probe_cmd, as in fleet
// membership. There is no feed: the view is always fresh (Age 0) and a
// silent agent is shown offline once it is lost.

var _ fleet.View = (*Pool)(nil)

// SourceAgent is the tier match "source" of an agent node.
//
// An agent node meets the configured tiers (priority, headroom, link speed,
// settle) on what its agent reports (agent contract 11, CR-5): platform (the
// one Potluck gave it at registration) and device (the GPU models of its
// registration and the device names of its heartbeat). The older matchers
// still apply to it when a tier sets them: hostname = the agent's name, role
// empty, source "agent". A console gets a new hostname at each flash, so its
// tier should set platform and device only. A node that matches no tier is
// listed with the reason no_tier_match and takes no load.
const SourceAgent = "agent"

var _ scheduler.NodeGate = (*Pool)(nil)

// Unplaced is scheduler.NodeGate: why a listed node takes no new load.
func (p *Pool) Unplaced() map[core.NodeID]string {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	out := make(map[core.NodeID]string, len(p.unplaced))
	for id, why := range p.unplaced {
		out[id] = why
	}
	return out
}

var _ scheduler.NodeGateDetail = (*Pool)(nil)

// UnplacedDetail is scheduler.NodeGateDetail: the message of a reason that
// has one (agent_unreachable).
func (p *Pool) UnplacedDetail() map[core.NodeID]string {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	out := make(map[core.NodeID]string, len(p.unplacedMsg))
	for id, msg := range p.unplacedMsg {
		out[id] = msg
	}
	return out
}

// Nodes is the node list, sorted by id.
func (p *Pool) Nodes() []core.Node {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	return append([]core.Node(nil), p.nodes...)
}

// Age is always 0: membership has no feed that could go stale.
func (p *Pool) Age() time.Duration { return 0 }

// Connected is always true.
func (p *Pool) Connected() bool { return true }

// LastReconnect is always zero.
func (p *Pool) LastReconnect() time.Time { return time.Time{} }

// Changed fires after the node list changed.
func (p *Pool) Changed() <-chan struct{} { return p.notify }

// FetchNode returns the node from the current view (an agent reports itself
// every heartbeat; there is nothing fresher to ask).
func (p *Pool) FetchNode(_ context.Context, id core.NodeID) (core.Node, error) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	for _, n := range p.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return core.Node{}, fmt.Errorf("%w: %s", fleet.ErrNotFound, id)
}

// Resolve maps an id or a name to a node id. A name shared by two live agents
// is ambiguous.
func (p *Pool) Resolve(name string) (core.NodeID, error) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	if p.isLocalNameLocked(name) {
		return p.localIDLocked(), nil
	}
	if a := p.agents[name]; a != nil && !a.left {
		return core.NodeID(name), nil
	}
	var ids []core.NodeID
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		if !a.left && !a.lost && strings.EqualFold(a.reg.Name, name) {
			ids = append(ids, core.NodeID(id))
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("%w: %s", fleet.ErrNotFound, name)
	}
	return "", &fleet.AmbiguousError{Name: name, IDs: ids}
}

func (p *Pool) localIDLocked() core.NodeID {
	if ln := p.o.Config().LocalNodeName(); ln != "" {
		return core.NodeID(ln)
	}
	return "local"
}

func (p *Pool) isLocalNameLocked(name string) bool {
	ln := p.o.Config().LocalNodeName()
	return ln != "" && strings.EqualFold(name, ln)
}

// isHostAgentLocked: the agent runs on the scheduler's own machine.
//
// Settled as built by CR-5 (agent contract 11.1). The question was: in phase 4 the scheduler's own child is the host and the
// local node comes from local_probe_cmd; the host machine's agent (4.1: its id
// becomes its MAC) is not told apart by anything in the contract. An agent
// whose id or name equals the local node name (Config.LocalNodeName) is taken as the host machine's and
// is not published as a remote node (it stays registered and in status).
func (p *Pool) isHostAgentLocked(a *agent) bool {
	return p.isLocalNameLocked(a.reg.ID) || p.isLocalNameLocked(a.reg.Name)
}

// rebuildLocked recomputes the node list and wakes Changed if it differs.
func (p *Pool) rebuildLocked() {
	cfg := p.o.Config()
	var nodes []core.Node
	unplaced, msgs := map[core.NodeID]string{}, map[core.NodeID]string{}
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		if a.left || p.admin[id].Removed || p.isHostAgentLocked(a) {
			continue
		}
		// A node without a tier is listed all the same (with no tier
		// settings), so it is never silent.
		n, _ := p.nodeFromAgentLocked(cfg, a)
		why := p.unplacedLocked(cfg, a)
		if why == scheduler.UnplacedNoTierMatch && !a.noTierLogged {
			a.noTierLogged = true // once per registration: Register makes a new agent
			p.log.Warn("agent node matches no tier: it is listed as no_tier_match and takes no load; add a tier that matches its platform and device",
				"id", id, "name", a.reg.Name, "platform", a.platform, "devices", tierDevices(a), "source", SourceAgent)
		}
		if why != "" {
			unplaced[n.ID] = why
		}
		if m := p.unplacedMsgLocked(a, why); m != "" {
			msgs[n.ID] = m
		}
		nodes = append(nodes, n)
	}
	if p.probeOK {
		ln := core.Node{ID: p.localIDLocked(), Hostname: cfg.LocalNodeName(), Local: true, Online: true,
			TotalMiB: p.probeTotal, FreeMiB: max(p.probeTotal-p.probeUsed, 0)}
		if t, ok := cfg.MatchTier(ln.Hostname, "", "", true); ok {
			ln.Priority, ln.HeadroomMiB, ln.SharedMemory, ln.LinkMBs, ln.Settle = t.Priority, t.HeadroomMiB, t.SharedMemory, t.LinkMBs, t.Settle
			nodes = append(nodes, ln)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	// With placement_uses_reserved off no decision reads the average, so a
	// change of the average alone is kept without notifying (review N2).
	useAvg := cfg.Defaults.PlacementUsesReserved
	if (!sameNodes(nodes, p.nodes, useAvg) && !(len(nodes) == 0 && len(p.nodes) == 0)) || !reflect.DeepEqual(unplaced, p.unplaced) ||
		!reflect.DeepEqual(msgs, p.unplacedMsg) || p.wantedFigureLocked(nodes) {
		p.nodes, p.unplaced, p.unplacedMsg = nodes, unplaced, msgs
		select {
		case p.notify <- struct{}{}:
		default:
		}
	} else {
		p.nodes = nodes
	}
	p.evaluateStartupsLocked()
}

// nodeFromAgentLocked is the core node of an agent (agent contract 11,
// placement memory).
//
// ok is false when the node matches no tier; the node is then returned with
// no tier settings.
func (p *Pool) nodeFromAgentLocked(cfg *config.Config, a *agent) (core.Node, bool) {
	t, tiered := agentTier(cfg, a)
	n := core.Node{ID: core.NodeID(a.reg.ID), Hostname: a.reg.Name, Online: !a.lost,
		Addr:     net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.WorkerPort)),
		Priority: t.Priority, HeadroomMiB: t.HeadroomMiB, SharedMemory: t.SharedMemory, LinkMBs: t.LinkMBs, Settle: t.Settle,
		Paused: p.pausedLocked(a.reg.ID, a), UnpausePending: p.unpausePendingLocked(a.reg.ID, a), FreeUnknown: true, SystemMiB: int(a.reg.MemTotalMiB),
		// The floor is the node's own when its agent reports it (CR-9).
		WatchdogFloorMiB: int(nodeFloor(cfg, a)), CachedMiB: a.cachedMiB, CachedKnown: a.cachedKnown}
	if a.hb == nil {
		return n, tiered
	}
	d, _, ok := pickDevice(a)
	if !ok {
		return n, tiered
	}
	n.TotalMiB, n.FreeMiB, n.FreeUnknown = int(d.TotalMiB), int(max(d.FreeMiB, 0)), false
	if len(a.mem) > 0 {
		n.FigureAt = a.mem[len(a.mem)-1].at
	}
	n.StopSeenAt, n.FiguresSinceStop = a.stopSeen, a.figsSinceStop
	_, avgDev, avgOK := a.memAverage()
	if avgOK {
		n.FreeAvgMiB, n.FreeAvgKnown = int(max(avgDev, 0)), true
	}
	// Shared by the device or by the tier (CR-9 review): the agent holds such
	// a device to MemAvailable too, so Potluck places on the same figure.
	if d.SharesSystemMemory || n.SharedMemory {
		n.SharedMemory = true
		usable := a.hb.MemAvailableMiB - int64(n.WatchdogFloorMiB)
		n.FreeMiB = int(max(min(d.FreeMiB, usable), 0))
		if avg, ok := a.sharedAverage(int64(n.WatchdogFloorMiB)); ok {
			n.FreeAvgMiB, n.FreeAvgKnown = int(max(avg, 0)), true
		}
	}
	return n, tiered
}

// agentTier matches an agent node to a tier.
func agentTier(cfg *config.Config, a *agent) (core.Tier, bool) {
	return cfg.MatchTierNode(config.TierQuery{Hostname: a.reg.Name, Source: SourceAgent,
		Platform: a.platform, Devices: tierDevices(a)})
}

// tierDevices are the device names a tier's device matcher is tried on: the
// GPU models of the registration, then the device names of the last
// heartbeat (llama's own list, known once the programs are verified).
func tierDevices(a *agent) []string {
	var out []string
	for _, g := range a.reg.GPUs {
		if g.Model != "" {
			out = append(out, g.Model)
		}
	}
	if a.hb != nil {
		for _, d := range a.hb.Devices {
			if d.Name != "" {
				out = append(out, d.Name)
			}
		}
	}
	return out
}

// unplacedLocked is why the node takes no new load, "" when it may.
//
//   - no_tier_match: no tier matches it (it is outside the pool).
//   - bad_floor: its agent reports a watchdog floor that cannot be right
//     (badFloor); it would drive the node's placement memory to 0. Like
//     no_tier_match it is outside the pool until the agent is set up again.
//   - orphan_launch: it reports a launch this run does not hold (Heartbeat).
//   - silent: no heartbeat for LostAfter while it runs a launch of ours; it
//     is lost after LeaseTTL (CheckLost).
//   - agent_unreachable: Potluck has not reached its agent API since its
//     registration, or a launch request or state read got no answer since
//     (the call-back check, CR-8); a launch would cost the connect timeout
//     and fail.
//   - programs_not_ready: its programs are not verified, or it has not sent a
//     heartbeat yet; the agent would refuse a launch with release_mismatch
//     (5.1, 6.2).
//   - model_source_unreachable: worker launches ask for model_dir
//     (worker_model_dir) and its last share probe did not succeed; the agent
//     would refuse with model_source_unreachable (6.2, 8). Without model_dir a
//     worker needs no model source, so it is not looked at.
func (p *Pool) unplacedLocked(cfg *config.Config, a *agent) string {
	if _, ok := agentTier(cfg, a); !ok {
		return scheduler.UnplacedNoTierMatch
	}
	if badFloor(a) != "" {
		return scheduler.UnplacedBadFloor
	}
	if a.orphan != "" {
		return scheduler.UnplacedOrphanLaunch
	}
	if a.silent {
		return scheduler.UnplacedSilent
	}
	if !a.reachable {
		return scheduler.UnplacedAgentUnreachable
	}
	if a.hb == nil || !a.hb.ProgramsReady {
		return scheduler.UnplacedProgramsNotReady
	}
	if r := a.hb.ModelSource.Reachable; cfg.Defaults.WorkerModelDir && (r == nil || !*r) {
		return scheduler.UnplacedModelSourceUnreachable
	}
	return ""
}

// Bounds of a reported watchdog floor (CR-9 review): larger than half the
// node's memory, or larger than maxFloorMiB, is a setup mistake.
const maxFloorMiB = 16384

// badFloor says why the floor the agent reports cannot be right; "" when it
// can (or none is reported).
func badFloor(a *agent) string {
	f := reportedFloor(a)
	switch {
	case f > maxFloorMiB:
		return fmt.Sprintf("the agent reports a watchdog floor of %d MiB, more than %d MiB; set the agent's --watchdog-floor-mib again (default 1000)", f, maxFloorMiB)
	case a.reg.MemTotalMiB > 0 && f > a.reg.MemTotalMiB/2:
		return fmt.Sprintf("the agent reports a watchdog floor of %d MiB, more than half of the node's %d MiB of memory; set the agent's --watchdog-floor-mib again (default 1000)", f, a.reg.MemTotalMiB)
	}
	return ""
}

// unplacedMsgLocked is the message of a reason that has one.
func (p *Pool) unplacedMsgLocked(a *agent, why string) string {
	switch why {
	case scheduler.UnplacedAgentUnreachable:
		return a.reachMsg
	case scheduler.UnplacedBadFloor:
		return badFloor(a)
	}
	return ""
}

// pickDevice is the device a worker of this node serves: the first one of
// the node's platform backend, else the first one. extra counts the other
// devices of that backend, which are not used.
//
// One device per node (answered by the PS5 side to the contract question): a
// worker (rpc-server) exposes one device. When a node lists more than one
// device of its platform's backend, the first is used and the rest are not
// guessed at silently: the node's status has extra_devices_ignored and the
// log warns once per registration (Heartbeat).
func pickDevice(a *agent) (dev agentproto.Device, extra int, ok bool) {
	if a.hb == nil || len(a.hb.Devices) == 0 {
		return dev, 0, false
	}
	ds := a.hb.Devices
	backend := a.platform[strings.LastIndex(a.platform, "-")+1:]
	found := false
	for _, d := range ds {
		if !strings.EqualFold(d.Backend, backend) {
			continue
		}
		if found {
			extra++
			continue
		}
		dev, found = d, true
	}
	if !found {
		return ds[0], 0, true
	}
	return dev, extra, true
}

// ---- local probe ----

func (p *Pool) probeOnce(ctx context.Context) {
	cmd := p.o.Config().LocalProbeCmd
	if strings.TrimSpace(cmd) == "" {
		return
	}
	run := p.o.RunProbe
	if run == nil {
		run = func(ctx context.Context, cmd string) ([]byte, error) {
			f := strings.Fields(cmd)
			return exec.CommandContext(ctx, f[0], f[1:]...).Output()
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	out, err := run(pctx, cmd)
	cancel()
	var total, used int
	if err == nil {
		total, used, err = fleet.ParseSMI(out)
	}
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	if err != nil {
		if !p.probeWarn {
			p.probeWarn = true
			p.log.Warn("local GPU probe failed; the local node is left out", "cmd", cmd, "err", err)
		}
		if p.probeOK {
			p.probeOK = false
			p.rebuildLocked()
		}
		return
	}
	p.probeWarn = false
	if p.probeOK && p.probeTotal == total && p.probeUsed == used {
		return
	}
	p.probeOK, p.probeTotal, p.probeUsed = true, total, used
	p.rebuildLocked()
}

// probeFailed treats a probe that panicked as a failed probe.
func (p *Pool) probeFailed() {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	p.probeWarn = true
	if p.probeOK {
		p.probeOK = false
		p.rebuildLocked()
	}
}

// ---- status ----

// NodeFacts is what status shows about an agent node.
type NodeFacts struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Addr          string  `json:"addr"`
	State         string  `json:"state"` // active, lost, left, removed
	Install       string  `json:"install,omitempty"`
	Release       string  `json:"release,omitempty"`
	Platform      string  `json:"platform,omitempty"`
	ProgramsReady bool    `json:"programs_ready"`
	ProgramsError *string `json:"programs_error"`
	ModelSource   any     `json:"model_source,omitempty"`
	Paused        bool    `json:"paused"`
	// UnpausePending: the operator unpaused the node, but no heartbeat or
	// registration answer has told its agent yet, so the node still takes no
	// new load (unplaced paused, with a message). Additive; absent when false.
	UnpausePending bool     `json:"unpause_pending,omitempty"`
	HostAgent      bool     `json:"host_agent,omitempty"`
	LastSeenAgoS   *float64 `json:"last_seen_ago_s,omitempty"`
	// Role and LaunchID are what the agent last reported in its heartbeat.
	Role     string  `json:"role,omitempty"`
	LaunchID *string `json:"launch_id"`
	// PotluckLaunchID is the launch Potluck holds for the node (sent or about
	// to be sent, whatever the agent reports) and LaunchState its state as
	// Potluck last read it.
	PotluckLaunchID *string `json:"potluck_launch_id,omitempty"`
	LaunchState     string  `json:"launch_state,omitempty"`
	Devices         any     `json:"devices,omitempty"`
	MemAvailMiB     *int64  `json:"mem_available_mib,omitempty"`
	// MemAvailAvgMiB is mem_available_mib averaged over the heartbeats of
	// defaults.free_average_window (memavg.go). Additive.
	MemAvailAvgMiB *int64          `json:"mem_available_avg_mib,omitempty"`
	Stats          json.RawMessage `json:"stats,omitempty"`
	StatsNote      string          `json:"stats_note,omitempty"`
	// GPUUtilPercent is the busiest GPU's utilisation from stats.gpus (the
	// agent's last heartbeat); absent when no GPU reports one. Additive.
	GPUUtilPercent *float64 `json:"gpu_util_percent,omitempty"`
	// ExtraDevicesIgnored counts the node's devices of its platform backend
	// beyond the first, which no worker uses (one device per node).
	ExtraDevicesIgnored int `json:"extra_devices_ignored"`
	// Placeable: the node may take a new load. Unplaced is why not, when the
	// node itself is the reason: host_machine, no_tier_match, bad_floor, orphan_launch,
	// silent, agent_unreachable, programs_not_ready or
	// model_source_unreachable; UnplacedMessage says more when there is more
	// to say (agent_unreachable: the address, the port and what to check;
	// bad_floor: the floor and the bound it breaks). A
	// node that is paused and has no reason of its own has paused (CR-9). A
	// lost, left or removed node is not placeable either; its state says so.
	// Capabilities are the words the agent advertised at registration (agent
	// contract 6.13), sorted; an empty list when it advertised none.
	Capabilities    []string `json:"capabilities"`
	Placeable       bool     `json:"placeable"`
	Unplaced        string   `json:"unplaced,omitempty"`
	UnplacedMessage string   `json:"unplaced_message,omitempty"`
	// Excluded: exclude_nodes names the node (ExcludedBy, the matching
	// entry), as /api/status's view says. Such a node is not placeable and
	// its unplaced reason is excluded, whatever else holds; the rest of its
	// entry is as for any node. Absent when not excluded.
	Excluded   bool   `json:"excluded,omitempty"`
	ExcludedBy string `json:"excluded_by,omitempty"`
	// RunningWhileExcluded: the node is excluded, yet an instance placed
	// before the exclusion still runs there. Exclusion only stops new
	// placements; the instance keeps running and serving until it is
	// unloaded. Absent otherwise.
	RunningWhileExcluded *RunningInstance `json:"running_while_excluded,omitempty"`
	// WatchdogFloorMiB is the watchdog floor the agent reports (CR-9); absent
	// for an agent that reports none (release 0.1.0). FloorMismatch says so
	// when it differs from defaults.watchdog_floor_mib; the agent's is used
	// for the node.
	WatchdogFloorMiB *int64 `json:"watchdog_floor_mib,omitempty"`
	FloorMismatch    string `json:"floor_mismatch,omitempty"`
	// StopReason is why the agent is stopping its launch, from its last
	// heartbeat, while the launch is stopping (CR-9).
	StopReason  string `json:"stop_reason,omitempty"`
	StopMessage string `json:"stop_message,omitempty"`
	// Logs is the node's central logging part (CR-10). The pool fills in what
	// the agent reports; the API adds what its log route counted. Absent when
	// there is neither.
	Logs *NodeLogs `json:"logs,omitempty"`
	// HostUptimeS is the machine's own uptime in seconds since its kernel
	// booted, as its agent reported it in its last heartbeat (that was
	// last_seen_ago_s ago); BootID is its kernel's boot id. Both absent for
	// an agent that reports neither.
	HostUptimeS *float64 `json:"host_uptime_s,omitempty"`
	BootID      string   `json:"boot_id,omitempty"`
	// The memory reservation rule (scheduler/reserve.go), filled in by the
	// API from the scheduler's node status; absent for a node the scheduler
	// does not list. Reserved lists what is placed, loading or loaded there
	// with its reserved figure; DriftMiB is MeasuredUsedMiB - ReservedMiB;
	// FreeForNewLoadMiB is the node's budget for a new load now.
	Reserved          any    `json:"reserved,omitempty"`
	ReservedMiB       *int   `json:"reserved_mib,omitempty"`
	MeasuredUsedMiB   *int   `json:"measured_used_mib,omitempty"`
	DriftMiB          *int   `json:"drift_mib,omitempty"`
	DriftWarning      string `json:"drift_warning,omitempty"`
	FreeForNewLoadMiB *int   `json:"free_for_new_load_mib,omitempty"`
	// LastStartup is the node's last start as Potluck recorded it: its kind
	// and the seconds from boot start to ready for model loads, or the
	// condition still missing. Absent for an agent that sends no start-up
	// report.
	LastStartup *startups.Last `json:"last_startup,omitempty"`
}

// UnplacedExcluded is the unplaced reason of a node that exclude_nodes names,
// in the node facts (/api/nodes, the agents of /api/status). The scheduler
// applies exclude_nodes itself, so its unplaced map never carries it.
const UnplacedExcluded = "excluded"

// ExcludedMessage is the unplaced message of an excluded node.
func ExcludedMessage(entry string) string { return "excluded by exclude_nodes: " + entry }

// UnpausePendingMessage is the unplaced message of a node whose unpause has
// not reached its agent yet.
const UnpausePendingMessage = "unpaused; the node takes load once its agent has been told (with its next heartbeat answer, within a few seconds, or when it registers again)"

// FeedModelSource is a node's model_source once it has sent a heartbeat: the
// heartbeat's state and the registration's target (the heartbeat has none).
// A dir:<path> node registers as kind share with the path as target (agent
// contract 6.10), so the target tells a model folder (an absolute path) from
// a share (host:export). Additive; absent for push.
type FeedModelSource struct {
	agentproto.ModelSourceState
	Target string `json:"target,omitempty"`
}

// Facts lists every agent node Potluck knows, removed ones included.
func (p *Pool) Facts() []NodeFacts {
	now := p.o.Clock()
	cfg := p.o.Config()
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	var out []NodeFacts
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		f := NodeFacts{ID: id, Name: a.reg.Name, Addr: a.reg.Addr, State: StateActive, Install: a.reg.Install,
			Release: a.reg.AgentRelease, Platform: a.platform, Paused: p.admin[id].Paused, UnpausePending: p.unpausePendingLocked(id, a),
			HostAgent:   p.isHostAgentLocked(a),
			ModelSource: a.reg.ModelSource, Stats: a.stats, StatsNote: a.statsNote, Capabilities: sortedWords(a.reg.Capabilities),
			GPUUtilPercent: gpuUtil(a.stats)}
		switch {
		case p.admin[id].Removed:
			f.State = StateRemoved
		case a.left:
			f.State = StateLeft
		case a.lost:
			f.State = StateLost
		}
		ago := now.Sub(a.lastSeen).Seconds()
		f.LastSeenAgoS = &ago
		if hb := a.hb; hb != nil {
			f.ProgramsReady, f.ProgramsError, f.Role, f.LaunchID = hb.ProgramsReady, hb.ProgramsError, hb.Role, hb.LaunchID
			f.ModelSource, f.Devices = FeedModelSource{hb.ModelSource, a.reg.ModelSource.Target}, hb.Devices
			m := hb.MemAvailableMiB
			f.MemAvailMiB = &m
			if avg, _, ok := a.memAverage(); ok {
				f.MemAvailAvgMiB = &avg
			}
			_, f.ExtraDevicesIgnored, _ = pickDevice(a)
			f.StopReason, f.StopMessage = hb.StopReason, hb.StopMessage
			f.HostUptimeS, f.BootID = hb.HostUptimeS, hb.BootID
			if sh := parseShipping(hb.Logs); sh != nil {
				f.Logs = &NodeLogs{Refused: map[string]int64{}, Agent: sh}
			}
		}
		f.LastStartup = startups.LastOf(a.rec)
		if fl := reportedFloor(a); fl > 0 {
			f.WatchdogFloorMiB, f.FloorMismatch = &fl, floorMismatch(cfg, a)
		}
		for _, l := range p.launches {
			if l.node == core.NodeID(id) {
				lid := l.id
				f.PotluckLaunchID, f.LaunchState = &lid, l.stateName()
			}
		}
		if f.State == StateActive || f.State == StateLost {
			f.Unplaced = p.unplacedLocked(cfg, a)
			if f.HostAgent {
				// The host machine's agent is not a remote node: the
				// scheduler's own child is the host (isHostAgentLocked).
				f.Unplaced = scheduler.UnplacedHostMachine
			}
			f.UnplacedMessage = p.unplacedMsgLocked(a, f.Unplaced)
			if f.Unplaced == "" && (f.Paused || f.UnpausePending) && f.State == StateActive {
				f.Unplaced = scheduler.UnplacedPaused // CR-9: never placeable false with no reason
				if f.UnpausePending {
					f.UnplacedMessage = UnpausePendingMessage
				}
			}
		}
		if e, ok := cfg.ExcludedBy(core.NodeID(id), a.reg.Name); ok {
			f.Excluded, f.ExcludedBy = true, e
			if f.State == StateActive || f.State == StateLost {
				f.Unplaced, f.UnplacedMessage = UnplacedExcluded, ExcludedMessage(e)
			}
		}
		f.Placeable = f.State == StateActive && !f.Paused && !f.UnpausePending && !f.HostAgent && f.Unplaced == ""
		out = append(out, f)
	}
	for id, adm := range p.admin {
		if _, ok := p.agents[id]; !ok && adm.Removed {
			out = append(out, NodeFacts{ID: id, Name: id, State: StateRemoved, Capabilities: []string{}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ReleaseStatus is the programs part of /api/system.
func (p *Pool) ReleaseStatus() map[string]any {
	pr := p.o.Programs
	m := map[string]any{"release": p.o.PotluckRelease, "served": pr.Release != nil}
	if pr.Err != "" {
		m["error"] = pr.Err
	}
	if pr.Release != nil {
		m["llama_build"] = pr.Release.Builds["llama"]
		var ps []string
		for name := range pr.Release.Platforms {
			ps = append(ps, name)
		}
		sort.Strings(ps)
		m["platforms"] = ps
	}
	if pr.HostBuild != "" {
		m["host_build"] = pr.HostBuild
	}
	if pr.HostErr != "" {
		m["host_build_error"] = pr.HostErr
	}
	return m
}

// sameNodes compares two node lists; without useAvg the average free
// figures are left out of the comparison.
func sameNodes(a, b []core.Node, useAvg bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		// the reading's time alone does not notify (FigureWatch does, for a
		// node the scheduler waits on)
		x.FigureAt, y.FigureAt = time.Time{}, time.Time{}
		x.StopSeenAt, y.StopSeenAt, x.FiguresSinceStop, y.FiguresSinceStop = time.Time{}, time.Time{}, 0, 0
		if !useAvg {
			x.FreeAvgMiB, x.FreeAvgKnown, y.FreeAvgMiB, y.FreeAvgKnown = 0, false, 0, false
		}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// WantFigures implements scheduler.FigureWatch: the scheduler waits for a
// memory reading of each node taken after the time given (something of
// ours just left it). The next such reading notifies, as a change does.
func (p *Pool) WantFigures(after map[core.NodeID]time.Time) {
	p.mu.Lock()
	defer supervise.Unlocked("pool", p.unlock)
	p.wantFig = after
	if p.figNotified == nil {
		p.figNotified = map[core.NodeID]notifiedFig{}
	}
	for id := range p.figNotified {
		if _, ok := after[id]; !ok {
			delete(p.figNotified, id)
		}
	}
}

// wantedFigureLocked reports whether a node the scheduler waits on carries
// a new reading taken after the time given, or a new launch end: each is
// notified once. The scheduler decides whether the reading is fresh enough
// (justReleased) and drops the node from its request when it is.
func (p *Pool) wantedFigureLocked(nodes []core.Node) bool {
	hit := false
	for _, n := range nodes {
		t, ok := p.wantFig[n.ID]
		if !ok {
			continue
		}
		last := p.figNotified[n.ID]
		if (n.FigureAt.After(t) && n.FigureAt.After(last.fig)) || n.StopSeenAt.After(last.stop) {
			p.figNotified[n.ID] = notifiedFig{n.FigureAt, n.StopSeenAt}
			hit = true
		}
	}
	return hit
}

// notifiedFig is the reading and launch end last notified for a node.
type notifiedFig struct{ fig, stop time.Time }

// sortedWords is a sorted copy of words, never nil.
func sortedWords(words []string) []string {
	out := append([]string{}, words...)
	sort.Strings(out)
	return out
}

// RunningInstance is an instance that holds a node: the profile, its state
// (starting, ready, draining, stopping) and since when it is there (unix ms).
type RunningInstance struct {
	Profile string `json:"profile"`
	State   string `json:"state"`
	Since   int64  `json:"since"`
}

// gpuUtil is the highest util_percent of stats.gpus, nil when no GPU (or no
// stats object) has one. A stats object that does not decode gives nil.
func gpuUtil(stats json.RawMessage) *float64 {
	if len(stats) == 0 {
		return nil
	}
	var st struct {
		GPUs []struct {
			UtilPercent *float64 `json:"util_percent"`
		} `json:"gpus"`
	}
	if json.Unmarshal(stats, &st) != nil {
		return nil
	}
	var out *float64
	for _, g := range st.GPUs {
		if u := g.UtilPercent; u != nil && (out == nil || *u > *out) {
			v := *u
			out = &v
		}
	}
	return out
}
