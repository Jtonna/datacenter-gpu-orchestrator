package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

// Child runs processes as children of the agent (Docker install). The
// container's own PID namespace and --init make sure no child outlives it.
type Child struct {
	mu    sync.Mutex
	procs map[string]*childProc
}

// NewChild returns a Child runner.
func NewChild() *Child { return &Child{procs: map[string]*childProc{}} }

// Kind implements Runner.
func (*Child) Kind() string { return "child" }

// Survives implements Runner: children die with the agent's container.
func (*Child) Survives() bool { return false }

type childProc struct {
	unit string
	cmd  *exec.Cmd
	done chan struct{}
	exit Exit
}

func (p *childProc) Unit() string          { return p.unit }
func (p *childProc) PID() int              { return p.cmd.Process.Pid }
func (p *childProc) Done() <-chan struct{} { return p.done }
func (p *childProc) Exit() Exit            { <-p.done; return p.exit }

func (p *childProc) Signal(sig osx.Sig) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return signalChild(p.cmd.Process, sig)
}

// Start implements Runner.
func (c *Child) Start(ctx context.Context, s Spec) (Proc, error) {
	c.mu.Lock()
	if old, ok := c.procs[s.Unit]; ok {
		select {
		case <-old.done:
		default:
			c.mu.Unlock()
			return nil, ErrExists
		}
	}
	c.mu.Unlock()

	cmd := exec.Command(runnablePath(s.Path), s.Args...)
	cmd.Env = scrubLlamaArgs(append(os.Environ(), s.Env...)) // no LLAMA_ARG_*: the command line is the one source
	var logf *os.File
	if s.LogPath != "" {
		f, err := os.OpenFile(s.LogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		logf = f
		cmd.Stdout, cmd.Stderr = f, f
	}
	setChildAttrs(cmd)
	if err := cmd.Start(); err != nil {
		if logf != nil {
			logf.Close()
		}
		return nil, err
	}
	if logf != nil {
		logf.Close() // the child holds its own copy of the descriptor
	}
	if s.OOMScoreAdjust != 0 {
		// Best effort; the file exists on Linux only.
		_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", cmd.Process.Pid), []byte(strconv.Itoa(s.OOMScoreAdjust)), 0o644)
	}
	p := &childProc{unit: s.Unit, cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.exit = exitFromState(cmd, err)
		if s.ExitPath != "" {
			kind, status := "exited", fmt.Sprint(p.exit.Code)
			if p.exit.Signal != "" {
				kind, status = "killed", p.exit.Signal
			}
			_ = os.WriteFile(s.ExitPath, []byte(kind+" "+status+"\n"), 0o644)
		}
		close(p.done)
	}()
	c.mu.Lock()
	c.procs[s.Unit] = p
	c.mu.Unlock()
	return p, nil
}

func exitFromState(cmd *exec.Cmd, err error) Exit {
	st := cmd.ProcessState
	if st == nil {
		return Exit{}
	}
	if sig, ok := signaledWith(st); ok {
		return Exit{Known: true, Code: 128 + sig, Signal: osx.SignalName(sig)}
	}
	return Exit{Known: true, Code: st.ExitCode()}
}

// Adopt implements Runner: a child never survives the agent.
func (*Child) Adopt(ctx context.Context, unit, exitPath string) (Proc, error) { return nil, nil }

// List implements Runner.
func (c *Child) List(ctx context.Context, prefix string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, p := range c.procs {
		select {
		case <-p.done:
			continue
		default:
		}
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	return out, nil
}

// Stop implements Runner: kills a live child of that name.
func (c *Child) Stop(ctx context.Context, unit string) error {
	c.mu.Lock()
	p := c.procs[unit]
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	_ = p.Signal(osx.SigKill)
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
