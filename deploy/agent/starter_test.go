// Package agentdeploy holds only the test harness of potluck-starter.sh.
//
// The harness runs the real script under a POSIX sh (Git Bash's sh on
// Windows) with fake curl, sha256sum, systemctl, systemd-run, uname and
// sleep first, and only, on PATH, plus thin wrappers for the real sed, head,
// mkdir, rm, chmod and mv. It uses the script's test hooks
// (POTLUCK_STARTER_TEST_PATH, _ETC, _SHM). It is skipped where no sh exists.
package agentdeploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	goodSum = "1111111111111111111111111111111111111111111111111111111111111111"
	badSum  = "2222222222222222222222222222222222222222222222222222222222222222"
)

// release is the answer of GET /api/pool/release, one entry per line, as
// agentproto.ReleaseInfo's layout rule allows.
const release = `{"release":"0.1.0","agents":{
"linux-arm64":{"path":"/api/agents/binary/0.1.0/linux-arm64","sha256":"` + badSum + `"},
"linux-amd64": {"sha256": "` + goodSum + `", "path": "/api/agents/binary/0.1.0/linux-amd64"}
}}
`

var fakes = map[string]string{
	"curl": `out=""; url=""
while [ $# -gt 0 ]; do
	case $1 in
		-o) out=$2; shift 2 ;;
		--connect-timeout|--max-time) shift 2 ;;
		-*) shift ;;
		*) url=$1; shift ;;
	esac
done
printf '%s\n' "$url" >> "$FAKE_DIR/curl.log"
case $url in
	*/api/pool/release)
		n=0; [ -f "$FAKE_DIR/unreachable" ] && read -r n < "$FAKE_DIR/unreachable"
		if [ "$n" -gt 0 ]; then
			printf '%s\n' $((n - 1)) > "$FAKE_DIR/unreachable"
			echo "curl: (7) Failed to connect" >&2
			exit 7
		fi
		while IFS= read -r l || [ -n "$l" ]; do printf '%s\n' "$l"; done < "$FAKE_DIR/release.json"
		;;
	*)
		n=0; [ -f "$FAKE_DIR/bad" ] && read -r n < "$FAKE_DIR/bad"
		if [ "$n" -gt 0 ]; then
			printf '%s\n' $((n - 1)) > "$FAKE_DIR/bad"
			printf 'BAD\n' > "$out"
		else
			printf 'GOOD\n' > "$out"
		fi
		;;
esac
`,
	"sha256sum": `IFS= read -r c < "$1"
if [ "$c" = GOOD ]; then echo "` + goodSum + `  $1"; else echo "` + badSum + `  $1"; fi
`,
	"systemctl": `printf '%s\n' "$*" >> "$FAKE_DIR/systemctl.log"
case $1 in
	is-active)
		s=inactive; [ -f "$FAKE_DIR/state" ] && read -r s < "$FAKE_DIR/state"
		echo "$s"
		[ "$s" = active ] ;;
	*) exit 0 ;;
esac
`,
	"systemd-run": `for a in "$@"; do printf '%s\n' "$a"; done > "$FAKE_DIR/systemd-run.args"
[ -f "$FAKE_DIR/systemd-run.fail" ] && exit 1
echo active > "$FAKE_DIR/state"
echo "Running as unit: potluck-agent.service"
`,
	"uname": `case $1 in
	-s) echo Linux ;;
	-m) a=x86_64; [ -f "$FAKE_DIR/arch" ] && read -r a < "$FAKE_DIR/arch"; echo "$a" ;;
esac
`,
	"sleep": `printf '%s\n' "$1" >> "$FAKE_DIR/sleeps"
`,
}

var realTools = []string{"sed", "head", "mkdir", "rm", "chmod", "mv"}

// findSh returns a POSIX sh, or "".
func findSh() string {
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Program Files\Git\bin\sh.exe`, `C:\Program Files\Git\usr\bin\sh.exe`} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// shPath turns a path into the form the sh understands ("/c/Users/..." for
// Git Bash, where ':' would split PATH).
func shPath(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

func realTool(sh, name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	// Git for Windows: sh in Git\bin, tools in Git\usr\bin.
	for _, p := range []string{filepath.Join(filepath.Dir(sh), "..", "usr", "bin", name+".exe"), filepath.Join(filepath.Dir(sh), name+".exe")} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

type starterEnv struct {
	t                  *testing.T
	sh, bin, fake, etc string
	shm, script        string
	env                map[string]string
	out                string
	code               int
	omit               map[string]bool
}

func newStarter(t *testing.T) *starterEnv {
	sh := findSh()
	if sh == "" {
		t.Skip("no POSIX sh on this machine")
	}
	dir := t.TempDir()
	s := &starterEnv{t: t, sh: sh, bin: filepath.Join(dir, "bin"), fake: filepath.Join(dir, "fake"),
		etc: filepath.Join(dir, "etc"), shm: filepath.Join(dir, "shm"), omit: map[string]bool{},
		env: map[string]string{"POTLUCK_URL": "http://192.0.2.1:3122"}}
	for _, d := range []string{s.bin, s.fake, s.etc} {
		os.MkdirAll(d, 0o755)
	}
	s.script, _ = filepath.Abs("potluck-starter.sh")
	s.write("release.json", release)
	return s
}

func (s *starterEnv) write(name, body string) {
	if err := os.WriteFile(filepath.Join(s.fake, name), []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *starterEnv) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(s.fake, name))
	return string(b)
}

func (s *starterEnv) tools() {
	for name, body := range fakes {
		if s.omit[name] {
			continue
		}
		os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body), 0o755)
	}
	for _, name := range realTools {
		p := realTool(s.sh, name)
		if p == "" {
			s.t.Skipf("no %s next to the sh", name)
		}
		os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\nexec '"+shPath(p)+"' \"$@\"\n"), 0o755)
	}
}

func (s *starterEnv) run() {
	s.t.Helper()
	s.tools()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.sh, shPath(s.script))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "POTLUCK_") && !strings.HasPrefix(strings.ToUpper(kv), "FAKE_DIR=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "POTLUCK_STARTER_TEST_PATH="+shPath(s.bin), "POTLUCK_STARTER_TEST_ETC="+shPath(s.etc),
		"POTLUCK_STARTER_TEST_SHM="+shPath(s.shm), "FAKE_DIR="+shPath(s.fake))
	for k, v := range s.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	s.out = string(out)
	s.code = 0
	if ctx.Err() != nil {
		s.t.Fatalf("starter did not finish:\n%s", s.out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		s.code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
}

func (s *starterEnv) wantCode(code int) {
	s.t.Helper()
	if s.code != code {
		s.t.Fatalf("exit %d, want %d; output:\n%s", s.code, code, s.out)
	}
}

func (s *starterEnv) wantStarted(potluck, source, floor string) {
	s.t.Helper()
	args := strings.Fields(s.read("systemd-run.args"))
	want := []string{"--unit=potluck-agent.service", "--description=Potluck", "agent", "-p", "Restart=always", "-p", "RestartSec=5",
		shPath(s.shm) + "/potluck-agent", "--ram-only", "--potluck", potluck, "--model-source", source, "--watchdog-floor-mib", floor}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		s.t.Fatalf("systemd-run args\n got %q\nwant %q\noutput:\n%s", args, want, s.out)
	}
	if b, _ := os.ReadFile(filepath.Join(s.shm, "potluck-agent")); string(b) != "GOOD\n" {
		s.t.Fatalf("installed agent %q", b)
	}
	if _, err := os.Stat(filepath.Join(s.shm, "potluck-agent.part")); err == nil {
		s.t.Fatal(".part left behind")
	}
}

func TestStarterMarker(t *testing.T) {
	s := newStarter(t)
	os.WriteFile(filepath.Join(s.etc, "disabled"), nil, 0o644)
	s.run()
	s.wantCode(0)
	if s.read("curl.log") != "" || s.read("systemd-run.args") != "" {
		t.Fatal("did something with the marker present")
	}
}

func TestStarterUnitActive(t *testing.T) {
	s := newStarter(t)
	for _, state := range []string{"active", "activating"} {
		s.write("state", state+"\n")
		s.run()
		s.wantCode(0)
		if s.read("curl.log") != "" || s.read("systemd-run.args") != "" {
			t.Fatalf("%s: did something", state)
		}
	}
}

func TestStarterUnreachableThenReachable(t *testing.T) {
	s := newStarter(t)
	s.write("unreachable", "3\n")
	s.run()
	s.wantCode(0)
	s.wantStarted("192.0.2.1:3122", "share:192.0.2.1:/", "1000")
	if got := strings.Fields(s.read("sleeps")); strings.Join(got, ",") != "5,10,20" {
		t.Fatalf("waits %v", got)
	}
	logs := strings.Fields(s.read("curl.log"))
	if logs[len(logs)-1] != "http://192.0.2.1:3122/api/agents/binary/0.1.0/linux-amd64" {
		t.Fatal(logs)
	}
}

func TestStarterWaitCapsAt60(t *testing.T) {
	s := newStarter(t)
	s.write("unreachable", "6\n")
	s.run()
	s.wantCode(0)
	if got := strings.Fields(s.read("sleeps")); strings.Join(got, ",") != "5,10,20,40,60,60" {
		t.Fatalf("waits %v", got)
	}
}

func TestStarterChecksumMismatchThenGood(t *testing.T) {
	s := newStarter(t)
	s.write("bad", "1\n")
	s.run()
	s.wantCode(0)
	s.wantStarted("192.0.2.1:3122", "share:192.0.2.1:/", "1000")
	if !strings.Contains(s.out, "checksum mismatch") || strings.TrimSpace(s.read("sleeps")) != "5" {
		t.Fatalf("output:\n%s\nsleeps %q", s.out, s.read("sleeps"))
	}
}

func TestStarterMissingCurl(t *testing.T) {
	s := newStarter(t)
	s.omit["curl"] = true
	s.run()
	s.wantCode(1)
	if !strings.Contains(s.out, "curl is missing") || s.read("systemd-run.args") != "" {
		t.Fatal(s.out)
	}
}

func TestStarterEnvFile(t *testing.T) {
	s := newStarter(t)
	delete(s.env, "POTLUCK_URL")
	os.WriteFile(filepath.Join(s.etc, "starter.env"), []byte("# Potluck\r\nPOTLUCK_URL=\"http://192.0.2.7:3122\"\r\nPOTLUCK_WATCHDOG_FLOOR_MIB=1500\r\n"), 0o644)
	s.run()
	s.wantCode(0)
	s.wantStarted("192.0.2.7:3122", "share:192.0.2.7:/", "1500")
	// The environment wins over the file.
	s2 := newStarter(t)
	s2.env["POTLUCK_MODEL_SOURCE"] = "push"
	os.WriteFile(filepath.Join(s2.etc, "starter.env"), []byte("POTLUCK_URL=http://192.0.2.7:3122\n"), 0o644)
	s2.run()
	s2.wantCode(0)
	s2.wantStarted("192.0.2.1:3122", "push", "1000")
}

func TestStarterLocalFaults(t *testing.T) {
	s := newStarter(t)
	s.write("arch", "riscv64\n")
	s.run()
	s.wantCode(1)
	if !strings.Contains(s.out, "unsupported architecture") {
		t.Fatal(s.out)
	}
	s = newStarter(t)
	s.write("systemd-run.fail", "")
	s.run()
	s.wantCode(1)
	if !strings.Contains(s.out, "systemd-run failed") {
		t.Fatal(s.out)
	}
	// /dev/shm/potluck cannot be created (a file is in the way).
	s = newStarter(t)
	os.WriteFile(s.shm, []byte("x"), 0o644)
	s.run()
	s.wantCode(1)
	if !strings.Contains(s.out, "cannot create") || s.read("curl.log") != "" {
		t.Fatal(s.out)
	}
}
