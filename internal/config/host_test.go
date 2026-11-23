package config

import (
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// Phase 6: profile host (server | node) and the host settings.
func TestProfileHost(t *testing.T) {
	agents := "defaults: {membership: agents}\n"
	llama := "  p:\n    model: m.gguf\n    ctx: 4096\n"
	c := mustParse(t, prof(llama+"    force_nodes: [console-a, console-b]\n    host: node\n")+agents)
	if p := c.Profiles["p"]; p.Host != core.HostNode {
		t.Fatalf("%+v", p)
	}
	for _, h := range []string{"", "    host: server\n"} {
		c := mustParse(t, prof(llama+h)+agents)
		if p := c.Profiles["p"]; p.Host != "" {
			t.Fatalf("%q: %q", h, p.Host)
		}
	}
	for _, tt := range []struct{ name, yaml, want string }{
		{"no force_nodes", prof(llama+"    host: node\n") + agents, "requires force_nodes"},
		{"fleet membership", prof(llama + "    force_nodes: [console-a]\n    host: node\n"), "requires membership: agents"},
		{"unknown", prof(llama+"    host: laptop\n") + agents, "unknown value"},
		{"command", prof("  c:\n    kind: command\n    cmd: run ${PORT}\n    needs_mib: 100\n    host: node\n") + agents, "only valid with kind: llama"},
	} {
		err := parseErr(tt.yaml, nil)
		if err == nil || !strings.Contains(err.Error(), "host") || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestHostSettings(t *testing.T) {
	c := mustParse(t, "profiles: {}\n")
	if c.Defaults.HostOverheadMiB != 1024 || c.Defaults.HostNoMmap {
		t.Fatalf("%+v", c.Defaults)
	}
	c = mustParse(t, "defaults: {host_overhead_mib: 1500, host_no_mmap: true}\nprofiles: {}\n")
	if c.Defaults.HostOverheadMiB != 1500 || !c.Defaults.HostNoMmap {
		t.Fatalf("%+v", c.Defaults)
	}
	for _, v := range []string{"-1", "16385"} {
		if err := parseErr("defaults: {host_overhead_mib: "+v+"}\nprofiles: {}\n", nil); err == nil || !strings.Contains(err.Error(), "host_overhead_mib") {
			t.Errorf("%s: %v", v, err)
		}
	}
	for _, k := range []string{"host_overhead_mib", "host_no_mmap"} {
		if _, ok := SettingFieldByKey(ScopeSettings, k); !ok {
			t.Errorf("%s is not in the schema", k)
		}
	}
}
