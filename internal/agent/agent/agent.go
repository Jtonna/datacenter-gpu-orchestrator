// Package agent is potluck-agent's core: registration, heartbeat, the
// program pipeline, devices, stats wiring, local status and the HTTP API
// (agent contract sections 4 to 9). The OS is reached only through the
// interfaces in Deps, so the whole flow is tested against a fake Potluck.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/busyfile"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/devices"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/exporters"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/filecache"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/hw"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/identity"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/launch"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/logship"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/modelsrc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/programs"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/startup"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/stats"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Options are the agent's install options (section 9) and paths.
type Options struct {
	Potluck          string // "<ip>:<port>"
	Release          string // the agent's own release
	Name             string // display label; hostname by default
	Install          string // bare or docker
	RAMOnly          bool
	ModelSource      modelsrc.Spec
	WatchdogFloorMiB int64
	// SharedMemory is --shared-memory: auto ("" too), true or false. It
	// overrides the heuristic that decides whether a device shares system
	// memory (launch.TestsSystemMemory); the devices are reported with the
	// result, so Potluck places on the same figure the agent tests.
	SharedMemory string
	AllowFrom    []netip.Prefix
	// AdvertiseAddr is --advertise-addr: the address sent as the
	// registration's addr, where Potluck reaches the agent and its launches
	// (an agent in a Docker container behind NAT whose ports are published
	// on the machine). The agent still binds its own address. "" sends its
	// own address.
	AdvertiseAddr string
	DataDir       string // programs and launch logs
	StateDir      string // state files and the share mount point
	ModelsDir     string // the node's model folder ("" for push)
	// LDLibraryPath is the agent's LD_LIBRARY_PATH, the one the library
	// check used; launches get the same value.
	LDLibraryPath string
	// LeaveMarker is the file that says the node was taken out on purpose
	// (LeaveMarkerPath on a real machine; "" checks nothing). When it exists
	// as the agent stops, the agent deregisters first (CR-9).
	LeaveMarker string
	// Central logging (CR-10). LogsOff is --logs off: no shipper at all.
	// LogBufferBytes is the ring (--log-buffer-mib; 0: 8 MiB). JournalDir is
	// --journal-dir: a mounted journal folder for a Docker install, which
	// ships no system log without it. A bare install follows the machine's
	// own journal.
	LogsOff        bool
	LogBufferBytes int
	JournalDir     string
	// LogLevel is --log-level: info (default) or debug (0.1.10).
	LogLevel string
	// MarksFile is the start-up marks file another component writes
	// (agentproto.StartupMarksFile on a bare install); "" reads none. The
	// agent only reads it, for a while after its start.
	MarksFile string
	// BusyFile is the busy file other software on the node reads (6.14;
	// busyfile.DefaultPath on a bare Linux install); "" writes none. The
	// state is tracked and shown in GET /status either way.
	BusyFile string
}

// LeaveMarkerPath is the marker the uninstall writes and the starter checks
// (deploy/agent: $ETC_DIR/disabled).
const LeaveMarkerPath = "/etc/potluck/disabled"

// leaveTimeout bounds the deregister call of a stop with the leave marker.
const leaveTimeout = 3 * time.Second

// Hardware is the machine facts the agent reports.
type Hardware interface {
	GPUs() []hw.GPUInfo
	Backends(gpus []hw.GPUInfo) []string
	MissingLibs(path string) ([]string, error)
}

// Refetcher is an Updater that can also install another build of the
// release it runs (the registration answer's agent_sha256, 5.4). A Docker
// install's updater is not one: its binary is the image's.
type Refetcher interface {
	Refetch(ctx context.Context, release, sha256 string) error
}

// Updater replaces the agent on agent_too_old (5.4).
type Updater interface {
	Update(ctx context.Context, required string) error
}

// Deps are the OS boundaries.
type Deps struct {
	Clock    osx.Clock
	Runner   runner.Runner
	Mem      osx.MemReader
	Sockets  osx.Sockets
	Procs    osx.Procs
	KLog     osx.KernelLog
	Net      identity.NetOS
	Hardware Hardware
	Mounter  modelsrc.Mounter
	Prober   modelsrc.ProbeStarter
	DiskFree func(string) (uint64, error)
	PortFree func(addr string, port int) error
	// Listen binds a TCP address for the agent API (port 50060). When nil
	// the agent binds nothing and the caller serves Handler and LocalHandler.
	Listen func(addr string) (net.Listener, error)
	// DevicesRun runs the llama program for --list-devices.
	DevicesRun  devices.Runner
	HostHealthy func(ctx context.Context, addr string) bool
	// ReadChars is a process's read count (launch.Deps.ReadChars); nil: none.
	ReadChars func(pid int) (int64, error)
	Updater   Updater
	HTTP      *http.Client
	Logger    *slog.Logger
	// FileCache does the page cache calls of POST /cache/evict (6.13); nil:
	// the agent does not serve it and does not advertise file_cache_evict.
	FileCache filecache.Sys

	// Central logging (CR-10). LogsHTTP is the log sender's own client (nil:
	// its own default). Journal starts the journal follower (nil:
	// journalctl). Monotonic reads CLOCK_MONOTONIC, for journal entries, and
	// BootID the kernel's boot id, read once at start for journal entries,
	// the heartbeat and start-up reporting (nil: the real ones). LaunchLogEvery is how often
	// the launch log is read (default 250 ms).
	LogsHTTP       *http.Client
	Journal        logship.JournalStart
	Monotonic      func() (time.Duration, error)
	BootID         func() string
	LaunchLogEvery time.Duration

	// Start-up reporting. Systemd is asked once, at start, for the userspace
	// start and the soft-restart count (nil: not asked, the fields are
	// absent; a Docker install never asks, it would see its container).
	// MarksEvery is how often the marks file is read while it is watched
	// (default 2 s).
	Systemd    startup.Systemd
	MarksEvery time.Duration

	// Timing; zero values get the contract values.
	RegisterRetry   time.Duration // after a refusal; default 60s
	BindRetry       time.Duration // after a failed bind of port 50060; default 5s (grows to 60s)
	RecoverRetry    time.Duration // after a failed launch re-adoption; default 1s
	ErrorRetry      time.Duration // after a network error; default 5s (grows to 60s)
	ProgramsRetry   time.Duration // after a failed program check; default 60s
	LaunchTick      time.Duration
	ProbeEvery      time.Duration
	ProbeTimeout    time.Duration
	StatsMin        time.Duration
	ProgramsRetryIn time.Duration // pause between download attempts; default 2s
	// ExporterPorts checked at start; default 50070, 50071.
	ExporterPorts []int
	// WorkerPort and HostPort override 50052 and 50061 (tests only).
	WorkerPort, HostPort int
}

// Agent is one running agent.
type Agent struct {
	opt  Options
	deps Deps
	log  *slog.Logger
	// srcMu guards srcSeen: when each source address of an allowed API
	// call was last named in the log (noteSource).
	srcMu   sync.Mutex
	srcSeen map[string]time.Duration

	startID   string
	startedAt time.Duration
	// bootID is the kernel's boot id, read once at start ("" when it cannot
	// be read; it is then left out of the heartbeat).
	bootID    string
	potluckIP net.IP
	base      string

	Launch   *launch.Manager
	Programs *programs.Manager
	Source   *modelsrc.Source
	Stats    *stats.Collector
	Export   *exporters.Supervisor
	// Ship is the log shipper (CR-10); nil with --logs off.
	Ship *logship.Shipper
	// Start is this process's start-up report: its marks on the machine's
	// own time axis, sent to Potluck until acknowledged.
	Start *startup.Recorder
	// Cache drops model files from the page cache (POST /cache/evict, 6.13);
	// nil on a system without the calls or a node without model files (push).
	Cache *filecache.Evictor
	// Busy is the node's busy state and file (6.14). busyMu orders every
	// update; busyPending counts launch requests accepted and not yet
	// answered, so the file exists before the launch's unit is started.
	Busy        *busyfile.File
	busyMu      sync.Mutex
	busyPending int
	// launchTail follows launch output; its starting point is taken when the
	// re-adoption is done, before anything waiting for it goes on.
	launchTail *logship.LaunchTail

	mu          sync.Mutex
	ident       identity.Identity
	virtualID   string // the id last logged as coming from a virtual interface
	identOK     bool
	registered  bool
	paused      bool
	hbEvery     time.Duration
	seq         uint64
	lastErr     string
	lastErrAt   time.Duration
	platform    string
	pipelineOK  bool
	devs        []agentproto.Device
	devsAt      time.Duration
	devRead     sync.Mutex
	gpus        []hw.GPUInfo
	pipeCancel  context.CancelFunc
	pipeGen     int
	answerProgs []agentproto.ProgramEntry
	devErr      string // set while llama-server lists no device (no access)

	recovered     chan struct{} // closed once the launch re-adoption is done
	recoverOnce   sync.Once
	localBound    bool // Run's goroutine only
	nodeBound     bool
	updateSkipped bool
	leftoversDone bool   // Run's goroutine only
	regWhy        string // why the next registration is sent (log only; Run's goroutine)
	// regSent and regRecv bracket the last registration on the agent's
	// clock (Run's goroutine): its answer's server time anchors the log
	// lines' time (CR-10).
	regSent, regRecv time.Duration
	// runCtx is Run's context: the exporters live as long as the agent
	// process, not as long as one registration.
	runCtx context.Context
	// statusWait is how long GET /status waits for the launch re-adoption
	// before it answers 503; default 2 s.
	statusWait time.Duration
}

// New builds an agent. Call Run.
func New(opt Options, deps Deps) (*Agent, error) {
	host, _, err := net.SplitHostPort(opt.Potluck)
	if err != nil {
		return nil, fmt.Errorf("--potluck %q: %w", opt.Potluck, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("--potluck %q: the host must be an IP address", opt.Potluck)
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, p := range opt.AllowFrom {
		if p.Bits() == 0 {
			deps.Logger.Warn("--allow-from covers every address: anyone who can reach this machine may start and stop launches; "+
				"name the Docker gateway network or Potluck's address instead", "allow_from", p.String())
		}
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if deps.RegisterRetry == 0 {
		deps.RegisterRetry = 60 * time.Second
	}
	if deps.BindRetry == 0 {
		deps.BindRetry = 5 * time.Second
	}
	if deps.RecoverRetry == 0 {
		deps.RecoverRetry = time.Second
	}
	if deps.ErrorRetry == 0 {
		deps.ErrorRetry = 5 * time.Second
	}
	if deps.ProgramsRetry == 0 {
		deps.ProgramsRetry = 60 * time.Second
	}
	if deps.ExporterPorts == nil {
		deps.ExporterPorts = []int{agentproto.PortNodeExporter, agentproto.PortNvidiaExporter}
	}
	if deps.PortFree == nil {
		deps.PortFree = launch.BindFree
	}
	if deps.DevicesRun == nil {
		deps.DevicesRun = devices.ExecRunner
	}
	b := make([]byte, 3)
	rand.Read(b)
	startID := hex.EncodeToString(b)
	// The log shipper first: every component logs through its handler. It
	// copies each line into the ring and never waits (CR-10).
	var ship *logship.Shipper
	if !opt.LogsOff {
		ship = logship.New(logship.Config{Base: "http://" + opt.Potluck, StartID: startID, Now: deps.Clock.Now,
			HTTP: deps.LogsHTTP, BufferBytes: opt.LogBufferBytes})
		deps.Logger = slog.New(logship.NewHandler(deps.Logger.Handler(), ship))
		ship.SetLogger(deps.Logger)
	}
	bootID := deps.BootID
	if bootID == nil {
		bootID = osx.BootID
	}
	a := &Agent{Ship: ship, opt: opt, deps: deps, log: deps.Logger, startID: startID,
		startedAt: deps.Clock.Now(), bootID: bootID(), potluckIP: ip, base: "http://" + opt.Potluck, hbEvery: 2 * time.Second,
		recovered: make(chan struct{})}
	a.Start = startup.NewRecorder(a.bootID, a.startedAt)

	progRoot := filepath.Join(opt.DataDir, "programs")
	a.Programs = programs.New(programs.Config{Root: progRoot, Release: opt.Release, BaseURL: a.base,
		DiskFree: deps.DiskFree, MissingLibs: deps.Hardware.MissingLibs, RetryWait: deps.ProgramsRetryIn,
		LibChecked: []string{agentproto.ProgramLlamaServer, agentproto.ProgramRPCServer}, Logger: deps.Logger})

	a.Source = modelsrc.New(modelsrc.Config{Spec: opt.ModelSource, Dir: opt.ModelsDir,
		Mount:  opt.ModelSource.Kind == modelsrc.KindShare && opt.Install == agentproto.InstallBare,
		NodeIP: func() string { a.mu.Lock(); defer a.mu.Unlock(); return a.ident.Addr },
		Every:  deps.ProbeEvery, Timeout: deps.ProbeTimeout, Clock: deps.Clock, Mounter: deps.Mounter,
		Prober: deps.Prober, Logger: deps.Logger, OnError: a.setError,
		OnReachable:  func() { a.mark(agentproto.StartupMarkShareMounted) },
		LaunchExists: func() bool { return a.Launch.CurrentID() != nil }})

	a.Launch = launch.New(launch.Config{
		StateDir: opt.StateDir, LogDir: filepath.Join(opt.DataDir, "launch"), ProgramRoot: progRoot,
		ModelDir: opt.ModelsDir, PushOnly: opt.ModelSource.Kind == modelsrc.KindPush, LibraryPath: opt.LDLibraryPath,
		WatchdogFloorMiB: opt.WatchdogFloorMiB, SharedMemory: opt.SharedMemory, Tick: deps.LaunchTick,
		WorkerPort: deps.WorkerPort, HostPort: deps.HostPort,
	}, launch.Deps{Clock: deps.Clock, Runner: deps.Runner, Mem: deps.Mem, Sockets: deps.Sockets,
		Procs: deps.Procs, KLog: deps.KLog, PortFree: deps.PortFree, HostHealthy: deps.HostHealthy, ReadChars: deps.ReadChars,
		Gate: a, Device: a.workerDevice, Programs: a.Programs, Logger: deps.Logger, OnError: a.setError,
		OnExit: func(agentproto.LaunchState) { a.afterLaunchExit() }})

	if ship != nil {
		a.launchTail = &logship.LaunchTail{Ship: ship, Current: a.Launch.CurrentLog, Every: deps.LaunchLogEvery}
	}
	if sys := deps.FileCache; sys != nil && opt.ModelSource.Kind != modelsrc.KindPush {
		a.Cache = &filecache.Evictor{Root: opt.ModelsDir, Sys: sys, Now: func() time.Time { return time.Unix(0, 0).Add(deps.Clock.Now()) }, Logger: deps.Logger, Busy: a.launchBusy}
	}
	a.Busy = &busyfile.File{Path: opt.BusyFile, Logger: deps.Logger}
	a.Export = &exporters.Supervisor{Runner: deps.Runner, Path: a.Programs.Path, Logger: deps.Logger}
	a.Stats = stats.New(stats.Config{Now: deps.Clock.Now, MinInterval: deps.StatsMin, Logger: deps.Logger,
		OnScrape: func(string) { a.mark(agentproto.StartupMarkFirstStats) },
		Primary:  func() string { a.mu.Lock(); defer a.mu.Unlock(); return a.ident.RouteIface }})
	a.Export.OnStart = func(program string) { a.Stats.Restarted(exporters.SourceName(program)) }
	return a, nil
}

// mark records a start-up mark at the agent's clock now, the first time.
func (a *Agent) mark(name string) {
	if now := a.deps.Clock.Now(); a.Start.Mark(name, now) {
		a.log.Debug("start-up mark", "mark", name, "s", secs(now))
	}
}

func (a *Agent) setError(msg string) {
	a.mu.Lock()
	a.lastErr, a.lastErrAt = msg, a.deps.Clock.Now()
	a.mu.Unlock()
	a.log.Warn("last_error", "msg", msg)
}

// --- launch.Gate ---

// Paused implements launch.Gate.
// launchBusy says whether a launch is starting, running or stopping, for the
// file cache eviction, which leaves a node with a launch alone.
func (a *Agent) launchBusy() (bool, string) {
	if id := a.Launch.CurrentID(); id != nil {
		return true, "launch " + *id + " exists on this node"
	}
	return false, ""
}

func (a *Agent) Paused() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.paused }

// Release implements launch.Gate.
func (a *Agent) Release() string { return a.opt.Release }

// ProgramsReady is true once the programs are verified, the exporters were
// started and the devices were read (5.3 steps 4 to 7).
func (a *Agent) ProgramsReady() bool {
	a.mu.Lock()
	ok := a.pipelineOK
	a.mu.Unlock()
	return ok && a.Programs.Ready()
}

// ModelSourceReachable implements launch.Gate.
func (a *Agent) ModelSourceReachable() bool { return a.Source.Reachable() }

// WaitDevicesRead implements launch.Gate.
func (a *Agent) WaitDevicesRead() { a.devRead.Lock(); a.devRead.Unlock() }

// workerDevice is the device a worker of this node serves, as Potluck picks
// it from the heartbeat (contract 11: one device per node): the first device
// of the platform's backend, else the first one. It is the launch manager's
// device for the would_not_fit check (CR-9).
func (a *Agent) workerDevice() (agentproto.Device, bool) {
	a.mu.Lock()
	devs, platform := append([]agentproto.Device(nil), a.devs...), a.platform
	a.mu.Unlock()
	if len(devs) == 0 {
		return agentproto.Device{}, false
	}
	devs = liveDevices(devs, a.Stats.Snapshot()) // livedev.go: a discrete GPU's current free figure
	backend := platform[strings.LastIndex(platform, "-")+1:]
	for _, d := range devs {
		if strings.EqualFold(d.Backend, backend) {
			return d, true
		}
	}
	return devs[0], true
}

// leaveOnPurpose deregisters when the leave marker exists (CR-9): a node
// that was taken out on purpose shows as left, not as lost. One attempt,
// bounded by leaveTimeout; a failure only delays the same outcome (the node
// is lost after the heartbeat silence).
func (a *Agent) leaveOnPurpose() {
	if a.opt.LeaveMarker == "" {
		return
	}
	if _, err := os.Stat(a.opt.LeaveMarker); err != nil {
		return
	}
	a.mu.Lock()
	id, ok := a.ident.ID, a.identOK
	a.mu.Unlock()
	if !ok || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaveTimeout)
	defer cancel()
	resp, err := a.post(ctx, fmt.Sprintf(agentproto.PathDeregisterFmt, id), struct{}{})
	if err != nil {
		a.log.Warn("leave marker found, but the deregister call failed; Potluck will show the node as lost", "marker", a.opt.LeaveMarker, "err", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		a.log.Info("leave marker found: deregistered before stopping", "marker", a.opt.LeaveMarker, "status", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		// The uninstall script deregistered first: the node has already left.
		a.log.Info("leave marker found: Potluck no longer knows this node (already deregistered)", "marker", a.opt.LeaveMarker, "status", resp.StatusCode)
	default:
		a.log.Warn("leave marker found, but Potluck refused the deregister call; it will show the node as lost", "marker", a.opt.LeaveMarker, "status", resp.StatusCode)
	}
}

// advertised is the registration's addr: --advertise-addr when set, else
// the agent's own address.
func (a *Agent) advertised(own string) string {
	if a.opt.AdvertiseAddr != "" {
		return a.opt.AdvertiseAddr
	}
	return own
}

// ID returns the node id, "" until resolved.
func (a *Agent) ID() string { a.mu.Lock(); defer a.mu.Unlock(); return a.ident.ID }

// Addr returns the node's address that reaches Potluck.
func (a *Agent) Addr() string { a.mu.Lock(); defer a.mu.Unlock(); return a.ident.Addr }

// ResolveIdentity computes the id (4.1).
func (a *Agent) ResolveIdentity() error {
	id, err := identity.Resolve(a.deps.Net, a.potluckIP, a.opt.Install == agentproto.InstallDocker)
	if err != nil {
		a.setError("no identity: " + err.Error())
		return err
	}
	a.mu.Lock()
	logVirtual := id.Virtual && a.virtualID != id.ID
	if logVirtual {
		a.virtualID = id.ID
	}
	a.ident, a.identOK = id, true
	a.mu.Unlock()
	if logVirtual {
		// Said once: the start loop resolves the id on every round.
		a.log.Info("node id from a virtual interface: it is only stable when the container's MAC address is fixed (compose mac_address)",
			"id", id.ID, "iface", id.Iface)
	}
	a.Launch.SetBindAddr(id.Addr)
	return nil
}

// Prepare re-hashes the program folder.
func (a *Agent) Prepare(ctx context.Context) error {
	if _, err := a.Programs.Prepare(); err != nil {
		return fmt.Errorf("program folder: %w", err)
	}
	return nil
}

// RecoverLaunch re-adopts a launch left by an earlier agent process (one
// attempt). Until it has worked the API answers 503 and the agent does not
// register: a GET /launch answering "none" or a registration without the
// launch id would make Potluck fail a healthy load.
func (a *Agent) RecoverLaunch(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := a.Launch.Recover(c); err != nil {
		return err
	}
	a.recoverOnce.Do(func() {
		// last_error of an abnormal exit is kept across an agent restart: it
		// is in the launch's exit record. Any other last_error is not stored.
		if text, at, ok := a.Launch.LastExitError(); ok {
			a.mu.Lock()
			if a.lastErr == "" {
				a.lastErr, a.lastErrAt = text, at
			}
			a.mu.Unlock()
		}
		// The launch tail's starting point: a re-adopted launch is followed
		// from its end. Taken before close(a.recovered), so no launch can
		// start in between (registration waits for the close).
		if a.launchTail != nil {
			a.launchTail.Begin()
		}
		close(a.recovered)
	})
	return nil
}

func (a *Agent) launchRecovered() bool {
	select {
	case <-a.recovered:
		return true
	default:
		return false
	}
}

func (a *Agent) recoverLoop(ctx context.Context) {
	for {
		err := a.RecoverLaunch(ctx)
		if err == nil {
			return
		}
		a.setError("launch re-adoption: " + err.Error())
		if !sleep(ctx, a.deps.RecoverRetry) {
			return
		}
	}
}

// bindAPI binds what is not bound yet of port 50060: 127.0.0.1 at once,
// the node's address once the identity is known. An address that cannot be
// bound is tried again by Run; the agent never exits over it.
func (a *Agent) bindAPI(ctx context.Context) error {
	if a.deps.Listen == nil {
		return nil
	}
	port := fmt.Sprint(agentproto.PortAgent)
	if !a.localBound {
		l, err := a.deps.Listen(net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			return fmt.Errorf("port %s is taken: %v", port, err)
		}
		a.localBound = true
		serveOn(ctx, l, a.LocalHandler())
	}
	addr := a.Addr()
	if addr == "" || a.nodeBound {
		return nil
	}
	a.nodeBound = true
	if addr == "127.0.0.1" {
		a.log.Warn("Potluck is reached over loopback; the Potluck API is not served")
		return nil
	}
	l, err := a.deps.Listen(net.JoinHostPort(addr, port))
	if err != nil {
		a.nodeBound = false
		return fmt.Errorf("cannot listen on %s port %s: %v", addr, port, err)
	}
	serveOn(ctx, l, a.Handler())
	return nil
}

func serveOn(ctx context.Context, l net.Listener, h http.Handler) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)
	go func() { <-ctx.Done(); srv.Close() }()
}

// checkPorts verifies the exporter ports are free (section 3).
// A port still held by the agent's own exporter unit (one that an earlier
// agent process left and that could not be stopped) is not a reason to stay
// out of the pool: the agent never refuses to start over its own leftovers.
// The supervisor replaces that unit when it can, and reads it until then.
func (a *Agent) checkPorts(ctx context.Context) error {
	own := []string{agentproto.NodeExporterUnit, agentproto.NvidiaExporterUnit}
	for i, p := range a.deps.ExporterPorts {
		err := a.deps.PortFree("127.0.0.1", p)
		if err == nil {
			continue
		}
		if i < len(own) && a.ownUnitAlive(ctx, own[i]) {
			a.log.Warn("exporter port is held by this agent's own leftover unit; going on", "port", p, "unit", own[i])
			continue
		}
		return fmt.Errorf("port %d is taken: %v", p, err)
	}
	return nil
}

func (a *Agent) ownUnitAlive(ctx context.Context, unit string) bool {
	if !a.deps.Runner.Survives() {
		return false
	}
	units, err := a.deps.Runner.List(ctx, unit)
	if err != nil {
		return false
	}
	for _, u := range units {
		if u == unit {
			return true
		}
	}
	return false
}

// Run runs the agent until ctx ends. It serves port 50060 itself when
// Deps.Listen is set; otherwise the caller serves Handler and LocalHandler.
func (a *Agent) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer a.leaveOnPurpose() // first of all, while the process still has its 3 s
	a.mu.Lock()
	a.runCtx = ctx
	a.mu.Unlock()
	a.askSystemd(ctx)
	// A busy file left by an earlier agent process goes before the launch
	// re-adoption; watchBusy writes it again if a launch is adopted.
	a.Busy.RemoveLeftover()
	wg.Add(1)
	go func() { defer wg.Done(); a.watchBusy(ctx) }()
	if a.opt.MarksFile != "" {
		wg.Add(1)
		go func() { defer wg.Done(); a.watchMarks(ctx) }()
	}
	// An existing launch first: its lease and its watchdog need no identity,
	// no free port, no registration and no Potluck.
	wg.Add(1)
	go func() { defer wg.Done(); a.recoverLoop(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.Launch.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.Stats.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.reviveLoop(ctx) }()
	a.startLogShipping(ctx, &wg)

	// Identity and ports, retried until they work.
	bindWait := a.deps.BindRetry
	for {
		wait := a.deps.RegisterRetry
		idErr := a.ResolveIdentity()
		err := a.bindAPI(ctx)
		switch {
		case err != nil:
			a.setError("registration not attempted: " + err.Error())
			wait = bindWait
			if bindWait *= 2; bindWait > 60*time.Second {
				bindWait = 60 * time.Second
			}
		case idErr != nil:
			err = idErr
		default:
			// Exporter units of an earlier agent process hold the exporter
			// ports; they are ours, so they go before the check. Once per
			// process, before this process has started an exporter: later
			// registrations never come here.
			if a.deps.Runner.Survives() && !a.leftoversDone {
				a.leftoversDone = true
				a.Export.StopLeftovers(ctx)
			}
			if err = a.checkPorts(ctx); err != nil {
				a.setError("registration not attempted: " + err.Error())
			}
		}
		if err == nil {
			break
		}
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
	}
	// Registration names the launch, so it waits for the re-adoption.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.recovered:
	}
	if err := a.Prepare(ctx); err != nil {
		a.setError(err.Error())
	}
	if err := a.Source.Start(ctx); err != nil {
		a.setError("share mount: " + err.Error())
	}
	wg.Add(1)
	go func() { defer wg.Done(); a.Source.Run(ctx) }()

	errWait := a.deps.ErrorRetry
	for ctx.Err() == nil {
		ans, err := a.register(ctx)
		if err != nil {
			a.log.Warn("register failed", "err", err)
			a.setError("registration failed: " + err.Error())
			if !sleep(ctx, errWait) {
				break
			}
			if errWait *= 2; errWait > 60*time.Second {
				errWait = 60 * time.Second
			}
			continue
		}
		errWait = a.deps.ErrorRetry
		if !ans.Accepted {
			a.refused(ctx, ans)
			if !sleep(ctx, a.deps.RegisterRetry) {
				break
			}
			continue
		}
		a.accepted(ctx, ans)
		a.heartbeatLoop(ctx)
	}
	a.mu.Lock()
	if a.pipeCancel != nil {
		a.pipeCancel()
	}
	a.mu.Unlock()
	a.Export.StopAll()
	return ctx.Err()
}

// startLogShipping starts the log sender and the readers that feed it
// (CR-10), each in its own goroutine. None of them is waited for by any
// other part of the agent.
func (a *Agent) startLogShipping(ctx context.Context, wg *sync.WaitGroup) {
	if a.Ship == nil {
		return
	}
	wg.Add(1)
	go func() { defer wg.Done(); a.Ship.Run(ctx) }()
	// Launch output: after the re-adoption, so a launch of an earlier agent
	// process is followed from its end.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-a.recovered:
		}
		a.launchTail.Run(ctx)
	}()
	// The system log: the machine's own journal on a bare install; on a
	// Docker install only a journal folder given with --journal-dir.
	if a.opt.Install == agentproto.InstallBare || a.opt.JournalDir != "" {
		mono := a.deps.Monotonic
		if mono == nil {
			mono = osx.MonotonicNow
		}
		j := &logship.Journal{Ship: a.Ship, Start: a.deps.Journal, Dir: a.opt.JournalDir, Monotonic: mono, BootID: a.bootID}
		wg.Add(1)
		go func() { defer wg.Done(); j.Run(ctx) }()
	}
}

// reviveLoop starts an exporter again that should run and does not (its
// process is gone and its scrape fails); the supervisor limits it to one
// start per exporter every 30 s.
func (a *Agent) reviveLoop(ctx context.Context) {
	for sleep(ctx, 5*time.Second) {
		a.Export.Revive(a.Stats.Failing)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RegisterRequest builds the registration body (4.2).
func (a *Agent) RegisterRequest() agentproto.RegisterRequest {
	a.mu.Lock()
	id := a.ident
	a.mu.Unlock()
	gpus := a.deps.Hardware.GPUs()
	a.mu.Lock()
	a.gpus = gpus
	a.mu.Unlock()
	req := agentproto.RegisterRequest{ID: id.ID, Name: a.opt.Name, AgentRelease: a.opt.Release,
		AgentStartID: a.startID, OS: runtime.GOOS, Arch: runtime.GOARCH, Install: a.opt.Install,
		Addr: a.advertised(id.Addr), Backends: a.deps.Hardware.Backends(gpus), RAMOnly: a.opt.RAMOnly,
		ModelSource: a.opt.ModelSource.Wire(), Programs: a.Programs.Held(), LaunchID: a.Launch.CurrentID(),
		ForeignProcesses: a.foreignProcesses(), WatchdogFloorMiB: a.Launch.WatchdogFloorMiB(), Startup: a.Start.Report()}
	if a.Cache != nil {
		req.Capabilities = append(req.Capabilities, agentproto.CapFileCacheEvict)
	}
	// A host reads its model from the node's own model folder (share or
	// dir); a push node has none and cannot host (6.2).
	if a.opt.ModelSource.Kind != modelsrc.KindPush {
		req.Capabilities = append(req.Capabilities, agentproto.CapHostLaunch)
	}
	for _, g := range gpus {
		req.GPUs = append(req.GPUs, g.GPU)
	}
	if mi, err := a.deps.Mem.MemInfo(); err == nil {
		req.MemTotalMiB = mi.TotalMiB
	}
	req.FillEmpty()
	return req
}

// foreignProcesses lists rpc-server and llama-server processes the agent did
// not start (4.2).
func (a *Agent) foreignProcesses() []agentproto.ForeignProcess {
	out := []agentproto.ForeignProcess{}
	if a.deps.Procs == nil {
		return out
	}
	list, err := a.deps.Procs.List()
	if err != nil {
		return out
	}
	self, launchPID := a.deps.Procs.Self(), a.Launch.PID()
	for _, p := range list {
		base := osx.ExeBase(p.Exe)
		if base != agentproto.ProgramRPCServer && base != agentproto.ProgramLlamaServer {
			continue
		}
		if p.PID == launchPID || p.PPID == self || p.PID == self || strings.Contains(p.Cgroup, agentproto.LaunchUnitPrefix) {
			continue
		}
		out = append(out, agentproto.ForeignProcess{PID: p.PID, Exe: p.Exe})
	}
	return out
}

func (a *Agent) post(ctx context.Context, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.deps.HTTP.Do(req)
}

func (a *Agent) register(ctx context.Context) (agentproto.RegisterAnswer, error) {
	var ans agentproto.RegisterAnswer
	body := a.RegisterRequest()
	a.regSent = a.deps.Clock.Now()
	resp, err := a.post(ctx, agentproto.PathRegister, body)
	a.regRecv = a.deps.Clock.Now()
	if err != nil {
		return ans, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ans, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		return ans, fmt.Errorf("answer: %w", err)
	}
	return ans, nil
}

// refused handles a refusal: start nothing, keep the reason on /status, and
// update on agent_too_old when no launch exists.
func (a *Agent) refused(ctx context.Context, ans agentproto.RegisterAnswer) {
	a.mu.Lock()
	a.registered = false
	a.pipelineOK = false
	if a.pipeCancel != nil {
		a.pipeCancel()
		a.pipeCancel = nil
	}
	a.mu.Unlock()
	a.Export.StopAll()
	a.Stats.SetSources(nil)
	a.setError(ans.Reason + ": " + ans.Message)
	a.log.Warn("registration refused", "reason", ans.Reason, "message", ans.Message)
	if a.Ship != nil {
		a.Ship.Refused(ans.ServerTimeUnixMs, a.regSent, a.regRecv)
	}
	if ans.Reason == agentproto.ReasonAgentTooOld && a.deps.Updater != nil {
		if a.Launch.CurrentID() != nil {
			a.log.Info("update waits for the launch to end", "required", ans.RequiredRelease)
			return
		}
		err := a.deps.Updater.Update(ctx, ans.RequiredRelease)
		if errors.Is(err, ErrUpdateNotRepeated) {
			if a.updateSkipped {
				return // said once
			}
			a.updateSkipped = true
		}
		if err != nil {
			a.setError("update to " + ans.RequiredRelease + " failed: " + err.Error())
		}
	}
}

func (a *Agent) accepted(ctx context.Context, ans agentproto.RegisterAnswer) {
	a.mu.Lock()
	a.registered, a.paused, a.platform = true, ans.Paused, ans.Platform
	if ans.HeartbeatEveryS > 0 {
		a.hbEvery = time.Duration(ans.HeartbeatEveryS * float64(time.Second))
	}
	if a.lastErr != "" && (strings.HasPrefix(a.lastErr, "registration failed") || isRefusal(a.lastErr)) {
		a.lastErr = ""
	}
	a.mu.Unlock()
	a.Launch.SetCUDA(strings.HasSuffix(ans.Platform, "-"+agentproto.BackendCUDA))
	if a.Start.Mark(agentproto.StartupMarkRegistered, a.regRecv) {
		a.log.Debug("start-up mark", "mark", agentproto.StartupMarkRegistered)
	}
	a.Start.Answered(ans.StartupAck, true)
	if a.Ship != nil {
		a.Ship.Registered(a.ID(), ans.Logs, ans.ServerTimeUnixMs, a.regSent, a.regRecv)
	}
	why := a.regWhy
	if why == "" {
		why = "agent start"
	}
	a.regWhy = ""
	a.log.Info("registered", "release", ans.Release, "platform", ans.Platform, "paused", ans.Paused, "why", why)
	if ans.StopLaunch != nil {
		if _, err := a.Launch.Stop(*ans.StopLaunch, false); err != nil {
			a.log.Warn("stop_launch", "launch_id", *ans.StopLaunch, "err", err)
		}
	}
	a.refetch(ctx, ans)
	a.startPipeline(ctx, ans)
}

// refetch installs the agent binary Potluck serves for this release when it
// is another build than the running one (5.4): only while no launch exists,
// once per (release, sha256) (the updater's attempt file), and a failure
// leaves the agent running as it is.
func (a *Agent) refetch(ctx context.Context, ans agentproto.RegisterAnswer) {
	r, ok := a.deps.Updater.(Refetcher)
	if !ok || ans.AgentSHA256 == "" {
		return
	}
	if a.Launch.CurrentID() != nil {
		a.log.Info("agent re-fetch waits for the launch to end", "sha256", ans.AgentSHA256)
		return
	}
	err := r.Refetch(ctx, ans.Release, ans.AgentSHA256)
	if errors.Is(err, ErrUpdateNotRepeated) {
		if a.updateSkipped {
			return
		}
		a.updateSkipped = true
	}
	if err != nil {
		a.log.Warn("agent re-fetch of this release failed; running on", "sha256", ans.AgentSHA256, "err", err)
	}
}

func isRefusal(s string) bool {
	for _, r := range []string{agentproto.ReasonAgentTooOld, agentproto.ReasonPotluckTooOld, agentproto.ReasonUnsupportedHardware,
		agentproto.ReasonDuplicateID, agentproto.ReasonForeignProcess, agentproto.ReasonRemoved} {
		if strings.HasPrefix(s, r+":") {
			return true
		}
	}
	return false
}

// startPipeline runs 5.3 steps 2 to 7 in the background, replacing a
// pipeline still running from an earlier registration.
func (a *Agent) startPipeline(ctx context.Context, ans agentproto.RegisterAnswer) {
	a.mu.Lock()
	if a.pipeCancel != nil {
		a.pipeCancel()
	}
	pctx, cancel := context.WithCancel(ctx)
	a.pipeCancel = cancel
	a.pipeGen++
	gen := a.pipeGen
	a.answerProgs = ans.Programs
	a.mu.Unlock()
	go a.pipeline(pctx, gen, ans)
}

func (a *Agent) pipeline(ctx context.Context, gen int, ans agentproto.RegisterAnswer) {
	for ctx.Err() == nil {
		err := a.Programs.Sync(ctx, ans.Programs, a.waitIdle)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.mu.Lock()
			a.pipelineOK = false
			a.mu.Unlock()
			a.setError("programs: " + err.Error())
			if !sleep(ctx, a.deps.ProgramsRetry) {
				return
			}
			continue
		}
		// 5. exporters. They are tied to the agent's run, not to this
		// pipeline: a later registration cancels the pipeline, and its own
		// pipeline keeps the exporters whose spec is unchanged.
		a.mu.Lock()
		current, runCtx := a.pipeGen == gen, a.runCtx
		a.mu.Unlock()
		if !current {
			return
		}
		if runCtx == nil {
			runCtx = context.Background()
		}
		srcs, refused := a.Export.Set(runCtx, ans.Exporters)
		for _, r := range refused {
			a.setError(r)
		}
		a.Stats.SetSources(srcs)
		if len(srcs) > 0 {
			a.mark(agentproto.StartupMarkExporterUp)
		}
		// 6. devices. An empty list (no access to the GPU device node) keeps
		// programs_ready false, reports why in programs_error and is read
		// again after ProgramsRetry; any other device error is only logged.
		for errors.Is(a.readDevices(ctx), devices.ErrNoDevices) {
			a.mu.Lock()
			a.pipelineOK = false
			a.mu.Unlock()
			if !sleep(ctx, a.deps.ProgramsRetry) {
				return
			}
		}
		// 7. ready
		a.mu.Lock()
		ready := a.pipeGen == gen
		if ready {
			a.pipelineOK = true
		}
		a.mu.Unlock()
		if ready && a.Programs.Ready() {
			a.mark(agentproto.StartupMarkProgramsReady)
		}
		a.Programs.DeleteOldReleases(a.Launch.CurrentID() != nil)
		return
	}
}

func (a *Agent) waitIdle(ctx context.Context) bool {
	for a.Launch.Busy() {
		if !sleep(ctx, 500*time.Millisecond) {
			return false
		}
	}
	return true
}

func (a *Agent) cuda() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.HasSuffix(a.platform, "-"+agentproto.BackendCUDA)
}

// readDevices reads the device list with llama-server (rpc-server has no
// --list-devices, also on a worker node); a POST /launch waits meanwhile.
func (a *Agent) readDevices(ctx context.Context) error {
	path, ok := a.Programs.Path(agentproto.ProgramLlamaServer)
	if !ok {
		return errors.New("llama-server is not verified")
	}
	a.devRead.Lock()
	defer a.devRead.Unlock()
	var env []string
	if a.cuda() {
		env = append(env, "CUDA_DEVICE_ORDER=PCI_BUS_ID")
	}
	a.mu.Lock()
	gpus := a.gpus
	a.mu.Unlock()
	devs, err := devices.Read(ctx, a.deps.DevicesRun, path, env, hw.DeviceGPUs(gpus), 30*time.Second)
	// shares_system_memory is reported as the launch check uses it (CR-9):
	// true for every device that is held to MemAvailable, so Potluck's
	// placement figure is min(device free, MemAvailable - floor) there too.
	for i := range devs {
		devs[i].SharesSystemMemory = launch.TestsSystemMemory(a.opt.SharedMemory, devs[i])
	}
	if errors.Is(err, devices.ErrNoDevices) {
		a.mu.Lock()
		a.devErr = err.Error()
		a.devs, a.devsAt = nil, a.deps.Clock.Now()
		a.mu.Unlock()
		a.setError(err.Error())
		return err
	}
	if err != nil {
		a.setError("devices: " + err.Error())
		return err
	}
	a.mu.Lock()
	a.devs, a.devsAt, a.devErr = devs, a.deps.Clock.Now(), ""
	a.mu.Unlock()
	var list []string
	for _, d := range devs {
		list = append(list, fmt.Sprintf("%s (%d MiB, %d MiB free)", d.Name, d.TotalMiB, d.FreeMiB))
	}
	a.log.Info("devices read", "count", len(devs), "devices", strings.Join(list, ", "))
	return nil
}

// programsError is the heartbeat's programs_error: a program fault first,
// else the no-device-access message.
func (a *Agent) programsError() *string {
	if e := a.Programs.Error(); e != nil {
		return e
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.devErr == "" {
		return nil
	}
	s := a.devErr
	return &s
}

func (a *Agent) afterLaunchExit() {
	a.readDevices(context.Background())
	a.Programs.DeleteOldReleases(false)
}

// Heartbeat builds the heartbeat body (4.3).
func (a *Agent) Heartbeat() agentproto.Heartbeat {
	now := a.deps.Clock.Now()
	a.mu.Lock()
	a.seq++
	// The agent's clock counts from kernel boot (CLOCK_BOOTTIME), so it is
	// also the machine's own uptime; on a Docker install, the host's.
	hostUp := float64(now.Milliseconds()) / 1000
	hb := agentproto.Heartbeat{AgentStartID: a.startID, Seq: a.seq, UptimeS: float64((now - a.startedAt).Milliseconds()) / 1000,
		HostUptimeS: &hostUp, BootID: a.bootID}
	devs := append([]agentproto.Device{}, a.devs...)
	devsAt := a.devsAt
	a.mu.Unlock()
	for i := range devs {
		devs[i].ReadAgeS = float64((now - devsAt).Milliseconds()) / 1000
	}
	hb.Role, hb.LaunchID = a.Launch.Role(), a.Launch.CurrentID()
	hb.WatchdogFloorMiB = a.Launch.WatchdogFloorMiB()
	if ls := a.Launch.State(); ls.State == agentproto.StateStopping {
		hb.StopReason, hb.StopMessage = ls.StopReason, ls.StopMessage
	}
	hb.ProgramsReady, hb.ProgramsError = a.ProgramsReady(), a.programsError()
	st := a.Stats.Snapshot()
	hb.ModelSource, hb.Devices = a.Source.State(), liveDevices(devs, st) // livedev.go
	if mi, err := a.deps.Mem.MemInfo(); err == nil {
		hb.MemAvailableMiB = mi.AvailableMiB
	}
	hb.Stats = &st
	if a.Ship != nil {
		hb.Logs = a.Ship.Figures() // nil while nothing is shipped
	}
	hb.Startup = a.Start.ForHeartbeat() // nil once Potluck has every mark
	return hb
}

// heartbeatLoop sends heartbeats until Potluck asks for a new registration
// (410, or a different release) or ctx ends. The three failure cases (4.3):
// a heartbeat that cannot be sent changes nothing; 410 re-registers at once
// and does not touch the launch; a different release re-registers.
func (a *Agent) heartbeatLoop(ctx context.Context) {
	a.mu.Lock()
	every := a.hbEvery
	a.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	// One WARN at the first heartbeat that fails, then nothing until one
	// works again.
	var failedAt time.Duration
	failing := false
	failed := func(why any) {
		a.log.Debug("heartbeat failed", "why", why)
		if !failing {
			failing, failedAt = true, a.deps.Clock.Now()
			a.log.Warn("Potluck unreachable: the heartbeat failed; trying again every heartbeat", "potluck", a.opt.Potluck, "why", why)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		id := a.ident.ID
		a.mu.Unlock()
		hctx, cancel := context.WithTimeout(ctx, every+3*time.Second)
		body := a.Heartbeat()
		sent := a.deps.Clock.Now()
		resp, err := a.post(hctx, fmt.Sprintf(agentproto.PathHeartbeatFmt, id), body)
		if err != nil {
			cancel()
			if ctx.Err() == nil {
				failed(err)
			}
			continue
		}
		var ans agentproto.HeartbeatAnswer
		decErr := json.NewDecoder(resp.Body).Decode(&ans)
		resp.Body.Close()
		cancel()
		recv := a.deps.Clock.Now()
		switch {
		case resp.StatusCode == http.StatusGone:
			a.regWhy = "heartbeat answered 410"
			a.log.Debug("heartbeat answered 410; registering again")
			a.mu.Lock()
			a.registered = false
			a.mu.Unlock()
			return
		case resp.StatusCode != http.StatusOK:
			failed(fmt.Sprintf("HTTP %d", resp.StatusCode))
			continue
		case decErr != nil:
			failed("answer: " + decErr.Error())
			continue
		}
		if failing {
			failing = false
			a.log.Info("Potluck reachable again: the heartbeat works", "failed_for_s", secs(a.deps.Clock.Now()-failedAt))
		}
		a.mu.Lock()
		a.paused = ans.Paused
		a.mu.Unlock()
		if body.Startup != nil {
			a.Start.Answered(ans.StartupAck, false)
		}
		if a.Ship != nil {
			// Returns at once: a new revision is read by the shipper's own
			// goroutine, never by the heartbeat.
			a.Ship.HeartbeatAnswered(ans.LogsRevision, ans.ServerTimeUnixMs, sent, recv)
		}
		if ans.Release != a.opt.Release {
			a.regWhy = "Potluck names release " + ans.Release
			a.log.Debug("Potluck names another release; registering again", "release", ans.Release)
			return
		}
	}
}

func secs(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

// askSystemd asks systemd once for the userspace start and the soft-restart
// count, before the first registration so that it carries them. A failure
// leaves both absent; it is never guessed.
func (a *Agent) askSystemd(ctx context.Context) {
	if a.deps.Systemd == nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	f, err := a.deps.Systemd.Show(c)
	if err != nil {
		a.log.Info("systemd gave no userspace start; the start-up report leaves it out", "err", err)
	}
	a.Start.SetSystemd(f)
}

// watchMarks reads the start-up marks file for a while after the start
// (startup.Watcher says how long). It never creates or deletes the file.
// busyEvery is how often the busy state follows the launch.
const busyEvery = 200 * time.Millisecond

// watchBusy keeps the busy file (6.14) in step with the launch, from the
// end of the re-adoption: it exists while a launch is starting, running or
// stopping. On a clean stop it is removed unless a launch outlives the
// agent (a bare launch does), which keeps the node busy.
func (a *Agent) watchBusy(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-a.recovered:
	}
	t := time.NewTicker(busyEvery)
	defer t.Stop()
	for {
		a.syncBusy()
		select {
		case <-ctx.Done():
			a.syncBusy() // removed when no launch is left; kept for one that outlives the agent
			return
		case <-t.C:
		}
	}
}

// syncBusy brings the busy state up to the launch: a launch that exists
// (starting, ready, or stopping until its process is reaped) or a launch
// request being started counts.
func (a *Agent) syncBusy() {
	a.busyMu.Lock()
	defer a.busyMu.Unlock()
	n := 0
	if a.Launch.CurrentID() != nil || a.busyPending > 0 {
		n = 1 // one launch per node (contract 2.3)
	}
	a.Busy.Set(n)
}

// launchAccepted marks the node busy before the launch's unit is started;
// the returned func ends the mark once the start has an answer.
func (a *Agent) launchAccepted() (done func()) {
	a.busyMu.Lock()
	a.busyPending++
	a.Busy.Set(1)
	a.busyMu.Unlock()
	return func() {
		a.busyMu.Lock()
		a.busyPending--
		a.busyMu.Unlock()
		a.syncBusy()
	}
}

func (a *Agent) watchMarks(ctx context.Context) {
	every := a.deps.MarksEvery
	if every <= 0 {
		every = 2 * time.Second
	}
	w := &startup.Watcher{Path: a.opt.MarksFile, UserspaceStart: a.Start.MarksCutoff, Logger: a.log}
	for {
		marks, changed, done, warn := w.Poll(a.deps.Clock.Now())
		if warn != "" {
			a.log.Warn(warn, "file", a.opt.MarksFile)
		}
		if changed {
			a.Start.SetExternal(marks)
		}
		if done {
			a.log.Debug("start-up marks file no longer watched", "file", a.opt.MarksFile)
			return
		}
		if !sleep(ctx, every) {
			return
		}
	}
}

// Status builds GET /status (6.9).
func (a *Agent) Status() agentproto.Status {
	// Launch calls are made before a.mu is taken: the launch manager calls
	// setError (a.mu) while holding its own lock.
	ls := a.Launch.State()
	model := a.Launch.Model()
	now := a.deps.Clock.Now()
	// The busy state is brought up to date from the launch state read here,
	// so the route and the file never disagree (once the re-adoption is done:
	// before it the launch state is not known).
	select {
	case <-a.recovered:
		a.syncBusy()
	default:
	}
	n, since := a.Busy.State()
	a.mu.Lock()
	defer a.mu.Unlock()
	st := agentproto.Status{Registered: a.registered, Potluck: a.opt.Potluck, Release: a.opt.Release,
		Paused: a.paused, Role: agentproto.RoleIdle, State: ls.State, Load: ls.Load, Busy: n > 0, Launches: n,
		AdvertiseAddr: a.opt.AdvertiseAddr}
	if n > 0 {
		s := since.Unix()
		st.BusySince = &s
	}
	switch ls.State {
	case agentproto.StateStarting, agentproto.StateReady, agentproto.StateStopping:
		st.Role, st.Model = ls.Role, model
	case agentproto.StateExited:
		// The node runs no model: the exited launch's model is named as the
		// last launch's, not as the current one.
		ll := agentproto.LastLaunch{ID: ls.LaunchID, Model: model, Load: ls.Load}
		st.Load = nil
		if ls.Exit != nil {
			ll.Reason, ll.EndedAgeS = ls.Exit.Reason, ls.Exit.AgeS
		}
		st.LastLaunch = &ll
	}
	if a.lastErr != "" {
		e := a.lastErr
		age := max(secs(now-a.lastErrAt), 0)
		st.LastError, st.LastErrorAgeS = &e, &age
	}
	return st
}

// ErrDockerUpdate tells the caller to exit with code 75 (5.4).
var ErrDockerUpdate = errors.New("docker install: exit 75 so the image can be replaced")
