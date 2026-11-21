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

// The Docker install of potluck-agent-setup and its removal by
// potluck-agent-uninstall (CR-9 review, D1 and D2), run through the test hooks
// POTLUCK_AGENT_TEST_ONLY=docker and POTLUCK_AGENT_TEST_ROOT with fake docker,
// systemctl, systemd-escape and ip first, and only, on PATH.

// mountUnit is what the fake systemd-escape prints. The real name has a
// backslash (var-lib-potluck\x2dagent-models.mount), which a Windows file
// name cannot hold, so the fake gives a plain one; the scripts only pass it on.
const mountUnit = "var-lib-potluck-agent-models.mount"

var dockerFakes = map[string]string{
	"docker": `printf '%s\n' "$*" >> "$FAKE_DIR/docker.log"
case "$1 $2" in
	"volume inspect") [ -f "$FAKE_DIR/volume" ] || exit 1 ;;
	"ps -aq") [ -f "$FAKE_DIR/container" ] && echo 0123456789ab ;;
esac
exit 0
`,
	"systemctl": `printf '%s\n' "$*" >> "$FAKE_DIR/systemctl.log"
if [ "$1" = is-active ]; then echo inactive; exit 3; fi
exit 0
`,
	"systemd-escape": `echo var-lib-potluck-agent-models.mount
`,
	"ip": `echo "192.0.2.1 dev eth0 src 192.0.2.50 uid 0"
`,
}

type dkEnv struct {
	t                        *testing.T
	sh, bin, fake, etc, root string
	out                      string
}

func newDk(t *testing.T) *dkEnv {
	sh := findSh()
	if sh == "" {
		t.Skip("no POSIX sh on this machine")
	}
	dir := t.TempDir()
	d := &dkEnv{t: t, sh: sh, bin: filepath.Join(dir, "bin"), fake: filepath.Join(dir, "fake"), etc: filepath.Join(dir, "etc"),
		root: filepath.Join(dir, "root")}
	for _, p := range []string{d.bin, d.fake, d.root} {
		os.MkdirAll(p, 0o755)
	}
	for name, body := range dockerFakes {
		os.WriteFile(filepath.Join(d.bin, name), []byte("#!/bin/sh\n"+body), 0o755)
	}
	for _, name := range []string{"mkdir", "rm", "rmdir", "cat", "chmod"} {
		p := realTool(sh, name)
		if p == "" {
			t.Skipf("no %s next to the sh", name)
		}
		os.WriteFile(filepath.Join(d.bin, name), []byte("#!/bin/sh\nexec '"+shPath(p)+"' \"$@\"\n"), 0o755)
	}
	return d
}

func (d *dkEnv) run(script string, args ...string) {
	d.t.Helper()
	abs, _ := filepath.Abs(script)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.sh, append([]string{shPath(abs)}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "POTLUCK_") && !strings.HasPrefix(strings.ToUpper(kv), "FAKE_DIR=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "POTLUCK_AGENT_TEST_PATH="+shPath(d.bin), "POTLUCK_AGENT_TEST_ETC="+shPath(d.etc),
		"POTLUCK_AGENT_TEST_ROOT="+shPath(d.root), "POTLUCK_AGENT_TEST_ONLY=docker", "FAKE_DIR="+shPath(d.fake))
	out, err := cmd.CombinedOutput()
	d.out = string(out)
	if err != nil {
		d.t.Fatalf("%s %v: %v\n%s", script, args, err, d.out)
	}
}

func (d *dkEnv) log(name string) []string {
	b, _ := os.ReadFile(filepath.Join(d.fake, name))
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// runLine is the "docker run" call of the setup.
func (d *dkEnv) runLine() string {
	d.t.Helper()
	for _, l := range d.log("docker.log") {
		if strings.HasPrefix(l, "run ") {
			return l
		}
	}
	d.t.Fatalf("no docker run:\n%s", d.out)
	return ""
}

func (d *dkEnv) dropin() string {
	return filepath.Join(d.root, "etc", "systemd", "system", "docker.service.d", "potluck-models.conf")
}

func index(lines []string, want string) int {
	last := -1
	for i, l := range lines {
		if l == want {
			last = i
		}
	}
	return last
}

func dockerArgs(source string) []string {
	return []string{"--docker", "--potluck", "192.0.2.1:3122", "--model-source", source, "--image", "potluck-agent:test", "--no-firewall"}
}

// D1: with a share, docker.service gets a drop-in that wants and orders it
// after the mount unit, systemd is reloaded after it is written, and the
// share is bound read-only with rslave propagation.
func TestDockerSetupOrdersDockerAfterTheShare(t *testing.T) {
	d := newDk(t)
	d.run("potluck-agent-setup", dockerArgs("share:192.0.2.1:/models")...)
	b, err := os.ReadFile(d.dropin())
	if err != nil {
		t.Fatalf("no drop-in: %v\n%s", err, d.out)
	}
	for _, want := range []string{"[Unit]", "Wants=" + mountUnit, "After=" + mountUnit} {
		if !strings.Contains(string(b), want+"\n") {
			t.Fatalf("drop-in lacks %q:\n%s", want, b)
		}
	}
	sc := d.log("systemctl.log")
	if start, reload := index(sc, "start "+mountUnit), index(sc, "daemon-reload"); start < 0 || reload < start {
		t.Fatalf("no daemon-reload after the drop-in was written: %q", sc)
	}
	if _, err := os.Stat(filepath.Join(d.root, "etc", "systemd", "system", mountUnit)); err != nil {
		t.Fatalf("no mount unit: %v", err)
	}
	if run := d.runLine(); !strings.Contains(run, " -v /var/lib/potluck-agent/models:/models:ro,rslave ") {
		t.Fatalf("share not bound ro,rslave: %s", run)
	}
}

// D2: the agent's data folder is the named volume potluck-agent-data at
// /var/lib/potluck-agent in the container; the models stay at /models, and no
// host folder is bound over the data folder.
func TestDockerSetupUsesTheDataVolume(t *testing.T) {
	for _, src := range []string{"share:192.0.2.1:/models", "push"} {
		d := newDk(t)
		os.MkdirAll(filepath.Join(d.root, "srv"), 0o755)
		d.run("potluck-agent-setup", dockerArgs(src)...)
		run := d.runLine()
		if !strings.Contains(run, " -v potluck-agent-data:/var/lib/potluck-agent ") {
			t.Fatalf("%s: no data volume: %s", src, run)
		}
		if strings.Count(run, ":/var/lib/potluck-agent ") != 1 || strings.Contains(run, ":/var/lib/potluck-agent/") {
			t.Fatalf("%s: something else is bound at the data folder: %s", src, run)
		}
		if !strings.HasSuffix(run, "potluck-agent:test --potluck 192.0.2.1:3122 --model-source "+src+" --install docker") {
			t.Fatalf("%s: image and agent arguments: %s", src, run)
		}
	}
}

// D1: a setup without a share writes no drop-in and removes the one of an
// earlier share setup; a model folder is bound read-only without rslave.
func TestDockerSetupWithoutAShareDropsTheDropIn(t *testing.T) {
	d := newDk(t)
	d.run("potluck-agent-setup", dockerArgs("share:192.0.2.1:/models")...)
	models := filepath.Join(d.root, "models")
	os.MkdirAll(models, 0o755)
	os.Remove(filepath.Join(d.fake, "docker.log"))
	d.run("potluck-agent-setup", dockerArgs("dir:"+shPath(models))...)
	if _, err := os.Stat(d.dropin()); !os.IsNotExist(err) {
		t.Fatalf("drop-in still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(d.dropin())); !os.IsNotExist(err) {
		t.Fatalf("empty docker.service.d left: %v", err)
	}
	if run := d.runLine(); !strings.Contains(run, " -v "+shPath(models)+":/models:ro ") || strings.Contains(run, "rslave") {
		t.Fatalf("model folder bind: %s", run)
	}
}

// D1 and D2: the uninstall removes the drop-in file only, the folder only
// when it is then empty, reloads systemd, and removes the data volume.
func TestDockerUninstallRemovesDropInAndVolume(t *testing.T) {
	for _, other := range []bool{false, true} {
		d := newDk(t)
		d.run("potluck-agent-setup", dockerArgs("share:192.0.2.1:/models")...)
		dir := filepath.Dir(d.dropin())
		if other {
			os.WriteFile(filepath.Join(dir, "override.conf"), []byte("[Service]\n"), 0o644)
		}
		os.WriteFile(filepath.Join(d.fake, "volume"), nil, 0o644)
		os.WriteFile(filepath.Join(d.fake, "container"), nil, 0o644)
		os.Remove(filepath.Join(d.fake, "systemctl.log"))
		d.run("potluck-agent-uninstall")
		if _, err := os.Stat(d.dropin()); !os.IsNotExist(err) {
			t.Fatalf("drop-in still there: %v", err)
		}
		_, err := os.Stat(dir)
		if other && err != nil {
			t.Fatalf("docker.service.d with another file was removed: %v", err)
		}
		if !other && !os.IsNotExist(err) {
			t.Fatalf("empty docker.service.d left: %v", err)
		}
		if other {
			if _, err := os.Stat(filepath.Join(dir, "override.conf")); err != nil {
				t.Fatalf("another drop-in was touched: %v", err)
			}
		}
		if index(d.log("systemctl.log"), "daemon-reload") < 0 {
			t.Fatalf("no daemon-reload: %q", d.log("systemctl.log"))
		}
		dl := d.log("docker.log")
		if index(dl, "volume rm potluck-agent-data") < 0 || index(dl, "rm -f potluck-agent") < 0 {
			t.Fatalf("docker calls: %q", dl)
		}
		if index(dl, "volume rm potluck-agent-data") < index(dl, "rm -f potluck-agent") {
			t.Fatalf("the volume was removed before the container: %q", dl)
		}
	}
	// No volume: nothing to remove, and no error.
	d := newDk(t)
	d.run("potluck-agent-uninstall")
	if index(d.log("docker.log"), "volume rm potluck-agent-data") >= 0 {
		t.Fatalf("removed a volume that is not there: %q", d.log("docker.log"))
	}
}
