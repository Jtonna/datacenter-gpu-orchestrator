package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Defaults for ProcessLauncher fields left at zero.
const (
	DefaultStopGrace      = 10 * time.Second
	DefaultTailLines      = 200
	DefaultHealthInterval = 250 * time.Millisecond
)

// ProcessLauncher is the real Launcher: it runs children with os/exec.
type ProcessLauncher struct {
	Ports          *PortAllocator // required
	Env            []string       // base environment; nil means os.Environ()
	StopGrace      time.Duration  // SIGTERM to SIGKILL delay; 0 means DefaultStopGrace
	TailLines      int            // output lines kept; 0 means DefaultTailLines
	HealthInterval time.Duration  // health poll period; 0 means DefaultHealthInterval

	// LogDir, when set, receives one file per start holding the child's
	// complete stdout and stderr; LogKeep (0 means DefaultLogKeep) is how many
	// of our files to keep there. A failure to create or write a file is
	// logged once (to Logger, default slog.Default) and never fails a start.
	LogDir  string
	LogKeep int
	Logger  *slog.Logger

	warnOnce sync.Once
}

var _ Launcher = (*ProcessLauncher)(nil)

// Start implements Launcher.
func (l *ProcessLauncher) Start(spec StartSpec) (Child, error) {
	if l.Ports == nil {
		return nil, errors.New("instance: ProcessLauncher has no port allocator")
	}
	if spec.Build == nil {
		return nil, errors.New("instance: StartSpec has no Build")
	}
	port, err := l.Ports.Acquire()
	if err != nil {
		return nil, err
	}
	argv, err := spec.Build(port)
	if err == nil && len(argv) == 0 {
		err = errors.New("empty argv")
	}
	if err != nil {
		l.Ports.Release(port)
		return nil, fmt.Errorf("instance: building command: %w", err)
	}

	base := l.Env
	if base == nil {
		base = os.Environ()
	}
	tail := l.TailLines
	if tail <= 0 {
		tail = DefaultTailLines
	}
	p := &proc{
		ports:    l.Ports,
		port:     port,
		argv:     argv,
		health:   spec.HealthPath,
		interval: l.HealthInterval,
		grace:    l.StopGrace,
		ring:     newRing(tail),
		load:     NewLoadLog(),
		done:     make(chan struct{}),
		exitCh:   make(chan ExitStatus, 1),
	}
	if p.health == "" {
		p.health = "/health"
	}
	if !strings.HasPrefix(p.health, "/") {
		p.health = "/" + p.health
	}
	if p.interval <= 0 {
		p.interval = DefaultHealthInterval
	}
	if p.grace <= 0 {
		p.grace = DefaultStopGrace
	}
	sink := func(line string) {
		p.ring.Add(line)
		p.load.Feed(line)
	}
	p.stdout, p.stderr = NewLineWriter(sink), NewLineWriter(sink)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = overrideEnv(scrubEnv(base), spec.Env)
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if sink := l.openChildLog(spec.Profile, spec.InstanceID); sink != nil {
		p.logSink = sink
		cmd.Stdout, cmd.Stderr = io.MultiWriter(p.stdout, sink), io.MultiWriter(p.stderr, sink)
	}
	cmd.WaitDelay = 2 * time.Second // never hang on pipes inherited by strays
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		if p.logSink != nil {
			p.logSink.Close()
		}
		l.Ports.Release(port)
		return nil, fmt.Errorf("instance: starting %s: %w", argv[0], err)
	}
	p.cmd = cmd
	go p.wait()
	return p, nil
}

// proc implements Child.
type proc struct {
	cmd      *exec.Cmd
	ports    *PortAllocator
	port     int
	argv     []string
	health   string
	interval time.Duration
	grace    time.Duration
	ring     *ring
	load     *LoadLog
	stdout   *LineWriter
	stderr   *LineWriter
	logSink  *fileSink // nil when the child has no log file

	mu      sync.Mutex
	stopReq bool // Stop was called
	termed  bool // graceful signal sent
	killed  bool // kill sent
	reaped  bool // cmd.Wait returned; no more signals

	done   chan struct{} // closed once status is final
	status ExitStatus
	exitCh chan ExitStatus
}

func (p *proc) Port() int      { return p.port }
func (p *proc) PID() int       { return p.cmd.Process.Pid }
func (p *proc) Argv() []string { return append([]string(nil), p.argv...) }
func (p *proc) LogFile() string {
	if p.logSink == nil {
		return ""
	}
	return p.logSink.path
}
func (p *proc) Tail(n int) []string { return p.ring.Last(n) }
func (p *proc) Load() LoadStats     { return p.load.Snapshot() }

func (p *proc) Exited() <-chan ExitStatus { return p.exitCh }

// wait reaps the child, finalises its status, releases the port, and then
// publishes the exit. It is the only place the child is waited on.
func (p *proc) wait() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.reaped = true
	requested := p.stopReq
	p.mu.Unlock()
	p.stdout.Close()
	p.stderr.Close()
	if p.logSink != nil {
		p.logSink.Close()
	}

	st := ExitStatus{Requested: requested, Tail: p.ring.Lines()}
	if ps := p.cmd.ProcessState; ps != nil {
		st.Code = ps.ExitCode()
		st.Signal = exitSignal(ps)
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		st.Err = err
	}
	p.status = st
	p.ports.Release(p.port)
	close(p.done)
	p.exitCh <- st
	close(p.exitCh)
}

// Stop implements Child.
func (p *proc) Stop(mode StopMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return
	}
	p.stopReq = true
	if mode == StopKill {
		if !p.killed {
			p.killed = true
			terminate(p.cmd.Process, false)
		}
		return
	}
	if p.termed || p.killed {
		return
	}
	p.termed = true
	terminate(p.cmd.Process, true)
	go func() {
		t := time.NewTimer(p.grace)
		defer t.Stop()
		select {
		case <-p.done:
		case <-t.C:
			p.Stop(StopKill)
		}
	}()
}

// WaitHealthy implements Child.
func (p *proc) WaitHealthy(ctx context.Context) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", p.port, p.health)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	for {
		select {
		case <-p.done:
			return p.exitedErr()
		default:
		}
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil); err == nil {
			if resp, err := client.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		t := time.NewTimer(p.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-p.done:
			t.Stop()
			return p.exitedErr()
		case <-t.C:
		}
	}
}

func (p *proc) exitedErr() error {
	return fmt.Errorf("%w: %s", ErrChildExited, p.status)
}
