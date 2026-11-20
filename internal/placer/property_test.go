package placer

import (
	"fmt"
	"math/bits"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/estimate"
)

// oracleNode is a usable node with its owner (-1 free).
type oracleNode struct {
	c     Candidate
	owner int
}

func oracleFits(req Request, set []oracleNode) bool {
	var b []int
	for _, n := range set {
		b = append(b, n.c.BudgetMiB)
	}
	if req.Need.Measured {
		return estimate.FitsMeasured(b, req.Need.MiB)
	}
	return estimate.FitsEstimated(b, req.Need.MiB, req.DeviceOverheadMiB)
}

// oracleKey flattens the documented total order into a comparable slice.
func oracleKey(set []oracleNode, ev []Evictable) []int64 {
	owners := map[int]bool{}
	var nBusy, load, idleSum int64
	worst, total := 0, 0
	var ids []string
	for _, n := range set {
		worst = max(worst, n.c.Priority)
		total += n.c.BudgetMiB
		ids = append(ids, string(n.c.ID))
		if n.owner >= 0 && !owners[n.owner] {
			owners[n.owner] = true
			e := ev[n.owner]
			if e.Busy {
				nBusy++
			}
			load += int64(e.LoadDuration)
			idleSum += int64(e.IdleFor)
		}
	}
	budgetTerm := int64(-total) // pools: largest total first
	if len(set) == 1 {
		budgetTerm = int64(total) // single: smallest budget first
	}
	return []int64{int64(worst), int64(len(owners)), nBusy, load, -idleSum, budgetTerm}
}

func oracleLess(a, b []oracleNode, ev []Evictable) bool {
	ka, kb := oracleKey(a, ev), oracleKey(b, ev)
	if c := slices.Compare(ka, kb); c != 0 {
		return c < 0
	}
	ia, ib := idsOf(a), idsOf(b)
	return slices.Compare(ia, ib) < 0
}

func idsOf(set []oracleNode) []string {
	var out []string
	for _, n := range set {
		out = append(out, string(n.c.ID))
	}
	sort.Strings(out)
	return out
}

// oracle is a literal brute-force reading of 7.3 plus the Plan order:
// every subset of every level is enumerated.
func oracle(req Request, free []Candidate, ev []Evictable) (ids []string, victims []string, ok bool) {
	var usable []oracleNode
	add := func(c Candidate, owner int) {
		if len(req.AllowTiers) > 0 && !slices.Contains(req.AllowTiers, c.Priority) {
			return
		}
		if c.BudgetMiB < req.MinShareMiB {
			return
		}
		usable = append(usable, oracleNode{c, owner})
	}
	for _, c := range free {
		add(c, -1)
	}
	for i, e := range ev {
		for _, c := range e.Nodes {
			add(c, i)
		}
	}
	levels := map[int]bool{}
	for _, n := range usable {
		levels[n.c.Priority] = true
	}
	var ls []int
	for l := range levels {
		ls = append(ls, l)
	}
	sort.Ints(ls)
	for _, t := range ls {
		var s []oracleNode
		for _, n := range usable {
			if n.c.Priority <= t {
				s = append(s, n)
			}
		}
		var best []oracleNode
		for size := 1; size <= req.MaxPool && best == nil; size++ {
			for mask := 1; mask < 1<<len(s); mask++ {
				if bits.OnesCount(uint(mask)) != size {
					continue
				}
				var set []oracleNode
				for i := range s {
					if mask&(1<<i) != 0 {
						set = append(set, s[i])
					}
				}
				if !oracleFits(req, set) {
					continue
				}
				if best == nil || oracleLess(set, best, ev) {
					best = set
				}
			}
		}
		if best != nil {
			// device order: tier ↑, budget ↓, ID ↑
			sort.Slice(best, func(i, j int) bool {
				a, b := best[i].c, best[j].c
				if a.Priority != b.Priority {
					return a.Priority < b.Priority
				}
				if a.BudgetMiB != b.BudgetMiB {
					return a.BudgetMiB > b.BudgetMiB
				}
				return a.ID < b.ID
			})
			vs := map[string]bool{}
			for _, n := range best {
				ids = append(ids, string(n.c.ID))
				if n.owner >= 0 {
					vs[ev[n.owner].Instance] = true
				}
			}
			for v := range vs {
				victims = append(victims, v)
			}
			sort.Strings(victims)
			return ids, victims, true
		}
	}
	return nil, nil, false
}

type genCase struct {
	req  Request
	free []Candidate
	ev   []Evictable
}

func genRandom(r *rand.Rand, withEvictables bool) genCase {
	budgets := []int{500, 1024, 2000, 3000, 4000, 5000, 7629, 7895, 13000}
	n := r.Intn(13)
	var nodes []Candidate
	for i := range n {
		b := budgets[r.Intn(len(budgets))]
		if r.Intn(2) == 0 {
			b = r.Intn(16000)
		}
		nodes = append(nodes, Candidate{ID: core.NodeID(fmt.Sprintf("n%02d", i)), Priority: 1 + r.Intn(3), BudgetMiB: b, Local: i == 0})
	}
	req := Request{MaxPool: 1 + r.Intn(5), DeviceOverheadMiB: 600}
	if r.Intn(2) == 0 {
		req.MinShareMiB = 1024
	}
	if r.Intn(2) == 0 {
		req.Need = Measured(r.Intn(40000))
	} else {
		req.Need = Estimated(r.Intn(30000))
	}
	if r.Intn(3) == 0 {
		for p := 1; p <= 3; p++ {
			if r.Intn(2) == 0 {
				req.AllowTiers = append(req.AllowTiers, p)
			}
		}
	}
	g := genCase{req: req}
	if !withEvictables {
		g.free = nodes
		return g
	}
	durs := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
	nInst := 1 + r.Intn(4)
	for i := range nInst {
		e := Evictable{Instance: fmt.Sprintf("i%d", i), LoadDuration: durs[r.Intn(3)]}
		if r.Intn(2) == 0 {
			e.Busy = true
		} else {
			e.IdleFor = durs[r.Intn(3)]
		}
		g.ev = append(g.ev, e)
	}
	for _, c := range nodes {
		if k := r.Intn(nInst + 1); k == nInst {
			g.free = append(g.free, c)
		} else {
			g.ev[k].Nodes = append(g.ev[k].Nodes, c)
		}
	}
	var ev []Evictable
	for _, e := range g.ev { // drop instances with no nodes
		if len(e.Nodes) > 0 {
			ev = append(ev, e)
		}
	}
	g.ev = ev
	return g
}

func shuffled(r *rand.Rand, g genCase) genCase {
	out := genCase{req: g.req, free: slices.Clone(g.free)}
	r.Shuffle(len(out.free), func(i, j int) { out.free[i], out.free[j] = out.free[j], out.free[i] })
	for _, e := range g.ev {
		e.Nodes = slices.Clone(e.Nodes)
		r.Shuffle(len(e.Nodes), func(i, j int) { e.Nodes[i], e.Nodes[j] = e.Nodes[j], e.Nodes[i] })
		out.ev = append(out.ev, e)
	}
	r.Shuffle(len(out.ev), func(i, j int) { out.ev[i], out.ev[j] = out.ev[j], out.ev[i] })
	return out
}

// checkInvariants checks a returned placement against the rules directly.
func checkInvariants(t *testing.T, g genCase, p core.Placement) {
	t.Helper()
	byID := map[core.NodeID]Candidate{}
	for _, c := range g.free {
		byID[c.ID] = c
	}
	for _, e := range g.ev {
		for _, c := range e.Nodes {
			byID[c.ID] = c
		}
	}
	usable := func(c Candidate) bool {
		return (len(g.req.AllowTiers) == 0 || slices.Contains(g.req.AllowTiers, c.Priority)) && c.BudgetMiB >= g.req.MinShareMiB
	}
	var set []oracleNode
	seen := map[core.NodeID]bool{}
	level := 0
	for _, pn := range p.Nodes {
		c, ok := byID[pn.ID]
		if !ok || seen[pn.ID] {
			t.Fatalf("unknown or duplicate node %s", pn.ID)
		}
		seen[pn.ID] = true
		if !usable(c) {
			t.Fatalf("node %s violates allow_tiers/min_share", pn.ID)
		}
		if pn.BudgetMiB != c.BudgetMiB {
			t.Fatalf("node %s budget %d, want %d", pn.ID, pn.BudgetMiB, c.BudgetMiB)
		}
		set = append(set, oracleNode{c, -1})
		level = max(level, c.Priority)
	}
	if len(set) == 0 || len(set) > g.req.MaxPool {
		t.Fatalf("pool size %d outside 1..%d", len(set), g.req.MaxPool)
	}
	if !oracleFits(g.req, set) {
		t.Fatalf("placement does not fit")
	}
	var all []oracleNode
	for _, c := range byID {
		if usable(c) {
			all = append(all, oracleNode{c, -1})
		}
	}
	// No lower level fits at all; at this level no single (if a pool was
	// returned) and no strictly smaller pool fits. Checked by the top-k
	// sums, which bound every subset of that size.
	topFits := func(maxPrio, k int) bool {
		var b []int
		for _, n := range all {
			if n.c.Priority <= maxPrio {
				b = append(b, n.c.BudgetMiB)
			}
		}
		sort.Sort(sort.Reverse(sort.IntSlice(b)))
		if k < 0 { // as many as max_pool allows
			k = min(g.req.MaxPool, len(b))
		}
		if k == 0 || len(b) < k {
			return false
		}
		var top []oracleNode
		for _, x := range b[:k] {
			top = append(top, oracleNode{Candidate{BudgetMiB: x}, -1})
		}
		return oracleFits(g.req, top)
	}
	singleFits := func(maxPrio int) bool {
		for _, n := range all {
			if n.c.Priority <= maxPrio && oracleFits(g.req, []oracleNode{n}) {
				return true
			}
		}
		return false
	}
	for lower := 1; lower < level; lower++ {
		if topFits(lower, -1) || singleFits(lower) {
			t.Fatalf("a placement fits at lower level %d but result is at level %d", lower, level)
		}
	}
	if len(set) > 1 && singleFits(level) {
		t.Fatalf("a single node fits at level %d but a pool of %d was returned", level, len(set))
	}
	for k := 2; k < len(set); k++ {
		if topFits(level, k) {
			t.Fatalf("a %d-node pool fits at level %d but %d nodes were returned", k, level, len(set))
		}
	}
}

func TestPlaceProperties(t *testing.T) {
	r := rand.New(rand.NewSource(20261117))
	placed := 0
	for i := range 600 {
		g := genRandom(r, false)
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p, err := Place(g.req, g.free)
			wantIDs, _, wantOK := oracle(g.req, g.free, nil)
			if (err == nil) != wantOK {
				t.Fatalf("Place err=%v, oracle ok=%v (req %+v free %+v)", err, wantOK, g.req, g.free)
			}
			if err != nil {
				if np := reasonOf(t, err); np.Reason != NoFit {
					t.Fatalf("reason %v", np.Reason)
				}
				return
			}
			placed++
			if got := placedIDs(p); !reflect.DeepEqual(got, wantIDs) {
				t.Fatalf("Place %v, oracle %v (req %+v free %+v)", got, wantIDs, g.req, g.free)
			}
			checkInvariants(t, g, p)
			s := shuffled(r, g)
			p2, err2 := Place(s.req, s.free)
			if err2 != nil || !reflect.DeepEqual(p, p2) {
				t.Fatalf("shuffled input changed the result: %v vs %v (%v)", placedIDs(p), placedIDs(p2), err2)
			}
		})
	}
	if placed < 150 {
		t.Fatalf("only %d of 600 cases placed; generator too harsh", placed)
	}
}

func TestPlanProperties(t *testing.T) {
	r := rand.New(rand.NewSource(17112026))
	planned := 0
	for i := range 600 {
		g := genRandom(r, true)
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			res, err := Plan(g.req, g.free, g.ev)
			wantIDs, wantVictims, wantOK := oracle(g.req, g.free, g.ev)
			if (err == nil) != wantOK {
				t.Fatalf("Plan err=%v, oracle ok=%v", err, wantOK)
			}
			if err != nil {
				return
			}
			planned++
			if got := placedIDs(res.Placement); !reflect.DeepEqual(got, wantIDs) || !reflect.DeepEqual(res.Victims, wantVictims) {
				t.Fatalf("Plan %v victims %v, oracle %v victims %v (req %+v free %+v ev %+v)",
					got, res.Victims, wantIDs, wantVictims, g.req, g.free, g.ev)
			}
			checkInvariants(t, g, res.Placement)
			s := shuffled(r, g)
			res2, err2 := Plan(s.req, s.free, s.ev)
			if err2 != nil || !reflect.DeepEqual(res, res2) {
				t.Fatalf("shuffled input changed the result")
			}
		})
	}
	if planned < 150 {
		t.Fatalf("only %d of 600 cases planned; generator too harsh", planned)
	}
}

// Best worst-tier and best single tier cannot decide through Place or
// Plan: the level walk already returns at the first level where anything
// fits, so every fitting candidate at that level has the same worst tier
// (a pool) or tier (a single). The comparators still implement them.
func TestComparatorsTierFirst(t *testing.T) {
	s := searcher{}
	better := s.poolKey([]cand{{nd("a", 1, 1000), -1}, {nd("b", 1, 1000), -1}})
	worse := s.poolKey([]cand{{nd("c", 1, 9000), -1}, {nd("d", 2, 9000), -1}})
	if !poolLess(better, worse) || poolLess(worse, better) {
		t.Fatal("pool with the better worst tier must win despite a smaller total")
	}
	a, b := cand{nd("a", 1, 9000), -1}, cand{nd("b", 2, 1000), -1}
	if !singleLess(a, victimKey{}, b, victimKey{}) || singleLess(b, victimKey{}, a, victimKey{}) {
		t.Fatal("single in the better tier must win despite a larger budget")
	}
}
