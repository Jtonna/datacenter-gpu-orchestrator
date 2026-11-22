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
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Contract figures.
const (
	HeartbeatEvery = 2 * time.Second // heartbeat_every_s in the registration answer
	// LostAfter: a node takes no new load after this long without a heartbeat,
	// and is lost unless it runs a launch of ours; then it is lost only after
	// LeaseTTL, when the agent has ended the launch by itself (4.3, CR-6).
	LostAfter      = 10 * time.Second
	LeaseTTL       = 20 * time.Second // lease_ttl_s of a launch (6.2)
	LeaseEvery     = 5 * time.Second  // lease renewal (6.4)
	ForceResetWait = 5 * time.Second  // forced stop, then /reset (7.2)
	ExitReadWait   = 2 * time.Second  // reading the launches of a load whose host program ended (11.1)
	// The call-back check (CR-8): after each registration Potluck reads the
	// agent's launch state once, with CallBackTimeout; while that fails it is
	// tried again every CallBackEvery and the node takes no new load.
	CallBackTimeout = 3 * time.Second
	CallBackEvery   = 15 * time.Second
	// ConnectTimeout: a connection to an agent that is not established within
	// this time fails (a firewall that drops the packets), whatever the
	// request's own timeout is.
	ConnectTimeout = 5 * time.Second
)

// ReasonReleaseUnavailable refuses a registration while Potluck serves no
// verified release.
//
// Settled as built by CR-5 (agent contract 11.1). The question was: 4.2 has no refusal word for "Potluck has no usable
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
	// ExitReadWait bounds Exits: how long a launch that has not exited is
	// read again; default ExitReadWait (tests shorten it).
	ExitReadWait time.Duration
	// CallBackEvery is the retry period of the call-back check; default
	// CallBackEvery (tests shorten it).
	CallBackEvery time.Duration
	// ConnectTimeout bounds connecting to an agent when HTTP is nil; default
	// ConnectTimeout (tests shorten it).
	ConnectTimeout time.Duration
	// Dial makes the connections to agents when HTTP is nil; default a
	// net.Dialer (tests replace it).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Logs returns the logs object of a node (CR-10), nil while central
	// logging is off. It must not block. Nil: no logs object is handed out
	// and the heartbeat answer has no logs_revision.
	Logs func(nodeID string) *agentproto.Logs
	// HostInfo reads the scheduler machine's own uptime and boot id for the
	// node feed; nil reads /proc (ProcHostInfo).
	HostInfo HostInfo
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
	// CR-9, all optional: an agent of release 0.1.0 sends none of them.
	WatchdogFloorMiB int64  `json:"watchdog_floor_mib,omitempty"`
	StopReason       string `json:"stop_reason,omitempty"`
	StopMessage      string `json:"stop_message,omitempty"`
	// CR-10, optional: the agent's log shipping figures. Raw, like stats: a
	// bad object must never reject the heartbeat.
	Logs json.RawMessage `json:"logs,omitempty"`
	// The machine's own uptime and boot id (optional; an older agent sends
	// neither). A bad value is dropped, never the heartbeat.
	HostUptimeS *float64 `json:"host_uptime_s,omitempty"`
	BootID      string   `json:"boot_id,omitempty"`
}

// optionalHeartbeatFields are dropped one by one when they do not decode;
// the rest of the heartbeat is kept.
var optionalHeartbeatFields = []string{"stats", "logs", "host_uptime_s", "boot_id", "watchdog_floor_mib", "stop_reason", "stop_message"}

// sanitize drops optional values that cannot be right.
func (hb *heartbeat) sanitize() {
	if hb.HostUptimeS != nil && *hb.HostUptimeS < 0 {
		hb.HostUptimeS = nil
	}
	if !validBootID(hb.BootID) {
		hb.BootID = ""
	}
}

// validBootID: up to 64 hex digits or dashes (the kernel writes 32 hex digits
// with dashes; the agent sends them without).
func validBootID(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-') {
			return false
		}
	}
	return true
}

type agent struct {
	reg          agentproto.RegisterRequest
	platform     string
	registeredAt time.Time
	lastSeen     time.Time
	hb           *heartbeat
	stats        json.RawMessage
	statsNote    string
	// cachedMiB is the file cache figure of the last stats object
	// (memory.cached_bytes); cachedKnown is false when it had none.
	cachedMiB   int
	cachedKnown bool
	lost, left  bool
	// silent: no heartbeat for LostAfter, not lost yet because the node runs
	// a launch of ours whose lease has not run out (CR-6). It takes no new load.
	silent bool
	// extraWarned: the warning about extra devices of the backend was given
	// for this registration.
	extraWarned bool
	// noTierLogged: the line about matching no tier was written for this
	// registration.
	noTierLogged bool
	// floorLogged: the line about a watchdog floor that differs from
	// Potluck's setting was written for this registration (CR-9).
	floorLogged bool
	// orphan is a launch the agent's heartbeat names that this run does not
	// hold; the node takes no load until the agent reports no launch.
	// orphanAsked is the launch the stop was sent for (once per launch).
	orphan, orphanAsked string
	// The call-back check (CR-8). reachable: Potluck reached the agent's API
	// since this registration and no launch or state read failed to since.
	// reachMsg says why not. reachWarned: the warning was written for this
	// failure. checking: a call-back loop runs for this agent.
	reachable, reachWarned, checking bool
	reachMsg                         string
	// toldPaused is the pause state carried by the last registration or
	// heartbeat answer sent to the agent: what the agent believes. The agent
	// refuses a launch while it believes it is paused, so the node stays
	// paused for placement until an answer has told it otherwise (unpause
	// delivery, agent contract 7.2).
	toldPaused bool
}

// Pool is the agent registry, node view and worker launcher.
type Pool struct {
	o   Options
	log *slog.Logger

	mu          sync.Mutex
	agents      map[string]*agent
	admin       map[string]cfgstore.NodeAdmin
	nodes       []core.Node
	notify      chan struct{}
	unplaced    map[core.NodeID]string // why a listed node takes no new load (view.go)
	unplacedMsg map[core.NodeID]string // more about that reason, when there is more to say
	// inlineCallBack makes Register run the first call-back check itself
	// before it returns (tests only; the check is otherwise a goroutine).
	inlineCallBack bool
	// callBack is one call-back check of the agent API at base.
	callBack   func(ctx context.Context, base string) error
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

	// CR-10 (logsnap.go): what the log routes read without taking mu, and
	// the launches that ended lately, whose last lines may still arrive.
	logSnap atomic.Pointer[LogSnapshot]
	ended   []endedLaunch
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
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = ConnectTimeout
	}
	if o.CallBackEvery <= 0 {
		o.CallBackEvery = CallBackEvery
	}
	if o.HTTP == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		dial := o.Dial
		if dial == nil {
			dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
		}
		limit := o.ConnectTimeout
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			return dial(ctx, network, addr)
		}
		o.HTTP = &http.Client{Transport: tr}
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
	if o.ExitReadWait <= 0 {
		o.ExitReadWait = ExitReadWait
	}
	p := &Pool{o: o, log: o.Logger, agents: map[string]*agent{}, admin: map[string]cfgstore.NodeAdmin{},
		notify: make(chan struct{}, 1), unplaced: map[core.NodeID]string{}, unplacedMsg: map[core.NodeID]string{},
		launches: map[string]*launch{}, byIID: map[uint64][]*launch{}, stopped: map[uint64]bool{}}
	if o.Store != nil {
		a, err := o.Store.NodeAdmins()
		if err != nil {
			return nil, err
		}
		p.admin = a
	}
	p.callBack = p.readLaunchState
	p.logSnap.Store(&LogSnapshot{})
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

// Register is POST /api/pool/register (4.2). An accepted registration starts
// the call-back check of the agent (CR-8).
func (p *Pool) Register(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
	ans, a := p.register(req)
	if a != nil {
		if p.inlineCallBack {
			p.callBackOnce(a)
		}
		p.startCallBack(a)
	}
	return ans
}

func (p *Pool) register(req agentproto.RegisterRequest) (agentproto.RegisterAnswer, *agent) {
	now := p.o.Clock()
	refuse := func(reason, format string, args ...any) (agentproto.RegisterAnswer, *agent) {
		msg := fmt.Sprintf(format, args...)
		p.log.Warn("agent registration refused", "id", req.ID, "name", req.Name, "addr", req.Addr, "reason", reason, "message", msg)
		return agentproto.RegisterAnswer{Accepted: false, Reason: reason, Message: msg}, nil
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
	// Settled as built by CR-5 (agent contract 11.1). The question was: 4.2 lists the refusals without an order. They are
	// checked: release served, removed, release (both directions), foreign
	// process, duplicate id, platform.
	if p.admin[req.ID].Removed {
		return refuse(agentproto.ReasonRemoved, "node %s was removed in Potluck; re-admit it there first", req.ID)
	}
	switch c := compareRelease(req.AgentRelease, p.o.PotluckRelease); {
	case c < 0:
		a, _ := refuse(agentproto.ReasonAgentTooOld, "agent release %s is older than Potluck's %s", req.AgentRelease, p.o.PotluckRelease)
		a.RequiredRelease = p.o.PotluckRelease
		return a, nil
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
	a.reachMsg = p.unreachableMsg(a, "the call-back check has not answered yet")
	if old != nil && old.reg.Addr == req.Addr && !old.reachable {
		a.reachWarned = old.reachWarned // one warning per failure, not per registration
	}
	p.agents[req.ID] = a
	a.toldPaused = p.admin[req.ID].Paused // carried by this answer
	p.checkFloorLocked(a)
	ans := agentproto.RegisterAnswer{Accepted: true, Release: rel.Release, Platform: platform,
		HeartbeatEveryS: HeartbeatEvery.Seconds(), Programs: rel.Programs(platform), Exporters: rel.Exporters(platform),
		Paused: p.admin[req.ID].Paused}
	if p.o.Logs != nil {
		ans.Logs = p.o.Logs(req.ID) // CR-10: where to push log lines; nil while logging is off
		if ans.Logs != nil {
			// The agent stamps its log lines with Potluck's time, from this
			// answer and every heartbeat answer. Sent only with logging on,
			// so the answers with logging off stay those of release 0.1.0.
			ans.ServerTimeUnixMs = now.UnixMilli()
		}
	}
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
	p.publishLogSnapLocked()
	return ans, a
}

// HeartbeatResult is the outcome of a heartbeat: Status 200 with Answer,
// 410 (register again) or 400.
type HeartbeatResult struct {
	Status int
	Answer agentproto.HeartbeatAnswer
	Err    string
}

// Heartbeat is POST /api/pool/nodes/{id}/heartbeat (4.3).
func (p *Pool) Heartbeat(id string, body []byte) HeartbeatResult {
	var hb heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		// A bad optional field alone (stats, or a later addition) must not
		// fail the heartbeat: retry without the optional fields that do not
		// decode.
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) != nil {
			return HeartbeatResult{Status: http.StatusBadRequest, Err: "the heartbeat is not a JSON object"}
		}
		statsBad := false
		for _, k := range optionalHeartbeatFields {
			v, ok := m[k]
			if !ok {
				continue
			}
			one, _ := json.Marshal(map[string]json.RawMessage{k: v})
			if json.Unmarshal(one, &heartbeat{}) != nil {
				delete(m, k)
				statsBad = statsBad || k == "stats"
			}
		}
		b, _ := json.Marshal(m)
		hb = heartbeat{} // the failed decode may have filled some fields
		if err := json.Unmarshal(b, &hb); err != nil {
			return HeartbeatResult{Status: http.StatusBadRequest, Err: "heartbeat: " + err.Error()}
		}
		if statsBad {
			hb.Stats = json.RawMessage(`"unreadable"`)
		}
	}
	hb.sanitize()
	now := p.o.Clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.agents[id]
	if a == nil || a.left || p.admin[id].Removed {
		return HeartbeatResult{Status: http.StatusGone, Err: "unknown agent; register again"}
	}
	if hb.AgentStartID != a.reg.AgentStartID {
		// Settled as built by CR-5 (agent contract 11.1). The question was: 4.2 says a new agent_start_id tells Potluck the
		// agent restarted, but not what a heartbeat with one means without a
		// registration first. It is answered 410 so the agent registers and
		// its launch is checked again.
		return HeartbeatResult{Status: http.StatusGone, Err: "agent_start_id changed; register again"}
	}
	wasLost := a.lost
	a.lastSeen, a.lost, a.silent = now, false, false
	a.hb = &hb
	a.stats, a.statsNote = parseStats(hb.Stats)
	a.cachedMiB, a.cachedKnown = statsCachedMiB(a.stats)
	p.checkFloorLocked(a)
	if d, extra, ok := pickDevice(a); ok && extra > 0 && !a.extraWarned {
		a.extraWarned = true
		p.log.Warn("agent node lists more than one device of its backend; extra devices ignored, the worker uses the first",
			"id", id, "name", a.reg.Name, "platform", a.platform, "device", d.Name, "extra_devices_ignored", extra)
	}
	if wasLost {
		p.log.Info("lost agent is back", "id", id, "name", a.reg.Name)
		p.recordLocked(history.TNodeMembership, history.Info, a,
			fmt.Sprintf("Node %s is back: its heartbeat arrived again.", a.reg.Name), map[string]any{"state": StateActive})
	}
	p.checkOrphanLocked(a, &hb)
	ans := agentproto.HeartbeatAnswer{Release: p.o.PotluckRelease, Paused: p.admin[id].Paused}
	if a.toldPaused && !ans.Paused {
		p.log.Info("unpause delivered to the agent; the node takes load again", "id", id, "name", a.reg.Name)
	}
	a.toldPaused = ans.Paused
	p.rebuildLocked()
	if p.o.Logs != nil {
		if l := p.o.Logs(id); l != nil {
			ans.LogsRevision = l.Revision          // CR-10: the agent reads the object again when this differs from its own
			ans.ServerTimeUnixMs = now.UnixMilli() // CR-10: Potluck's time for the agent's log lines
		}
	}
	return HeartbeatResult{Status: http.StatusOK, Answer: ans}
}

// reportedFloor is the watchdog floor the agent reports (CR-9): the last
// heartbeat's, else the registration's; 0 when it reports none (release 0.1.0).
func reportedFloor(a *agent) int64 {
	if a.hb != nil && a.hb.WatchdogFloorMiB > 0 {
		return a.hb.WatchdogFloorMiB
	}
	return max(a.reg.WatchdogFloorMiB, 0)
}

// nodeFloor is the watchdog floor used for the node's placement memory: the
// one its agent reports, else defaults.watchdog_floor_mib.
func nodeFloor(cfg *config.Config, a *agent) int64 {
	if f := reportedFloor(a); f > 0 {
		return f
	}
	return int64(cfg.Defaults.WatchdogFloorMiB)
}

// floorMismatch says, for status, that the agent's floor differs from
// Potluck's setting; "" when it does not or the agent reports none.
func floorMismatch(cfg *config.Config, a *agent) string {
	f := reportedFloor(a)
	if f == 0 || f == int64(cfg.Defaults.WatchdogFloorMiB) {
		return ""
	}
	return fmt.Sprintf("the agent's watchdog floor is %d MiB, defaults.watchdog_floor_mib is %d MiB; the agent's is used for this node",
		f, cfg.Defaults.WatchdogFloorMiB)
}

// checkFloorLocked logs a floor mismatch once per registration.
func (p *Pool) checkFloorLocked(a *agent) {
	if a.floorLogged {
		return
	}
	cfg := p.o.Config()
	if bad := badFloor(a); bad != "" {
		a.floorLogged = true
		p.log.Warn("agent watchdog floor is out of bounds; the node takes no load (bad_floor)",
			"id", a.reg.ID, "name", a.reg.Name, "agent_floor_mib", reportedFloor(a), "mem_total_mib", a.reg.MemTotalMiB, "why", bad)
		return
	}
	if floorMismatch(cfg, a) == "" {
		return
	}
	a.floorLogged = true
	p.log.Warn("agent watchdog floor differs from defaults.watchdog_floor_mib; the agent's is used for this node's placement memory",
		"id", a.reg.ID, "name", a.reg.Name, "agent_floor_mib", reportedFloor(a), "setting_mib", cfg.Defaults.WatchdogFloorMiB)
}

// checkOrphanLocked handles a heartbeat's launch_id (agent contract 11, "lost
// node with a launch"). A launch this run does not hold on that node is an
// orphan: its load was given up while the node was lost, or its start request
// got no answer. The agent is told to stop it (DELETE /launch/{id}, 6.5), once
// per launch, and the node takes no load until the agent reports no launch;
// otherwise the launch would end only by its lease and a new load placed
// meanwhile would be refused as busy.
func (p *Pool) checkOrphanLocked(a *agent, hb *heartbeat) {
	lid := ""
	if hb.LaunchID != nil {
		lid = *hb.LaunchID
	}
	if l := p.launches[lid]; lid == "" || (l != nil && l.node == core.NodeID(a.reg.ID)) {
		a.orphan, a.orphanAsked = "", ""
		return
	}
	a.orphan = lid
	if a.orphanAsked == lid {
		return
	}
	a.orphanAsked = lid
	base := "http://" + net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.AgentPort))
	p.log.Warn("agent reports a launch Potluck does not hold; telling it to stop the launch", "id", a.reg.ID, "name", a.reg.Name, "launch", lid)
	p.recordLocked(history.TNodeMembership, history.Warn, a,
		fmt.Sprintf("Node %s reported launch %s, which Potluck does not hold; it was told to stop it.", a.reg.Name, lid),
		map[string]any{"state": StateActive, "launch_id": lid, "unplaced": scheduler.UnplacedOrphanLaunch})
	p.stops.Add(1)
	go func() {
		defer p.stops.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.deleteLaunch(ctx, base, lid, false); err != nil {
			p.log.Warn("stopping a launch Potluck does not hold failed; a later heartbeat that still names it sends the stop again",
				"id", a.reg.ID, "launch", lid, "err", err)
			p.mu.Lock()
			if a.orphanAsked == lid {
				a.orphanAsked = "" // not delivered: "once" is once delivered
			}
			p.mu.Unlock()
		}
	}()
}

// ---- call-back check (CR-8) ----

func (p *Pool) agentBase(a *agent) string {
	return "http://" + net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.AgentPort))
}

// unreachableMsg is the message of the agent_unreachable reason.
func (p *Pool) unreachableMsg(a *agent, why string) string {
	return fmt.Sprintf("Potluck cannot reach the agent of %s at %s (%s). The usual cause is a firewall on the node that drops inbound connections: "+
		"allow TCP from Potluck to port %d (agent) and port %d (worker).",
		a.reg.Name, net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.AgentPort)), why, p.o.AgentPort, p.o.WorkerPort)
}

// readLaunchState is the call-back: GET /launch on the agent port.
func (p *Pool) readLaunchState(ctx context.Context, base string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+agentproto.PathLaunch, nil)
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d", agentproto.PathLaunch, resp.StatusCode)
	}
	return nil
}

// startCallBack starts the call-back loop of an agent that is not known to
// be reachable, unless one runs already.
func (p *Pool) startCallBack(a *agent) {
	p.mu.Lock()
	if a.reachable || a.checking || p.agents[a.reg.ID] != a {
		p.mu.Unlock()
		return
	}
	a.checking = true
	p.mu.Unlock()
	go func() {
		for !p.callBackOnce(a) {
			time.Sleep(p.o.CallBackEvery)
		}
	}()
}

// callBackOnce runs one call-back check. It returns true when the loop is
// over: the agent answered, or it is no longer the node's registration.
func (p *Pool) callBackOnce(a *agent) bool {
	p.mu.Lock()
	if p.agents[a.reg.ID] != a || a.left || a.reachable {
		a.checking = false
		p.mu.Unlock()
		return true
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), CallBackTimeout)
	err := p.callBack(ctx, p.agentBase(a))
	cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.agents[a.reg.ID] != a || a.left {
		a.checking = false
		return true
	}
	if err != nil {
		p.unreachableLocked(a, err)
		return false
	}
	a.reachable, a.reachMsg, a.checking = true, "", false
	lvl := slog.LevelDebug // the ordinary case is not worth a line per registration
	if a.reachWarned {
		lvl = slog.LevelInfo
	}
	a.reachWarned = false
	p.log.Log(context.Background(), lvl, "agent reachable: the call-back check answered", "id", a.reg.ID, "name", a.reg.Name,
		"addr", net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.AgentPort)))
	p.rebuildLocked()
	return true
}

// unreachableLocked puts the node into the agent_unreachable state: it takes
// no new load until a call-back check answers. One warning per failure.
func (p *Pool) unreachableLocked(a *agent, err error) {
	a.reachable, a.reachMsg = false, p.unreachableMsg(a, err.Error())
	if !a.reachWarned {
		a.reachWarned = true
		p.log.Warn("agent unreachable: Potluck cannot call the agent back; the node takes no new load until it can (checked again every "+
			p.o.CallBackEvery.String()+"). The usual cause is a firewall on the node that drops inbound connections",
			"id", a.reg.ID, "name", a.reg.Name, "addr", net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.AgentPort)),
			"worker_port", p.o.WorkerPort, "err", err)
	}
	p.rebuildLocked()
}

// agentUnreachable is called when a launch request or a state read of a node
// failed without an answer: the node goes back to agent_unreachable and the
// call-back check runs again.
func (p *Pool) agentUnreachable(node core.NodeID, err error) {
	p.mu.Lock()
	a := p.agents[string(node)]
	if a == nil || a.left {
		p.mu.Unlock()
		return
	}
	if a.reachable {
		p.unreachableLocked(a, err)
	}
	p.mu.Unlock()
	p.startCallBack(a)
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

// statsCachedMiB is memory.cached_bytes of a stats object in MiB; ok is false
// when the object has none (an older agent, or the node source is absent).
func statsCachedMiB(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var v struct {
		Memory *struct {
			CachedBytes *int64 `json:"cached_bytes"`
		} `json:"memory"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Memory == nil || v.Memory.CachedBytes == nil || *v.Memory.CachedBytes < 0 {
		return 0, false
	}
	return int(*v.Memory.CachedBytes >> 20), true
}

// Deregister is POST /api/pool/nodes/{id}/deregister (10): the node's instance is
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
	p.publishLogSnapLocked()
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
	if ag := p.agents[id]; !paused && ag != nil && ag.toldPaused {
		p.log.Info("node unpaused; it takes load once its agent has been told (next heartbeat answer)", "id", id, "name", ag.reg.Name)
	}
	p.rebuildLocked()
	return prev, nil
}

// pausedLocked: the node takes no new load because of a pause, either the
// operator's or one its agent has not yet been told is over.
func (p *Pool) pausedLocked(id string, a *agent) bool {
	return p.admin[id].Paused || p.unpausePendingLocked(id, a)
}

// unpausePendingLocked: the node was unpaused, but no answer has told its
// agent yet, so the agent would still refuse a launch as paused.
func (p *Pool) unpausePendingLocked(id string, a *agent) bool {
	return a != nil && a.toldPaused && !p.admin[id].Paused
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
	p.publishLogSnapLocked()
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

// CheckLost marks agents without a heartbeat for LostAfter as lost. An agent
// whose node runs a launch of ours is only silent then (no new load), and
// lost after LeaseTTL: an agent restart that takes longer than LostAfter
// re-adopts its launch, which must not be given up before the agent itself
// would end it (CR-6). Run calls it every second; tests call it directly.
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
		var held []string
		for _, l := range p.launches {
			if l.node == core.NodeID(id) {
				held = append(held, l.id)
			}
		}
		sort.Strings(held)
		if len(held) > 0 && now.Sub(a.lastSeen) < LeaseTTL {
			if !a.silent {
				a.silent, changed = true, true
				p.log.Warn("agent silent: no heartbeat; its launch is kept until the lease ends, the node takes no new load",
					"id", id, "name", a.reg.Name, "silent_for", now.Sub(a.lastSeen).Round(time.Second), "launches", held)
			}
			continue
		}
		a.lost, a.silent, changed = true, false, true
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
