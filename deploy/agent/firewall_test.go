package agentdeploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The firewall step of potluck-agent-setup and potluck-agent-uninstall, run
// through the scripts' test hooks (POTLUCK_AGENT_TEST_PATH, _ETC, _ONLY) with
// a fake ufw and firewall-cmd first, and only, on PATH.

var fwFakes = map[string]string{
	"ufw": `printf '%s\n' "$*" >> "$FAKE_DIR/ufw.log"
if [ "$1" = status ]; then
	s="Status: inactive"; [ -f "$FAKE_DIR/ufw.status" ] && IFS= read -r s < "$FAKE_DIR/ufw.status"
	echo "$s"
fi
exit 0
`,
	"firewall-cmd": `printf '%s\n' "$*" >> "$FAKE_DIR/firewall-cmd.log"
if [ -f "$FAKE_DIR/firewalld.running" ]; then echo running; exit 0; fi
echo "not running"; exit 252
`,
}

type fwEnv struct {
	t                  *testing.T
	sh, bin, fake, etc string
	out                string
}

func newFw(t *testing.T, tools ...string) *fwEnv {
	sh := findSh()
	if sh == "" {
		t.Skip("no POSIX sh on this machine")
	}
	dir := t.TempDir()
	f := &fwEnv{t: t, sh: sh, bin: filepath.Join(dir, "bin"), fake: filepath.Join(dir, "fake"), etc: filepath.Join(dir, "etc")}
	for _, d := range []string{f.bin, f.fake} {
		os.MkdirAll(d, 0o755)
	}
	for _, name := range tools {
		os.WriteFile(filepath.Join(f.bin, name), []byte("#!/bin/sh\n"+fwFakes[name]), 0o755)
	}
	for _, name := range []string{"mkdir", "rm"} {
		p := realTool(sh, name)
		if p == "" {
			t.Skipf("no %s next to the sh", name)
		}
		os.WriteFile(filepath.Join(f.bin, name), []byte("#!/bin/sh\nexec '"+shPath(p)+"' \"$@\"\n"), 0o755)
	}
	return f
}

func (f *fwEnv) set(name, body string) {
	os.WriteFile(filepath.Join(f.fake, name), []byte(body), 0o644)
}

func (f *fwEnv) lines(name string) []string {
	b, _ := os.ReadFile(filepath.Join(f.fake, name))
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && l != "status" {
			out = append(out, l)
		}
	}
	return out
}

func (f *fwEnv) run(script string, args ...string) {
	f.t.Helper()
	abs, _ := filepath.Abs(script)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.sh, append([]string{shPath(abs)}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "POTLUCK_") && !strings.HasPrefix(strings.ToUpper(kv), "FAKE_DIR=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "POTLUCK_AGENT_TEST_PATH="+shPath(f.bin), "POTLUCK_AGENT_TEST_ETC="+shPath(f.etc),
		"POTLUCK_AGENT_TEST_ONLY=firewall", "FAKE_DIR="+shPath(f.fake))
	out, err := cmd.CombinedOutput()
	f.out = string(out)
	if err != nil {
		f.t.Fatalf("%s %v: %v\n%s", script, args, err, f.out)
	}
}

// fwPorts are the ports the setup opens: the agent API and the worker. 50061
// (an agent-hosted server, phase 6) is not opened yet (CR-9 review, D3).
var fwPorts = []string{"50060", "50052", "50061"}

func allow(src string) []string {
	var out []string
	for _, port := range fwPorts {
		out = append(out, "allow proto tcp from "+src+" to any port "+port+" comment potluck-agent")
	}
	return out
}

func remove(src string, ports ...string) []string {
	if len(ports) == 0 {
		ports = fwPorts
	}
	var out []string
	for _, port := range ports {
		out = append(out, "delete allow proto tcp from "+src+" to any port "+port)
	}
	return out
}

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s\n got %q\nwant %q", what, got, want)
	}
}

var setupArgs = []string{"--bare", "--potluck", "192.0.2.1:3122", "--model-source", "push"}

// With ufw active the setup allows TCP from Potluck, and from every
// --allow-from network, to the agent and worker ports; the uninstall
// removes exactly those rules.
func TestSetupOpensUfwAndUninstallClosesIt(t *testing.T) {
	f := newFw(t, "ufw", "firewall-cmd")
	f.set("ufw.status", "Status: active\n")
	f.run("potluck-agent-setup", append(setupArgs, "--allow-from", "192.0.2.128/25")...)
	same(t, "ufw calls of the setup", f.lines("ufw.log"), append(allow("192.0.2.1"), allow("192.0.2.128/25")...))
	if len(f.lines("firewall-cmd.log")) != 0 {
		t.Fatalf("firewalld was asked although ufw is active: %v", f.lines("firewall-cmd.log"))
	}
	// Running the setup again adds the same rules (ufw skips the ones it has)
	// and removes none.
	os.Remove(filepath.Join(f.fake, "ufw.log"))
	f.run("potluck-agent-setup", append(setupArgs, "--allow-from", "192.0.2.128/25")...)
	same(t, "ufw calls of the second setup", f.lines("ufw.log"), append(allow("192.0.2.1"), allow("192.0.2.128/25")...))

	os.Remove(filepath.Join(f.fake, "ufw.log"))
	f.run("potluck-agent-uninstall")
	same(t, "ufw calls of the uninstall", f.lines("ufw.log"), append(remove("192.0.2.1"), remove("192.0.2.128/25")...))
	// A second uninstall has nothing left to remove.
	os.Remove(filepath.Join(f.fake, "ufw.log"))
	f.run("potluck-agent-uninstall")
	same(t, "ufw calls of the second uninstall", f.lines("ufw.log"), nil)
}

// A setup for another Potluck address removes the rules of the old one.
func TestSetupReplacesTheRulesOfAnOlderSetup(t *testing.T) {
	f := newFw(t, "ufw")
	f.set("ufw.status", "Status: active\n")
	f.run("potluck-agent-setup", setupArgs...)
	os.Remove(filepath.Join(f.fake, "ufw.log"))
	f.run("potluck-agent-setup", "--bare", "--potluck", "192.0.2.2:3122", "--model-source", "push")
	same(t, "ufw calls", f.lines("ufw.log"), append(allow("192.0.2.2"), remove("192.0.2.1")...))
}

func TestSetupLeavesAnInactiveUfwAlone(t *testing.T) {
	f := newFw(t, "ufw")
	f.run("potluck-agent-setup", setupArgs...)
	same(t, "ufw calls", f.lines("ufw.log"), nil)
	f.run("potluck-agent-uninstall")
	same(t, "ufw calls of the uninstall", f.lines("ufw.log"), nil)
}

func TestSetupNoFirewallFlag(t *testing.T) {
	f := newFw(t, "ufw", "firewall-cmd")
	f.set("ufw.status", "Status: active\n")
	f.set("firewalld.running", "")
	f.run("potluck-agent-setup", append(setupArgs, "--no-firewall")...)
	if b, err := os.ReadFile(filepath.Join(f.fake, "ufw.log")); err == nil {
		t.Fatalf("ufw was called with --no-firewall: %s", b)
	}
	if len(f.lines("firewall-cmd.log")) != 0 {
		t.Fatal("firewall-cmd was called with --no-firewall")
	}
}

// firewalld is not changed: the setup says what to allow.
func TestSetupPrintsAnInstructionForFirewalld(t *testing.T) {
	f := newFw(t, "firewall-cmd")
	f.set("firewalld.running", "")
	f.run("potluck-agent-setup", setupArgs...)
	// 50061: a host launch (release 0.1.9) listens there; Potluck checks it.
	for _, want := range []string{"firewalld is active", "192.0.2.1", "50060", "50052", "50061"} {
		if !strings.Contains(f.out, want) {
			t.Fatalf("the instruction does not say %q:\n%s", want, f.out)
		}
	}
	same(t, "firewall-cmd calls", f.lines("firewall-cmd.log"), []string{"--state"})
}

// The rules a setup made are listed in /etc/potluck/firewall-rules; the
// uninstall removes them all (50061 among them), and a new setup keeps the
// 50061 rule, which it asks for again (release 0.1.9: host launches).
func TestOlder50061RulesAreKept(t *testing.T) {
	old := "192.0.2.1 50060\n192.0.2.1 50052\n192.0.2.1 50061\n"
	f := newFw(t, "ufw")
	f.set("ufw.status", "Status: active\n")
	os.MkdirAll(f.etc, 0o755)
	os.WriteFile(filepath.Join(f.etc, "firewall-rules"), []byte(old), 0o644)
	f.run("potluck-agent-uninstall")
	same(t, "ufw calls of the uninstall", f.lines("ufw.log"), remove("192.0.2.1", "50060", "50052", "50061"))

	f = newFw(t, "ufw")
	f.set("ufw.status", "Status: active\n")
	os.MkdirAll(f.etc, 0o755)
	os.WriteFile(filepath.Join(f.etc, "firewall-rules"), []byte(old), 0o644)
	f.run("potluck-agent-setup", setupArgs...)
	same(t, "ufw calls of the setup", f.lines("ufw.log"), allow("192.0.2.1"))
	b, _ := os.ReadFile(filepath.Join(f.etc, "firewall-rules"))
	if string(b) != "192.0.2.1 50060\n192.0.2.1 50052\n192.0.2.1 50061\n" {
		t.Fatalf("rule list %q", b)
	}
}
