package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoveryConfigDefaults(t *testing.T) {
	r := mustParse(t, "profiles: {}\n").Recovery
	want := Recovery{Enabled: false, Actions: []string{RecoveryRestartRPC}, MinInterval: 120 * time.Second,
		StepWait: 90 * time.Second, RequestTTL: 45 * time.Second, HTTPTimeout: 3 * time.Second}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("defaults %+v, want %+v", r, want)
	}
}

func TestRecoveryConfigParsesAllKeys(t *testing.T) {
	r := mustParse(t, `
recovery:
  enabled: true
  actions: [restart_rpc_server, restart_docker, reboot]
  min_interval: 5m
  step_wait: 2m
  request_ttl: 60s
  http_timeout: 1s
profiles: {}
`).Recovery
	want := Recovery{Enabled: true, Actions: []string{RecoveryRestartRPC, RecoveryRestartDocker, RecoveryReboot},
		MinInterval: 5 * time.Minute, StepWait: 2 * time.Minute, RequestTTL: 60 * time.Second, HTTPTimeout: time.Second}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	// A subset in the right order, and a block with only some keys.
	r = mustParse(t, "recovery: {actions: [restart_rpc_server, reboot]}\nprofiles: {}\n").Recovery
	if !reflect.DeepEqual(r.Actions, []string{RecoveryRestartRPC, RecoveryReboot}) || r.MinInterval != 120*time.Second {
		t.Fatalf("subset: %+v", r)
	}
	r = mustParse(t, "recovery: {actions: [restart_docker]}\nprofiles: {}\n").Recovery
	if !reflect.DeepEqual(r.Actions, []string{RecoveryRestartDocker}) {
		t.Fatalf("single non-default action: %+v", r)
	}
}

func TestRecoveryConfigRejects(t *testing.T) {
	for y, key := range map[string]string{
		"recovery: {actions: []}":                                           "at least one",
		"recovery: {actions: [format_disk]}":                                `unknown action "format_disk"`,
		"recovery: {actions: [restart_rpc_server, restart_rpc_server]}":     "listed twice",
		"recovery: {actions: [restart_docker, restart_rpc_server]}":         "must come before",
		"recovery: {actions: [reboot, restart_docker]}":                     "must come before",
		"recovery: {actions: [restart_rpc_server, reboot, restart_docker]}": "must come before",
		"recovery: {min_interval: 59s}":                                     "min_interval: must be at least 60s",
		"recovery: {min_interval: 0}":                                       "min_interval",
		"recovery: {step_wait: 0}":                                          "step_wait",
		"recovery: {step_wait: -5s}":                                        "step_wait",
		"recovery: {request_ttl: 61s}":                                      "request_ttl: must be positive and at most 60s",
		"recovery: {request_ttl: 0}":                                        "request_ttl",
		"recovery: {http_timeout: 0}":                                       "http_timeout",
		"recovery: {http_timeout: -1s}":                                     "http_timeout",
		"recovery: {enabled: true, actions: [nonsense]}":                    "unknown action",
		"recovery: {unknown_key: 1}":                                        "unknown_key",
	} {
		err := parseErr(baseHeader+y+"\nprofiles: {}\n", nil)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: want an error containing %q, got %v", y, key, err)
		}
	}
	// The boundary values are accepted.
	if err := parseErr(baseHeader+"recovery: {min_interval: 60s, request_ttl: 60s}\nprofiles: {}\n", nil); err != nil {
		t.Errorf("boundary values rejected: %v", err)
	}
}

// Both config files describe membership agents, which uses neither the
// recovery nor the fleet section: neither appears, not even as a comment, and
// the parsed values are the defaults.
func TestExampleConfigsLeaveRecoveryAndFleetOut(t *testing.T) {
	for _, rel := range []string{"configs/example.yaml", "deploy/seed-config.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			l := strings.TrimLeft(strings.TrimSpace(line), "# ")
			if strings.HasPrefix(l, "recovery:") || strings.HasPrefix(l, "fleet:") || strings.HasPrefix(l, "node_overrides:") ||
				strings.Contains(l, "host.docker.internal") || strings.Contains(l, ":8090") {
				t.Errorf("%s: %q belongs to the fleet console", rel, line)
			}
		}
		c, err := LoadWithOptions(filepath.Join("..", "..", rel), Options{FileExists: existsAll})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Recovery, DefaultRecovery()) {
			t.Errorf("%s: recovery %+v, want the defaults", rel, c.Recovery)
		}
	}
}
