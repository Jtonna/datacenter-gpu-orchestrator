// Package osx is the agent's boundary to the operating system. Everything the
// agent needs from Linux (clocks, /proc, signals, sockets, mounts, commands)
// is behind a small interface here. Parsers are portable and take a reader or
// a root path, so they are tested on any OS; the few calls that only exist on
// Linux live in osx_linux.go, and osx_other.go returns ErrUnsupported.
package osx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnsupported is returned by the real implementations on non-Linux builds.
var ErrUnsupported = errors.New("not supported on this OS (potluck-agent targets Linux)")

// Clock is a monotonic clock that keeps counting across agent restarts
// (CLOCK_BOOTTIME on Linux). Now returns the time since boot.
type Clock interface {
	Now() time.Duration
}

// FakeClock is a hand-advanced Clock for tests. Safe for concurrent use.
type FakeClock struct {
	mu sync.Mutex
	t  time.Duration
}

// NewFakeClock returns a FakeClock that starts at start.
func NewFakeClock(start time.Duration) *FakeClock { return &FakeClock{t: start} }

// Now returns the current fake time.
func (c *FakeClock) Now() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the fake time forward by d.
func (c *FakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t += d; c.mu.Unlock() }

// Sig is a signal the agent sends.
type Sig int

// The two signals the agent ever sends (contract 6.1: SIGTERM, wait, SIGKILL).
const (
	SigTerm Sig = 15
	SigKill Sig = 9
)

func (s Sig) String() string {
	switch s {
	case SigTerm:
		return "TERM"
	case SigKill:
		return "KILL"
	}
	return "SIG" + strconv.Itoa(int(s))
}

// SignalName returns the name without "SIG" of a Linux signal number, or
// the number as text.
func SignalName(n int) string {
	names := map[int]string{1: "HUP", 2: "INT", 3: "QUIT", 4: "ILL", 5: "TRAP", 6: "ABRT", 7: "BUS",
		8: "FPE", 9: "KILL", 10: "USR1", 11: "SEGV", 12: "USR2", 13: "PIPE", 14: "ALRM", 15: "TERM"}
	if s, ok := names[n]; ok {
		return s
	}
	return strconv.Itoa(n)
}

// ProcInfo is one process of the process table.
type ProcInfo struct {
	PID    int
	PPID   int
	Exe    string // resolved executable path, may end in " (deleted)"
	Cgroup string // the process's cgroup path(s), joined
}

// Procs lists processes and signals them.
type Procs interface {
	List() ([]ProcInfo, error)
	Signal(pid int, sig Sig) error
	Self() int
}

// MemInfo is the part of /proc/meminfo the agent uses.
type MemInfo struct {
	TotalMiB     int64
	AvailableMiB int64
}

// MemReader reads MemInfo. The real one reads /proc/meminfo directly; it
// never depends on an exporter (stats contract 1.1).
type MemReader interface {
	MemInfo() (MemInfo, error)
}

// MemInfoFile reads a meminfo-format file (default /proc/meminfo).
type MemInfoFile struct{ Path string }

// MemInfo reads and parses the file.
func (m MemInfoFile) MemInfo() (MemInfo, error) {
	p := m.Path
	if p == "" {
		p = "/proc/meminfo"
	}
	f, err := os.Open(p)
	if err != nil {
		return MemInfo{}, err
	}
	defer f.Close()
	return ParseMemInfo(f)
}

// ParseMemInfo parses /proc/meminfo text. MemTotal and MemAvailable are
// required.
func ParseMemInfo(r io.Reader) (MemInfo, error) {
	var mi MemInfo
	var haveT, haveA bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		// Values are in kB (KiB).
		switch f[0] {
		case "MemTotal:":
			mi.TotalMiB, haveT = v/1024, true
		case "MemAvailable:":
			mi.AvailableMiB, haveA = v/1024, true
		}
	}
	if err := sc.Err(); err != nil {
		return mi, err
	}
	if !haveT || !haveA {
		return mi, errors.New("meminfo: MemTotal or MemAvailable missing")
	}
	return mi, nil
}

// Sockets reads the TCP socket table without connecting to anything.
type Sockets interface {
	// Listening reports whether any socket listens on the local port.
	Listening(port int) (bool, error)
	// Established reports whether a connection on the local port is
	// established (a client holds a worker's connection).
	Established(port int) (bool, error)
}

// TCP states in /proc/net/tcp.
const (
	TCPEstablished = 0x01
	TCPListen      = 0x0A
)

// TCPEntry is one line of /proc/net/tcp or tcp6.
type TCPEntry struct {
	LocalPort  int
	RemotePort int
	State      int
}

// ParseProcNetTCP parses /proc/net/tcp or /proc/net/tcp6.
func ParseProcNetTCP(r io.Reader) ([]TCPEntry, error) {
	var out []TCPEntry
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		lp, ok1 := hexPort(f[1])
		rp, ok2 := hexPort(f[2])
		st, err := strconv.ParseInt(f[3], 16, 32)
		if !ok1 || !ok2 || err != nil {
			continue
		}
		out = append(out, TCPEntry{LocalPort: lp, RemotePort: rp, State: int(st)})
	}
	return out, sc.Err()
}

func hexPort(addrPort string) (int, bool) {
	i := strings.LastIndexByte(addrPort, ':')
	if i < 0 {
		return 0, false
	}
	p, err := strconv.ParseInt(addrPort[i+1:], 16, 32)
	return int(p), err == nil
}

// ProcNetSockets implements Sockets from /proc/net/tcp and tcp6 under Root
// (default "/proc").
type ProcNetSockets struct{ Root string }

func (s ProcNetSockets) entries() ([]TCPEntry, error) {
	root := s.Root
	if root == "" {
		root = "/proc"
	}
	var all []TCPEntry
	var firstErr error
	n := 0
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := os.Open(root + "/" + name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		e, err := ParseProcNetTCP(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		n++
		all = append(all, e...)
	}
	if n == 0 {
		return nil, firstErr
	}
	return all, nil
}

// Listening implements Sockets.
func (s ProcNetSockets) Listening(port int) (bool, error) {
	e, err := s.entries()
	if err != nil {
		return false, err
	}
	for _, x := range e {
		if x.LocalPort == port && x.State == TCPListen {
			return true, nil
		}
	}
	return false, nil
}

// Established implements Sockets.
func (s ProcNetSockets) Established(port int) (bool, error) {
	e, err := s.entries()
	if err != nil {
		return false, err
	}
	for _, x := range e {
		if x.LocalPort == port && x.State == TCPEstablished {
			return true, nil
		}
	}
	return false, nil
}

// ParseMountPoints returns the mount points listed in /proc/self/mountinfo
// text (field 5), with octal escapes decoded.
func ParseMountPoints(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		out = append(out, unescapeOctal(f[4]))
	}
	return out, sc.Err()
}

func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// IsMountPoint reports whether path is a mount point according to the
// mountinfo file (default /proc/self/mountinfo).
func IsMountPoint(mountinfo, path string) (bool, error) {
	if mountinfo == "" {
		mountinfo = "/proc/self/mountinfo"
	}
	f, err := os.Open(mountinfo)
	if err != nil {
		return false, err
	}
	defer f.Close()
	mps, err := ParseMountPoints(f)
	if err != nil {
		return false, err
	}
	for _, m := range mps {
		if m == path {
			return true, nil
		}
	}
	return false, nil
}

// Commander runs a command and returns its combined output.
type Commander interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecCommander runs real commands.
type ExecCommander struct{}

// Run runs the command with ctx, returning combined output. A non-zero exit
// is an error that includes the output.
func (ExecCommander) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// KernelLog answers whether the kernel log names a process as killed for
// lack of memory.
type KernelLog interface {
	OOMKilled(pid int) bool
}

// OOMLineNames reports whether one kernel log line says pid was killed by
// the OOM killer ("Out of memory: Killed process 1234 (rpc-server)" or
// "oom-kill:...,pid=1234,...").
func OOMLineNames(line string, pid int) bool {
	p := strconv.Itoa(pid)
	if i := strings.Index(line, "Killed process "); i >= 0 {
		rest := line[i+len("Killed process "):]
		if strings.HasPrefix(rest, p+" ") || rest == p {
			return true
		}
	}
	if strings.Contains(line, "oom-kill:") {
		for _, part := range strings.Split(line, ",") {
			if strings.TrimSpace(part) == "pid="+p {
				return true
			}
		}
	}
	return false
}

// DiskFree returns the free bytes of the filesystem holding path.
type DiskFree func(path string) (uint64, error)
