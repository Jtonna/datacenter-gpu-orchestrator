package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
)

// CR-10: the log store address comes from the environment. Unset or
// unusable, central logging is off and nothing else changes.
func TestLogRelayFromTheEnvironment(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Logs: config.DefaultLogs()}
	conf := func() *config.Config { return cfg }
	noPool := func() *pool.Pool { return nil }

	t.Setenv(envLogsStoreURL, "")
	r := newLogRelay(conf, noPool, log)
	if r == nil || r.HasStore() || r.On() || r.Object("02:00:00:00:00:01") != nil {
		t.Fatal("central logging is on without a store address")
	}

	t.Setenv(envLogsStoreURL, " http://potluck-logs:9428/ ")
	r = newLogRelay(conf, noPool, log)
	if r == nil || !r.HasStore() || !r.On() {
		t.Fatal("central logging is off with a store address")
	}
	if o := r.Object("02:00:00:00:00:01"); o == nil || o.URL != "/api/pool/nodes/02:00:00:00:00:01/logs" {
		t.Fatalf("logs object %+v", o)
	}

	// An address that cannot be used does not stop Potluck: logging is off.
	t.Setenv(envLogsStoreURL, "potluck-logs:9428")
	if r = newLogRelay(conf, noPool, log); r == nil || r.HasStore() || r.On() {
		t.Fatal("an unusable store address was not taken as unset")
	}

	// The settings switch it off with a store address present.
	t.Setenv(envLogsStoreURL, "http://potluck-logs:9428")
	cfg = &config.Config{Logs: config.Logs{Enabled: false, RetentionDays: 14, MaxDiskGiB: 20, ClientIntake: true}}
	if r = newLogRelay(conf, noPool, log); r.On() || !r.HasStore() {
		t.Fatal("logs_enabled off did not switch central logging off")
	}
}
