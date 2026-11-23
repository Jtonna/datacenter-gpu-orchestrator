package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The last log line says what happens to a running launch: a systemd launch
// unit outlives the agent, a child process (Docker install) does not.
func TestStoppedMessageDependsOnTheRunner(t *testing.T) {
	if m := stoppedMessage(true); !strings.Contains(m, "keeps running") {
		t.Fatalf("systemd runner: %q", m)
	}
	if m := stoppedMessage(false); strings.Contains(m, "keeps running") || !strings.Contains(m, "ends with") {
		t.Fatalf("child runner: %q", m)
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	var errb bytes.Buffer
	o, err := parseFlags([]string{"--potluck", "192.0.2.1:3122", "--ram-only", "--model-source", "share:192.0.2.1:/"}, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if o.DataDir != "/dev/shm/potluck" || o.StateDir != "/run/potluck" || o.ModelsDir != "/run/potluck/models" ||
		o.WatchdogFloorMiB != 1000 || o.Install != "bare" || !o.RAMOnly || o.Release != Release || o.SharedMemory != "auto" {
		t.Fatalf("%+v", o)
	}
	for _, v := range []string{"true", "false", "auto"} {
		o, err := parseFlags([]string{"--potluck", "192.0.2.1:3122", "--model-source", "push", "--shared-memory", v}, &errb)
		if err != nil || o.SharedMemory != v {
			t.Fatalf("--shared-memory %s: %q %v", v, o.SharedMemory, err)
		}
	}
	o, _ = parseFlags([]string{"--potluck", "192.0.2.1:3122", "--model-source", "share:192.0.2.1:/", "--install", "docker",
		"--allow-from", "172.30.0.0/24", "--allow-from", "192.0.2.5", "--watchdog-floor-mib", "1500"}, &errb)
	if o.DataDir != "/var/lib/potluck-agent" || o.ModelsDir != "/models" || len(o.AllowFrom) != 2 ||
		o.AllowFrom[1].String() != "192.0.2.5/32" || o.WatchdogFloorMiB != 1500 {
		t.Fatalf("%+v", o)
	}
	o, _ = parseFlags([]string{"--potluck", "192.0.2.1:3122", "--model-source", "dir:/srv/models"}, &errb)
	if o.ModelsDir != "/srv/models" {
		t.Fatal(o.ModelsDir)
	}
	o, _ = parseFlags([]string{"--potluck", "192.0.2.1:3122", "--model-source", "push"}, &errb)
	if o.ModelsDir != "" {
		t.Fatal(o.ModelsDir)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--model-source", "push"},
		{"--potluck", "potluck.lan:3122", "--model-source", "push"},
		{"--potluck", "192.0.2.1:3122"},
		{"--potluck", "192.0.2.1:3122", "--model-source", "nfs"},
		{"--potluck", "192.0.2.1:3122", "--model-source", "push", "--install", "podman"},
		{"--potluck", "192.0.2.1:3122", "--model-source", "push", "--allow-from", "nonsense"},
		{"--potluck", "192.0.2.1:3122", "--model-source", "push", "extra"},
		{"--potluck", "192.0.2.1:3122", "--model-source", "push", "--shared-memory", "yes"},
	} {
		if _, err := parseFlags(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestSubcommands(t *testing.T) {
	var out, errb bytes.Buffer
	if run([]string{"version"}, &out, &errb) != 0 || strings.TrimSpace(out.String()) != Release {
		t.Fatal(out.String())
	}
	dir := t.TempDir()
	if code := run([]string{"probe-read", dir}, &out, &errb); code != 1 {
		t.Fatal("empty folder must fail the probe", code)
	}
	os.WriteFile(filepath.Join(dir, "m.gguf"), []byte("GGUF"), 0o644)
	if code := run([]string{"probe-read", dir}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	// On Linux this would start a real agent, which runs until it is signalled
	// (it retries for ever, it never exits over an unreachable Potluck).
	if runtime.GOOS != "linux" {
		if code := run([]string{"--potluck", "192.0.2.1:3122", "--model-source", "push", "--install", "docker"}, &out, &errb); code != 1 {
			t.Fatalf("non-Linux must refuse to run: %d %s", code, errb.String())
		}
	}
}

// The id subcommand takes --install like the agent (bare by default): a
// Docker install may take its id from a virtual interface (contract 4.1).
func TestIDSubcommandInstall(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"id", "--install", "vm", "--potluck", "192.0.2.1:3122"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--install") {
		t.Fatalf("bad --install: code %d, %s", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"id", "--install", "docker", "--potluck", "nope"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "--potluck") {
		t.Fatalf("bad --potluck: code %d, %s", code, errb.String())
	}
}

// 0.1.10: --log-level info|debug (default info); debug shows, among
// others, the source address of allowed API calls.
func TestParseFlagsLogLevel(t *testing.T) {
	base := []string{"--potluck", "192.0.2.1:3122", "--model-source", "push"}
	if o, err := parseFlags(base, &bytes.Buffer{}); err != nil || o.LogLevel != "info" || logLevel(o) != slog.LevelInfo {
		t.Fatalf("default %q %v", o.LogLevel, err)
	}
	if o, err := parseFlags(append(base, "--log-level", "debug"), &bytes.Buffer{}); err != nil || logLevel(o) != slog.LevelDebug {
		t.Fatalf("debug %q %v", o.LogLevel, err)
	}
	if _, err := parseFlags(append(base, "--log-level", "trace"), &bytes.Buffer{}); err == nil {
		t.Fatal("--log-level trace accepted")
	}
}

// CR-10: --logs on|off (default on), --log-buffer-mib (default 8),
// --journal-dir (default none).
func TestParseFlagsLogs(t *testing.T) {
	base := []string{"--potluck", "192.0.2.1:3122", "--model-source", "push"}
	o, err := parseFlags(base, &bytes.Buffer{})
	if err != nil || o.LogsOff || o.LogBufferBytes != 8<<20 || o.JournalDir != "" {
		t.Fatalf("defaults %+v %v", o, err)
	}
	o, err = parseFlags(append(base, "--logs", "off", "--log-buffer-mib", "2", "--journal-dir", "/host/journal"), &bytes.Buffer{})
	if err != nil || !o.LogsOff || o.LogBufferBytes != 2<<20 || o.JournalDir != "/host/journal" {
		t.Fatalf("%+v %v", o, err)
	}
	if o, err = parseFlags(append(base, "--logs", "on"), &bytes.Buffer{}); err != nil || o.LogsOff {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{{"--logs", "maybe"}, {"--log-buffer-mib", "0"}, {"--log-buffer-mib", "-1"}, {"--log-buffer-mib", "4097"}} {
		if _, err := parseFlags(append(append([]string{}, base...), bad...), &bytes.Buffer{}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestParseFlagsAdvertiseAddr(t *testing.T) {
	var errb bytes.Buffer
	base := []string{"--potluck", "192.0.2.1:3122", "--model-source", "push"}
	o, err := parseFlags(append(base, "--advertise-addr", "192.0.2.40"), &errb)
	if err != nil || o.AdvertiseAddr != "192.0.2.40" {
		t.Fatal(o.AdvertiseAddr, err)
	}
	if o, _ := parseFlags(base, &errb); o.AdvertiseAddr != "" {
		t.Fatal(o.AdvertiseAddr)
	}
	for _, bad := range []string{"laptop.lan", "0.0.0.0", "127.0.0.1", "::", "192.0.2.40:50060", "224.0.0.1"} {
		if _, err := parseFlags(append(base, "--advertise-addr", bad), &errb); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
