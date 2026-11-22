package config

import (
	"fmt"
	"sort"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// ExclusionBlock is a profile that forces a node exclude_nodes names: the
// profile is kept and is refused at run time until the exclusion is lifted.
type ExclusionBlock struct {
	Profile string `json:"profile"`
	Node    string `json:"node"`  // the force_nodes name
	Entry   string `json:"entry"` // the matching exclude_nodes entry
}

// Warning is the block in words, as configuration writes return it.
func (b ExclusionBlock) Warning() string {
	return fmt.Sprintf("profile %q: force_nodes: node %q is excluded by exclude_nodes entry %q; the profile is kept and is refused until the exclusion is lifted",
		b.Profile, b.Node, b.Entry)
}

// ExclusionBlocks lists the profiles that force an excluded node, one entry
// per profile (its first such node), sorted by profile. Only what the
// configuration alone can tell is matched: each force_nodes name against the
// entries as a node id and as a hostname. A hostname glob against a node
// named by id is found by the scheduler at request time.
func (c *Config) ExclusionBlocks() []ExclusionBlock {
	var out []ExclusionBlock
	for name := range c.Profiles {
		if b, ok := c.BlockedByExclusion(name); ok {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// BlockedByExclusion says whether the profile forces an excluded node (see
// ExclusionBlocks).
func (c *Config) BlockedByExclusion(profile string) (ExclusionBlock, bool) {
	p := c.Profiles[profile]
	if p == nil {
		return ExclusionBlock{}, false
	}
	for _, n := range p.ForceNodes {
		if e, ok := c.ExcludedBy(core.NodeID(n), n); ok {
			return ExclusionBlock{Profile: profile, Node: n, Entry: e}, true
		}
	}
	return ExclusionBlock{}, false
}
