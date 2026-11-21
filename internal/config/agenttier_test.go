package config

import "testing"

// Tiers for agent nodes match on what the agent reports (platform, device),
// so they do not depend on a hostname (agent contract 11, CR-5).
const agentTierCfg = `
tiers:
  - match: { platform: "linux-amd64-vulkan", device: "amd custom gpu*" }
    priority: 1
    headroom_mib: 1024
  - match: { platform: "linux-amd64-cuda" }
    priority: 2
  - match: { device: "Vulkan?", hostname: "bench-*" }
    priority: 3
  - match: { hostname: "ps5-*" }
    priority: 4
  - match: { local: true }
    priority: 5
profiles:
  p:
    model: p.gguf
    ctx: 1
`

func TestMatchTierNode(t *testing.T) {
	c := mustParse(t, agentTierCfg)
	tests := []struct {
		name string
		q    TierQuery
		prio int // 0: no tier
	}{
		{"platform and device, any hostname", TierQuery{Hostname: "x7f3a", Source: "agent", Platform: "linux-amd64-vulkan", Devices: []string{"AMD Custom GPU 0405", "Vulkan0"}}, 1},
		{"platform is exact", TierQuery{Hostname: "x", Platform: "linux-amd64-vulkan2", Devices: []string{"AMD Custom GPU 0405"}}, 0},
		{"every set matcher must hold", TierQuery{Hostname: "x", Platform: "linux-amd64-vulkan", Devices: []string{"Intel Arc"}}, 0},
		{"platform alone", TierQuery{Hostname: "y", Platform: "linux-amd64-cuda", Devices: []string{"RTX 5060"}}, 2},
		{"device with a hostname", TierQuery{Hostname: "bench-1", Platform: "linux-arm64-vulkan", Devices: []string{"Vulkan0"}}, 3},
		{"device needs its hostname too", TierQuery{Hostname: "other", Platform: "linux-arm64-vulkan", Devices: []string{"Vulkan0"}}, 0},
		{"no device reported", TierQuery{Hostname: "bench-1", Platform: "linux-arm64-vulkan"}, 0},
		{"hostname matcher still works for an agent node", TierQuery{Hostname: "ps5-9", Source: "agent", Platform: "linux-arm64-vulkan"}, 4},
		{"local", TierQuery{Hostname: "host-1", Local: true}, 5},
	}
	for _, tc := range tests {
		tier, ok := c.MatchTierNode(tc.q)
		if ok != (tc.prio != 0) || tier.Priority != tc.prio {
			t.Errorf("%s: got priority %d ok %v, want %d", tc.name, tier.Priority, ok, tc.prio)
		}
	}
	// A fleet-feed node reports no platform and no device: the agent tiers never match it.
	if tier, ok := c.MatchTier("laptop-1", "console", "fleet", false); ok {
		t.Errorf("fleet node matched an agent tier: %+v", tier)
	}
	if tier, ok := c.MatchTier("ps5-1", "", "", false); !ok || tier.Priority != 4 {
		t.Errorf("fleet node by hostname: %+v %v", tier, ok)
	}
}

func TestAgentTierMatchersSurviveTheDocument(t *testing.T) {
	c := mustParse(t, agentTierCfg)
	m := c.Tiers[0].Match
	if m.Platform != "linux-amd64-vulkan" || m.Device != "amd custom gpu*" {
		t.Fatalf("match %+v", m)
	}
}
