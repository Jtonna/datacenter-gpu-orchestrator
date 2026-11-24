package pool

import (
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// TierNoMatch is a zone's tier_reason when no configured tier matches it.
const TierNoMatch = "no tier matches"

// ZoneTier is the tier a zone is in, as the Tiers page shows it: its
// 1-based position in the configured list, its match fields as stored, and
// its settings.
type ZoneTier struct {
	Index        int            `json:"index"`
	Priority     int            `json:"priority"`
	Rule         map[string]any `json:"rule"`
	HeadroomMiB  int            `json:"headroom_mib"`
	SharedMemory bool           `json:"shared_memory"`
	LinkMBs      float64        `json:"link_mb_s"`
	Settle       string         `json:"settle"`
}

// TierFacts is the tier at 1-based index of cfg, nil for 0 or an index the
// configuration no longer has.
func TierFacts(cfg *config.Config, index int) *ZoneTier {
	if index < 1 || index > len(cfg.Tiers) {
		return nil
	}
	t := cfg.Tiers[index-1]
	return &ZoneTier{Index: index, Priority: t.Priority, Rule: TierRuleOf(t.Match), HeadroomMiB: t.HeadroomMiB,
		SharedMemory: t.SharedMemory, LinkMBs: t.LinkMBs, Settle: t.Settle.String()}
}

// TierRuleOf is a tier's match fields as stored: the keys it sets, nothing
// else ({} for an empty match).
func TierRuleOf(m config.Match) map[string]any {
	r := map[string]any{}
	for k, v := range map[string]string{"hostname": m.Hostname, "role": m.Role, "source": m.Source, "platform": m.Platform, "device": m.Device} {
		if v != "" {
			r[k] = v
		}
	}
	if m.Local != nil {
		r["local"] = *m.Local
	}
	return r
}

// agentTierFacts is the tier of an agent node from agentTier, the match the
// node view (nodeFromAgentLocked) and the no_tier_match reason use.
func agentTierFacts(cfg *config.Config, a *agent) (*ZoneTier, string) {
	t, ok := agentTier(cfg, a)
	if !ok {
		return nil, TierNoMatch
	}
	return TierFacts(cfg, t.Index), ""
}
