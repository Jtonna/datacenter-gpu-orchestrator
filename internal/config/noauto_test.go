package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// CR-9 review, C: no_auto_nodes has the syntax and matching of exclude_nodes.
func TestNoAutoNodes(t *testing.T) {
	c := mustParse(t, "no_auto_nodes: [\"02:00:00:00:00:AB\", \"laptop-*\"]\ntiers:\n  - match: { local: true }\n    priority: 1\n")
	for _, tt := range []struct {
		id, host string
		want     bool
	}{
		{"02:00:00:00:00:ab", "ps5-zzz", true},
		{"aa:bb", "LAPTOP-2", true},
		{"aa:bb", "ps5-a", false},
	} {
		if got := c.NoAuto(core.NodeID(tt.id), tt.host); got != tt.want {
			t.Errorf("NoAuto(%q,%q) = %v, want %v", tt.id, tt.host, got, tt.want)
		}
	}
	if (&Config{}).NoAuto("x", "y") {
		t.Error("empty list matches nothing")
	}
}

// CR-9 review, C: a forced profile may name a no_auto node. It may also name
// an excluded one (0.1.7): the configuration is valid, excluding wins at run
// time, and ExclusionBlocks names the profile.
func TestValidateNoAutoNodes(t *testing.T) {
	llama := "  p:\n    model: m.gguf\n    ctx: 4096\n"
	forced := prof(llama + "    force_nodes: [laptop-1]\n")
	if err := parseErr(forced+"no_auto_nodes: [\"laptop-*\"]\n", nil); err != nil {
		t.Fatalf("force_nodes on a no_auto node refused: %v", err)
	}
	if err := parseErr(forced+"no_auto_nodes: [\"laptop-*\"]\nexclude_nodes: [laptop-1]\n", nil); err != nil {
		t.Fatalf("force_nodes on an excluded node refused: %v", err)
	}
	for _, tt := range []struct{ name, yaml, want string }{
		{"empty entry", baseHeader + "no_auto_nodes: [\"\"]\n", "empty"},
		{"bad glob", baseHeader + "no_auto_nodes: [\"ps5-[\"]\n", "invalid glob"},
	} {
		err := parseErr(tt.yaml, nil)
		if err == nil || !strings.Contains(err.Error(), "no_auto_nodes") || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

// CR-9 review, C: no_auto_nodes is part of the stored document and of a YAML
// import, and survives the round trip.
func TestNoAutoNodesInTheDocument(t *testing.T) {
	imp, err := ParseYAMLImport([]byte("no_auto_nodes: [laptop-*]\nprofiles: {}\n"), Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Doc.NoAutoNodes) != 1 || imp.Doc.NoAutoNodes[0] != "laptop-*" {
		t.Fatalf("import: %+v", imp.Doc.NoAutoNodes)
	}
	b, _ := json.Marshal(imp.Doc)
	if !strings.Contains(string(b), `"no_auto_nodes":["laptop-*"]`) {
		t.Fatalf("document JSON %s", b)
	}
	doc, err := ParseDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Build(doc, Bootstrap{ModelsDir: "/m"}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoAuto("x", "laptop-3") {
		t.Fatalf("built config: %v", c.NoAutoNodes)
	}
	empty, _ := json.Marshal(NewDocument())
	if !strings.Contains(string(empty), `"no_auto_nodes":[]`) {
		t.Fatalf("an empty document must carry an empty list: %s", empty)
	}
}
