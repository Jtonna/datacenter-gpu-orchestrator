package scheduler

import (
	"errors"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
)

// Zone facts (UI Phase 1, slice 1a): what every page reads about a machine
// besides its memory. All of it is derived from state the core already
// holds; nothing here changes scheduling.
//
//   - Role and share of each member of an instance (memberRoles).
//   - Why a node is set aside (quarantined), since when, in which load
//     (setAsideCause, kept on the node when it is quarantined).
//   - When a claimed or settling node is expected to be free (freeEta).

// Reason words of a set-aside (QuarantineStatus.Reason,
// NodeStatus.QuarantineReason).
const (
	SetAsidePreflight        = "preflight_failed"  // a pre-flight check of a load failed on the node
	SetAsideAgentUnreachable = "agent_unreachable" // the worker launch of a load could not reach the node's agent
	SetAsideWorkerHung       = "worker_hung"       // the node's worker stopped answering (design 8.4a)
	SetAsideNodeLost         = "node_lost"         // the node was lost while a worker of ours ran there
	SetAsideOther            = "other"
)

// Host values of InstanceStatus.Host and roles of PlacedNodeStatus.Role
// (the same words as the request log's lease roles).
const (
	HostServer = "server"
	HostOnNode = "node"
	MemberHost   = "host"
	MemberHelper = "helper"
)

// Free bases of NodeStatus.FreeBasis.
const (
	FreeSettling = "settling"
	FreeFetching = "fetching"
	FreeStopping = "stopping"
	FreeDraining = "draining"
	FreeClaimed  = "claimed"
)

// setAsideCause is why a node was quarantined, kept on its nodeState.
type setAsideCause struct {
	reason, profile, message string
	since                    time.Time
}

// setAsideError carries the reason word and the load to quarantine; its
// text is the cause's own, so log lines and history read as before.
type setAsideError struct {
	reason, profile string
	err             error
}

func (e *setAsideError) Error() string { return e.err.Error() }
func (e *setAsideError) Unwrap() error { return e.err }

func setAsideErr(reason, profile string, err error) error {
	return &setAsideError{reason: reason, profile: profile, err: err}
}

// causeOf names the reason of a quarantine from its cause.
func causeOf(cause error, now time.Time) setAsideCause {
	c := setAsideCause{reason: SetAsideOther, since: now}
	if cause != nil {
		c.message = cause.Error()
	}
	var se *setAsideError
	switch {
	case errors.As(cause, &se):
		c.reason, c.profile = se.reason, se.profile
	case errors.Is(cause, errWorkerHung):
		c.reason = SetAsideWorkerHung
	case errors.Is(cause, errNodeLost):
		c.reason = SetAsideNodeLost
	}
	return c
}

// quarantineStatus is one entry of Status.Quarantines.
func (c *schedCore) quarantineStatus(id core.NodeID, ns *nodeState) QuarantineStatus {
	w := ns.quarWhy
	return QuarantineStatus{Node: id, Until: ns.quarUntil, Level: ns.quarLevel,
		Reason: w.reason, Message: w.message, Profile: w.profile, Since: w.since}
}

// memberRoles fills the host, the parallel slots and each member's role and
// share of a llama instance. The share is the member's tensor split weight
// over the sum (instance.SplitFractions: the placement's split, else the
// profile's, else the node budgets), which is exactly the fraction of the
// model's layers llama-server puts on that device. The host is the node
// that runs llama-server (host: node), or the scheduler's machine: then the
// local member, when the placement uses its GPU, is the host, and every
// remote member a helper.
func (c *schedCore) memberRoles(in *inst, s *InstanceStatus) {
	s.Parallel = max(in.prof.Parallel, 1)
	if in.prof.Kind == core.KindCommand || len(s.Nodes) == 0 {
		return
	}
	host := hostOf(in.prof, in.placement)
	if host != "" {
		s.Host, s.HostNode = HostOnNode, host
	} else {
		s.Host = HostServer
	}
	shares := instance.SplitFractions(*in.prof, in.placement)
	for i := range s.Nodes {
		m := &s.Nodes[i]
		m.Role = MemberHelper
		if (host != "" && m.ID == host) || (host == "" && m.Local) {
			m.Role = MemberHost
		}
		if i < len(shares) {
			m.Share = shares[i]
		}
	}
}

// zoneNodeFacts fills the set-aside reason and the expected free time of a
// node's status.
func (c *schedCore) zoneNodeFacts(id core.NodeID, n core.Node, ns *nodeState, s *NodeStatus) {
	if ns == nil {
		return
	}
	if c.now.Before(ns.quarUntil) {
		s.QuarantineReason = ns.quarWhy.reason
	}
	s.FreeAt, s.FreeBasis = c.freeEta(n, ns)
}

// freeEta is when a claimed or settling node is expected to be free (P6b):
// a settling node at its settle end; a claimed node whose victim stops at
// now plus the node's settle; a victim still draining has no time (it ends
// when its requests do), only the basis. A node neither claimed nor
// settling has neither.
func (c *schedCore) freeEta(n core.Node, ns *nodeState) (time.Time, string) {
	switch ns.settle {
	case setWaiting, setFailed:
		return ns.settleAt, FreeSettling
	case setFetching:
		return c.now, FreeFetching
	}
	if ns.claim == "" {
		return time.Time{}, ""
	}
	in := c.insts[ns.holder]
	switch {
	case ns.holder == "" || in == nil:
		return c.now, FreeClaimed
	case in.state == stDraining:
		return time.Time{}, FreeDraining
	default: // stopping (or about to stop)
		return c.now.Add(n.Settle), FreeStopping
	}
}
