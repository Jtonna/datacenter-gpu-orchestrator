package api

import (
	"context"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
)

// tierZone is one entry of GET /api/config/tiers tier_zones, in the order of
// tiers: the zones the scheduler's node view put in that tier now, and
// never_reached when a tier above it has an empty match (it takes every
// zone, so nothing reaches this one).
type tierZone struct {
	Index        int              `json:"index"`
	Zones        []tierZoneMember `json:"zones"`
	NeverReached bool             `json:"never_reached,omitempty"`
}

type tierZoneMember struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func emptyMatch(m config.MatchSpec) bool {
	return m.Hostname == "" && m.Role == "" && m.Source == "" && m.Local == nil && m.Platform == "" && m.Device == ""
}

// tierZones lists each tier's zones. With agents membership they come from
// the pool's zone facts (the tier each zone's node view was built with;
// active and lost zones, the local node included); otherwise from the
// scheduler's node view. Removed and left zones are in no tier.
func (s *Server) tierZones(ctx context.Context, tiers []config.TierSpec) []tierZone {
	out := make([]tierZone, len(tiers))
	blocked := false
	for i, t := range tiers {
		out[i] = tierZone{Index: i + 1, Zones: []tierZoneMember{}, NeverReached: blocked}
		if emptyMatch(t.Match) {
			blocked = true
		}
	}
	add := func(index int, id, name string) {
		if index < 1 || index > len(out) {
			return
		}
		if name == "" {
			name = id
		}
		out[index-1].Zones = append(out[index-1].Zones, tierZoneMember{ID: id, Name: name})
	}
	if p := s.opts.Pool; p != nil {
		for _, f := range p.Facts() {
			if f.Tier != nil && (f.State == pool.StateActive || f.State == pool.StateLost) && !f.HostAgent {
				add(f.Tier.Index, f.ID, f.Name)
			}
		}
		if l, ok := p.Local(); ok && l.Tier != nil {
			add(l.Tier.Index, l.ID, l.Name)
		}
	} else if st, err := s.sched.Status(ctx); err == nil {
		for _, n := range st.View.Nodes {
			add(n.TierIndex, string(n.ID), n.Hostname)
		}
	}
	for i := range out {
		sort.Slice(out[i].Zones, func(a, b int) bool {
			x, y := out[i].Zones[a], out[i].Zones[b]
			if x.Name != y.Name {
				return x.Name < y.Name
			}
			return x.ID < y.ID
		})
	}
	return out
}
