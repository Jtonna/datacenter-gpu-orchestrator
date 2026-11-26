package config

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// stage and display_name are read from a document: optional, for people
// and lists only.
func TestProfileStageAndDisplayNameAreRead(t *testing.T) {
	llama := "  p:\n    model: m.gguf\n    ctx: 4096\n"
	c := mustParse(t, prof(llama+"    stage: test\n    display_name: Gemma 27B fast\n"))
	if p := c.Profiles["p"]; p.Stage != StageTest || p.DisplayName != "Gemma 27B fast" {
		t.Fatalf("%q %q", p.Stage, p.DisplayName)
	}
	if p := mustParse(t, prof(llama)).Profiles["p"]; p.Stage != "" || p.DisplayName != "" {
		t.Fatalf("absent: %q %q", p.Stage, p.DisplayName)
	}
	keys := map[string]bool{}
	for _, f := range profileFields {
		keys[f.Key] = true
	}
	if !keys["stage"] || !keys["display_name"] {
		t.Error("stage or display_name is not in the profile schema")
	}
}

// Refused: an unknown stage; a display name too long, of two lines, that
// starts "TEST - ", that is an id or alias, or that another profile has;
// "@" in an id or an alias (reserved for run-scoped names).
func TestProfileStageValidation(t *testing.T) {
	l := func(id, extra string) string { return "  " + id + ":\n    model: m.gguf\n    ctx: 4096\n" + extra }
	cases := []struct{ yaml, want string }{
		{l("p", "    stage: staging\n"), "stage"},
		{l("p", "    display_name: \""+strings.Repeat("x", 81)+"\"\n"), "display_name"},
		{l("p", "    display_name: \"a\\nb\"\n"), "display_name"},
		{l("p", "    display_name: \"TEST - mine\"\n"), "display_name"},
		{l("p", "    display_name: \"test - mine\"\n"), "display_name"},
		{l("p", "    display_name: q\n") + l("q", ""), "display_name"},
		{l("p", "    display_name: old\n") + l("q", "    aliases: [old]\n"), "display_name"},
		{l("p", "    display_name: same\n") + l("q", "    display_name: same\n"), "display_name"},
		{l("p@x", ""), "@"},
		{l("p", "    aliases: [\"p@old\"]\n"), "@"},
	}
	for _, c := range cases {
		err := parseErr(prof(c.yaml), nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want an error naming %s", c.yaml, err, c.want)
		}
	}
	// accepted: the stages, a display name equal to its own id
	mustParse(t, prof(l("p", "    stage: production\n    display_name: p\n")+l("q", "    stage: test\n")))
}

// EffectiveStage: an explicit stage decides; absent, the rule (generated,
// by the field or else the id, then force_nodes) decides.
func TestEffectiveStage_Rule(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		p     core.Profile
		want  string
		isSet bool
	}{
		{core.Profile{Name: "gemma:32k"}, StageProduction, false},
		{core.Profile{Name: "bench-gemma-auto-8k"}, StageTest, false},
		{core.Profile{Name: "b-x"}, StageTest, false},
		{core.Profile{Name: "m-x"}, StageTest, false},
		{core.Profile{Name: "bench-x", Generated: &no}, StageProduction, false},
		{core.Profile{Name: "plain", Generated: &yes}, StageTest, false},
		{core.Profile{Name: "plain", ForceNodes: []string{"a"}}, StageTest, false},
		{core.Profile{Name: "plain", Stage: StageTest}, StageTest, true},
		{core.Profile{Name: "bench-x", Generated: &yes, ForceNodes: []string{"a"}, Stage: StageProduction}, StageProduction, true},
	}
	for _, c := range cases {
		got, set := EffectiveStage(&c.p)
		if got != c.want || set != c.isSet {
			t.Errorf("%+v: %s %v, want %s %v", c.p, got, set, c.want, c.isSet)
		}
	}
	if s := ShownName(&core.Profile{Name: "bench-x"}); s != "TEST - bench-x" {
		t.Error(s)
	}
	if s := ShownName(&core.Profile{Name: "g:32k", DisplayName: "Gemma"}); s != "Gemma" {
		t.Error(s)
	}
	if s := ShownName(&core.Profile{Name: "g", DisplayName: "Gemma", Stage: StageTest}); s != "TEST - Gemma" {
		t.Error(s)
	}
}

// The reviewer's named test: a standard-set profile (written with stage
// production, automatic, generated unset; plan section 6) is never TEST,
// whatever else it carries, and neither is any profile with stage
// production.
func TestEffectiveStage_StandardSetIsNeverTest(t *testing.T) {
	yes := true
	c := mustParse(t, prof("  \"gemma-4-27b:32k\":\n    model: g.gguf\n    ctx: 32768\n    stage: production\n    description: standard set\n"))
	p := c.Profiles["gemma-4-27b:32k"]
	if s, set := EffectiveStage(p); s != StageProduction || !set {
		t.Fatalf("standard set profile is %s (set %v)", s, set)
	}
	if strings.HasPrefix(ShownName(p), "TEST") {
		t.Fatal(ShownName(p))
	}
	// even if a tool later marks it generated, forces it, or names it like a sweep
	for _, mod := range []func(*core.Profile){
		func(q *core.Profile) { q.Generated = &yes },
		func(q *core.Profile) { q.ForceNodes = []string{"a", "b"} },
		func(q *core.Profile) { q.Name = "bench-gemma-auto-32k" },
	} {
		q := *p
		mod(&q)
		if s, _ := EffectiveStage(&q); s != StageProduction || strings.HasPrefix(ShownName(&q), "TEST") {
			t.Fatalf("%+v shows as %s %q", q, s, ShownName(&q))
		}
	}
}
