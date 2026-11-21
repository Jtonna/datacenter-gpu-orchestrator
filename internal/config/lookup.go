package config

import (
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Resolve finds a profile by exact, case-sensitive name or alias. isAlias
// is true when name was an alias; the returned profile is then the real one.
func (c *Config) Resolve(name string) (profile *core.Profile, isAlias bool, ok bool) {
	if p, found := c.Profiles[name]; found {
		return p, false, true
	}
	for _, p := range c.Profiles {
		for _, a := range p.Aliases {
			if a == name {
				return p, true, true
			}
		}
	}
	return nil, false, false
}

// MatchTier returns the settings of the first tier (in file order) whose
// match clause holds for the node; every key set in one clause must hold.
// hostname is a case-insensitive glob; role and source are exact. A node
// matching no tier is never used, so ok is false then.
//
// A tier that sets platform or device never matches here: those are matched
// on what an agent reports (MatchTierNode).
func (c *Config) MatchTier(hostname, role, source string, local bool) (core.Tier, bool) {
	return c.MatchTierNode(TierQuery{Hostname: hostname, Role: role, Source: source, Local: local})
}

// TierQuery is what a node is matched to a tier on. Platform and Devices are
// what an agent node reports (agent contract 11); they are empty for a node
// from the fleet feed and for the local node.
type TierQuery struct {
	Hostname string
	Role     string
	Source   string
	Local    bool
	Platform string   // the platform given at registration, for example linux-amd64-vulkan
	Devices  []string // the device names the node reports
}

// MatchTierNode is MatchTier with the agent matchers: platform is exact, and
// device is a case-insensitive glob (the hostname syntax) that one of the
// node's device names must match. Every key set in a clause must hold, so a
// tier that sets only platform and device does not depend on the hostname.
func (c *Config) MatchTierNode(q TierQuery) (core.Tier, bool) {
	for _, t := range c.Tiers {
		m := t.Match
		if m.Hostname != "" && !globFold(m.Hostname, q.Hostname) {
			continue
		}
		if m.Role != "" && m.Role != q.Role {
			continue
		}
		if m.Source != "" && m.Source != q.Source {
			continue
		}
		if m.Local != nil && *m.Local != q.Local {
			continue
		}
		if m.Platform != "" && m.Platform != q.Platform {
			continue
		}
		if m.Device != "" && !slices.ContainsFunc(q.Devices, func(d string) bool { return globFold(m.Device, d) }) {
			continue
		}
		return t.Tier, true
	}
	return core.Tier{}, false
}

// globFold matches name against a glob, ignoring case; a bad glob matches nothing.
func globFold(glob, name string) bool {
	ok, err := path.Match(strings.ToLower(glob), strings.ToLower(name))
	return err == nil && ok
}

// EffectiveTTL is the profile's idle-unload time; 0 means never unload.
func (c *Config) EffectiveTTL(p *core.Profile) time.Duration {
	if p.TTL != nil {
		return *p.TTL
	}
	return c.Defaults.TTL
}

// EffectiveStallTimeout is the profile's stream stall timeout; 0 means disabled.
func (c *Config) EffectiveStallTimeout(p *core.Profile) time.Duration {
	if p.StallTimeout != nil {
		return *p.StallTimeout
	}
	return c.Defaults.StallTimeout
}

// EffectiveLoadStallTimeout is how long a measured upload may acknowledge no
// new bytes before the load fails: the profile's own, else the default; 0
// means disabled.
func (c *Config) EffectiveLoadStallTimeout(p *core.Profile) time.Duration {
	if p.LoadStallTimeout != nil {
		return *p.LoadStallTimeout
	}
	return c.Defaults.LoadStallTimeout
}

// EffectiveLoadTimeout returns the configured load timeout: the profile's
// own, else command_load_timeout for kind: command. ok is false for a llama
// profile without one; the caller then computes the design 7.6 formula.
func (c *Config) EffectiveLoadTimeout(p *core.Profile) (d time.Duration, ok bool) {
	if p.LoadTimeout > 0 {
		return p.LoadTimeout, true
	}
	if p.Kind == core.KindCommand {
		return c.Defaults.CommandLoadTimeout, true
	}
	return 0, false
}

// ProfilesWithoutNeedsMiB lists kind: llama profiles that rely on the
// memory estimate, sorted by name. Profiles with force_nodes are left out:
// a forced placement skips the fit check and never uses the estimate (7.3). It is the hook for the design-section-6
// sliding-window warning: the caller reads each model's GGUF metadata and
// warns when it declares a sliding window. Config cannot do that itself.
func (c *Config) ProfilesWithoutNeedsMiB() []*core.Profile {
	var out []*core.Profile
	for _, p := range c.Profiles {
		if p.Kind == core.KindLlama && p.NeedsMiB == 0 && len(p.ForceNodes) == 0 {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Excluded reports whether the node is kept out of the pool by exclude_nodes.
// An entry matches when it equals the node id (case-insensitive) or matches
// the hostname as a glob (case-insensitive, the same matching as a tier's
// hostname). A hostname of "" matches no glob.
func (c *Config) Excluded(id core.NodeID, hostname string) bool {
	_, ok := c.ExcludedBy(id, hostname)
	return ok
}

// ExcludedBy is Excluded that also returns the matching entry.
func (c *Config) ExcludedBy(id core.NodeID, hostname string) (string, bool) {
	return matchNodeList(c.ExcludeNodes, id, hostname)
}

// NoAuto reports whether automatic placement must never pick the node
// (no_auto_nodes, CR-9 review): the matching of Excluded. A forced profile may
// still use the node; an excluded node stays excluded.
func (c *Config) NoAuto(id core.NodeID, hostname string) bool {
	_, ok := matchNodeList(c.NoAutoNodes, id, hostname)
	return ok
}

// matchNodeList is the matching of exclude_nodes and no_auto_nodes: an entry
// equals the node id (case-insensitive) or matches the hostname as a glob
// (case-insensitive). A hostname of "" matches no glob.
func matchNodeList(list []string, id core.NodeID, hostname string) (string, bool) {
	for _, e := range list {
		if strings.EqualFold(e, string(id)) {
			return e, true
		}
		if hostname != "" {
			if ok, err := path.Match(strings.ToLower(e), strings.ToLower(hostname)); err == nil && ok {
				return e, true
			}
		}
	}
	return "", false
}
