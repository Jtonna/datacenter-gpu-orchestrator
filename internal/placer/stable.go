package placer

import (
	"cmp"
	"slices"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The stable order (Request.EqualMiB above 0).
//
// Free memory on an idle node moves by a few MiB between reads. Ordering
// nodes by their exact budgets, and splitting a model by them, makes an idle
// plan change from one second to the next. With EqualMiB set:
//
//   - Each node has a steady figure (Candidate.SteadyMiB, the caller's
//     noise-filtered budget; BudgetMiB when 0). Sorted by that figure, nodes
//     whose neighbours are closer than EqualMiB form one class; the class
//     figure is its smallest member's steady figure.
//   - Where today's rule compares budgets, the class figures are compared;
//     within a class the nodes are equal: a warm node (its page cache likely
//     holds the profile's model file) first, then by name, then by ID.
//   - A split of two or more nodes is by the class figures (core.PlacedNode
//     .SplitMiB) when every node holds its share that way on its exact
//     budget; otherwise it stays the split by exact budget.
//
// Fit is never decided on these figures: a set is a candidate only when it
// fits on the exact budgets by today's rule (fitsSet), so a node that does
// not fit is never chosen because it sorts first, and every profile's action
// is what it was. A forced placement never uses any of this.

func stableOn(req Request) bool { return req.EqualMiB > 0 && len(req.Force) == 0 }

func steadyOf(c Candidate) int {
	if c.SteadyMiB > 0 {
		return c.SteadyMiB
	}
	return c.BudgetMiB
}

func nameOf(c Candidate) string {
	if c.Name != "" {
		return c.Name
	}
	return string(c.ID)
}

// classFigures returns each node's class figure (see above), in input order.
func classFigures(nodes []Candidate, equalMiB int) []int {
	idx := make([]int, len(nodes))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		x, y := nodes[idx[a]], nodes[idx[b]]
		if steadyOf(x) != steadyOf(y) {
			return steadyOf(x) < steadyOf(y)
		}
		return x.ID < y.ID
	})
	out := make([]int, len(nodes))
	rep, prev := 0, 0
	for k, i := range idx {
		s := steadyOf(nodes[i])
		if k == 0 || s-prev >= equalMiB {
			rep = max(s, 1)
		}
		out[i], prev = rep, s
	}
	return out
}

// ClassFigures is classFigures for callers that order nodes the same way
// outside a placement (the scheduler's hypothetical placement).
func ClassFigures(nodes []Candidate, equalMiB int) []int {
	if equalMiB <= 0 {
		return nil
	}
	return classFigures(nodes, equalMiB)
}

// StableLess orders two nodes of the same class: warm first, then by name,
// then by ID.
func StableLess(a, b Candidate) bool { return stableLess(a, b) }

func stableLess(a, b Candidate) bool {
	if a.Warm != b.Warm {
		return a.Warm
	}
	if c := cmp.Compare(nameOf(a), nameOf(b)); c != 0 {
		return c < 0
	}
	return a.ID < b.ID
}

func setClasses(req Request, nodes []cand) {
	if !stableOn(req) {
		return
	}
	reps := classFigures(candidatesOf(nodes), req.EqualMiB)
	for i := range nodes {
		nodes[i].rep = reps[i]
	}
}

// steadyFits: the set splits by its class figures (every node has one, the
// request carries no explicit split) and every node holds its share of that
// split on its exact budget.
func steadyFits(req Request, set []Candidate) bool {
	if len(set) < 2 || len(req.TensorSplit) > 0 {
		return false
	}
	for _, c := range set {
		if c.rep <= 0 {
			return false
		}
	}
	req = req.For(len(set))
	budgets := make([]int, len(set))
	ovh := make([]int, len(set))
	for i, c := range set {
		budgets[i] = c.BudgetMiB
		ovh[i] = req.deviceMiB(c.ID, len(set))
	}
	return fitsWith(req, budgets, ovh) && sharesFit(req, set, ovh, true)
}

// SplitFits says whether nodes, split by weights (in order), each hold their
// share on their budgets by the same rule the placer used to keep a steady
// split. The pre-flight asks it with the exact figures before keeping a
// placement's steady split.
func SplitFits(req Request, nodes []Candidate, weights []int) bool {
	if len(weights) != len(nodes) {
		return false
	}
	set := slices.Clone(nodes)
	for i := range set {
		set[i].rep = weights[i]
	}
	return steadyFits(req, set)
}

// stablePlacement is toPlacement under the stable order: better tier first,
// then the larger class, then warm, then name; the split by class figures
// when it holds.
func stablePlacement(req Request, nodes []cand) core.Placement {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.rep != b.rep {
			return a.rep > b.rep
		}
		return stableLess(a.Candidate, b.Candidate)
	})
	p := core.Placement{Nodes: make([]core.PlacedNode, len(nodes))}
	steady := steadyFits(req, candidatesOf(nodes))
	for i, c := range nodes {
		p.Nodes[i] = core.PlacedNode{ID: c.ID, Addr: c.Addr, Local: c.Local, BudgetMiB: c.BudgetMiB}
		if steady {
			p.Nodes[i].SplitMiB = c.rep
		}
	}
	return p
}
