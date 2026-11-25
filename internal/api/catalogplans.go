package api

import (
	"context"
	"encoding/json"
	"sort"
)

// catalogPlans is the profile entries (with their plans) a catalog build
// joins, and the status signature they were computed under. With fresh, or
// when the cached ones are out of date by the rules at catalogDriftGap,
// every profile's plan is computed again (one read on the scheduler loop per
// profile); otherwise the cached entries are reused. Call it under buildMu.
func (s *Server) catalogPlans(ctx context.Context, st any, sig string, fresh bool) ([]map[string]any, string) {
	c := &s.catalog
	pc := &c.plans
	cfg := s.conf()
	key, free := planKey(st), planFree(st)
	now := c.now()
	_, driftGap := c.gaps()
	if pc.entries != nil && pc.cfg == cfg {
		if pc.sig == sig { // computed under this very status: what a fresh build would compute
			return pc.entries, pc.sig
		}
		if !fresh && pc.key == key && !freeMoved(pc.free, free) && now.Sub(pc.at) < driftGap {
			return pc.entries, pc.sig
		}
	}
	c.planRounds.Add(1)
	entries := s.profileEntriesOf(ctx, cfg)
	*pc = planCache{entries: entries, cfg: cfg, key: key, free: free, sig: sig, at: now}
	return entries, sig
}

// freeMoved reports whether a node's budget for a new load moved by
// catalogFreeStep or more, or a node is new or gone.
func freeMoved(was, now map[string]int) bool {
	if len(was) != len(now) {
		return true
	}
	for id, f := range now {
		w, ok := was[id]
		if !ok {
			return true
		}
		if d := f - w; d >= catalogFreeStep || d <= -catalogFreeStep {
			return true
		}
	}
	return false
}

// planFree is each node's free_for_new_load_mib in a status document.
func planFree(st any) map[string]int {
	out := map[string]int{}
	m, _ := st.(map[string]any)
	for _, n := range jlist(jmap(m["view"])["nodes"]) {
		nm := jmap(n)
		out[jstr(nm["id"])] = int(jnum(nm["free_for_new_load_mib"]))
	}
	return out
}

// The fields of a status document a plan is decided on, apart from the
// memory figures (planFree) — anything else that moves with a heartbeat
// (memory averages, cache figures, agent fields, request counters, clocks)
// is left out on purpose and reaches the plans within catalogDriftGap.
var (
	planNodeFields = []string{"id", "online", "eligible", "excluded", "excluded_by", "auto", "unplaced", "total_mib",
		"priority", "holder", "claimed_by", "settling", "quarantined", "reserved_mib"}
	planInstanceFields = []string{"profile", "state", "nodes", "stale_profile", "drain", "pinned_nodes", "host"}
)

// planKey is what of a status document the plans depend on, besides the
// free memory: the configuration revision, the nodes' membership and
// standing, the instances, the learned needs, backoffs and quarantines.
func planKey(st any) string {
	m, ok := st.(map[string]any)
	if !ok {
		return ""
	}
	view := jmap(m["view"])
	pick := func(x map[string]any, keys []string) map[string]any {
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			if v, ok := x[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	var nodes []map[string]any
	for _, n := range jlist(view["nodes"]) {
		nm := pick(jmap(n), planNodeFields)
		if q := nm["quarantined"]; q != nil && q != "" { // when it ends is a clock
			nm["quarantined"] = true
		}
		nodes = append(nodes, nm)
	}
	sort.Slice(nodes, func(i, j int) bool { return jstr(nodes[i]["id"]) < jstr(nodes[j]["id"]) })
	var insts []map[string]any
	for _, i := range jlist(m["instances"]) {
		im := pick(jmap(i), planInstanceFields)
		var ids []string
		for _, n := range jlist(im["nodes"]) {
			ids = append(ids, jstr(jmap(n)["id"]))
		}
		im["nodes"] = ids
		insts = append(insts, im)
	}
	named := func(list any, keys ...string) []string { // who, not when
		var out []string
		for _, e := range jlist(list) {
			em := jmap(e)
			s := ""
			for _, k := range keys {
				s += jstr(em[k]) + "\x00"
			}
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}
	learned := map[string]any{}
	for id, v := range jmap(m["learned_needs"]) { // the figures, not their age
		lm := jmap(v)
		learned[id] = []any{lm["need_mib"], lm["measured_mib"], lm["source"], lm["peak_mib"]}
	}
	b, _ := json.Marshal(map[string]any{
		"config_revision": m["config_revision"], "configured": m["configured"], "shutting_down": m["shutting_down"],
		"connected": view["connected"], "stale": view["stale"], "nodes": nodes, "instances": insts,
		"learned_needs": learned, "backoffs": named(m["backoffs"], "profile"), "quarantines": named(m["quarantines"], "node", "profile"),
	})
	return string(b)
}
