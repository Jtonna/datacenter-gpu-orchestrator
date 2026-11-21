// Package launch runs the node's one launch at a time (agent contract
// section 6): the start checks, the state machine, readiness, progress, the
// lease, the memory watchdog, stop, reset, the log, and re-adoption after an
// agent restart.
package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Gate is what the rest of the agent tells the launch manager for its checks.
type Gate interface {
	// Paused is the paused flag of the last heartbeat answer.
	Paused() bool
	// Release is the agent's release.
	Release() string
	ProgramsReady() bool
	// ModelSourceReachable is the result of the last finished share probe.
	ModelSourceReachable() bool
	// WaitDevicesRead blocks while a device read is in progress (4.3).
	WaitDevicesRead()
}

// Programs gives the verified programs of the current release.
type Programs interface {
	// Path returns the path of a verified program.
	Path(name string) (string, bool)
	Build(name string) string
}

// Config holds paths and tunables. Zero durations get the contract values.
type Config struct {
	StateDir    string // launch state files go to StateDir/launch
	LogDir      string // launch logs
	ProgramRoot string // all release folders; for reset
	BindAddr    string // the node's address that reaches Potluck
	ModelDir    string // the node's model folder; "" for push
	PushOnly    bool   // model source is push
	CUDA        bool   // start launches with CUDA_DEVICE_ORDER=PCI_BUS_ID
	// LibraryPath is the LD_LIBRARY_PATH the agent's library check used; a
	// launch gets the same value ("" sets nothing).
	LibraryPath string

	WatchdogFloorMiB int64

	WorkerPort int // default 50052
	HostPort   int // default 50061

	Tick               time.Duration // loop period; default 100ms (watchdog needs >= 4 Hz)
	StopWait           time.Duration // TERM to KILL; default 10s
	KillRetry          time.Duration // next kill attempt after one that was delivered; default 1s
	KillRetryFail      time.Duration // next kill attempt after one that failed; default 250ms
	StartTimeout       time.Duration // starting the unit; default 30s
	PortRetry          time.Duration // port_in_use retry; default 3s
	WorkerProbeTimeout time.Duration // default 5s
	PortCloseWait      time.Duration // after exit, wait at most this for the port to close; default 10s
	LogCap             int64
}

// Deps are the OS boundaries the manager uses.
type Deps struct {
	Clock   osx.Clock
	Runner  runner.Runner
	Mem     osx.MemReader
	Sockets osx.Sockets
	Procs   osx.Procs
	KLog    osx.KernelLog
	// PortFree reports nil when addr:port can be bound.
	PortFree func(addr string, port int) error
	// Probe is a TCP connect closed at once with no bytes sent (6.2).
	Probe func(ctx context.Context, addr string) error
	// HostHealthy asks llama-server's /health.
	HostHealthy func(ctx context.Context, addr string) bool
	Gate        Gate
	Programs    Programs
	Logger      *slog.Logger
	// OnExit is called (in its own goroutine) after a launch reached exited.
	OnExit func(st agentproto.LaunchState)
	// OnError records the agent's last_error (6.9).
	OnError func(msg string)
}

// record is the persisted part of a launch (StateDir/launch/<id>.json).
type record struct {
	LaunchID    string                   `json:"launch_id"`
	Role        string                   `json:"role"`
	Label       string                   `json:"label"`
	Release     string                   `json:"release"`
	Model       string                   `json:"model,omitempty"`
	Port        int                      `json:"port"`
	Command     []string                 `json:"command"`
	Build       string                   `json:"build"`
	Unit        string                   `json:"unit"`
	LogPath     string                   `json:"log_path"`
	ExitPath    string                   `json:"exit_path"`
	LeasePath   string                   `json:"lease_path"`
	StartedAt   int64                    `json:"started_at_boottime_ns"`
	LeaseTTL    int64                    `json:"lease_ttl_ns"`
	ReadyAt     int64                    `json:"ready_at_boottime_ns,omitempty"`
	Cause       string                   `json:"cause,omitempty"`
	CauseMsg    string                   `json:"cause_message,omitempty"`
	StopReqAt   int64                    `json:"stop_requested_at_boottime_ns,omitempty"`
	Force       bool                     `json:"force,omitempty"`
	Dropped     int64                    `json:"log_dropped,omitempty"`
	MemLowWater int64                    `json:"mem_low_water_mib,omitempty"`
	Exit        *agentproto.ExitInfo     `json:"exit,omitempty"`
	ExitedAt    int64                    `json:"exited_at_boottime_ns,omitempty"`
	LoadFinal   *agentproto.LoadProgress `json:"load,omitempty"`
}

type launchRun struct {
	rec        record
	proc       runner.Proc
	state      string
	stateSince time.Duration
	deadline   time.Duration
	scan       logScan
	scanOff    int64
	partial    []byte
	log        *LogFile
	kill       bool          // SIGKILL is wanted until the process is gone
	killBusy   bool          // a kill attempt is running
	killNext   time.Duration // earliest time of the next attempt
	killTries  int
	everReady  bool
	clientNow  bool
	clientSeen bool
	memAvail   int64
	lowWater   int64
	goneAt     time.Duration // when the process was seen gone (0: not yet)
	exitAt     time.Duration
	exit       *agentproto.ExitInfo
	load       *agentproto.LoadProgress
}

// Manager runs launches. Create with New, then call Recover once and Run.
type Manager struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	startMu sync.Mutex // serialises Start
	mu      sync.Mutex
	cur     *launchRun

	ticks atomic.Int64 // completed loop rounds (tests wait on it)
}

// New returns a Manager.
func New(cfg Config, deps Deps) *Manager {
	if cfg.WorkerPort == 0 {
		cfg.WorkerPort = agentproto.PortWorker
	}
	if cfg.HostPort == 0 {
		cfg.HostPort = agentproto.PortHost
	}
	if cfg.Tick == 0 {
		cfg.Tick = 100 * time.Millisecond
	}
	if cfg.StopWait == 0 {
		cfg.StopWait = 10 * time.Second
	}
	if cfg.KillRetry == 0 {
		cfg.KillRetry = time.Second
	}
	if cfg.KillRetryFail == 0 {
		cfg.KillRetryFail = 250 * time.Millisecond
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 30 * time.Second
	}
	if cfg.PortRetry == 0 {
		cfg.PortRetry = 3 * time.Second
	}
	if cfg.WorkerProbeTimeout == 0 {
		cfg.WorkerProbeTimeout = 5 * time.Second
	}
	if cfg.PortCloseWait == 0 {
		cfg.PortCloseWait = 10 * time.Second
	}
	if cfg.WatchdogFloorMiB == 0 {
		cfg.WatchdogFloorMiB = 1000
	}
	l := deps.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if deps.PortFree == nil {
		deps.PortFree = BindFree
	}
	if deps.Probe == nil {
		deps.Probe = ConnectClose
	}
	return &Manager{cfg: cfg, deps: deps, log: l}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (m *Manager) stateDir() string { return filepath.Join(m.cfg.StateDir, "launch") }

// Refusal is a refused call with its HTTP status.
type Refusal struct {
	Status int
	Body   agentproto.LaunchRefusal
}

func (r *Refusal) Error() string { return r.Body.Reason + ": " + r.Body.Message }

func refuse(reason, format string, a ...any) *Refusal {
	return &Refusal{Status: agentproto.LaunchRefusalStatus[reason], Body: agentproto.LaunchRefusal{Reason: reason, Message: fmt.Sprintf(format, a...)}}
}

var launchIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Flags the agent sets itself; extra_args may not contain them (6.2).
var (
	workerOwnFlags = []string{"--host", "-H", "--port", "-p", "--model-dir"}
	hostOwnFlags   = []string{"--host", "--port", "-m", "--model", "-c", "--ctx-size", "-np", "--parallel",
		"--rpc", "-ts", "--tensor-split"}
)

// EnvRPCFileLoad is read by the fork's llama-server (host process only):
// "0" never asks workers to read files (weights are pushed), "1" uses a
// worker's files where it offers them and falls back per worker, "required"
// fails the load unless every worker reads from its files. A host launch
// sets "required" when strict_file_load is true, else "1". There is no
// llama-server flag for it.
const (
	// LaunchOOMScoreAdjust makes the kernel's OOM killer pick the launch
	// before the agent or sshd.
	LaunchOOMScoreAdjust = 900

	EnvRPCFileLoad          = "LLAMA_RPC_FILE_LOAD"
	RPCFileLoadRequired     = "required"
	RPCFileLoadWhereOffered = "1"
)

func hasOwnFlag(args, own []string) string {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		for _, o := range own {
			if name == o {
				return o
			}
		}
	}
	return ""
}

func busyState(s string) bool {
	return s == agentproto.StateStarting || s == agentproto.StateReady || s == agentproto.StateStopping
}

// Busy reports whether a launch is running or stopping.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && busyState(m.cur.state)
}

// CurrentID returns the id of a launch that exists (not exited), or nil.
func (m *Manager) CurrentID() *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || !busyState(m.cur.state) {
		return nil
	}
	id := m.cur.rec.LaunchID
	return &id
}

// Role returns the node's role.
func (m *Manager) Role() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || !busyState(m.cur.state) {
		return agentproto.RoleIdle
	}
	return m.cur.rec.Role
}

// Model returns the host's model or the worker's label of the current or
// last launch, for GET /status.
func (m *Manager) Model() *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return nil
	}
	s := m.cur.rec.Label
	if m.cur.rec.Role == agentproto.RoleHost {
		s = m.cur.rec.Model
	}
	return &s
}

// PID returns the current launch's main pid (0 when none).
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.proc == nil {
		return 0
	}
	return m.cur.proc.PID()
}

func (m *Manager) memAvailable() (int64, error) {
	mi, err := m.deps.Mem.MemInfo()
	if err != nil {
		return 0, err
	}
	return mi.AvailableMiB, nil
}

// Start runs the checks of 6.2 and starts the launch asynchronously.
func (m *Manager) Start(ctx context.Context, req agentproto.LaunchRequest) (agentproto.LaunchState, *Refusal) {
	// A launch that arrives during a device read waits for it (4.3).
	if m.deps.Gate != nil {
		m.deps.Gate.WaitDevicesRead()
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()

	// Request shape. CONTRACT-QUESTION: no reason word is defined for a
	// malformed request; the agent answers 400 invalid_request.
	if !launchIDRE.MatchString(req.LaunchID) {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "launch_id %q is not a valid id", req.LaunchID)
	}
	switch req.Role {
	case agentproto.RoleWorker:
		if req.Worker == nil {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "role worker needs a worker object")
		}
		if f := hasOwnFlag(req.Worker.ExtraArgs, workerOwnFlags); f != "" {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "extra_args may not set %s; the agent sets it", f)
		}
		if req.Worker.ModelDir && m.cfg.PushOnly {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "model_dir is refused on a node whose model source is push")
		}
	case agentproto.RoleHost:
		h := req.Host
		if h == nil {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "role host needs a host object")
		}
		if m.cfg.PushOnly {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "a push node cannot be a host")
		}
		if !filepath.IsLocal(filepath.FromSlash(h.Model)) || strings.Contains(h.Model, `\`) {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "model %q leaves the model folder", h.Model)
		}
		if f := hasOwnFlag(h.ExtraArgs, hostOwnFlags); f != "" {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "extra_args may not set %s; the agent sets it", f)
		}
		if len(h.Workers) > 0 && len(h.TensorSplit) != len(h.Workers)+1 {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "tensor_split needs %d entries (host first), got %d", len(h.Workers)+1, len(h.TensorSplit))
		}
		if h.Ctx <= 0 || h.Parallel <= 0 {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "ctx and parallel must be positive")
		}
	default:
		return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "role must be worker or host")
	}

	// 1. busy
	m.mu.Lock()
	if m.cur != nil && busyState(m.cur.state) {
		id, st := m.cur.rec.LaunchID, m.cur.state
		m.mu.Unlock()
		r := refuse(agentproto.ReasonBusy, "launch %s is %s", id, st)
		r.Body.LaunchID = id
		return agentproto.LaunchState{}, r
	}
	m.mu.Unlock()
	g := m.deps.Gate
	// 2. paused
	if g.Paused() {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonPaused, "the node is paused")
	}
	// 3. release
	if req.Release != g.Release() || !g.ProgramsReady() {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonReleaseMismatch, "request release %q, agent release %q, programs ready %v", req.Release, g.Release(), g.ProgramsReady())
	}
	// 4. model source
	needShare := req.Role == agentproto.RoleHost || (req.Worker != nil && req.Worker.ModelDir)
	if needShare && !g.ModelSourceReachable() {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonModelSourceUnreachable, "the last share probe failed")
	}
	// 5. model file (host)
	modelPath := ""
	if req.Role == agentproto.RoleHost {
		modelPath = filepath.Join(m.cfg.ModelDir, filepath.FromSlash(req.Host.Model))
		if err := readableWithin(modelPath, 5*time.Second); err != nil {
			return agentproto.LaunchState{}, refuse(agentproto.ReasonModelNotFound, "%s: %v", req.Host.Model, err)
		}
	}
	// 6. memory
	avail, err := m.memAvailable()
	if err != nil {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonLowMemory, "cannot read MemAvailable: %v", err)
	}
	if avail < req.MinMemAvailableMiB {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonLowMemory, "MemAvailable %d MiB is below the required %d MiB", avail, req.MinMemAvailableMiB)
	}
	// 7. workers (host)
	if req.Role == agentproto.RoleHost && len(req.Host.Workers) > 0 {
		if failed := m.probeWorkers(ctx, req.Host.Workers); len(failed) > 0 {
			r := refuse(agentproto.ReasonWorkerUnreachable, "%d of %d workers did not accept a connection", len(failed), len(req.Host.Workers))
			r.Body.FailedWorkers = failed
			return agentproto.LaunchState{}, r
		}
	}
	// 8. port
	port := m.cfg.WorkerPort
	if req.Role == agentproto.RoleHost {
		port = m.cfg.HostPort
	}
	if err := m.waitPortFree(ctx, port); err != nil {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonPortInUse, "port %d: %v", port, err)
	}

	// Build the command.
	prog := agentproto.ProgramRPCServer
	if req.Role == agentproto.RoleHost {
		prog = agentproto.ProgramLlamaServer
	}
	path, ok := m.deps.Programs.Path(prog)
	if !ok {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonReleaseMismatch, "%s is not verified", prog)
	}
	args := []string{"--host", m.bindAddr(), "--port", strconv.Itoa(port)}
	if req.Role == agentproto.RoleWorker {
		if req.Worker.ModelDir {
			args = append(args, "--model-dir", m.cfg.ModelDir)
		}
		args = append(args, req.Worker.ExtraArgs...)
	} else {
		h := req.Host
		args = append(args, "-m", modelPath, "-c", strconv.Itoa(h.Ctx*h.Parallel), "--parallel", strconv.Itoa(h.Parallel))
		if len(h.Workers) > 0 {
			ts := make([]string, len(h.TensorSplit))
			for i, v := range h.TensorSplit {
				ts[i] = strconv.FormatFloat(v, 'f', -1, 64)
			}
			args = append(args, "--rpc", strings.Join(h.Workers, ","), "--tensor-split", strings.Join(ts, ","))
		}
		args = append(args, h.ExtraArgs...)
	}
	var env []string
	if m.cuda() {
		env = append(env, "CUDA_DEVICE_ORDER=PCI_BUS_ID")
	}
	if m.cfg.LibraryPath != "" {
		env = append(env, "LD_LIBRARY_PATH="+m.cfg.LibraryPath)
	}
	if req.Host != nil {
		mode := RPCFileLoadWhereOffered
		if req.Host.StrictFileLoad {
			mode = RPCFileLoadRequired
		}
		env = append(env, EnvRPCFileLoad+"="+mode)
	}

	// Replace the previous launch: its log and state files go (the log is
	// kept for the last launch only).
	m.mu.Lock()
	if prev := m.cur; prev != nil {
		os.Remove(prev.rec.LogPath)
		m.removeFiles(prev.rec)
	}
	m.cur = nil
	m.mu.Unlock()
	if err := os.MkdirAll(m.stateDir(), 0o755); err != nil {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "state dir: %v", err)
	}
	if err := os.MkdirAll(m.cfg.LogDir, 0o755); err != nil {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "log dir: %v", err)
	}
	now := m.deps.Clock.Now()
	rec := record{
		LaunchID: req.LaunchID, Role: req.Role, Label: req.Label, Release: req.Release,
		Port: port, Command: append([]string{prog}, args...), Build: m.deps.Programs.Build(prog),
		Unit:      agentproto.LaunchUnitPrefix + req.LaunchID,
		LogPath:   filepath.Join(m.cfg.LogDir, req.LaunchID+".log"),
		ExitPath:  filepath.Join(m.stateDir(), req.LaunchID+".exit"),
		LeasePath: filepath.Join(m.stateDir(), req.LaunchID+".lease"),
		StartedAt: int64(now),
		LeaseTTL:  int64(time.Duration(req.LeaseTTLS * float64(time.Second))),
	}
	if req.Role == agentproto.RoleHost {
		rec.Model = req.Host.Model
	}
	os.Remove(rec.LogPath)
	os.Remove(rec.ExitPath)
	run := &launchRun{rec: rec, state: agentproto.StateStarting, stateSince: now,
		deadline: now + time.Duration(rec.LeaseTTL), memAvail: avail, lowWater: avail,
		log: &LogFile{Path: rec.LogPath, Cap: m.cfg.LogCap}}
	if req.Role == agentproto.RoleHost {
		run.load = &agentproto.LoadProgress{State: agentproto.LoadWaiting}
	}
	if err := writeLease(rec.LeasePath, run.deadline); err != nil {
		return agentproto.LaunchState{}, refuse(agentproto.ReasonInvalidRequest, "lease file: %v", err)
	}
	m.persist(run)

	// The start does not end with the HTTP request: a request cancelled while
	// systemd-run runs would leave a unit with no record, lease or watchdog.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.StartTimeout)
	proc, err := m.deps.Runner.Start(sctx, runner.Spec{Unit: rec.Unit, Path: path, Args: args, Env: env,
		LogPath: rec.LogPath, ExitPath: rec.ExitPath, OOMScoreAdjust: LaunchOOMScoreAdjust})
	cancel()
	if err != nil {
		// The unit may have started all the same; it must not keep the port.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.StartTimeout)
		if serr := m.deps.Runner.Stop(stopCtx, rec.Unit); serr != nil {
			m.log.Warn("stop after a failed start", "launch_id", rec.LaunchID, "err", serr)
		}
		cancel()
	}
	m.mu.Lock()
	m.cur = run
	if err != nil {
		m.log.Error("launch start failed", "launch_id", rec.LaunchID, "err", err)
		m.finishLocked(run, runner.Exit{}, agentproto.ExitStartFailed, "could not start: "+err.Error())
		st := m.stateLocked(run)
		m.mu.Unlock()
		return st, nil
	}
	run.proc = proc
	st := m.stateLocked(run)
	m.mu.Unlock()
	m.log.Info("launch started", "launch_id", rec.LaunchID, "role", rec.Role, "command", strings.Join(rec.Command, " "))
	return st, nil
}

func readableWithin(path string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		f, err := os.Open(path)
		if err == nil {
			var b [1]byte
			_, err = f.Read(b[:])
			f.Close()
			if err == io.EOF {
				err = nil
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("timed out reading the model file")
	}
}

func (m *Manager) probeWorkers(ctx context.Context, workers []string) []string {
	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, m.cfg.WorkerProbeTimeout)
			defer cancel()
			if err := m.deps.Probe(c, w); err != nil {
				mu.Lock()
				failed = append(failed, w)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	sort.Strings(failed)
	return failed
}

func (m *Manager) waitPortFree(ctx context.Context, port int) error {
	deadline := time.Now().Add(m.cfg.PortRetry)
	for {
		err := m.deps.PortFree(m.bindAddr(), port)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// BindFree tries to bind addr:port and closes at once.
func BindFree(addr string, port int) error {
	l, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return l.Close()
}

// ConnectClose opens a TCP connection and closes it at once, sending no
// bytes (contract 6.2, probing a worker).
func ConnectClose(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// State returns GET /launch.
func (m *Manager) State() agentproto.LaunchState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return agentproto.LaunchState{State: agentproto.StateNone}
	}
	return m.stateLocked(m.cur)
}

func secs(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

func (m *Manager) stateLocked(r *launchRun) agentproto.LaunchState {
	now := m.deps.Clock.Now()
	st := agentproto.LaunchState{
		LaunchID: r.rec.LaunchID, Role: r.rec.Role, State: r.state,
		StateAgeS: secs(now - r.stateSince), Port: r.rec.Port, Command: r.rec.Command, Build: r.rec.Build,
		MemAvailableMiB: r.memAvail, MemLowWaterMiB: r.lowWater,
	}
	end := now
	if r.state == agentproto.StateExited {
		end = r.exitAt
	}
	st.UptimeS = secs(end - time.Duration(r.rec.StartedAt))
	if r.rec.Role == agentproto.RoleWorker {
		c := r.clientNow && r.state != agentproto.StateExited
		st.ClientConnected = &c
	}
	if r.load != nil {
		l := *r.load
		st.Load = &l
	}
	if r.exit != nil {
		e := *r.exit
		e.AgeS = secs(now - r.exitAt)
		st.Exit = &e
	} else {
		left := secs(r.deadline - now)
		if left < 0 {
			left = 0
		}
		st.LeaseExpiresInS = &left
	}
	return st
}

// Renew extends the lease (6.4). It returns false for an id that is not the
// current launch.
// CONTRACT-QUESTION: 6.4 says 404 "when the id is not the current launch";
// the agent also answers 404 for the current id once it has exited, since an
// exited launch has no lease.
func (m *Manager) Renew(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.cur
	if r == nil || r.rec.LaunchID != id || r.state == agentproto.StateExited {
		return false
	}
	r.deadline = m.deps.Clock.Now() + time.Duration(r.rec.LeaseTTL)
	if err := writeLease(r.rec.LeasePath, r.deadline); err != nil {
		m.log.Error("lease file", "err", err)
	}
	return true
}

func writeLease(path string, deadline time.Duration) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(int64(deadline), 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readLease(path string) (time.Duration, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(n), true
}

// ErrNotCurrent is returned by Stop for an id that is not the current launch.
var ErrNotCurrent = errors.New("not the current launch")

// Stop stops the launch (6.5). It answers at once; the state is stopping
// until the process is gone and its port closed.
func (m *Manager) Stop(id string, force bool) (agentproto.LaunchState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.cur
	if r == nil || r.rec.LaunchID != id {
		return agentproto.LaunchState{}, ErrNotCurrent
	}
	if r.state == agentproto.StateExited {
		return m.stateLocked(r), nil
	}
	cause := agentproto.ExitStopped
	if force {
		cause = agentproto.ExitForced
	}
	m.stopLocked(r, cause, "", force)
	return m.stateLocked(r), nil
}

// stopLocked records the cause and sends the signal. The first cause wins,
// except that a forced stop or a reset replaces a plain stop.
func (m *Manager) stopLocked(r *launchRun, cause, msg string, force bool) {
	now := m.deps.Clock.Now()
	if r.rec.Cause == "" || (r.rec.Cause == agentproto.ExitStopped && cause == agentproto.ExitForced) || cause == agentproto.ExitReset {
		r.rec.Cause, r.rec.CauseMsg = cause, msg
	}
	if r.rec.StopReqAt == 0 {
		r.rec.StopReqAt = int64(now)
	}
	if r.state != agentproto.StateStopping {
		r.state, r.stateSince = agentproto.StateStopping, now
	}
	if force {
		r.rec.Force = true
		r.kill = true
	}
	m.persist(r)
	if r.kill {
		m.killLocked(r, now)
		return
	}
	if p := r.proc; p != nil {
		id := r.rec.LaunchID
		go func() {
			// A TERM that fails is covered by the kill after StopWait.
			if err := p.Signal(osx.SigTerm); err != nil {
				m.log.Error("stop: SIGTERM failed", "launch_id", id, "err", err)
				return
			}
			m.log.Info("stop: SIGTERM sent", "launch_id", id)
		}()
	}
}

func procGone(p runner.Proc) bool {
	if p == nil {
		return true
	}
	select {
	case <-p.Done():
		return true
	default:
		return false
	}
}

// killLocked starts one kill attempt unless one is running. The tick calls
// it again until the process is gone; it never gives up. The attempts
// escalate: 1 SIGKILL through the runner (systemctl kill), 2 a stop of the
// unit (systemctl stop), 3 SIGKILL sent directly to the processes of the
// unit's own cgroup, and from 4 on both SIGKILLs together, for ever. (An
// ordinary stop or a lease expiry has sent SIGTERM and waited StopWait
// before attempt 1; a forced stop and the watchdog start with attempt 1.)
// The next attempt comes KillRetry after one that was delivered and
// KillRetryFail after one that failed, so there are never more than about
// four a second. Every attempt writes one log line: number, method, error.
func (m *Manager) killLocked(r *launchRun, now time.Duration) {
	p := r.proc
	if p == nil || r.killBusy {
		return
	}
	r.killBusy, r.killNext = true, now+m.cfg.KillRetry
	r.killTries++
	n, id, unit := r.killTries, r.rec.LaunchID, r.rec.Unit
	go func() {
		how, err := m.killStep(n, p, unit)
		if err != nil {
			m.log.Error("kill attempt failed", "launch_id", id, "attempt", n, "how", how, "err", err)
		} else {
			m.log.Warn("kill attempt", "launch_id", id, "attempt", n, "how", how)
		}
		m.mu.Lock()
		r.killBusy = false
		if err != nil {
			r.killNext = now + m.cfg.KillRetryFail
		}
		m.mu.Unlock()
	}()
}

// killStep runs attempt n of the kill escalation.
func (m *Manager) killStep(n int, p runner.Proc, unit string) (string, error) {
	direct := func() (string, error) {
		k := m.killUnitProcs(unit)
		if k == 0 {
			return "SIGKILL to the unit's processes", errors.New("no process found in the unit's cgroup")
		}
		return fmt.Sprintf("SIGKILL to the unit's %d processes", k), nil
	}
	switch n {
	case 1:
		return "SIGKILL", p.Signal(osx.SigKill)
	case 2:
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return "stop of the unit", m.deps.Runner.Stop(ctx, unit)
	case 3:
		return direct()
	}
	// From here on both, each time; one of them delivered is a delivery.
	err := p.Signal(osx.SigKill)
	how, derr := direct()
	if err != nil && derr != nil {
		return "SIGKILL and " + how, fmt.Errorf("%v; %v", err, derr)
	}
	return "SIGKILL and " + how, nil
}

// InUnitCgroup reports whether a /proc/<pid>/cgroup text places the process
// in the systemd unit (a path segment "<unit>.service").
func InUnitCgroup(cgroup, unit string) bool {
	for _, line := range strings.Split(cgroup, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), ":", 3)
		for _, seg := range strings.Split(f[len(f)-1], "/") {
			if seg == unit+".service" {
				return true
			}
		}
	}
	return false
}

// killUnitProcs sends SIGKILL to every process whose cgroup is the launch
// unit, and to no other. It returns how many were signalled.
func (m *Manager) killUnitProcs(unit string) int {
	if m.deps.Procs == nil || !strings.HasPrefix(unit, agentproto.LaunchUnitPrefix) {
		return 0
	}
	procs, err := m.deps.Procs.List()
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range procs {
		if p.PID <= 1 || p.PID == m.deps.Procs.Self() || !InUnitCgroup(p.Cgroup, unit) {
			continue
		}
		// The pid may have ended and been given to another process since
		// the list was read: where the OS allows it the process is pinned
		// and its cgroup read again right before the signal.
		if v, ok := m.deps.Procs.(osx.VerifiedSignaller); ok {
			sent, err := v.SignalVerified(p.PID, osx.SigKill, func(cg string) bool { return InUnitCgroup(cg, unit) })
			if sent && err == nil {
				n++
			}
			continue
		}
		if m.deps.Procs.Signal(p.PID, osx.SigKill) == nil {
			n++
		}
	}
	return n
}

// Reset kills every launch unit or child and every process run from the
// program folder that is not an exporter (6.8).
func (m *Manager) Reset(ctx context.Context) agentproto.ResetAnswer {
	ans := agentproto.ResetAnswer{Killed: []agentproto.KilledProcess{}}
	m.mu.Lock()
	curUnit, curPID := "", 0
	if r := m.cur; r != nil && r.state != agentproto.StateExited {
		curUnit = r.rec.Unit
		if r.proc != nil {
			curPID = r.proc.PID()
		}
		m.stopLocked(r, agentproto.ExitReset, "", true)
		ans.Killed = append(ans.Killed, agentproto.KilledProcess{PID: curPID, Unit: r.rec.Unit, LaunchID: r.rec.LaunchID})
	}
	m.mu.Unlock()
	if units, err := m.deps.Runner.List(ctx, agentproto.LaunchUnitPrefix); err == nil {
		for _, u := range units {
			if u == curUnit {
				continue
			}
			if p, err := m.deps.Runner.Adopt(ctx, u, ""); err == nil && p != nil {
				_ = p.Signal(osx.SigKill)
				ans.Killed = append(ans.Killed, agentproto.KilledProcess{PID: p.PID(), Unit: u, LaunchID: strings.TrimPrefix(u, agentproto.LaunchUnitPrefix)})
			}
		}
	}
	if m.deps.Procs != nil && m.cfg.ProgramRoot != "" {
		root := filepath.Clean(m.cfg.ProgramRoot) + string(filepath.Separator)
		procs, _ := m.deps.Procs.List()
		for _, p := range procs {
			exe := strings.TrimSuffix(p.Exe, " (deleted)")
			if exe == "" || p.PID == curPID || p.PID == m.deps.Procs.Self() || !strings.HasPrefix(filepath.Clean(exe), root) {
				continue
			}
			base := osx.ExeBase(exe)
			if base == agentproto.ProgramNodeExporter || base == agentproto.ProgramNvidiaGPUExporter {
				continue
			}
			if err := m.deps.Procs.Signal(p.PID, osx.SigKill); err == nil {
				ans.Killed = append(ans.Killed, agentproto.KilledProcess{PID: p.PID, Exe: p.Exe})
			}
		}
	}
	m.log.Warn("reset", "killed", len(ans.Killed))
	return ans
}

// Log reads the launch log from since (6.6).
func (m *Manager) Log(since int64, max int) (data []byte, next, dropped int64, ok bool) {
	m.mu.Lock()
	r := m.cur
	m.mu.Unlock()
	if r == nil {
		return nil, 0, 0, false
	}
	d, n, dr, err := r.log.Read(since, max)
	if err != nil {
		m.log.Warn("log read", "err", err)
	}
	return d, n, dr, true
}

// Run drives every launch rule until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
			m.ticks.Add(1)
		}
	}
}

// tick runs one round. Slow calls (socket table, probes, health) are made
// without holding the lock.
func (m *Manager) tick(ctx context.Context) {
	m.mu.Lock()
	r := m.cur
	if r == nil || r.state == agentproto.StateExited {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	now := m.deps.Clock.Now()

	// Watchdog (6.7): MemAvailable read from /proc/meminfo every tick.
	if avail, err := m.memAvailable(); err == nil {
		m.mu.Lock()
		r.memAvail = avail
		if avail < r.lowWater {
			r.lowWater = avail
			r.rec.MemLowWater = avail
		}
		if avail < m.cfg.WatchdogFloorMiB && r.rec.Cause != agentproto.ExitWatchdog && r.state != agentproto.StateExited {
			msg := fmt.Sprintf("MemAvailable %d MiB fell below the watchdog floor of %d MiB", avail, m.cfg.WatchdogFloorMiB)
			m.log.Error("watchdog", "launch_id", r.rec.LaunchID, "msg", msg)
			// CONTRACT-QUESTION: 6.7 says "kills"; a PS5 out of memory freezes
			// within seconds, so the watchdog kills at once (SIGKILL) instead of
			// TERM and a 10 s wait. Its reason replaces an earlier plain stop.
			r.rec.Cause = ""
			m.stopLocked(r, agentproto.ExitWatchdog, msg, true)
		}
		m.mu.Unlock()
	}

	// Log: progress, tokens, tail, cap.
	m.scanLog(r)

	m.mu.Lock()
	// Lease (6.4).
	if (r.state == agentproto.StateStarting || r.state == agentproto.StateReady) && now >= r.deadline {
		m.log.Warn("lease expired", "launch_id", r.rec.LaunchID)
		m.stopLocked(r, agentproto.ExitLeaseExpired, "", false)
	}
	// TERM, wait, KILL.
	if r.state == agentproto.StateStopping && !r.kill && r.rec.StopReqAt != 0 && now-time.Duration(r.rec.StopReqAt) >= m.cfg.StopWait {
		r.kill = true
	}
	// A kill is repeated, escalating, until the process is gone: one attempt
	// can fail, and a kill recorded by an earlier agent process is not trusted.
	if r.kill && now >= r.killNext && !procGone(r.proc) {
		m.killLocked(r, now)
	}
	state, role, port, proc := r.state, r.rec.Role, r.rec.Port, r.proc
	started := r.scan.started
	m.mu.Unlock()

	// Process gone?
	gone := false
	if proc != nil {
		select {
		case <-proc.Done():
			gone = true
		default:
		}
	} else {
		gone = true
	}

	addr := net.JoinHostPort(m.bindAddr(), strconv.Itoa(port))
	if !gone && role == agentproto.RoleWorker && m.deps.Sockets != nil {
		if c, err := m.deps.Sockets.Established(port); err == nil {
			m.mu.Lock()
			r.clientNow = c
			if c {
				r.clientSeen = true
			}
			m.mu.Unlock()
		}
	}
	// Readiness (6.3).
	if !gone && state == agentproto.StateStarting {
		ready := false
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		if role == agentproto.RoleWorker {
			ready = started && m.deps.Probe(pctx, addr) == nil
		} else if m.deps.HostHealthy != nil {
			ready = m.deps.HostHealthy(pctx, addr)
		}
		cancel()
		if ready {
			m.mu.Lock()
			if r.state == agentproto.StateStarting {
				r.state, r.stateSince, r.everReady = agentproto.StateReady, m.deps.Clock.Now(), true
				r.rec.ReadyAt = int64(r.stateSince)
				if r.load != nil && role == agentproto.RoleHost {
					r.load.State = agentproto.LoadDone
				}
				m.persist(r)
				m.log.Info("launch ready", "launch_id", r.rec.LaunchID)
			}
			m.mu.Unlock()
		}
	}
	if !gone {
		return
	}
	// Exited only after the port is closed too (6.5).
	m.mu.Lock()
	if r.goneAt == 0 {
		r.goneAt = now
	}
	goneAt := r.goneAt
	m.mu.Unlock()
	open := false
	if m.deps.Sockets != nil {
		open, _ = m.deps.Sockets.Listening(port)
	}
	if open && now-goneAt < m.cfg.PortCloseWait {
		m.mu.Lock()
		if r.state != agentproto.StateStopping {
			r.state, r.stateSince = agentproto.StateStopping, now
		}
		m.mu.Unlock()
		return
	}
	m.scanLog(r) // last lines
	var exit runner.Exit
	if proc != nil {
		exit = proc.Exit()
	}
	m.mu.Lock()
	if r.state == agentproto.StateExited {
		m.mu.Unlock()
		return
	}
	reason, msg := decideExit(r.rec.Role, r.rec.Cause, r.rec.CauseMsg, &r.scan, r.everReady, r.scan.began() || r.clientSeen, exit.Signal, func() bool {
		return proc != nil && m.deps.KLog != nil && m.deps.KLog.OOMKilled(proc.PID())
	})
	m.finishLocked(r, exit, reason, msg)
	m.mu.Unlock()
}

func (m *Manager) scanLog(r *launchRun) {
	for i := 0; i < 64; i++ { // bounded per tick
		m.mu.Lock()
		off := r.scanOff
		m.mu.Unlock()
		data, next, _, err := r.log.Read(off, 256<<10)
		if err != nil || len(data) == 0 {
			break
		}
		m.mu.Lock()
		r.scan.feed(&r.partial, data)
		r.scanOff = next
		m.applyProgressLocked(r)
		m.mu.Unlock()
	}
	if dropped, err := r.log.Enforce(); err == nil && dropped {
		m.mu.Lock()
		r.rec.Dropped = r.log.Dropped()
		m.persist(r)
		m.mu.Unlock()
	}
}

func (m *Manager) applyProgressLocked(r *launchRun) {
	s := &r.scan
	switch r.rec.Role {
	case agentproto.RoleWorker:
		if s.haveBytes {
			b, t := s.bytesLoaded, s.bytesTotal
			st := agentproto.LoadReceiving
			if t > 0 && b >= t {
				st = agentproto.LoadDone
			}
			r.load = &agentproto.LoadProgress{BytesLoaded: &b, BytesTotal: &t, State: st}
		}
	case agentproto.RoleHost:
		if s.haveFrac {
			f := s.fraction
			if r.load == nil {
				r.load = &agentproto.LoadProgress{}
			}
			r.load.Fraction = &f
			if r.load.State != agentproto.LoadDone {
				r.load.State = agentproto.LoadReceiving
				if f >= 1 {
					r.load.State = agentproto.LoadDone
				}
			}
		}
	}
}

func (m *Manager) finishLocked(r *launchRun, exit runner.Exit, reason, msg string) {
	now := m.deps.Clock.Now()
	ei := &agentproto.ExitInfo{Reason: reason, Message: msg, LogTail: append([]string{}, r.scan.tail...)}
	if exit.Known {
		c := exit.Code
		ei.Code = &c
		if exit.Signal != "" {
			s := exit.Signal
			ei.Signal = &s
		}
	}
	r.exit, r.exitAt = ei, now
	r.state, r.stateSince = agentproto.StateExited, now
	r.rec.Exit, r.rec.ExitedAt = ei, int64(now)
	r.rec.LoadFinal = r.load
	os.Remove(r.rec.LeasePath)
	m.persist(r)
	m.log.Info("launch exited", "launch_id", r.rec.LaunchID, "reason", reason, "message", msg)
	if text, abnormal := exitError(r.rec.LaunchID, ei); abnormal && m.deps.OnError != nil {
		m.deps.OnError(text)
	}
	if m.deps.OnExit != nil {
		st := m.stateLocked(r)
		go m.deps.OnExit(st)
	}
}

// exitError is the last_error text of an exit; abnormal is false for a stop
// that was asked for.
func exitError(id string, e *agentproto.ExitInfo) (text string, abnormal bool) {
	if e.Reason == agentproto.ExitStopped || e.Reason == agentproto.ExitForced || e.Reason == agentproto.ExitReset {
		return "", false
	}
	text = "launch " + id + " exited: " + e.Reason
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text, true
}

// LastExitError returns the last_error text of the exited launch and when it
// exited; ok is false when there is none or it was stopped on request. It
// gives an agent that restarted its last_error back (the exit is persisted).
func (m *Manager) LastExitError() (text string, at time.Duration, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.exit == nil {
		return "", 0, false
	}
	text, ok = exitError(m.cur.rec.LaunchID, m.cur.exit)
	return text, m.cur.exitAt, ok
}

func (m *Manager) persist(r *launchRun) {
	b, err := json.MarshalIndent(r.rec, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(m.stateDir(), r.rec.LaunchID+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, p)
	}
}

func (m *Manager) removeFiles(rec record) {
	for _, p := range []string{filepath.Join(m.stateDir(), rec.LaunchID+".json"), rec.ExitPath, rec.LeasePath} {
		os.Remove(p)
	}
}

// Recover re-adopts a launch left by a previous agent process (6.1, 6.4).
// With a runner whose processes survive (systemd) the launch continues and
// its lease continues from the lease file; a missing lease file means the
// lease has expired (a restart never grants a fresh lease). With a child
// runner (Docker) the launch died with the container: its files are
// removed and the node has no launch.
func (m *Manager) Recover(ctx context.Context) error {
	files, _ := filepath.Glob(filepath.Join(m.stateDir(), "*.json"))
	var recs []record
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rec record
		if json.Unmarshal(b, &rec) != nil || rec.LaunchID == "" {
			os.Remove(f)
			continue
		}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return nil
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].StartedAt > recs[j].StartedAt })
	for _, old := range recs[1:] {
		m.removeFiles(old)
	}
	rec := recs[0]
	if !m.deps.Runner.Survives() {
		m.removeFiles(rec)
		os.Remove(rec.LogPath)
		return nil
	}
	run := &launchRun{rec: rec, log: &LogFile{Path: rec.LogPath, Cap: m.cfg.LogCap}, lowWater: rec.MemLowWater, memAvail: rec.MemLowWater}
	run.log.SetDropped(rec.Dropped)
	run.scanOff = rec.Dropped
	now := m.deps.Clock.Now()
	if rec.Exit != nil {
		// Already exited before the restart: keep it readable.
		run.state, run.exit, run.exitAt = agentproto.StateExited, rec.Exit, time.Duration(rec.ExitedAt)
		run.stateSince = run.exitAt
		run.load = rec.LoadFinal
		m.mu.Lock()
		m.cur = run
		m.mu.Unlock()
		return nil
	}
	if rec.Role == agentproto.RoleHost {
		run.load = &agentproto.LoadProgress{State: agentproto.LoadWaiting}
	}
	// Rebuild progress, tokens and tail from what the log still holds.
	m.scanLog(run)
	if run.lowWater == 0 {
		if a, err := m.memAvailable(); err == nil {
			run.lowWater, run.memAvail = a, a
		}
	}
	proc, err := m.deps.Runner.Adopt(ctx, rec.Unit, rec.ExitPath)
	if err != nil {
		return err
	}
	run.proc = proc
	if dl, ok := readLease(rec.LeasePath); ok {
		run.deadline = dl
	} else {
		run.deadline = now
	}
	switch {
	case rec.StopReqAt != 0:
		run.state, run.stateSince = agentproto.StateStopping, now
		// Whatever the earlier agent process sent, the process still exists:
		// the tick kills it again.
		run.kill = rec.Force
	case rec.ReadyAt != 0:
		run.state, run.stateSince, run.everReady = agentproto.StateReady, time.Duration(rec.ReadyAt), true
	default:
		run.state, run.stateSince = agentproto.StateStarting, time.Duration(rec.StartedAt)
	}
	if rec.ReadyAt != 0 {
		run.everReady = true
	}
	m.mu.Lock()
	m.cur = run
	if proc == nil {
		// The unit is gone and left no exit file.
		m.finishLocked(run, runner.Exit{}, firstNonEmpty(rec.Cause, agentproto.ExitCrashed), firstNonEmpty(rec.CauseMsg, "the launch unit was gone when the agent restarted"))
	}
	m.mu.Unlock()
	m.log.Info("launch re-adopted", "launch_id", rec.LaunchID, "state", run.state)
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// SetBindAddr sets the node's address that reaches Potluck (known once the
// identity is resolved).
func (m *Manager) SetBindAddr(addr string) { m.mu.Lock(); m.cfg.BindAddr = addr; m.mu.Unlock() }

// SetCUDA sets whether launches get CUDA_DEVICE_ORDER=PCI_BUS_ID (known once
// the platform is known).
func (m *Manager) SetCUDA(on bool) { m.mu.Lock(); m.cfg.CUDA = on; m.mu.Unlock() }

func (m *Manager) bindAddr() string { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.BindAddr }
func (m *Manager) cuda() bool       { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.CUDA }
