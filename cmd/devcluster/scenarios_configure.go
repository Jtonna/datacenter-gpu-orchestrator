package main

import "time"

// Scenarios for the Configure screens (Profiles, Tiers and nodes, Settings,
// Audit). They only script the status; the configuration, the audit log and the
// benchmark references come from the config mock and can be changed over REST
// (a conflict is made by a second client writing between a page's load and
// save; a long extra_args list by a PUT of a profile).

func init() {
	registerScenario(scenario{Name: "cfg-nodes", Description: "Configure screens: console-2 excluded, console-3 offline, the host machine matches no tier, one PS5 eligible.", Status: scCfgNodes})
	registerScenario(scenario{Name: "cfg-stale", Description: "Configure screens: qwen3.8-27b and gemma-4-12b ready, both running an older profile definition.", Status: scCfgStale})
}

func scCfgNodes(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.nodes[consoleB.Host].Excluded, b.nodes[consoleB.Host].Eligible = true, false
	c := b.nodes[consoleC.Host]
	c.Online, c.MissingSince = false, now.Add(-3*time.Minute)
	o := b.nodes[hostNode.Host]
	o.Priority, o.Eligible = 0, false
	return b
}

func scCfgStale(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.8-27b", ps3, instOpts{readyAgo: 50 * time.Minute, served: 40, lastTPS: 9.9, lastAgo: 2 * time.Minute})
	b.ready("gemma-4-12b", []string{hostNode.Host}, instOpts{readyAgo: 12 * time.Minute, served: 9, lastTPS: 30, lastAgo: time.Minute})
	b.staleProfile = map[string]bool{"qwen3.8-27b": true, "gemma-4-12b": true}
	return b
}
