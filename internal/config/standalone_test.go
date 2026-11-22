package config

import (
	"strings"
	"testing"
)

// The fleet wording stays as it is with membership fleet; with agents every
// phrase of the table is replaced and no "fleet" is left.
func TestPoolTextBothModes(t *testing.T) {
	cases := map[string]string{
		"forced node ghost is not in the fleet view":                        "forced node ghost is not in the pool",
		"forced node b is absent from the fleet (for 1m21s); profile f ...": "forced node b is absent from the pool (for 1m21s); profile f ...",
		"offline in the fleet":                                              "offline in the pool",
		"excluded node in fleet view":                                       "excluded node in pool",
		"shared memory, headroom kept, from fleet figures":                  "shared memory, headroom kept, from agent figures",
		"It is tied to x, which the fleet does not report.":                 "It is tied to x, which is not in the pool.",
		"The fleet reports 3 GB free; as the fleet reports it":              "Its agent reports 3 GB free; as its agent reports it",
		"Nodes come from the fleet feed.":                                   "Nodes come from the pool.",
	}
	for fleet, pool := range cases {
		if got := PoolText(false, fleet); got != fleet {
			t.Errorf("fleet mode changed %q to %q", fleet, got)
		}
		if got := PoolText(true, fleet); got != pool {
			t.Errorf("agents mode: %q -> %q, want %q", fleet, got, pool)
		}
	}
	for _, p := range PoolPhrases {
		if got := PoolText(true, p[0]); strings.Contains(strings.ToLower(got), "fleet") {
			t.Errorf("phrase %q still says fleet: %q", p[0], got)
		}
	}
	agents := &Config{Defaults: Defaults{Membership: MembershipAgents}}
	fleet := &Config{Defaults: Defaults{Membership: MembershipFleet}}
	if agents.PoolText("the fleet") != "the pool" || fleet.PoolText("the fleet") != "the fleet" {
		t.Error("Config.PoolText does not follow the membership")
	}
}

// The scheduler's own node name: local_node with agents, falling back to
// fleet.local_node while it is empty; fleet membership keeps fleet.local_node.
func TestLocalNodeNameFallback(t *testing.T) {
	c := &Config{Defaults: Defaults{Membership: MembershipAgents}, Fleet: Fleet{LocalNode: "host-old"}}
	if got := c.LocalNodeName(); got != "host-old" {
		t.Errorf("agents, only fleet.local_node: %q", got)
	}
	c.LocalNode = "host-new"
	if got := c.LocalNodeName(); got != "host-new" {
		t.Errorf("agents, both set: %q", got)
	}
	c.Fleet.LocalNode = ""
	if got := c.LocalNodeName(); got != "host-new" {
		t.Errorf("agents, only local_node: %q", got)
	}
	c.Defaults.Membership = MembershipFleet
	c.Fleet.LocalNode = "host-old"
	if got := c.LocalNodeName(); got != "host-old" {
		t.Errorf("fleet membership must keep fleet.local_node: %q", got)
	}
}

// A stored document with no fleet and no recovery section is valid with
// membership agents, and the local_node setting is stored, built and
// round-trips through the YAML import (top-level local_node and a logs block).
func TestAgentsDocumentWithoutFleetSection(t *testing.T) {
	doc := NewDocument()
	doc.Fleet, doc.Recovery = nil, nil
	doc.Settings["membership"] = MembershipAgents
	doc.Settings["local_node"] = "host-1"
	c, err := Build(doc, Bootstrap{}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if !c.AgentsMode() || c.LocalNodeName() != "host-1" || c.Fleet.URL != "" {
		t.Fatalf("%+v", c)
	}
	f, ok := SettingFieldByKey(ScopeSettings, "local_node")
	if !ok || f.RestartRequired || f.Type != "string" || !strings.Contains(f.Description, "fleet.local_node") {
		t.Fatalf("schema field %+v %v", f, ok)
	}
	imp, err := ParseYAMLImport([]byte(`
local_node: host-1
defaults: {membership: agents}
logs: {retention_days: 30, max_disk_gib: 50, client_intake: false}
profiles: {}
`), Bootstrap{}, Options{FileExists: always})
	if err != nil {
		t.Fatal(err)
	}
	if imp.Doc.Settings["local_node"] != "host-1" || imp.Doc.Settings["logs_retention_days"] != 30.0 ||
		imp.Doc.Settings["logs_max_disk_gib"] != 50.0 || imp.Doc.Settings["logs_client_intake"] != false {
		t.Fatalf("settings %v", imp.Doc.Settings)
	}
	if _, set := imp.Doc.Settings["logs_enabled"]; set || len(imp.Doc.Fleet) != 0 || len(imp.Doc.Recovery) != 0 {
		t.Fatalf("defaults were written: %v fleet %v recovery %v", imp.Doc.Settings, imp.Doc.Fleet, imp.Doc.Recovery)
	}
}

// Every fleet and recovery key says in its description that agents
// membership does not use it, except fleet.local_node (the fallback).
func TestFleetAndRecoveryKeysSayNotUsedWithAgents(t *testing.T) {
	for _, scope := range []string{ScopeFleet, ScopeRecovery} {
		for _, f := range SettingFields(scope) {
			if scope == ScopeFleet && f.Key == "local_node" {
				if !strings.Contains(f.Description, "local_node setting is empty") {
					t.Errorf("fleet.local_node: %q", f.Description)
				}
				continue
			}
			if !strings.HasSuffix(f.Description, notWithAgents) {
				t.Errorf("%s.%s: %q does not say it is unused with agents", scope, f.Key, f.Description)
			}
		}
	}
}
