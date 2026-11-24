package api

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// One set of zone counts for every reader: the Zones row of /api/health,
// the counts of GET /api/nodes (the Zones page heading) and each group's
// counts all come from zoneCountsOf, one availability.Classify of the
// status and the pool's facts. Each zone carries the state it was counted
// in as zone_state.

// ZoneStateRemoved is the zone_state of a removed zone (not counted).
const ZoneStateRemoved = "removed"

// zoneCountsOf classifies every zone once: the counts (removed zones left
// out), each zone's state by id, and the names of the zones that are down
// (lost or set aside), in the order Classify gives them.
func zoneCountsOf(st *scheduler.Status, facts []pool.NodeFacts) (ZoneCounts, map[string]string, []string) {
	var c ZoneCounts
	states := map[string]string{}
	down := []string{}
	removed := map[string]bool{}
	for _, f := range facts {
		if f.State == pool.StateRemoved {
			removed[f.ID] = true
		}
	}
	if st == nil {
		st = &scheduler.Status{}
	}
	for _, smp := range availability.Classify(st, facts) {
		if smp.Removed || removed[smp.Node] {
			states[smp.Node] = ZoneStateRemoved
			continue
		}
		states[smp.Node] = string(smp.State)
		c.add(smp.State)
		if smp.State == availability.Lost || smp.State == availability.SetAside {
			down = append(down, smp.Name)
		}
	}
	return c, states, down
}

// setZoneStates writes zone_state and attention_rank on each fact.
func setZoneStates(st *scheduler.Status, facts []pool.NodeFacts, states map[string]string) {
	hung := hungNodes(st)
	for i := range facts {
		f := &facts[i]
		f.ZoneState = states[f.ID]
		f.AttentionRank = attentionRank(f.ZoneState, f.Unplaced, f.Excluded, hung[f.ID])
		f.Chip = chipOf(f.Stats)
	}
}

// chipOf names the GPUs a stats object reports: the distinct gpus[].name
// in order, joined with " + "; "" when none has a name.
func chipOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s struct {
		GPUs []struct {
			Name string `json:"name"`
		} `json:"gpus"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	var names []string
	for _, g := range s.GPUs {
		if n := strings.TrimSpace(g.Name); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return strings.Join(names, " + ")
}

// ChipMixed is a group's chip when members report different names.
const ChipMixed = "mixed"

// groupChip is a group's common chip: the one every member that reports a
// name reports, when all do; ChipMixed when two names differ; "" when a
// member reports none (a console today) and the others agree, or when none
// reports one: Potluck does not know that member's chip.
func groupChip(chips []string) string {
	common, some, none := "", false, false
	for _, c := range chips {
		switch {
		case c == "":
			none = true
		case !some:
			common, some = c, true
		case c != common:
			return ChipMixed
		}
	}
	switch {
	case !some:
		return ""
	case none:
		return ""
	}
	return common
}

// hungNodes are the nodes whose worker stopped answering (worker health).
func hungNodes(st *scheduler.Status) map[string]bool {
	out := map[string]bool{}
	if st == nil {
		return out
	}
	for _, n := range st.View.Nodes {
		if n.Health != nil && n.Health.State == "hung" {
			out[string(n.ID)] = true
		}
	}
	return out
}

// Attention ranks, lowest first: the order of "needs attention first" on
// the Zones page, in GET /api/nodes?sort=attention and in MCP list_zones.
const (
	RankDown     = 0 // lost, or its worker stopped answering
	RankTrouble  = 1 // set aside, or kept out of placement for a fault (no tier, unreachable, ...)
	RankLoading  = 2
	RankInUse    = 3
	RankPaused   = 4 // switched off by a pause
	RankFree     = 5
	RankExcluded = 6 // switched off in the configuration (exclude_nodes)
	RankRemoved  = 7
)

// attentionRank places a zone in the attention order. unplaced is the
// zone's unplaced reason; paused, excluded and host-machine are not faults.
func attentionRank(state, unplaced string, excluded, hung bool) int {
	switch {
	case state == ZoneStateRemoved:
		return RankRemoved
	case state == string(availability.Lost) || hung:
		return RankDown
	case state == string(availability.SetAside):
		return RankTrouble
	case state == string(availability.Off) && excluded:
		return RankExcluded
	case state == string(availability.Off):
		return RankPaused
	case unplaced != "" && unplaced != scheduler.UnplacedPaused && unplaced != pool.UnplacedExcluded && unplaced != scheduler.UnplacedHostMachine:
		return RankTrouble
	case state == string(availability.Loading):
		return RankLoading
	case state == string(availability.InUse):
		return RankInUse
	}
	return RankFree
}

// rankedZone is one entry of the node feed with what it is sorted by.
type rankedZone struct {
	rank     int
	name, id string
	v        any
}

// sortByName orders the feed by name, then id.
func sortByName(zs []rankedZone) {
	sort.SliceStable(zs, func(i, j int) bool {
		if zs[i].name != zs[j].name {
			return zs[i].name < zs[j].name
		}
		return zs[i].id < zs[j].id
	})
}

// sortByAttention orders the feed: attention rank, then name, then id.
func sortByAttention(zs []rankedZone) {
	sort.SliceStable(zs, func(i, j int) bool {
		a, b := zs[i], zs[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.id < b.id
	})
}
