package config

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

const lookupCfg = `
defaults:
  ttl: 100s
  stall_timeout: 50s
  command_load_timeout: 777s
tiers:
  - match: { hostname: "ps5-*" }
    priority: 1
    headroom_mib: 1024
  - match: { hostname: "laptop-*", role: console }
    priority: 2
  - match: { local: true }
    priority: 3
  - match: { source: relay }
    priority: 4
  - match: { hostname: "ps5-special" }
    priority: 9
profiles:
  Gemma:
    model: g.gguf
    ctx: 1
    needs_mib: 5
    aliases: [old-gemma]
  plain:
    model: p.gguf
    ctx: 1
  zero:
    model: z.gguf
    ctx: 1
    ttl: 0
    stall_timeout: 0
  custom:
    model: c.gguf
    ctx: 1
    ttl: 7s
    stall_timeout: 8s
    load_timeout: 9s
  cmd:
    kind: command
    cmd: run ${PORT}
    needs_mib: 1
  cmd2:
    kind: command
    cmd: run ${PORT}
    needs_mib: 1
    load_timeout: 3s
`

func TestResolve(t *testing.T) {
	c := mustParse(t, lookupCfg)
	tests := []struct {
		name      string
		in        string
		wantName  string
		wantAlias bool
		wantOK    bool
	}{
		{"exact", "Gemma", "Gemma", false, true},
		{"alias", "old-gemma", "Gemma", true, true},
		{"case sensitive name", "gemma", "", false, false},
		{"case sensitive alias", "OLD-GEMMA", "", false, false},
		{"unknown", "nope", "", false, false},
		{"empty", "", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, isAlias, ok := c.Resolve(tt.in)
			if ok != tt.wantOK || isAlias != tt.wantAlias {
				t.Fatalf("Resolve(%q) = _, %v, %v; want %v, %v", tt.in, isAlias, ok, tt.wantAlias, tt.wantOK)
			}
			if ok && p.Name != tt.wantName {
				t.Fatalf("profile = %q, want %q", p.Name, tt.wantName)
			}
			if !ok && p != nil {
				t.Fatalf("profile should be nil when not found")
			}
		})
	}
}

func TestMatchTier(t *testing.T) {
	c := mustParse(t, lookupCfg)
	tests := []struct {
		name            string
		host, role, src string
		local           bool
		wantPriority    int
		wantOK          bool
	}{
		{"glob match", "ps5-a", "", "", false, 1, true},
		{"first match wins over later", "ps5-special", "", "", false, 1, true},
		{"all keys must hold: role mismatch", "laptop-1", "node", "", false, 0, false},
		{"all keys must hold: role ok", "laptop-1", "console", "", false, 2, true},
		{"local", "host-1", "", "", true, 3, true},
		{"local flag only for local nodes", "host-1", "", "", false, 0, false},
		{"source exact", "other", "", "relay", false, 4, true},
		{"source is not a glob", "other", "", "relay2", false, 0, false},
		{"no tier matches", "laptop", "", "", false, 0, false},
		{"hostname case-insensitive", "PS5-AB", "", "", false, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier, ok := c.MatchTier(tt.host, tt.role, tt.src, tt.local)
			if ok != tt.wantOK || tier.Priority != tt.wantPriority {
				t.Fatalf("got priority %d ok %v; want %d %v", tier.Priority, ok, tt.wantPriority, tt.wantOK)
			}
		})
	}
	tier, _ := c.MatchTier("ps5-x", "", "", false)
	if tier != (core.Tier{Priority: 1, HeadroomMiB: 1024, Index: 1}) { // Index: its 1-based place in the list
		t.Errorf("settings not carried: %+v", tier)
	}
}

func TestEffectiveDurations(t *testing.T) {
	c := mustParse(t, lookupCfg)
	tests := []struct {
		profile    string
		ttl, stall time.Duration
		load       time.Duration
		loadOK     bool
	}{
		{"plain", 100 * time.Second, 50 * time.Second, 0, false},
		{"zero", 0, 0, 0, false},
		{"custom", 7 * time.Second, 8 * time.Second, 9 * time.Second, true},
		{"cmd", 100 * time.Second, 50 * time.Second, 777 * time.Second, true},
		{"cmd2", 100 * time.Second, 50 * time.Second, 3 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			p := c.Profiles[tt.profile]
			if got := c.EffectiveTTL(p); got != tt.ttl {
				t.Errorf("ttl = %v, want %v", got, tt.ttl)
			}
			if got := c.EffectiveStallTimeout(p); got != tt.stall {
				t.Errorf("stall = %v, want %v", got, tt.stall)
			}
			got, ok := c.EffectiveLoadTimeout(p)
			if got != tt.load || ok != tt.loadOK {
				t.Errorf("load = %v,%v want %v,%v", got, ok, tt.load, tt.loadOK)
			}
		})
	}
}

func TestProfilesWithoutNeedsMiB(t *testing.T) {
	c := mustParse(t, lookupCfg)
	var names []string
	for _, p := range c.ProfilesWithoutNeedsMiB() {
		names = append(names, p.Name)
	}
	want := []string{"custom", "plain", "zero"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v (sorted, llama only, needs_mib unset)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestExcluded(t *testing.T) {
	c := mustParse(t, "exclude_nodes: [\"02:00:00:00:00:AB\", \"ps5-abc123\", \"PS5-test*\", host-1]\ntiers:\n  - match: { local: true }\n    priority: 1\n")
	if got := c.ExcludeNodes; len(got) != 4 {
		t.Fatalf("ExcludeNodes = %v", got)
	}
	tests := []struct {
		id, host string
		want     bool
	}{
		{"02:00:00:00:00:ab", "ps5-zzz", true}, // id, case-insensitive
		{"02:00:00:00:00:AB", "", true},        // id, same case
		{"aa:bb", "PS5-ABC123", true},          // hostname exact, case-insensitive
		{"aa:bb", "ps5-test9", true},           // hostname glob
		{"aa:bb", "ps5-tes", false},            // glob needs the t
		{"host-1", "host-1", true},             // local node by id
		{"aa:bb", "ps5-other", false},
		{"ps5-abc123x", "ps5-abc123x", false}, // an entry is not a prefix match
		{"ps5-test1", "laptop-1", false},      // a glob is matched against the hostname only
	}
	for _, tt := range tests {
		if got := c.Excluded(core.NodeID(tt.id), tt.host); got != tt.want {
			t.Errorf("Excluded(%q,%q) = %v, want %v", tt.id, tt.host, got, tt.want)
		}
	}
	if (&Config{}).Excluded("x", "y") {
		t.Error("empty list excludes nothing")
	}
}
