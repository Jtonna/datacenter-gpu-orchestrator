package api

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// The UI words the two memberships with the same table as the scheduler, so
// the two cannot drift apart: core/words.js lists config.PoolPhrases, in order.
func TestUIPoolPhrasesMatchTheScheduler(t *testing.T) {
	text := uiText(t, "core/words.js")
	got := regexp.MustCompile(`(?m)^\s*\['([^']+)', '([^']+)'\],$`).FindAllStringSubmatch(text, -1)
	if len(got) != len(config.PoolPhrases) {
		t.Fatalf("core/words.js has %d phrases, internal/config has %d", len(got), len(config.PoolPhrases))
	}
	for i, p := range config.PoolPhrases {
		if got[i][1] != p[0] || got[i][2] != p[1] {
			t.Errorf("phrase %d: UI %q -> %q, scheduler %q -> %q", i, got[i][1], got[i][2], p[0], p[1])
		}
	}
	// the reason text of a forced node outside the pool is read in both wordings
	if r := uiText(t, "core/reasons.js"); !strings.Contains(r, `is not in the (fleet view|pool)$/i`) || !strings.Contains(r, "poolText(") {
		t.Error("core/reasons.js does not read the pool wording of a forced node that is not in the view")
	}
}

// Every section of the settings schema has a card on the Settings screen (a
// section without one hides its settings), and with membership agents the
// recovery and fleet sections are marked as not used.
func TestUISettingsSectionsAndUnusedWithAgents(t *testing.T) {
	text := uiText(t, "views/settings.js")
	shown := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  \['([a-z]+)', '`).FindAllStringSubmatch(text, -1) {
		shown[m[1]] = true
	}
	for _, scope := range []string{config.ScopeSettings, config.ScopeRecovery, config.ScopeFleet} {
		for _, f := range config.SettingFields(scope) {
			if !shown[f.Section] {
				t.Errorf("views/settings.js has no section %q, so %s.%s is not shown", f.Section, scope, f.Key)
			}
		}
	}
	for _, want := range []string{"recovery: 'Not used in this mode:", "fleet: 'Not used in this mode:", "cfg.settings.membership === 'agents'",
		"badge('Not used in this mode'", "h('details'"} {
		if !strings.Contains(text, want) {
			t.Errorf("views/settings.js lacks %s", want)
		}
	}
}

// With agents the connection facts and the About screen say where the nodes
// come from, not that a fleet feed is fresh.
func TestUIAgentsModeHasNoFleetFeedFacts(t *testing.T) {
	if s := uiText(t, "core/shell.js"); !strings.Contains(s, "if (agentsMode(st)) return { dot: '', title: 'Live', note: 'Nodes come from agent registrations' };") {
		t.Error("core/shell.js: the connection note of agents mode is gone")
	}
	if s := uiText(t, "views/about.js"); !strings.Contains(s, "agentsMode(st) ? [h('dt', 'Nodes'), h('dd', 'From agent registrations") {
		t.Error("views/about.js: agents mode still shows the fleet feed row")
	}
	if s := uiText(t, "views/tiers.js"); !strings.Contains(s, "badge('Not used in this mode'") {
		t.Error("views/tiers.js: node overrides are not marked unused with agents")
	}
}
