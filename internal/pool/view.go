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
)

// The node view (fleet.View) built from the agents. Remote nodes are the
// registered agents; the local node comes from local_probe_cmd, as in fleet
// membership. There is no feed: the view is always fresh (Age 0) and a
// silent agent is shown offline once it is lost.

var _ fleet.View = (*Pool)(nil)

// SourceAgent is the tier match "source" of an agent node.
//
// CONTRACT-QUESTION: the contract does not say how agent nodes meet the
// configured tiers (priority, headroom, link speed, settle). A tier is matched
// with hostname = the agent's name, role empty and source "agent"; a node that
// matches no tier is left out of the pool and logged, as in fleet membership.
const SourceAgent = "agent"

// Nodes is the node list, sorted by id.
func (p *Pool) Nodes() []core.Node {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	defer p.mu.Unlock()
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
	defer p.mu.Unlock()
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
	if ln := p.o.Config().Fleet.LocalNode; ln != "" {
		return core.NodeID(ln)
	}
	return "local"
}

func (p *Pool) isLocalNameLocked(name string) bool {
	ln := p.o.Config().Fleet.LocalNode
	return ln != "" && strings.EqualFold(name, ln)
}

// isHostAgentLocked: the agent runs on the scheduler's own machine.
//
// CONTRACT-QUESTION: in phase 4 the scheduler's own child is the host and the
// local node comes from local_probe_cmd; the host machine's agent (4.1: its id
// becomes its MAC) is not told apart by anything in the contract. An agent
// whose id or name equals fleet.local_node is taken as the host machine's and
// is not published as a remote node (it stays registered and in status).
func (p *Pool) isHostAgentLocked(a *agent) bool {
	return p.isLocalNameLocked(a.reg.ID) || p.isLocalNameLocked(a.reg.Name)
}

// rebuildLocked recomputes the node list and wakes Changed if it differs.
func (p *Pool) rebuildLocked() {
	cfg := p.o.Config()
	var nodes []core.Node
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		if a.left || p.admin[id].Removed || p.isHostAgentLocked(a) {
			continue
		}
		n, ok := p.nodeFromAgentLocked(cfg, a)
		if !ok {
			if !p.noTier[id] {
				p.noTier[id] = true
				p.log.Info("agent node matches no tier, excluded", "id", id, "name", a.reg.Name, "source", SourceAgent)
			}
			continue
		}
		nodes = append(nodes, n)
	}
	if p.probeOK {
		ln := core.Node{ID: p.localIDLocked(), Hostname: cfg.Fleet.LocalNode, Local: true, Online: true,
			TotalMiB: p.probeTotal, FreeMiB: max(p.probeTotal-p.probeUsed, 0)}
		if t, ok := cfg.MatchTier(ln.Hostname, "", "", true); ok {
			ln.Priority, ln.HeadroomMiB, ln.SharedMemory, ln.LinkMBs, ln.Settle = t.Priority, t.HeadroomMiB, t.SharedMemory, t.LinkMBs, t.Settle
			nodes = append(nodes, ln)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	if !reflect.DeepEqual(nodes, p.nodes) && !(len(nodes) == 0 && len(p.nodes) == 0) {
		p.nodes = nodes
		select {
		case p.notify <- struct{}{}:
		default:
		}
	}
}

// nodeFromAgentLocked is the core node of an agent (agent contract 11,
// placement memory).
func (p *Pool) nodeFromAgentLocked(cfg *config.Config, a *agent) (core.Node, bool) {
	t, ok := cfg.MatchTier(a.reg.Name, "", SourceAgent, false)
	if !ok {
		return core.Node{}, false
	}
	n := core.Node{ID: core.NodeID(a.reg.ID), Hostname: a.reg.Name, Online: !a.lost,
		Addr:     net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.WorkerPort)),
		Priority: t.Priority, HeadroomMiB: t.HeadroomMiB, SharedMemory: t.SharedMemory, LinkMBs: t.LinkMBs, Settle: t.Settle,
		Paused: p.admin[a.reg.ID].Paused, FreeUnknown: true}
	if a.hb == nil {
		return n, true
	}
	d, _, ok := pickDevice(a)
	if !ok {
		return n, true
	}
	n.TotalMiB, n.FreeMiB, n.FreeUnknown = int(d.TotalMiB), int(max(d.FreeMiB, 0)), false
	if d.SharesSystemMemory {
		n.SharedMemory = true
		usable := a.hb.MemAvailableMiB - int64(cfg.Defaults.WatchdogFloorMiB)
		n.FreeMiB = int(max(min(d.FreeMiB, usable), 0))
	}
	return n, true
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
	defer p.mu.Unlock()
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

// ---- status ----

// NodeFacts is what status shows about an agent node.
type NodeFacts struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Addr          string          `json:"addr"`
	State         string          `json:"state"` // active, lost, left, removed
	Install       string          `json:"install,omitempty"`
	Release       string          `json:"release,omitempty"`
	Platform      string          `json:"platform,omitempty"`
	ProgramsReady bool            `json:"programs_ready"`
	ProgramsError *string         `json:"programs_error"`
	ModelSource   any             `json:"model_source,omitempty"`
	Paused        bool            `json:"paused"`
	HostAgent     bool            `json:"host_agent,omitempty"`
	LastSeenAgoS  *float64        `json:"last_seen_ago_s,omitempty"`
	Role          string          `json:"role,omitempty"`
	LaunchID      *string         `json:"launch_id"`
	LaunchState   string          `json:"launch_state,omitempty"`
	Devices       any             `json:"devices,omitempty"`
	MemAvailMiB   *int64          `json:"mem_available_mib,omitempty"`
	Stats         json.RawMessage `json:"stats,omitempty"`
	StatsNote     string          `json:"stats_note,omitempty"`
	// ExtraDevicesIgnored counts the node's devices of its platform backend
	// beyond the first, which no worker uses (one device per node).
	ExtraDevicesIgnored int `json:"extra_devices_ignored"`
}

// Facts lists every agent node Potluck knows, removed ones included.
func (p *Pool) Facts() []NodeFacts {
	now := p.o.Clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []NodeFacts
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		f := NodeFacts{ID: id, Name: a.reg.Name, Addr: a.reg.Addr, State: StateActive, Install: a.reg.Install,
			Release: a.reg.AgentRelease, Platform: a.platform, Paused: p.admin[id].Paused, HostAgent: p.isHostAgentLocked(a),
			ModelSource: a.reg.ModelSource, Stats: a.stats, StatsNote: a.statsNote}
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
			f.ModelSource, f.Devices = hb.ModelSource, hb.Devices
			m := hb.MemAvailableMiB
			f.MemAvailMiB = &m
			_, f.ExtraDevicesIgnored, _ = pickDevice(a)
		}
		for _, l := range p.launches {
			if l.node == core.NodeID(id) {
				lid := l.id
				f.LaunchID, f.LaunchState = &lid, l.stateName()
			}
		}
		out = append(out, f)
	}
	for id, adm := range p.admin {
		if _, ok := p.agents[id]; !ok && adm.Removed {
			out = append(out, NodeFacts{ID: id, Name: id, State: StateRemoved})
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
