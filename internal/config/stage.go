package config

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The TEST / PRODUCTION marking (profiles plan, section 5; slice B1: read
// only). A profile's stage is "test", "production" or not said; not said,
// the rule decides: generated (the field, else the id) → test, force_nodes
// → test, else production. An explicit stage always wins, so a profile
// written with stage production (the standard set) is never shown as test.

const (
	StageTest       = "test"
	StageProduction = "production"
)

// TestPrefix starts the shown name of a test profile.
const TestPrefix = "TEST - "

// MaxDisplayName is the longest display name, in characters.
const MaxDisplayName = 80

// GeneratedID is the naming rule for profiles a tool made: the benchmark
// matrix writes bench-<model>-<place>-<ctx>, the earlier sweeps b-* and m-*.
var GeneratedID = regexp.MustCompile(`^(bench|b|m)-`)

// IsGenerated reports whether a profile counts as made by a tool: its
// generated field when set, else its id.
func IsGenerated(id string, marker *bool) bool {
	if marker != nil {
		return *marker
	}
	return GeneratedID.MatchString(id)
}

// EffectiveStage is p's stage and whether the profile says it (false: the
// rule decided).
func EffectiveStage(p *core.Profile) (stage string, set bool) {
	switch p.Stage {
	case StageTest, StageProduction:
		return p.Stage, true
	}
	if IsGenerated(p.Name, p.Generated) || len(p.ForceNodes) > 0 {
		return StageTest, false
	}
	return StageProduction, false
}

// ShownName is the name lists show: the display name, else the id, with
// "TEST - " before it for a test profile.
func ShownName(p *core.Profile) string {
	name := p.DisplayName
	if name == "" {
		name = p.Name
	}
	if s, _ := EffectiveStage(p); s == StageTest {
		return TestPrefix + name
	}
	return name
}

// validateStage checks profile n's stage, display name and id (and its
// aliases: "@" is kept for run-scoped names).
func (c *Config) validateStage(n string, p *core.Profile, pf func(field, format string, a ...any)) {
	if strings.Contains(n, "@") {
		pf("id", "must not contain \"@\" (kept for run-scoped names)")
	}
	for _, a := range p.Aliases {
		if strings.Contains(a, "@") {
			pf("aliases", "%q must not contain \"@\" (kept for run-scoped names)", a)
		}
	}
	switch p.Stage {
	case "", StageTest, StageProduction:
	default:
		pf("stage", "unknown value %q (want test or production, or leave it empty)", p.Stage)
	}
	d := p.DisplayName
	if d == "" {
		return
	}
	switch {
	case utf8.RuneCountInString(d) > MaxDisplayName:
		pf("display_name", "is %d characters; at most %d", utf8.RuneCountInString(d), MaxDisplayName)
	case strings.ContainsFunc(d, unicode.IsControl):
		pf("display_name", "must be one line of text (no line breaks or control characters)")
	case strings.HasPrefix(strings.ToUpper(d), strings.ToUpper(TestPrefix)):
		pf("display_name", "must not start with %q: a test profile is shown with it already", TestPrefix)
	}
	for id, q := range c.Profiles {
		if id == n {
			continue
		}
		if id == d {
			pf("display_name", "%q is the id of profile %q", d, id)
		}
		for _, a := range q.Aliases {
			if a == d {
				pf("display_name", "%q is an alias of profile %q", d, id)
			}
		}
		if q.DisplayName == d {
			pf("display_name", "%q is also the display name of profile %q", d, id)
		}
	}
}
