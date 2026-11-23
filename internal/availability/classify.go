// Package availability records, per machine and hour, how long each machine
// was free, in use (and how busy), loading, down or switched off, and reads
// that back as the hour-by-hour strips, uptime figures and day grids of the
// Overview and Zones pages (UI slice 1a, S3).
//
// The sampler reads only what Potluck already holds in memory: the
// scheduler's status (one read-only query, as any status reader) and the
// pool's membership facts. It never calls a machine. It writes through the
// usage store (usage.db), never on the scheduler's core loop: one small
// addition per machine at most once a minute and at each hour's end, queued
// with Submit, which never blocks.
package availability

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// State is what a machine was doing at one sample.
type State string

// The states. Down is split in two when stored (lost, set aside); the
// reader shows both as down. Unknown is never sampled: it is the time no
// sample covers.
const (
	Free     State = "free"
	InUse    State = "in_use"
	Loading  State = "loading"
	Lost     State = "lost"      // down: lost or gone (membership state lost or left; offline in the fleet view)
	SetAside State = "set_aside" // down: quarantined (set aside)
	Off      State = "off"       // switched off: paused, excluded or removed
	Down     State = "down"      // Lost + SetAside, as the reader answers
	Unknown  State = "unknown"
)

// Sample is one machine at one moment.
type Sample struct {
	Node  string
	Name  string
	State State
	// Busy is requests in flight over the instance's parallel slots (0 to
	// 1), in use only.
	Busy float64
	// Reachable: the machine answers (agents: membership state active;
	// fleet: online), whatever its state.
	Reachable bool
}

// Classify turns the scheduler's status and the pool's facts (nil without
// agents membership) into one sample per machine, in this order of
// precedence:
//
//	off       paused, excluded (exclude_nodes) or removed
//	lost      membership state lost or left, or offline in the scheduler's view
//	set_aside quarantined now (the scheduler's view)
//	loading   placed for an instance that is starting
//	in_use    placed for an instance that is ready, draining or stopping;
//	          busy = in flight / parallel (0 while stopping)
//	free      anything else
//
// It deliberately reads membership state, not the nodeLost event, which
// fires only for a node that held an instance.
func Classify(st *scheduler.Status, facts []pool.NodeFacts) []Sample {
	type node struct {
		name string
		view *scheduler.NodeStatus
		fact *pool.NodeFacts
	}
	nodes := map[string]*node{}
	var order []string
	get := func(id string) *node {
		n := nodes[id]
		if n == nil {
			n = &node{}
			nodes[id] = n
			order = append(order, id)
		}
		return n
	}
	for i := range facts {
		f := &facts[i]
		n := get(f.ID)
		n.fact, n.name = f, f.Name
	}
	placed := map[core.NodeID]*scheduler.InstanceStatus{}
	if st != nil {
		for i := range st.View.Nodes {
			v := &st.View.Nodes[i]
			n := get(string(v.ID))
			n.view = v
			if n.name == "" {
				n.name = v.Hostname
			}
		}
		for i := range st.Instances {
			in := &st.Instances[i]
			for _, m := range in.Nodes {
				placed[m.ID] = in
			}
		}
	}
	out := make([]Sample, 0, len(order))
	for _, id := range order {
		n := nodes[id]
		s := Sample{Node: id, Name: n.name, State: Free}
		f, v := n.fact, n.view
		if n.name == "" {
			s.Name = id
		}
		switch {
		case f != nil:
			s.Reachable = f.State == pool.StateActive
		case v != nil:
			s.Reachable = v.Online
		}
		in := placed[core.NodeID(id)]
		if in == nil && v != nil && v.Holder != "" && st != nil {
			for i := range st.Instances {
				if st.Instances[i].Profile == v.Holder {
					in = &st.Instances[i]
				}
			}
		}
		switch {
		case f != nil && (f.Paused || f.Excluded || f.State == pool.StateRemoved), v != nil && v.Excluded:
			s.State = Off
		case f != nil && (f.State == pool.StateLost || f.State == pool.StateLeft), v != nil && !v.Online, f == nil && v == nil:
			s.State = Lost
		case v != nil && !v.Quarantined.IsZero():
			s.State = SetAside
		case in != nil && in.State == "starting":
			s.State = Loading
		case in != nil:
			s.State = InUse
			if (in.State == "ready" || in.State == "draining") && in.Parallel > 0 {
				s.Busy = min(1, float64(in.InFlight)/float64(in.Parallel))
			}
		}
		out = append(out, s)
	}
	return out
}
