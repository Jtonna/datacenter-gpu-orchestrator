package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// A console as host (phase 6). A profile with host: node runs its
// llama-server host process on the first of its forced nodes, started there
// by the node's agent as a host launch, instead of on the scheduler's
// machine. The host node is also the first device of the load; the other
// nodes are worker launches as before, started first. The host is the
// instance's child: a remote child (instance.RemoteChild) the pool drives,
// so the queue, eviction, leases, failure rules and history are those of
// any instance.

// HostLaunchSpec is the host launch of one load.
type HostLaunchSpec struct {
	IID     uint64
	Profile string
	Label   string // the model file, for the agent's logs and status
	Node    core.NodeID
	Model   string // file name relative to the node's model folder
	Ctx     int    // per slot
	// Parallel is the number of slots.
	Parallel int
	// Workers are the worker launches' rpc addresses, in device order after
	// the host's own device; TensorSplit has one weight more (host first).
	Workers     []string
	TensorSplit []float64
	RPCFileLoad string
	ExtraArgs   []string
	// ExpectMiB and MinMemAvailableMiB as for a worker launch, the host
	// process included.
	ExpectMiB          int64
	MinMemAvailableMiB int64
	// APIKey is the per-launch key the proxy then sends.
	APIKey string
}

// HostLauncher is implemented by a WorkerLauncher that can start a host
// launch (the pool). StartHost returns once the agent accepted the launch;
// a refusal or a node that cannot host is an error.
type HostLauncher interface {
	StartHost(ctx context.Context, spec HostLaunchSpec) (instance.Child, error)
}

// HostGate is implemented by a view that can say whether a node can host
// (its agent advertises host_launch, its model source is not push). Without
// it, StartHost alone decides.
type HostGate interface {
	// CanHost reports whether the node may run a host launch; when it may
	// not, why says so.
	CanHost(id core.NodeID) (ok bool, why string)
}

// ReasonHostNotCapable fails a load whose host node cannot host.
const ReasonHostNotCapable = "host_not_capable"

// hostOf is the node that hosts p on pl: the first node of the placement
// for a profile with host: node, "" for the scheduler's machine.
func hostOf(p *core.Profile, pl core.Placement) core.NodeID {
	if pl.Host != "" {
		return pl.Host
	}
	if p == nil || p.Host != core.HostNode || len(pl.Nodes) == 0 || pl.Nodes[0].Local {
		return ""
	}
	return pl.Nodes[0].ID
}

// hostOverhead is what hosting adds on node id of pl (host_overhead_mib on
// the host node, 0 elsewhere).
func (c *schedCore) hostOverhead(p *core.Profile, pl core.Placement, id core.NodeID) int {
	if h := hostOf(p, pl); h != "" && h == id {
		return max(c.cfg.Defaults.HostOverheadMiB, 0)
	}
	return 0
}

// hostPlacementError says why p cannot be hosted on pl; "" when it can.
func (c *schedCore) hostPlacementError(p *core.Profile, pl core.Placement) string {
	if p.Host != core.HostNode {
		return ""
	}
	if !c.agents {
		return "host: node needs membership: agents"
	}
	if len(pl.Nodes) == 0 {
		return "host: node needs a node to host"
	}
	for _, n := range pl.Nodes {
		if n.Local {
			// The scheduler's own GPU is a device of the scheduler's host
			// process only; a node's host cannot use it.
			return fmt.Sprintf("host: node cannot use the scheduler's machine (%s) as a device; remove it from force_nodes", n.ID)
		}
	}
	if g, ok := c.view.(HostGate); ok {
		if ok, why := g.CanHost(pl.Nodes[0].ID); !ok {
			return fmt.Sprintf("%s: node %s cannot host: %s", ReasonHostNotCapable, c.nodeName(pl.Nodes[0].ID), why)
		}
	}
	return ""
}

// newAPIKey is a fresh per-launch key: 32 characters of [A-Za-z0-9_-].
func newAPIKey() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// hostSpec is the host launch of in (launch, phase 6).
func (c *schedCore) hostSpec(in *inst) HostLaunchSpec {
	p, pl := in.prof, in.placement
	parallel := max(p.Parallel, 1)
	spec := HostLaunchSpec{IID: in.iid, Profile: p.Name, Label: path.Base(p.Model), Node: pl.Host,
		Model: p.Model, Ctx: p.Ctx, Parallel: parallel,
		ExtraArgs:          instance.HostLaunchArgs(*p, c.cfg.Defaults.HostNoMmap),
		ExpectMiB:          in.hostTarget.ExpectMiB,
		MinMemAvailableMiB: in.hostTarget.MinMemAvailableMiB,
		RPCFileLoad:        "0",
		APIKey:             newAPIKey(),
	}
	if c.cfg.Defaults.WorkerModelDir {
		spec.RPCFileLoad = "required"
	}
	for _, n := range pl.Nodes[1:] {
		spec.Workers = append(spec.Workers, n.Addr)
	}
	if len(pl.Nodes) > 1 {
		for _, f := range instance.SplitFractions(*p, pl) {
			spec.TensorSplit = append(spec.TensorSplit, f)
		}
	}
	return spec
}

// startHost starts the host launch through the WorkerLauncher.
func (s *Scheduler) startHost(ctx context.Context, spec HostLaunchSpec) (instance.Child, error) {
	h, ok := s.opts.Workers.(HostLauncher)
	if !ok {
		return nil, fmt.Errorf("%s: this scheduler cannot start a host on a node", ReasonHostNotCapable)
	}
	return h.StartHost(ctx, spec)
}

// hostExitInfo is the agent's exit of a host launch as the need rule reads
// it: the reason word is the first word of st.Reason.
func hostExitInfo(st instance.ExitStatus) *agentproto.ExitInfo {
	reason, msg, _ := strings.Cut(st.Reason, ": ")
	return &agentproto.ExitInfo{Reason: reason, Message: msg}
}

// planHost fills the host part of a plan: where llama-server would run, the
// overhead on that node, and why it cannot host now.
func (c *schedCore) planHost(r *PlanReport, p *core.Profile, placed *core.Placement) {
	r.Host = core.HostServer
	if p.Host != core.HostNode {
		return
	}
	r.Host, r.HostOverheadMiB = core.HostNode, max(c.cfg.Defaults.HostOverheadMiB, 0)
	var pl core.Placement
	switch {
	case placed != nil && len(placed.Nodes) > 0:
		pl = *placed
	case len(p.ForceNodes) > 0:
		for _, f := range p.ForceNodes {
			id, err := c.view.Resolve(f)
			if err != nil {
				r.HostProblem = fmt.Sprintf("forced node %q: %v", f, err)
				return
			}
			n, _ := c.node(id)
			pl.Nodes = append(pl.Nodes, core.PlacedNode{ID: id, Addr: n.Addr, Local: n.Local})
		}
	}
	if len(pl.Nodes) > 0 {
		r.HostNode = pl.Nodes[0].ID
	}
	r.HostProblem = c.hostPlacementError(p, pl)
}

// LeaseNode is one machine of the instance that serves a request, as the
// request log keeps it.
type LeaseNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`           // hostname when known, else the id
	Role string `json:"role"`           // host (runs llama-server) or helper (an RPC worker)
	Addr string `json:"addr,omitempty"` // rpc address; empty for the local node
	// Share is the machine's fraction of the model (its tensor split weight
	// over the sum, as in /api/status). 0 when it holds none of the split:
	// the scheduler's machine hosting with no layers, or a command profile.
	Share float64 `json:"share,omitempty"`
}

// Roles of a LeaseNode.
const (
	RoleHost   = "host"
	RoleHelper = "helper"
)

// leaseNodesOf is in's machines with their roles and shares, host first,
// built once per instance. The host is the placement's host node; with
// none, llama-server runs on the scheduler's machine: the local node when
// it is placed, else the scheduler's machine is added as host with no
// share (it holds no layers). Every other node is a helper.
func (c *schedCore) leaseNodesOf(in *inst) []LeaseNode {
	if in.leaseNodes != nil {
		return in.leaseNodes
	}
	llama := in.prof.Kind != core.KindCommand
	if len(in.placement.Nodes) == 0 && !llama {
		return nil
	}
	host := hostOf(in.prof, in.placement)
	var shares []float64
	if llama {
		shares = instance.SplitFractions(*in.prof, in.placement)
	}
	var hosts, helpers []LeaseNode
	for i, pn := range in.placement.Nodes {
		n := LeaseNode{ID: string(pn.ID), Name: c.nodeName(pn.ID), Role: RoleHelper, Addr: pn.Addr}
		if i < len(shares) {
			n.Share = shares[i]
		}
		if pn.ID == host || host == "" && pn.Local {
			n.Role = RoleHost
			hosts = append(hosts, n)
			continue
		}
		helpers = append(helpers, n)
	}
	if len(hosts) == 0 && host == "" && llama {
		id, name := c.serverMachine()
		hosts = append(hosts, LeaseNode{ID: id, Name: name, Role: RoleHost})
	}
	in.leaseNodes = append(hosts, helpers...)
	return in.leaseNodes
}

// serverMachine is the scheduler's own machine as an instance member: the
// local node of the view (its id and name), else the configured local node
// name.
func (c *schedCore) serverMachine() (id, name string) {
	for nid, n := range c.raw {
		if n.Local {
			id = string(nid)
			break
		}
	}
	name = c.cfg.LocalNodeName()
	switch {
	case id == "" && name == "":
		return "scheduler", "scheduler"
	case id == "":
		return name, name
	case name == "":
		return id, c.nodeName(core.NodeID(id))
	}
	return id, name
}
