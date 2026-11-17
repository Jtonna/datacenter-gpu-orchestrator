package config

import (
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
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
func (c *Config) MatchTier(hostname, role, source string, local bool) (core.Tier, bool) {
	for _, t := range c.Tiers {
		m := t.Match
		if m.Hostname != "" {
			if ok, err := path.Match(strings.ToLower(m.Hostname), strings.ToLower(hostname)); err != nil || !ok {
				continue
			}
		}
		if m.Role != "" && m.Role != role {
			continue
		}
		if m.Source != "" && m.Source != source {
			continue
		}
		if m.Local != nil && *m.Local != local {
			continue
		}
		return t.Tier, true
	}
	return core.Tier{}, false
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
// memory estimate, sorted by name. It is the hook for the design-section-6
// sliding-window warning: the caller reads each model's GGUF metadata and
// warns when it declares a sliding window. Config cannot do that itself.
func (c *Config) ProfilesWithoutNeedsMiB() []*core.Profile {
	var out []*core.Profile
	for _, p := range c.Profiles {
		if p.Kind == core.KindLlama && p.NeedsMiB == 0 {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
