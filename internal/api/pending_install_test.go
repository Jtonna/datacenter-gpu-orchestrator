package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	agentdeploy "github.com/Jtonna/datacenter-gpu-orchestrator/deploy/agent"
)

// installRun runs the install script made for zone z at host on a fake
// machine: fakes for curl (writes a setup that prints its arguments), ip,
// getent and lspci go first on PATH; root holds the machine's /etc and
// /proc/mounts. It returns the output and whether the script succeeded.
type fakeMachine struct {
	addrs  []string // what `ip -o addr show` lists
	hosts  string   // what `getent hosts` prints
	lspci  string   // what `lspci` prints
	mounts string   // /proc/mounts
	files  []string // files to create under root
}

func installRun(t *testing.T, script string, m fakeMachine, args ...string) (string, bool) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX sh on this machine")
	}
	dir := t.TempDir()
	bin, root := filepath.Join(dir, "bin"), filepath.Join(dir, "root")
	for _, d := range []string{bin, filepath.Join(root, "proc"), filepath.Join(root, "etc", "potluck")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ip := "#!/bin/sh\n"
	for i, a := range m.addrs {
		fam := "inet"
		if strings.Contains(a, ":") {
			fam = "inet6"
		}
		ip += "echo '" + string(rune('1'+i)) + ": eth0    " + fam + " " + a + "/24 brd 192.0.2.255 scope global eth0'\n"
	}
	fakes := map[string]string{
		"ip":     ip,
		"getent": "#!/bin/sh\nprintf '%s' '" + m.hosts + "'\n[ -n '" + m.hosts + "' ]\n",
		"lspci":  "#!/bin/sh\nprintf '%s\\n' '" + m.lspci + "'\n",
		"curl": "#!/bin/sh\necho \"CURL $*\" >>\"" + posixPath(filepath.Join(dir, "curl.log")) + "\"\n" +
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then shift; printf '%s\\n' 'echo \"SETUP $*\"' >\"$1\"; fi; shift; done\n",
	}
	for name, text := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(m.mounts), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range m.files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(sh, append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "POTLUCK_INSTALL_TEST_PATH="+posixPath(bin), "POTLUCK_INSTALL_TEST_ROOT="+posixPath(root))
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// posixPath is p as the POSIX sh sees it (Git for Windows: /c/...).
func posixPath(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

const diskMounts = "/dev/sda2 / ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n"

func testPendingZone() PendingZone {
	return PendingZone{ID: "pz-0123456789abcdef", Name: "zone-9", Group: "rack-a", Mode: "bare",
		ModelSource: "share:192.0.2.10:/srv/models", ExpiresAt: "2026-01-01T01:00:00Z"}
}

// Condition 4: the script prints what it will do before doing it, then runs
// the very setup script with the same options; --dry-run only prints.
func TestInstallScript_PrintsItsPlanFirst(t *testing.T) {
	script := installScriptText(testPendingZone(), "192.0.2.10:3122")
	m := fakeMachine{addrs: []string{"192.0.2.21"}, mounts: diskMounts}
	out, ok := installRun(t, script, m, "--no-firewall")
	if !ok {
		t.Fatalf("failed:\n%s", out)
	}
	plan := strings.Index(out, "potluck: this will, on this machine:")
	run := strings.Index(out, "SETUP --bare --potluck 192.0.2.10:3122 --model-source share:192.0.2.10:/srv/models --name zone-9 --no-firewall")
	if plan < 0 || run < 0 || plan > run {
		t.Fatalf("plan at %d, setup at %d:\n%s", plan, run, out)
	}
	for _, s := range []string{"potluck-agent.service", "nothing is rebooted", "no other service is restarted", "--no-firewall"} {
		if !strings.Contains(out[:run], s) {
			t.Errorf("the plan lacks %q:\n%s", s, out)
		}
	}
	out, ok = installRun(t, script, m, "--dry-run")
	if !ok || !strings.Contains(out, "potluck: this will, on this machine:") || strings.Contains(out, "SETUP") || strings.Contains(out, "CURL") {
		t.Fatalf("dry run:\n%s", out)
	}
	z := testPendingZone()
	z.Mode = "docker"
	out, ok = installRun(t, installScriptText(z, "192.0.2.10:3122"), m)
	if !ok || !strings.Contains(out, "potluck-agent container") || !strings.Contains(out, "SETUP --docker") {
		t.Fatalf("docker:\n%s", out)
	}
}

// Condition 5: it refuses on a console and on the scheduler's machine,
// before downloading anything.
func TestInstallScript_RefusesConsoleAndSchedulerMachine(t *testing.T) {
	script := installScriptText(testPendingZone(), "192.0.2.10:3122")
	console := "Consoles are provisioned by the fleet console"
	cases := []struct {
		name string
		m    fakeMachine
		want string
	}{
		{"starter settings", fakeMachine{addrs: []string{"192.0.2.21"}, mounts: diskMounts, files: []string{"etc/potluck/starter.env"}}, console},
		{"root in memory", fakeMachine{addrs: []string{"192.0.2.21"}, mounts: "none / tmpfs rw 0 0\n"}, console},
		{"console GPU", fakeMachine{addrs: []string{"192.0.2.21"}, mounts: diskMounts, lspci: "03:00.0 VGA compatible controller: Advanced Micro Devices, Inc. [AMD/ATI] Oberon"}, console},
		{"scheduler address", fakeMachine{addrs: []string{"192.0.2.21", "192.0.2.10"}, mounts: diskMounts}, "runs the Potluck scheduler"},
	}
	for _, c := range cases {
		out, ok := installRun(t, script, c.m)
		if ok || !strings.Contains(out, c.want) || strings.Contains(out, "CURL") || strings.Contains(out, "SETUP") {
			t.Errorf("%s: ok %v:\n%s", c.name, ok, out)
		}
	}
	// A disk machine whose older kernel lists rootfs for / first is not a console.
	if out, ok := installRun(t, script, fakeMachine{addrs: []string{"192.0.2.21"}, mounts: "rootfs / rootfs rw 0 0\n" + diskMounts}); !ok {
		t.Errorf("rootfs line refused:\n%s", out)
	}
	// Potluck named by a host name that resolves to this machine; and loopback.
	named := installScriptText(testPendingZone(), "potluck.example:3122")
	if out, ok := installRun(t, named, fakeMachine{addrs: []string{"192.0.2.10"}, hosts: "192.0.2.10      potluck.example\n", mounts: diskMounts}); ok || !strings.Contains(out, "runs the Potluck scheduler") {
		t.Errorf("by name: %v\n%s", ok, out)
	}
	if out, ok := installRun(t, named, fakeMachine{addrs: []string{"192.0.2.21"}, hosts: "192.0.2.10      potluck.example\n", mounts: diskMounts}); !ok {
		t.Errorf("by name, another machine:\n%s", out)
	}
	if out, ok := installRun(t, installScriptText(testPendingZone(), "127.0.0.1:3122"), fakeMachine{addrs: []string{"192.0.2.21"}, mounts: diskMounts}); ok || !strings.Contains(out, "runs the Potluck scheduler") {
		t.Errorf("loopback: %v\n%s", ok, out)
	}
}

// Condition 6: nothing in the script or the setup it runs reboots a machine
// or stops, restarts or reloads a service other than Potluck's own agent.
func TestInstallScript_NoRebootNoOtherRestart(t *testing.T) {
	bad := regexp.MustCompile(`\b(reboot|poweroff|shutdown|halt|kexec|telinit|init 6)\b`)
	svc := regexp.MustCompile(`(systemctl\s+(restart|stop|kill|reload|try-restart|reload-or-restart|isolate)|docker\s+(restart|stop|rm|kill)|service\s+\S+\s+(restart|stop))\s*(.*)`)
	for name, text := range map[string]string{
		"install script": installScriptText(testPendingZone(), "192.0.2.10:3122"),
		"setup":          string(agentdeploy.Setup),
	} {
		for i, line := range strings.Split(text, "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "#") || strings.HasPrefix(code, "log ") || strings.HasPrefix(code, "echo ") || strings.HasPrefix(code, "say ") {
				continue
			}
			if bad.MatchString(code) {
				t.Errorf("%s line %d: %s", name, i+1, code)
			}
			if m := svc.FindStringSubmatch(code); m != nil && !strings.Contains(m[0], "potluck-agent") {
				t.Errorf("%s line %d acts on another service: %s", name, i+1, code)
			}
		}
	}
}

// Condition 3: a pending zone lives at most an hour.
func TestPendingZone_TTLAtMostAnHour(t *testing.T) {
	e, _, _ := groupsEnv(t)
	status(t, e.post("/api/zones/pending", `{"name":"zone-a","model_source":"push","ttl_min":61}`), http.StatusBadRequest)
	status(t, e.post("/api/zones/pending", `{"name":"zone-a","model_source":"push","ttl_min":1440}`), http.StatusBadRequest)
	status(t, e.post("/api/zones/pending", `{"name":"zone-a","model_source":"push","ttl_min":60}`), http.StatusCreated)
}

// Condition 7: a zone created before its machine joins is waiting, never
// down or lost, and counted nowhere: health, zone counts, uptime,
// availability, the zone list and its group's counts are as before it was
// made, and no answer names it except the pending list.
func TestPendingZone_CountedNowhere(t *testing.T) {
	e, _, _ := groupsEnv(t)
	status(t, e.post("/api/groups", `{"name":"rack-a"}`), http.StatusCreated)
	routes := []string{"/api/health", "/api/nodes", "/api/nodes/uptime", "/api/nodes/availability", "/api/status"}
	counts := func() map[string]any {
		out := map[string]any{}
		var g struct {
			Groups []struct {
				Name   string         `json:"name"`
				Count  int            `json:"count"`
				Counts map[string]any `json:"counts"`
			} `json:"groups"`
		}
		json.Unmarshal([]byte(body(t, e.get("/api/groups"))), &g)
		for _, x := range g.Groups {
			out[x.Name] = []any{x.Count, x.Counts}
		}
		var h map[string]any
		json.Unmarshal([]byte(body(t, e.get("/api/health"))), &h)
		out["health"] = []any{h["state"], h["issues"]}
		rows, _ := h["rows"].([]any)
		for _, r := range rows {
			if m, _ := r.(map[string]any); m["id"] == "zones" {
				out["health zones"] = m // total, up, down, lost ...
			}
		}
		return out
	}
	before := counts()
	var z PendingZone
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-new","group":"rack-a","model_source":"push"}`), http.StatusCreated)), &z)
	if z.State != PendingWaiting || z.StateText != "Waiting for the machine to check in" {
		t.Fatalf("%+v", z)
	}
	for _, r := range routes {
		if b := body(t, e.get(r)); strings.Contains(b, "zone-new") || strings.Contains(b, z.ID) {
			t.Errorf("%s names the pending zone", r)
		}
	}
	if after := counts(); !reflect.DeepEqual(before, after) {
		t.Fatalf("counts changed: %v -> %v", before, after)
	}
}
