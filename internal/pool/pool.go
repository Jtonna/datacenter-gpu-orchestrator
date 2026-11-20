// Package pool is the scheduler's membership from potluck-agent (agent
// contract, defaults.membership: agents): the registry of agents built from
// registrations and heartbeats, the node view the scheduler core reads
// (fleet.View), and the worker launches of loads (scheduler.WorkerLauncher).
//
// Every time here is the scheduler's own clock: nothing an agent sends is
// taken as a wall-clock time (agent contract 2.1).
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
)

// Contract figures.
const (
	HeartbeatEvery = 2 * time.Second  // heartbeat_every_s in the registration answer
	LostAfter      = 10 * time.Second // a node is lost after this long without a heartbeat (4.3)
	LeaseTTL       = 20 * time.Second // lease_ttl_s of a launch (6.2)
	LeaseEvery     = 5 * time.Second  // lease renewal (6.4)
	ForceResetWait = 5 * time.Second  // forced stop, then /reset (7.2)
)

// ReasonReleaseUnavailable refuses a registration while Potluck serves no
// verified release.
//
// CONTRACT-QUESTION: 4.2 has no refusal word for "Potluck has no usable
// program set"; the agent retries any unknown reason every 60 s, which is
// what is wanted, so this word is used with a plain message.
const ReasonReleaseUnavailable = "release_unavailable"

// Node states shown in status.
const (
	StateActive  = "active"
	StateLost    = "lost"
	StateLeft    = "left"
	StateRemoved = "removed"
)

// AdminStore keeps the operator state of nodes (pause, removal).
type AdminStore interface {
	NodeAdmins() (map[string]cfgstore.NodeAdmin, error)
	SetNodeAdmin(id string, a cfgstore.NodeAdmin) error
}

// HistorySink receives membership and launch events.
type HistorySink interface{ Record(history.Event) }

// Programs is the release Potluck serves. Release is nil when none is
// served; Err then says why.
type Programs struct {
	Release *release.Release
	Err     string
	// HostBuild is the commit of the scheduler's own host program, and
	// HostErr why it could not be read or why it is not the release's build.
	HostBuild string
	HostErr   string
}

// Options configure a Pool.
type Options struct {
	Config         func() *config.Config
	Programs       Programs
	PotluckRelease string
	Store          AdminStore // nil keeps the admin state in memory only
	History        HistorySink
	Clock          func() time.Time
	Logger         *slog.Logger
	HTTP           *http.Client
	// AgentPort and WorkerPort default to the contract's 50060 and 50052;
	// only tests change them.
	AgentPort, WorkerPort int
	// LogDir receives a copy of each worker launch's log; empty keeps none.
	LogDir string
	// RunProbe runs local_probe_cmd for the local node.
	RunProbe func(ctx context.Context, cmd string) ([]byte, error)
	// ProbeEvery is the local probe period; default 2 s.
	ProbeEvery time.Duration
	// LeaseEvery is the lease renewal period; default LeaseEvery (tests shorten it).
	LeaseEvery time.Duration
}

// heartbeat is a decoded heartbeat. Stats stays raw: a missing or bad stats
// object must never reject the heartbeat (4.3), which agentproto.Heartbeat's
// typed Stats field cannot express.
type heartbeat struct {
	AgentStartID    string                      `json:"agent_start_id"`
	Seq             uint64                      `json:"seq"`
	UptimeS         float64                     `json:"uptime_s"`
	Role            string                      `json:"role"`
	LaunchID        *string                     `json:"launch_id"`
	ProgramsReady   bool                        `json:"programs_ready"`
	ProgramsError   *string                     `json:"programs_error"`
	ModelSource     agentproto.ModelSourceState `json:"model_source"`
	Devices         []agentproto.Device         `json:"devices"`
	MemAvailableMiB int64                       `json:"mem_available_mib"`
	Stats           json.RawMessage             `json:"stats,omitempty"`
}

type agent struct {
	reg          agentproto.RegisterRequest
	platform     string
	registeredAt time.Time
	lastSeen     time.Time
	hb           *heartbeat
	stats        json.RawMessage
	statsNote    string
	lost, left   bool
}

// Pool is the agent registry, node view and worker launcher.
type Pool struct {
	o   Options
	log *slog.Logger

	mu         sync.Mutex
	agents     map[string]*agent
	admin      map[string]cfgstore.NodeAdmin
	nodes      []core.Node
	notify     chan struct{}
	noTier     map[string]bool
	probeOK    bool
	probeTotal int
	probeUsed  int
	probeWarn  bool

	launches map[string]*launch   // by launch id
	byIID    map[uint64][]*launch // the launches of one load
	stopped  map[uint64]bool      // loads whose stop was asked for
	seq      uint64

	forceUnload func(ctx context.Context, id core.NodeID) (string, error)
	stops       sync.WaitGroup // stop goroutines under way
}

// New builds a pool. It loads the operator state from the store.
func New(o Options) (*Pool, error) {
	if o.Config == nil {
		return nil, errors.New("pool: Config is required")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.AgentPort == 0 {
		o.AgentPort = agentproto.PortAgent
	}
	if o.WorkerPort == 0 {
		o.WorkerPort = agentproto.PortWorker
	}
	if o.LeaseEvery <= 0 {
		o.LeaseEvery = LeaseEvery
	}
	if o.ProbeEvery <= 0 {
		o.ProbeEvery = 2 * time.Second
	}
	p := &Pool{o: o, log: o.Logger, agents: map[string]*agent{}, admin: map[string]cfgstore.NodeAdmin{},
		notify: make(chan struct{}, 1), noTier: map[string]bool{},
		launches: map[string]*launch{}, byIID: map[uint64][]*launch{}, stopped: map[uint64]bool{}}
	if o.Store != nil {
		a, err := o.Store.NodeAdmins()
		if err != nil {
			return nil, err
		}
		p.admin = a
	}
	return p, nil
}

// SetForceUnload connects the scheduler's force unload, used by deregister
// and removal (agent contract 10).
func (p *Pool) SetForceUnload(f func(ctx context.Context, id core.NodeID) (string, error)) {
	p.mu.Lock()
	p.forceUnload = f
	p.mu.Unlock()
}

// Release is Potluck's release.
func (p *Pool) Release() string { return p.o.PotluckRelease }

// Programs is the served release, or nil.
func (p *Pool) Programs() *release.Release { return p.o.Programs.Release }

// LoadGate refuses loads when no release is served or the host program is
// not the release's llama build (5.1).
func (p *Pool) LoadGate() error {
	pr := p.o.Programs
	if pr.Release == nil {
		return fmt.Errorf("Potluck serves no verified program release: %s", pr.Err)
	}
	if pr.HostErr != "" {
		return errors.New(pr.HostErr)
	}
	return nil
}

// ---- registration ----

// Register is POST /api/agents/register (4.2).
func (p *Pool) Register(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
	now := p.o.Clock()
	refuse := func(reason, format string, args ...any) agentproto.RegisterAnswer {
		msg := fmt.Sprintf(format, args...)
		p.log.Warn("agent registration refused", "id", req.ID, "name", req.Name, "addr", req.Addr, "reason", reason, "message", msg)
		return agentproto.RegisterAnswer{Accepted: false, Reason: reason, Message: msg}
	}
	if req.ID == "" {
		return refuse(agentproto.ReasonUnsupportedHardware, "the registration has no id")
	}
	rel := p.o.Programs.Release
	if rel == nil {
		return refuse(ReasonReleaseUnavailable, "Potluck serves no verified program release: %s", p.o.Programs.Err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// CONTRACT-QUESTION: 4.2 lists the refusals without an order. They are
	// checked: release served, removed, release (both directions), foreign
	// process, duplicate id, platform.
	if p.admin[req.ID].Removed {
		return refuse(agentproto.ReasonRemoved, "node %s was removed in Potluck; re-admit it there first", req.ID)
	}
	switch c := compareRelease(req.AgentRelease, p.o.PotluckRelease); {
	case c < 0:
		a := refuse(agentproto.ReasonAgentTooOld, "agent release %s is older than Potluck's %s", req.AgentRelease, p.o.PotluckRelease)
		a.RequiredRelease = p.o.PotluckRelease
		return a
	case c > 0:
		return refuse(agentproto.ReasonPotluckTooOld, "agent release %s is newer than Potluck's %s; the node waits for Potluck", req.AgentRelease, p.o.PotluckRelease)
	}
	if len(req.ForeignProcesses) > 0 {
		var ps []string
		for _, f := range req.ForeignProcesses {
			ps = append(ps, fmt.Sprintf("%s (pid %d)", f.Exe, f.PID))
		}
		return refuse(agentproto.ReasonForeignProcess, "the node runs llama programs the agent did not start: %s", strings.Join(ps, ", "))
	}
	if old := p.agents[req.ID]; old != nil && !old.lost && !old.left && old.reg.Addr != req.Addr {
		return refuse(agentproto.ReasonDuplicateID, "id %s is held by a live agent at %s", req.ID, old.reg.Addr)
	}
	platform, ok := rel.ChoosePlatform(req.OS, req.Arch, req.Backends, req.GPUs)
	if !ok {
		return refuse(agentproto.ReasonUnsupportedHardware, "release %s has no platform for %s/%s with backends %v", rel.Release, req.OS, req.Arch, req.Backends)
	}

	old := p.agents[req.ID]
	a := &agent{reg: req, platform: platform, registeredAt: now, lastSeen: now}
	p.agents[req.ID] = a
	ans := agentproto.RegisterAnswer{Accepted: true, Release: rel.Release, Platform: platform,
		HeartbeatEveryS: HeartbeatEvery.Seconds(), Programs: rel.Programs(platform), Exporters: rel.Exporters(platform),
		Paused: p.admin[req.ID].Paused}
	// A launch the node reports that this run does not hold is stopped
	// (4.2, 11); a launch this run holds on the node that the node no longer
	// reports is gone with the agent's restart (6.1, agent_restarted).
	if req.LaunchID != nil && *req.LaunchID != "" {
		if l := p.launches[*req.LaunchID]; l == nil || l.node != core.NodeID(req.ID) {
			id := *req.LaunchID
			ans.StopLaunch = &id
		}
	}
	for _, l := range p.launches {
		if l.node == core.NodeID(req.ID) && (req.LaunchID == nil || *req.LaunchID != l.id) {
			l.gone(agentproto.InstanceReasonAgentRestarted, "the agent restarted and no longer runs this launch")
		}
	}
	what := "registered"
	if old != nil {
		what = "registered again"
	}
	p.log.Info("agent "+what, "id", req.ID, "name", req.Name, "addr", req.Addr, "release", req.AgentRelease,
		"install", req.Install, "platform", platform, "stop_launch", ans.StopLaunch)
	if old == nil || old.lost || old.left {
		p.recordLocked(history.TNodeMembership, history.Info, a,
			fmt.Sprintf("Node %s (%s) joined the pool (%s, %s).", req.Name, req.ID, platform, req.Install),
			map[string]any{"state": StateActive, "addr": req.Addr, "platform": platform, "install": req.Install})
	}
	p.rebuildLocked()
	return ans
}

// HeartbeatResult is the outcome of a heartbeat: Status 200 with Answer,
// 410 (register again) or 400.
type HeartbeatResult struct {
	Status int
	Answer agentproto.HeartbeatAnswer
	Err    string
}

// Heartbeat is POST /api/agents/{id}/heartbeat (4.3).
func (p *Pool) Heartbeat(id string, body []byte) HeartbeatResult {
	var hb heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		// A bad stats object alone must not fail the heartbeat: retry
		// without it.
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) != nil {
			return HeartbeatResult{Status: http.StatusBadRequest, Err: "the heartbeat is not a JSON object"}
		}
		delete(m, "stats")
		b, _ := json.Marshal(m)
		if err := json.Unmarshal(b, &hb); err != nil {
			return HeartbeatResult{Status: http.StatusBadRequest, Err: "heartbeat: " + err.Error()}
		}
		hb.Stats = json.RawMessage(`"unreadable"`)
	}
	now := p.o.Clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.agents[id]
	if a == nil || a.left || p.admin[id].Removed {
		return HeartbeatResult{Status: http.StatusGone, Err: "unknown agent; register again"}
	}
	if hb.AgentStartID != a.reg.AgentStartID {
		// CONTRACT-QUESTION: 4.2 says a new agent_start_id tells Potluck the
		// agent restarted, but not what a heartbeat with one means without a
		// registration first. It is answered 410 so the agent registers and
		// its launch is checked again.
		return HeartbeatResult{Status: http.StatusGone, Err: "agent_start_id changed; register again"}
	}
	wasLost := a.lost
	a.lastSeen, a.lost = now, false
	a.hb = &hb
	a.stats, a.statsNote = parseStats(hb.Stats)
	if wasLost {
		p.log.Info("lost agent is back", "id", id, "name", a.reg.Name)
		p.recordLocked(history.TNodeMembership, history.Info, a,
			fmt.Sprintf("Node %s is back: its heartbeat arrived again.", a.reg.Name), map[string]any{"state": StateActive})
	}
	p.rebuildLocked()
	return HeartbeatResult{Status: http.StatusOK, Answer: agentproto.HeartbeatAnswer{Release: p.o.PotluckRelease, Paused: p.admin[id].Paused}}
}

// parseStats keeps a stats object of the supported stats_version (stats
// contract 7); anything else is not stored and says why.
func parseStats(raw json.RawMessage) (json.RawMessage, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	var v struct {
		StatsVersion *int `json:"stats_version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, "stats unreadable"
	}
	if v.StatsVersion == nil || *v.StatsVersion != agentproto.StatsVersion {
		return nil, "stats version not supported"
	}
	return append(json.RawMessage(nil), raw...), ""
}

// Deregister is POST /api/agents/{id}/deregister (10): the node's instance is
// stopped as in a force unload and the node is marked left. It returns false
// for an unknown id.
func (p *Pool) Deregister(ctx context.Context, id string) bool {
	p.mu.Lock()
	a := p.agents[id]
	if a == nil || a.left {
		p.mu.Unlock()
		return false
	}
	fu := p.forceUnload
	p.mu.Unlock()
	if fu != nil {
		if prof, err := fu(ctx, core.NodeID(id)); err != nil {
			p.log.Warn("force unload on deregister failed", "id", id, "err", err)
		} else if prof != "" {
			p.log.Info("instance stopped because its node left", "id", id, "profile", prof)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a.left = true
	p.setAdminLocked(id, cfgstore.NodeAdmin{}) // pause state is forgotten (10)
	p.log.Info("agent deregistered; node left the pool", "id", id, "name", a.reg.Name)
	p.recordLocked(history.TNodeMembership, history.Info, a,
		fmt.Sprintf("Node %s left the pool (deregistered by its uninstall).", a.reg.Name), map[string]any{"state": StateLeft})
	p.rebuildLocked()
	return true
}

// ErrUnknownNode is returned by the node actions for an id Potluck does not know.
var ErrUnknownNode = errors.New("pool: unknown node")

func (p *Pool) knownLocked(id string) bool {
	_, ok := p.agents[id]
	_, adm := p.admin[id]
	return ok || adm
}

// SetPaused pauses or unpauses a node (7.2). It returns the previous value.
func (p *Pool) SetPaused(id string, paused bool) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.knownLocked(id) {
		return false, ErrUnknownNode
	}
	a := p.admin[id]
	if a.Removed {
		return a.Paused, fmt.Errorf("node %s is removed", id)
	}
	prev := a.Paused
	a.Paused = paused
	if err := p.setAdminLocked(id, a); err != nil {
		return prev, err
	}
	p.rebuildLocked()
	return prev, nil
}

// Remove is DELETE /api/nodes/{id} (10): the instance on the node is stopped
// and the node is refused until re-admitted.
func (p *Pool) Remove(ctx context.Context, id string) error {
	p.mu.Lock()
	if !p.knownLocked(id) {
		p.mu.Unlock()
		return ErrUnknownNode
	}
	fu := p.forceUnload
	p.mu.Unlock()
	if fu != nil {
		if _, err := fu(ctx, core.NodeID(id)); err != nil {
			p.log.Warn("force unload on removal failed", "id", id, "err", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.setAdminLocked(id, cfgstore.NodeAdmin{Removed: true}); err != nil {
		return err
	}
	delete(p.agents, id) // its programs state and place in the pool are forgotten
	p.rebuildLocked()
	return nil
}

// Readmit is POST /api/nodes/{id}/readmit: a removed node may register again.
func (p *Pool) Readmit(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.admin[id].Removed {
		return ErrUnknownNode
	}
	return p.setAdminLocked(id, cfgstore.NodeAdmin{})
}

func (p *Pool) setAdminLocked(id string, a cfgstore.NodeAdmin) error {
	if p.o.Store != nil {
		if err := p.o.Store.SetNodeAdmin(id, a); err != nil {
			return err
		}
	}
	if a == (cfgstore.NodeAdmin{}) {
		delete(p.admin, id)
	} else {
		p.admin[id] = a
	}
	return nil
}

// CheckLost marks agents without a heartbeat for LostAfter as lost. Run calls
// it every second; tests call it directly.
func (p *Pool) CheckLost() {
	now := p.o.Clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for _, id := range p.sortedIDsLocked() {
		a := p.agents[id]
		if a.lost || a.left || now.Sub(a.lastSeen) < LostAfter {
			continue
		}
		a.lost, changed = true, true
		var held []string
		for _, l := range p.launches {
			if l.node == core.NodeID(id) {
				held = append(held, l.id)
			}
		}
		p.log.Error("agent lost: no heartbeat", "id", id, "name", a.reg.Name, "silent_for", now.Sub(a.lastSeen).Round(time.Second), "launches", held)
		p.recordLocked(history.TNodeMembership, history.Error, a,
			fmt.Sprintf("Node %s was lost: no heartbeat for %s.", a.reg.Name, now.Sub(a.lastSeen).Round(time.Second)),
			map[string]any{"state": StateLost, "launches": held})
	}
	if changed {
		p.rebuildLocked()
	}
}

func (p *Pool) sortedIDsLocked() []string {
	ids := make([]string, 0, len(p.agents))
	for id := range p.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (p *Pool) recordLocked(typ, sev string, a *agent, summary string, data map[string]any) {
	if p.o.History == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["id"] = a.reg.ID
	p.o.History.Record(history.Event{Ts: p.o.Clock().UnixMilli(), Type: typ, Severity: sev,
		Nodes: []string{a.reg.Name}, Summary: summary, Data: data})
}

// Record writes an operator action to history (pause, force unload, removal).
func (p *Pool) Record(id, summary string, data map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := id
	if a := p.agents[id]; a != nil {
		name = a.reg.Name
	}
	if p.o.History == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["id"] = id
	p.o.History.Record(history.Event{Ts: p.o.Clock().UnixMilli(), Type: history.TNodeAdmin, Severity: history.Info,
		Nodes: []string{name}, Summary: summary, Data: data})
}

// Run checks for lost agents and runs the local probe until ctx ends.
func (p *Pool) Run(ctx context.Context) {
	lost := time.NewTicker(time.Second)
	defer lost.Stop()
	probe := time.NewTimer(0)
	defer probe.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-lost.C:
			p.CheckLost()
		case <-probe.C:
			p.probeOnce(ctx)
			probe.Reset(p.o.ProbeEvery)
		}
	}
}

// compareRelease compares dotted numeric releases; a part that is not a
// number compares as text. An empty release is older than any other.
func compareRelease(a, b string) int {
	if a == b {
		return 0
	}
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		xn, xe := strconv.Atoi(x)
		yn, ye := strconv.Atoi(y)
		switch {
		case xe == nil && ye == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	if a < b {
		return -1
	}
	return 1
}

// Refresh rebuilds the node view after a configuration change (tiers).
func (p *Pool) Refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rebuildLocked()
}

// SetHistory sets the history sink (before Run).
func (p *Pool) SetHistory(h HistorySink) {
	p.mu.Lock()
	p.o.History = h
	p.mu.Unlock()
}

// WaitStops waits up to d for the worker launch stops under way.
func (p *Pool) WaitStops(d time.Duration) {
	done := make(chan struct{})
	go func() { p.stops.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		p.log.Warn("worker launch stops still running at exit; their leases will end them")
	}
}
