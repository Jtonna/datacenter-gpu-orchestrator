package runner

import (
	"context"

	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

// Systemd runs each process as a systemd transient unit with systemd-run and
// watches it with systemctl (bare-metal install, contract 6.1). All OS work
// goes through Cmd, so the command lines and output parsing are tested with
// a fake Commander; the real commands only run on Linux.
//
// Exit status: a transient unit that ends cleanly is garbage-collected at
// once, so its status cannot be read afterwards with systemctl. Each unit
// therefore gets an ExecStopPost that writes "$EXIT_CODE $EXIT_STATUS" to
// Spec.ExitPath, which also survives an agent restart. Such a unit is run
// with --collect, so it leaves no failed unit record either.
type Systemd struct {
	Cmd  osx.Commander
	Poll time.Duration // how often a unit's state is read; default 250ms
	// Shell is the shell used by ExecStopPost; default /bin/sh.
	Shell string
}

// NewSystemd returns a Systemd runner using real commands. It refuses to run
// on a non-Linux OS.
func NewSystemd() (*Systemd, error) {
	if runtime.GOOS != "linux" {
		return nil, osx.ErrUnsupported
	}
	return &Systemd{Cmd: osx.ExecCommander{}}, nil
}

// Kind implements Runner.
func (*Systemd) Kind() string { return "systemd" }

// Survives implements Runner.
func (*Systemd) Survives() bool { return true }

func (s *Systemd) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return 250 * time.Millisecond
}

func (s *Systemd) shell() string {
	if s.Shell != "" {
		return s.Shell
	}
	return "/bin/sh"
}

func service(unit string) string {
	if strings.HasSuffix(unit, ".service") {
		return unit
	}
	return unit + ".service"
}

// StartArgs returns the systemd-run arguments for a spec (exported for tests
// and for the report of what runs on a real machine).
func (s *Systemd) StartArgs(sp Spec) []string {
	args := []string{
		"--unit=" + service(sp.Unit),
		"--description=potluck " + sp.Unit,
		"--service-type=exec",
		// Stopping is done by the agent (TERM, wait, KILL); systemd must not
		// add its own timeout or restart.
		"-p", "Restart=no",
		"-p", "KillMode=control-group",
		"-p", "SendSIGKILL=yes",
		"-p", "TimeoutStopSec=15s",
	}
	if sp.OOMScoreAdjust != 0 {
		args = append(args, "-p", "OOMScoreAdjust="+strconv.Itoa(sp.OOMScoreAdjust))
	}
	for _, e := range sp.Env {
		args = append(args, "--setenv="+e)
	}
	if sp.LogPath != "" {
		args = append(args, "-p", "StandardOutput=append:"+sp.LogPath, "-p", "StandardError=append:"+sp.LogPath)
	} else {
		args = append(args, "-p", "StandardOutput=null", "-p", "StandardError=null")
	}
	if sp.ExitPath != "" {
		// $$ is systemd's escape for a literal $, so the shell expands the
		// variables systemd sets for ExecStopPost. The path holds no quote or
		// $ (agent-built from a validated launch id).
		args = append(args, "--collect", "-p", fmt.Sprintf(`ExecStopPost=%s -c "echo $$EXIT_CODE $$EXIT_STATUS > %s"`, s.shell(), sp.ExitPath))
		// A unit that ends killed or with an error would stay in systemd as a
		// failed unit record until reset-failed; --collect unloads it at once,
		// also when no agent is there to do it. ExecStopPost runs before that,
		// and the exit status is read from its file. A unit without an exit
		// file is not collected: its status is read from the failed unit
		// (finish), which then runs reset-failed.
	}
	args = append(args, "--", sp.Path)
	args = append(args, sp.Args...)
	return args
}

// Start implements Runner.
func (s *Systemd) Start(ctx context.Context, sp Spec) (Proc, error) {
	st, err := s.show(ctx, sp.Unit)
	if err == nil && st.alive() {
		return nil, ErrExists
	}
	// A unit that failed earlier stays loaded; clear it so the name is free.
	_, _ = s.Cmd.Run(ctx, "systemctl", "reset-failed", service(sp.Unit))
	if sp.ExitPath != "" {
		_ = os.Remove(sp.ExitPath)
	}
	if _, err := s.Cmd.Run(ctx, "systemd-run", s.StartArgs(sp)...); err != nil {
		return nil, err
	}
	return s.watch(sp.Unit, sp.ExitPath, st0(ctx, s, sp.Unit)), nil
}

func st0(ctx context.Context, s *Systemd, unit string) int {
	st, err := s.show(ctx, unit)
	if err != nil {
		return 0
	}
	return st.mainPID
}

// Adopt implements Runner.
func (s *Systemd) Adopt(ctx context.Context, unit, exitPath string) (Proc, error) {
	st, err := s.show(ctx, unit)
	if err != nil {
		return nil, err
	}
	if !st.alive() {
		if _, statErr := os.Stat(exitPath); exitPath != "" && statErr == nil {
			// Ended while the agent was away: hand back a finished proc.
			p := &unitProc{s: s, unit: unit, exitPath: exitPath, done: make(chan struct{})}
			p.finish(st)
			return p, nil
		}
		return nil, nil
	}
	return s.watch(unit, exitPath, st.mainPID), nil
}

// List implements Runner.
func (s *Systemd) List(ctx context.Context, prefix string) ([]string, error) {
	out, err := s.Cmd.Run(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--full", "--type=service", prefix+"*")
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		// UNIT LOAD ACTIVE SUB DESCRIPTION
		name := strings.TrimPrefix(f[0], "●")
		if name == "" && len(f) > 4 {
			f = f[1:]
			name = f[0]
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		active := f[2]
		if active == "active" || active == "activating" || active == "deactivating" || active == "reloading" {
			units = append(units, strings.TrimSuffix(name, ".service"))
		}
	}
	return units, nil
}

// Stop implements Runner.
func (s *Systemd) Stop(ctx context.Context, unit string) error {
	_, err := s.Cmd.Run(ctx, "systemctl", "stop", service(unit))
	_, _ = s.Cmd.Run(ctx, "systemctl", "reset-failed", service(unit))
	return err
}

type unitState struct {
	load, active, sub string
	mainPID           int
	execCode          string // ExecMainCode: 1 exited, 2 killed, 3 dumped
	execStatus        int
}

func (u unitState) alive() bool {
	if u.load == "not-found" {
		return false
	}
	switch u.active {
	case "active", "activating", "deactivating", "reloading":
		return true
	}
	return false
}

// ParseShow parses `systemctl show -p ...` output.
func ParseShow(out string) unitState {
	var u unitState
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			u.load = v
		case "ActiveState":
			u.active = v
		case "SubState":
			u.sub = v
		case "MainPID":
			u.mainPID, _ = strconv.Atoi(v)
		case "ExecMainCode":
			u.execCode = v
		case "ExecMainStatus":
			u.execStatus, _ = strconv.Atoi(v)
		}
	}
	return u
}

func (s *Systemd) show(ctx context.Context, unit string) (unitState, error) {
	out, err := s.Cmd.Run(ctx, "systemctl", "show", service(unit),
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID",
		"-p", "ExecMainCode", "-p", "ExecMainStatus")
	if err != nil {
		return unitState{}, err
	}
	return ParseShow(string(out)), nil
}

type unitProc struct {
	s        *Systemd
	unit     string
	exitPath string
	mu       sync.Mutex
	pid      int
	done     chan struct{}
	exit     Exit
	once     sync.Once
}

func (s *Systemd) watch(unit, exitPath string, pid int) *unitProc {
	p := &unitProc{s: s, unit: unit, exitPath: exitPath, pid: pid, done: make(chan struct{})}
	go p.loop()
	return p
}

func (p *unitProc) Unit() string          { return p.unit }
func (p *unitProc) Done() <-chan struct{} { return p.done }
func (p *unitProc) Exit() Exit            { <-p.done; return p.exit }
func (p *unitProc) PID() int              { p.mu.Lock(); defer p.mu.Unlock(); return p.pid }

func (p *unitProc) Signal(sig osx.Sig) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Default --kill-whom=all: every process of the unit's cgroup.
	_, err := p.s.Cmd.Run(ctx, "systemctl", "kill", "--signal=SIG"+sig.String(), service(p.unit))
	return err
}

func (p *unitProc) loop() {
	misses := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := p.s.show(ctx, p.unit)
		cancel()
		if err != nil {
			// systemctl itself failing is not the unit ending; give up only
			// after a long run of failures.
			misses++
			if misses < 240 {
				time.Sleep(p.s.poll())
				continue
			}
			st = unitState{load: "not-found"}
		} else {
			misses = 0
			if st.mainPID > 0 {
				p.mu.Lock()
				p.pid = st.mainPID
				p.mu.Unlock()
			}
		}
		if !st.alive() {
			p.finish(st)
			return
		}
		time.Sleep(p.s.poll())
	}
}

// finish records the exit, preferring the ExecStopPost file, then the
// unit's own ExecMain fields (a failed unit stays loaded with them).
func (p *unitProc) finish(st unitState) {
	p.once.Do(func() {
		var e Exit
		if p.exitPath != "" {
			// ExecStopPost has run before the unit reaches inactive or failed;
			// allow a short moment for the file system anyway.
			for i := 0; i < 10; i++ {
				if b, err := os.ReadFile(p.exitPath); err == nil {
					e = ParseExitFile(string(b))
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		if !e.Known && st.load != "not-found" && st.execCode != "" {
			switch st.execCode {
			case "1", "exited":
				e = Exit{Known: true, Code: st.execStatus}
			case "2", "3", "killed", "dumped":
				e = Exit{Known: true, Code: 128 + st.execStatus, Signal: osx.SignalName(st.execStatus)}
			}
		}
		p.exit = e
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = p.s.Cmd.Run(ctx, "systemctl", "reset-failed", service(p.unit))
		cancel()
		close(p.done)
	})
}
