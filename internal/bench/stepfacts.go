package bench

import (
	"context"
	"slices"
	"sort"
	"time"
)

// The facts of a step that tell one hardware scenario from another without
// reading descriptions: the lowest available memory per node, how the weights
// travelled (load mode), which node was the host and which were workers, and
// whether a reload could come from the page cache.

// LowestMiB is the lowest available system memory of any node during the
// step (nodes whose figure is system memory), nil when none was recorded.
func LowestMiB(st *Step) *int64 { return lowestOfKind(st, MemKindSystem) }

// LowestGPUMiB is the lowest free GPU memory of any node with a dedicated GPU
// during the step, nil when none was recorded.
func LowestGPUMiB(st *Step) *int64 { return lowestOfKind(st, MemKindGPU) }

func lowestOfKind(st *Step, kind string) *int64 {
	var low *int64
	for _, n := range st.MemLow {
		if n.KindOf() == kind && n.StepMiB != nil && (low == nil || *n.StepMiB < *low) {
			v := *n.StepMiB
			low = &v
		}
	}
	return low
}

// StepLoadMode is the load mode of the step's first load that recorded one.
func StepLoadMode(st *Step) string {
	if st.Load != nil && st.Load.LoadMode != "" {
		return st.Load.LoadMode
	}
	for _, p := range st.Phases {
		if p.Load != nil && p.Load.LoadMode != "" {
			return p.Load.LoadMode
		}
	}
	return ""
}

// StepHost is the host node's name, "" when not recorded.
func StepHost(st *Step) string {
	for _, r := range st.Roles {
		if r.Role == RoleHost {
			return r.Node
		}
	}
	return ""
}

// StepWorkers are the worker nodes' names, sorted.
func StepWorkers(st *Step) []string {
	var out []string
	for _, r := range st.Roles {
		if r.Role == RoleWorker {
			out = append(out, r.Node)
		}
	}
	sort.Strings(out)
	return out
}

// observe feeds a status into the memory watch and notes the local host.
func (br *benchRun) observe(s *StatusInfo) {
	if s == nil {
		return
	}
	br.watch.observe(s)
	names := map[string]string{}
	for _, w := range s.Workers {
		if w.Hostname != "" {
			names[w.ID] = w.Hostname
		}
		if w.Local {
			br.mu.Lock()
			br.localID = w.ID
			br.mu.Unlock()
		}
	}
	br.x.a.mu.Lock()
	br.x.a.hosts = mergeHosts(br.x.a.hosts, names)
	br.x.a.mu.Unlock()
}

// sampleNow reads the status once for the memory watch.
func (br *benchRun) sampleNow() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s, err := br.x.m.o.API.Status(ctx); err == nil {
		br.x.a.mu.Lock()
		br.x.a.hosts = mergeHosts(br.x.a.hosts, s.Hostnames)
		br.x.a.mu.Unlock()
		br.observe(s)
	}
}

// stepFacts fills in the step's lowest available memory, roles and reload
// figures from what the step saw. The nodes that took part are the local host,
// the plan's nodes and the nodes the instance was placed on.
func (br *benchRun) stepFacts() {
	x, st := br.x, br.st
	x.a.mu.Lock()
	names := mergeHosts(nil, x.a.hosts)
	x.a.mu.Unlock()
	name := func(id string) string {
		if n := names[id]; n != "" {
			return n
		}
		return id
	}
	br.mu.Lock()
	plan, nodes, localID := br.plan, br.nodes, br.localID
	br.mu.Unlock()

	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	add(localID)
	if plan != nil {
		for _, id := range plan.Nodes {
			add(id)
		}
	}
	for _, n := range nodes {
		add(n.ID)
	}
	st.MemLow = br.watch.result(ids, names)

	// Roles: the host is the scheduler's machine; the workers are the remote
	// nodes the instance was placed on (the plan's when it never started).
	// Each node's share of the model's memory comes from the load record
	// (history) when it has one, else from the instance.
	measured := map[string]*float64{}
	if st.Load != nil {
		for _, n := range st.Load.Nodes {
			measured[n.Node] = n.MemoryMiB
		}
	}
	share := func(id string, inst float64) *float64 {
		if v := measured[name(id)]; v != nil {
			return v
		}
		if inst > 0 {
			return &inst
		}
		return nil
	}
	var roles []NodeRole
	if localID != "" {
		r := NodeRole{ID: localID, Node: name(localID), Role: RoleHost}
		for _, n := range nodes {
			if n.ID == localID || n.Local {
				r.MemoryMiB = share(n.ID, n.MemoryMiB)
			}
		}
		roles = append(roles, r)
	}
	if len(nodes) > 0 {
		for _, n := range nodes {
			if !n.Local && n.ID != localID {
				roles = append(roles, NodeRole{ID: n.ID, Node: name(n.ID), Role: RoleWorker, MemoryMiB: share(n.ID, n.MemoryMiB)})
			}
		}
	} else if plan != nil {
		for _, id := range plan.Nodes {
			if id != localID {
				roles = append(roles, NodeRole{ID: id, Node: name(id), Role: RoleWorker})
			}
		}
	}
	st.Roles = roles

	if plan != nil {
		st.Reload = nil
		for _, r := range plan.Reload {
			r.Node = name(r.ID)
			st.Reload = append(st.Reload, r)
		}
	}
}
