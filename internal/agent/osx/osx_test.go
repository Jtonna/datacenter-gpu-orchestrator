package osx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseMemInfo(t *testing.T) {
	mi, err := ParseMemInfo(strings.NewReader("MemTotal:       15624192 kB\nMemFree: 1 kB\nMemAvailable:   14766080 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if mi.TotalMiB != 15258 || mi.AvailableMiB != 14420 {
		t.Fatalf("got %+v", mi)
	}
	if _, err := ParseMemInfo(strings.NewReader("MemTotal: 1 kB\n")); err == nil {
		t.Fatal("missing MemAvailable must be an error")
	}
}

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 6500020A:C3C4 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 6500020A:C3C4 0100020A:D431 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:C3CE 0100007F:9C40 06 00000000:00000000 03:00000000 00000000     0        0 0 3 0000000000000000
`

func TestProcNetSockets(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(procNetTCP), 0o644)
	s := ProcNetSockets{Root: root}
	if ok, _ := s.Listening(50116); !ok { // 0xC3C4
		t.Fatal("50116 should listen")
	}
	if ok, _ := s.Established(50116); !ok {
		t.Fatal("50116 should have an established client")
	}
	if ok, _ := s.Established(50126); ok { // TIME_WAIT only
		t.Fatal("TIME_WAIT is not a connected client")
	}
	if ok, _ := s.Listening(50052); ok {
		t.Fatal("50052 is not listening")
	}
}

func TestMountPoints(t *testing.T) {
	mi := "36 35 98:0 /mnt1 /mnt1 rw,noatime master:1 - ext3 /dev/root rw\n" +
		"40 30 0:50 / /run/potluck/models ro,relatime - nfs4 192.0.2.1:/ ro,vers=4.2\n" +
		"41 30 0:51 / /with\\040space rw - tmpfs tmpfs rw\n"
	p := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(p, []byte(mi), 0o644)
	for path, want := range map[string]bool{"/run/potluck/models": true, "/with space": true, "/run/potluck": false} {
		got, err := IsMountPoint(p, path)
		if err != nil || got != want {
			t.Errorf("%s: got %v %v want %v", path, got, err, want)
		}
	}
}

// MountSource names what is mounted at a path: the source field after the
// " - " separator of mountinfo. With two mounts on one path the last one
// (the one on top) counts.
func TestMountSource(t *testing.T) {
	mi := "36 35 98:0 /mnt1 /mnt1 rw,noatime master:1 - ext3 /dev/root rw\n" +
		"40 30 0:50 / /run/potluck/models ro,relatime shared:7 - nfs4 192.0.2.9:/old ro,vers=4.2\n" +
		"41 30 0:51 / /with\\040space rw - tmpfs tmpfs rw\n" +
		"42 30 0:52 / /run/potluck/models ro,relatime - nfs4 192.0.2.1:/ ro,vers=4.2\n"
	p := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(p, []byte(mi), 0o644)
	for path, want := range map[string]string{"/run/potluck/models": "192.0.2.1:/", "/with space": "tmpfs", "/mnt1": "/dev/root", "/run/potluck": ""} {
		got, mounted, err := MountSource(p, path)
		if err != nil || got != want || mounted != (want != "") {
			t.Errorf("%s: got %q %v %v want %q", path, got, mounted, err, want)
		}
	}
}

func TestOOMLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"6,1234,5678,-;Out of memory: Killed process 4242 (rpc-server) total-vm:1kB", true},
		{"6,1234,5678,-;Out of memory: Killed process 42420 (x)", false},
		{"6,1,2,-;oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),task=rpc-server,pid=4242,uid=0", true},
		{"6,1,2,-;something else 4242", false},
	}
	for _, c := range cases {
		if got := OOMLineNames(c.line, 4242); got != c.want {
			t.Errorf("%q: %v", c.line, got)
		}
	}
}

func TestProcFSList(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "123")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "exe"), []byte("/dev/shm/potluck/programs/0.1.0/rpc-server (deleted)\n"), 0o644)
	os.WriteFile(filepath.Join(d, "stat"), []byte("123 (rpc server) S 77 123 123 0"), 0o644)
	os.WriteFile(filepath.Join(d, "cgroup"), []byte("0::/system.slice/potluck-launch-ln-1.service\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "self"), 0o755)
	ps, err := ProcFS{Root: root}.List()
	if err != nil || len(ps) != 1 {
		t.Fatalf("%v %+v", err, ps)
	}
	p := ps[0]
	if p.PID != 123 || p.PPID != 77 || ExeBase(p.Exe) != "rpc-server" || !strings.Contains(p.Cgroup, "potluck-launch-") {
		t.Fatalf("%+v", p)
	}
}

func TestFakeClock(t *testing.T) {
	c := NewFakeClock(5)
	c.Advance(10)
	if c.Now() != 15 {
		t.Fatal(c.Now())
	}
}

func TestSignalVerifiedChecksTheCgroupFirst(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "5000000"), 0o755)
	os.WriteFile(filepath.Join(root, "5000000", "cgroup"), []byte("0::/system.slice/ssh.service\n"), 0o644)
	p := ProcFS{Root: root}
	var seen string
	// Pid 5000000 does not exist (it is above the kernel's pid limit), so on
	// Linux the process counts as gone; elsewhere the cgroup is refused.
	sent, err := p.SignalVerified(5000000, SigKill, func(cg string) bool { seen = cg; return false })
	if sent || err != nil {
		t.Fatalf("sent %v, err %v", sent, err)
	}
	if runtime.GOOS != "linux" && seen != "0::/system.slice/ssh.service" {
		t.Fatalf("cgroup read %q", seen)
	}
	if sent, _ := p.SignalVerified(5000001, SigKill, func(string) bool { return true }); sent {
		t.Fatal("a process with no cgroup file was signalled")
	}
	if sent, _ := p.SignalVerified(1, SigKill, func(string) bool { return true }); sent {
		t.Fatal("pid 1 was signalled")
	}
}
