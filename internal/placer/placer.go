package placer

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/estimate"
)

// Need is a profile's memory need: measured (needs_mib) or estimated
// (weights + kv, design 7.2). The two use different fit rules:
// measured fits when Σ budget ≥ MiB (estimate.FitsMeasured); estimated
// fits when Σ (budget − device_overhead) ≥ MiB × 1.05
// (estimate.FitsEstimated).
type Need struct {
	MiB      int
	Measured bool
}

// Measured returns a measured need (profile needs_mib).
func Measured(mib int) Need { return Need{MiB: mib, Measured: true} }

// Estimated returns an estimated need (estimate.NeedMiB).
func Estimated(mib int) Need { return Need{MiB: mib} }

// ForcedNode is one force_nodes entry. Name is as written in config; ID is
// what the caller resolved it to under design section 6 (an id, or the
// hostname of exactly one online member), or empty when it resolves to
// nothing. The placer matches on ID only and reports Name when missing.
type ForcedNode struct {
	Name string
	ID   core.NodeID
}

// Request is everything about the profile and policy the placer needs.
type Request struct {
	Need       Need
	AllowTiers []int // permitted tier priorities; empty means all
	// Force is force_nodes in device order. When non-empty the placement
	// is exactly these nodes, with no fit check and no max_pool limit.
	Force             []ForcedNode
	TensorSplit       []int // profile tensor_split, copied into a forced placement
	MaxPool           int   // defaults.max_pool, at least 1 (not checked when forced)
	MinShareMiB       int   // defaults.min_share_mib
	DeviceOverheadMiB int   // defaults.device_overhead_mib (estimated needs only)
}

// Candidate is one node offered to the placer.
//
// The caller passes only nodes that are, per design 7.1: the local node or
// online in a view no older than stale_after; remote with a known address;
// matched to a tier (Priority is that tier's priority); not quarantined;
// and, for Place, free (not held, not claimed by any entry, not settling)
// or, for Plan, either free or held by an Evictable. The placer itself
// applies the two remaining eligibility rules: the profile's allow_tiers
// and budget ≥ min_share (on the budget given). A candidate failing either
// is silently not usable.
type Candidate struct {
	ID        core.NodeID
	Addr      string // rpc ip:port; empty for the local node
	Local     bool
	Priority  int // matched tier's priority; lower is preferred
	BudgetMiB int // live budget (free node) or plan budget (held node), design 7.1
}

// FromNode builds a Candidate from a node view entry and the budget the
// caller computed for it (n.LiveBudgetMiB() for a free node, the plan
// budget for a held one).
func FromNode(n core.Node, budgetMiB int) Candidate {
	return Candidate{ID: n.ID, Addr: n.Addr, Local: n.Local, Priority: n.Priority, BudgetMiB: budgetMiB}
}

// Evictable is an instance the caller has found evictable for the entry
// (design 8.3: ready, past its hold, and idle or the entry has waited
// drain_after), with every node it holds at plan budget.
type Evictable struct {
	Instance     string
	Nodes        []Candidate // plan budgets
	Busy         bool        // in_flight > 0
	LoadDuration time.Duration
	IdleFor      time.Duration // now − last_used; 0 when busy
}

// PlanResult is a placement over free and evictable nodes.
type PlanResult struct {
	// Placement's nodes are the entry's claim.
	Placement core.Placement
	// Victims are the instances holding at least one placed node, sorted.
	// Using one node of an instance makes the whole instance a victim;
	// its other nodes are not claimed.
	Victims []string
}

// Place implements design 7.3 over free nodes at live budgets: 8.3 step 1,
// and step 4 when given exactly the claimed nodes (the placement may use a
// subset; the caller releases the rest).
//
// Forced (req.Force non-empty): exactly the forced nodes in config order,
// no fit check; ForcedUnavailable names any not usable.
//
// Otherwise, for each distinct priority t ascending, over the usable nodes
// with priority ≤ t:
//
//  1. single nodes that fit: best (lowest) priority, then smallest budget,
//     then ID;
//  2. else for k = 2..max_pool, the first k for which some k-node pool fits:
//     best worst-tier (lowest maximum priority), then largest total
//     budget, then the lexicographically smallest list of sorted IDs;
//  3. else the next t.
//
// No level fits: NoFit. Results do not depend on input order.
//
// Device order of the returned nodes: better tier first, then larger
// budget, then ID. Each PlacedNode carries its budget for the tensor
// split; Placement.TensorSplit is set only for forced placements.
//
// Complexity: n usable nodes, L levels. Singles O(n) per level. The pool
// size and level are found with top-k sums (O(L·n log n)); the best pool
// of that size is then found by enumerating k-subsets in budget order with
// a bound that prunes branches which cannot reach the need, at most
// C(n, max_pool) subsets (12 870 for n = 16, max_pool = 8), done once.
func Place(req Request, free []Candidate) (core.Placement, error) {
	nodes, err := prepare(req, free, nil)
	if err != nil {
		return core.Placement{}, err
	}
	chosen, err := choose(req, nodes, nil)
	if err != nil {
		return core.Placement{}, err
	}
	return toPlacement(req, chosen), nil
}

// Plan implements design 8.3 step 2: Place's rules over the free nodes
// (live budgets) together with every node of every evictable instance
// (plan budgets).
//
// "The placer's tier rules pick the nodes; ties go to the plan with the
// fewest victims, then idle victims over busy, then the shortest
// load_duration, then the longest idle" is read as this total order on
// candidate node sets at the level and size Place would use:
//
//	single: priority ↑, victims, budget ↑, ID ↑
//	pool:   worst priority ↑, victims, total budget ↓, sorted IDs ↑
//
// where "victims" compares, in turn: number of victim instances ↑, number
// of busy victims ↑ (idle over busy), Σ load_duration of victims ↑
// (shortest first), Σ idle time of victims ↓ (longest first). The tier rules (level, single
// before pool, pool size, tier) come first; the victim criteria come
// before the budget preferences, so a plan never evicts more than the
// tier rules require just to gain budget. With no evictables Plan picks
// exactly what Place picks.
//
// Forced: exactly the forced nodes; victims are their holders.
func Plan(req Request, free []Candidate, evictable []Evictable) (PlanResult, error) {
	nodes, err := prepare(req, free, evictable)
	if err != nil {
		return PlanResult{}, err
	}
	chosen, err := choose(req, nodes, evictable)
	if err != nil {
		return PlanResult{}, err
	}
	var victims []string
	for _, c := range chosen {
		if c.owner >= 0 && !slices.Contains(victims, evictable[c.owner].Instance) {
			victims = append(victims, evictable[c.owner].Instance)
		}
	}
	sort.Strings(victims)
	return PlanResult{Placement: toPlacement(req, chosen), Victims: victims}, nil
}

// CheckCapacity implements the fail-fast check of design 7.2: does the
// need fit the plan budgets of all eligible nodes together, using the best
// max_pool of them? eligible is every node meeting 7.1's eligibility
// except allow_tiers and min_share (applied here), held or not, at plan
// budget. It returns nil when the need fits, else a *NoPlacement with
// Reason ExceedsCapacity and the need, required and capacity figures.
// A forced request always passes (forced placement has no fit check).
func CheckCapacity(req Request, eligible []Candidate) error {
	if err := validateRequest(req); err != nil {
		return err
	}
	if len(req.Force) > 0 {
		return nil
	}
	nodes, err := prepare(req, eligible, nil)
	if err != nil {
		return err
	}
	budgets := make([]int, len(nodes))
	for i, c := range nodes {
		budgets[i] = c.BudgetMiB
	}
	sort.Sort(sort.Reverse(sort.IntSlice(budgets)))
	if len(budgets) > req.MaxPool {
		budgets = budgets[:req.MaxPool]
	}
	if len(budgets) > 0 && fits(req, budgets) {
		return nil
	}
	capacity := 0
	for _, b := range budgets {
		if !req.Need.Measured {
			b -= req.DeviceOverheadMiB
		}
		if b > 0 {
			capacity += b
		}
	}
	return &NoPlacement{Reason: ExceedsCapacity, NeedMiB: req.Need.MiB, RequiredMiB: required(req.Need), CapacityMiB: capacity}
}

// Fits reports whether the given budgets together hold the need under the
// need's fit rule (design 7.5: re-check on exactly the pre-flighted nodes).
// A forced request always fits.
func Fits(req Request, budgetsMiB []int) bool {
	if len(req.Force) > 0 {
		return true
	}
	return fits(req, budgetsMiB)
}

// ---- internals ----

type cand struct {
	Candidate
	owner int // index into evictables; -1 when free
}

func fits(req Request, budgets []int) bool {
	if req.Need.Measured {
		return estimate.FitsMeasured(budgets, req.Need.MiB)
	}
	return estimate.FitsEstimated(budgets, req.Need.MiB, req.DeviceOverheadMiB)
}

// required is what the fit rule asks Σ to reach: need, or ceil(need × 1.05).
func required(n Need) int {
	if n.Measured {
		return n.MiB
	}
	return (n.MiB*105 + 99) / 100
}

func invalid(format string, a ...any) error {
	return &NoPlacement{Reason: InvalidInput, Detail: fmt.Sprintf(format, a...)}
}

func validateRequest(req Request) error {
	if req.Need.MiB < 0 {
		return invalid("negative need %d", req.Need.MiB)
	}
	if len(req.Force) == 0 && req.MaxPool < 1 {
		return invalid("max_pool %d below 1", req.MaxPool)
	}
	seen := map[core.NodeID]bool{}
	for _, f := range req.Force {
		if f.ID == "" {
			continue
		}
		if seen[f.ID] {
			return invalid("forced node %s listed twice", f.ID)
		}
		seen[f.ID] = true
	}
	return nil
}

// prepare validates, merges free and evictable nodes, drops nodes failing
// allow_tiers or min_share, and sorts by ID.
func prepare(req Request, free []Candidate, ev []Evictable) ([]cand, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	all := make([]cand, 0, len(free))
	for _, c := range free {
		all = append(all, cand{c, -1})
	}
	instances := map[string]bool{}
	for i, e := range ev {
		if e.Instance == "" {
			return nil, invalid("evictable instance with empty name")
		}
		if instances[e.Instance] {
			return nil, invalid("evictable instance %s listed twice", e.Instance)
		}
		instances[e.Instance] = true
		for _, c := range e.Nodes {
			all = append(all, cand{c, i})
		}
	}
	seen := map[core.NodeID]bool{}
	out := all[:0:0]
	for _, c := range all {
		if c.ID == "" {
			return nil, invalid("candidate with empty id")
		}
		if seen[c.ID] {
			return nil, invalid("node %s offered twice", c.ID)
		}
		seen[c.ID] = true
		if len(req.AllowTiers) > 0 && !slices.Contains(req.AllowTiers, c.Priority) {
			continue
		}
		if c.BudgetMiB < req.MinShareMiB {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func choose(req Request, nodes []cand, ev []Evictable) ([]cand, error) {
	if len(req.Force) > 0 {
		return chooseForced(req, nodes)
	}
	var levels []int
	for _, c := range nodes {
		if !slices.Contains(levels, c.Priority) {
			levels = append(levels, c.Priority)
		}
	}
	sort.Ints(levels)
	s := searcher{req: req, ev: ev}
	for _, t := range levels {
		var level []cand
		for _, c := range nodes {
			if c.Priority <= t {
				level = append(level, c)
			}
		}
		if best := s.bestSingle(level); best != nil {
			return best, nil
		}
		for k := 2; k <= req.MaxPool && k <= len(level); k++ {
			if best := s.bestPool(level, k); best != nil {
				return best, nil
			}
		}
	}
	return nil, &NoPlacement{Reason: NoFit, NeedMiB: req.Need.MiB, RequiredMiB: required(req.Need)}
}

func chooseForced(req Request, nodes []cand) ([]cand, error) {
	byID := map[core.NodeID]cand{}
	for _, c := range nodes {
		byID[c.ID] = c
	}
	var out []cand
	var missing []string
	for _, f := range req.Force {
		c, ok := byID[f.ID]
		if f.ID == "" || !ok {
			missing = append(missing, f.Name)
			continue
		}
		out = append(out, c)
	}
	if len(missing) > 0 {
		return nil, &NoPlacement{Reason: ForcedUnavailable, Missing: missing, NeedMiB: req.Need.MiB, RequiredMiB: required(req.Need)}
	}
	return out, nil
}

// toPlacement orders non-forced nodes for --device: better tier first,
// then larger budget, then ID. Forced nodes keep config order.
func toPlacement(req Request, chosen []cand) core.Placement {
	nodes := slices.Clone(chosen)
	if len(req.Force) == 0 {
		sort.Slice(nodes, func(i, j int) bool {
			a, b := nodes[i], nodes[j]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if a.BudgetMiB != b.BudgetMiB {
				return a.BudgetMiB > b.BudgetMiB
			}
			return a.ID < b.ID
		})
	}
	p := core.Placement{Nodes: make([]core.PlacedNode, len(nodes))}
	for i, c := range nodes {
		p.Nodes[i] = core.PlacedNode{ID: c.ID, Addr: c.Addr, Local: c.Local, BudgetMiB: c.BudgetMiB}
	}
	if len(req.Force) > 0 && req.TensorSplit != nil {
		p.TensorSplit = slices.Clone(req.TensorSplit)
	}
	return p
}

// victimKey is the victim part of the plan order (see Plan).
type victimKey struct {
	count, busy int
	load, idle  time.Duration
}

// compare returns <0 when a is the better plan.
func (a victimKey) compare(b victimKey) int {
	switch {
	case a.count != b.count:
		return a.count - b.count
	case a.busy != b.busy:
		return a.busy - b.busy
	case a.load != b.load:
		return cmpDur(a.load, b.load)
	case a.idle != b.idle:
		return cmpDur(b.idle, a.idle)
	}
	return 0
}

func cmpDur(a, b time.Duration) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

type searcher struct {
	req Request
	ev  []Evictable
}

func (s searcher) victims(set []cand) victimKey {
	var k victimKey
	var seen []int
	for _, c := range set {
		if c.owner < 0 || slices.Contains(seen, c.owner) {
			continue
		}
		seen = append(seen, c.owner)
		e := s.ev[c.owner]
		k.count++
		if e.Busy {
			k.busy++
		}
		k.load += e.LoadDuration
		k.idle += e.IdleFor
	}
	return k
}

// bestSingle: priority ↑, victims, budget ↑, ID ↑ among single nodes that fit.
func (s searcher) bestSingle(level []cand) []cand {
	var best *cand
	var bestV victimKey
	for i := range level {
		c := &level[i]
		if !fits(s.req, []int{c.BudgetMiB}) {
			continue
		}
		v := s.victims([]cand{*c})
		if best == nil || singleLess(*c, v, *best, bestV) {
			best, bestV = c, v
		}
	}
	if best == nil {
		return nil
	}
	return []cand{*best}
}

func singleLess(a cand, av victimKey, b cand, bv victimKey) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if c := av.compare(bv); c != 0 {
		return c < 0
	}
	if a.BudgetMiB != b.BudgetMiB {
		return a.BudgetMiB < b.BudgetMiB
	}
	return a.ID < b.ID
}

// poolKey is a pool's position in the order: worst priority ↑, victims,
// total budget ↓, sorted IDs ↑.
type poolKey struct {
	worst   int
	victims victimKey
	total   int
	ids     []core.NodeID // sorted
}

func (s searcher) poolKey(set []cand) poolKey {
	k := poolKey{victims: s.victims(set)}
	for _, c := range set {
		k.worst = max(k.worst, c.Priority)
		k.total += c.BudgetMiB
		k.ids = append(k.ids, c.ID)
	}
	slices.Sort(k.ids)
	return k
}

func poolLess(a, b poolKey) bool {
	if a.worst != b.worst {
		return a.worst < b.worst
	}
	if c := a.victims.compare(b.victims); c != 0 {
		return c < 0
	}
	if a.total != b.total {
		return a.total > b.total
	}
	return slices.Compare(a.ids, b.ids) < 0
}

// bestPool returns the best k-node pool of level that fits, or nil. It
// first rejects k when even the k largest budgets do not fit (fit is
// monotone in every budget, so then no k-pool fits), then enumerates
// k-subsets in descending budget order, pruning a branch when its chosen
// budgets plus the largest remaining ones cannot fit.
func (s searcher) bestPool(level []cand, k int) []cand {
	byBudget := slices.Clone(level)
	sort.Slice(byBudget, func(i, j int) bool {
		if byBudget[i].BudgetMiB != byBudget[j].BudgetMiB {
			return byBudget[i].BudgetMiB > byBudget[j].BudgetMiB
		}
		return byBudget[i].ID < byBudget[j].ID
	})
	top := make([]int, k)
	for i := range top {
		top[i] = byBudget[i].BudgetMiB
	}
	if !fits(s.req, top) {
		return nil
	}
	var best []cand
	var bestKey poolKey
	chosen := make([]cand, 0, k)
	budgets := make([]int, 0, k)
	var walk func(start int)
	walk = func(start int) {
		if len(chosen) == k {
			if !fits(s.req, budgets) {
				return
			}
			key := s.poolKey(chosen)
			if best == nil || poolLess(key, bestKey) {
				best, bestKey = slices.Clone(chosen), key
			}
			return
		}
		need := k - len(chosen)
		for i := start; i+need <= len(byBudget); i++ {
			// Bound: the best this branch can do is byBudget[i : i+need].
			bound := append(slices.Clone(budgets), budgetsOf(byBudget[i:i+need])...)
			if !fits(s.req, bound) {
				return // later i only have smaller budgets
			}
			chosen = append(chosen, byBudget[i])
			budgets = append(budgets, byBudget[i].BudgetMiB)
			walk(i + 1)
			chosen = chosen[:len(chosen)-1]
			budgets = budgets[:len(budgets)-1]
		}
	}
	walk(0)
	return best
}

func budgetsOf(cs []cand) []int {
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.BudgetMiB
	}
	return out
}
