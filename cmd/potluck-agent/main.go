// Command potluck-agent is Potluck's per-machine agent (docs/agent-contract.md).
//
//	potluck-agent --potluck <addr:port> --model-source share:<target>|dir:<path>|push
//	              [--watchdog-floor-mib N] [--allow-from ADDR|CIDR]... [--ram-only]
//	              [--shared-memory auto|true|false]
//	              [--install bare|docker] [--name LABEL]
//	              [--data-dir DIR] [--state-dir DIR] [--models-dir DIR]
//	              [--logs on|off] [--log-buffer-mib N] [--journal-dir DIR]
//	potluck-agent id --potluck <addr:port>   print the node id and exit
//	potluck-agent version                   print the release and exit
//	potluck-agent probe-read <dir>          (internal) one share probe
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/agent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/hw"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/identity"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/launch"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/modelsrc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Release is the agent's release (agentproto.Release); it can be set at
// build time with -ldflags "-X main.Release=0.1.2".
var Release = agentproto.Release

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case modelsrc.ProbeCommand:
			if len(args) != 2 {
				fmt.Fprintln(stderr, "usage: potluck-agent probe-read <dir>")
				return 2
			}
			if err := modelsrc.ProbeRead(args[1]); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		case "version":
			fmt.Fprintln(stdout, Release)
			return 0
		case "id":
			fs := flag.NewFlagSet("id", flag.ContinueOnError)
			fs.SetOutput(stderr)
			potluck := fs.String("potluck", "", "Potluck address <ip>:<port>")
			if err := fs.Parse(args[1:]); err != nil {
				return 2
			}
			id, err := resolveID(*potluck)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintln(stdout, id)
			return 0
		}
	}
	opt, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "potluck-agent:", err)
		return 2
	}
	return serve(opt, stderr)
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

// parseFlags reads the options of section 9 and fills the defaults:
// programs and logs under /dev/shm/potluck on a RAM-only node, else
// /var/lib/potluck-agent; state and the share mount point under /run/potluck;
// a Docker install reads the share at /models (mounted by the machine).
func parseFlags(args []string, stderr io.Writer) (agent.Options, error) {
	fs := flag.NewFlagSet("potluck-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o agent.Options
	var allow multiFlag
	var source, logs string
	var bufMiB int
	host, _ := os.Hostname()
	fs.StringVar(&o.Potluck, "potluck", "", "Potluck address <ip>:<port> (required)")
	fs.StringVar(&source, "model-source", "", "share:<ip>:/<export> | dir:<path> | push (required)")
	fs.Int64Var(&o.WatchdogFloorMiB, "watchdog-floor-mib", 1000, "kill a launch when MemAvailable falls below this")
	fs.StringVar(&o.SharedMemory, "shared-memory", launch.SharedMemoryAuto,
		"auto, true or false: whether the GPU uses system memory, so a launch must also fit MemAvailable (auto: every device except a CUDA one)")
	fs.Var(&allow, "allow-from", "extra source address or network accepted on the agent API (repeatable)")
	fs.BoolVar(&o.RAMOnly, "ram-only", false, "RAM-only node: programs and logs under /dev/shm/potluck")
	fs.StringVar(&o.Install, "install", agentproto.InstallBare, "bare or docker")
	fs.StringVar(&o.Name, "name", host, "display label")
	fs.StringVar(&o.DataDir, "data-dir", "", "programs and launch logs (default /dev/shm/potluck with --ram-only, else /var/lib/potluck-agent)")
	fs.StringVar(&o.StateDir, "state-dir", "/run/potluck", "state files and the share mount point")
	fs.StringVar(&o.ModelsDir, "models-dir", "", "model folder (default <state-dir>/models for a bare share, /models in Docker, the path for dir:)")
	fs.StringVar(&logs, "logs", "on", "on or off: ship this machine's log lines to Potluck when Potluck hands out a logs object")
	fs.IntVar(&bufMiB, "log-buffer-mib", 8, "RAM for log lines not yet shipped; the oldest go first when it is full")
	fs.StringVar(&o.JournalDir, "journal-dir", "", "a journal folder to ship (Docker install: the host's journal, mounted read-only); a bare install ships its own journal")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch logs {
	case "on":
	case "off":
		o.LogsOff = true
	default:
		return o, fmt.Errorf("--logs %q: want on or off", logs)
	}
	if bufMiB < 1 || bufMiB > 4096 {
		return o, fmt.Errorf("--log-buffer-mib %d: want 1 to 4096", bufMiB)
	}
	o.LogBufferBytes = bufMiB << 20
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	o.Release = Release
	if o.Potluck == "" {
		return o, errors.New("--potluck is required")
	}
	h, p, err := net.SplitHostPort(o.Potluck)
	if err != nil || net.ParseIP(h) == nil {
		return o, fmt.Errorf("--potluck %q: want <ip>:<port>", o.Potluck)
	}
	if _, err := strconv.Atoi(p); err != nil {
		return o, fmt.Errorf("--potluck %q: bad port", o.Potluck)
	}
	if source == "" {
		return o, errors.New("--model-source is required")
	}
	if o.ModelSource, err = modelsrc.ParseSpec(source); err != nil {
		return o, err
	}
	if o.Install != agentproto.InstallBare && o.Install != agentproto.InstallDocker {
		return o, fmt.Errorf("--install %q: want bare or docker", o.Install)
	}
	switch o.SharedMemory {
	case launch.SharedMemoryAuto, launch.SharedMemoryTrue, launch.SharedMemoryFalse:
	default:
		return o, fmt.Errorf("--shared-memory %q: want auto, true or false", o.SharedMemory)
	}
	if o.WatchdogFloorMiB <= 0 {
		return o, errors.New("--watchdog-floor-mib must be positive")
	}
	for _, a := range allow {
		pfx, err := netip.ParsePrefix(a)
		if err != nil {
			ip, err2 := netip.ParseAddr(a)
			if err2 != nil {
				return o, fmt.Errorf("--allow-from %q: want an address or a network", a)
			}
			pfx = netip.PrefixFrom(ip, ip.BitLen())
		}
		o.AllowFrom = append(o.AllowFrom, pfx.Masked())
	}
	if o.DataDir == "" {
		o.DataDir = "/var/lib/potluck-agent"
		if o.RAMOnly {
			o.DataDir = "/dev/shm/potluck"
		}
	}
	if o.ModelsDir == "" {
		switch {
		case o.ModelSource.Kind == modelsrc.KindDir:
			o.ModelsDir = o.ModelSource.Target
		case o.ModelSource.Kind == modelsrc.KindShare && o.Install == agentproto.InstallDocker:
			o.ModelsDir = "/models"
		case o.ModelSource.Kind == modelsrc.KindShare:
			o.ModelsDir = filepath.ToSlash(filepath.Join(o.StateDir, "models"))
		}
	}
	return o, nil
}

func realNet(m hw.Machine) identity.RealNet {
	return identity.RealNet{DefaultRoute: m.DefaultRouteInterface, Physical: m.IsPhysical}
}

func resolveID(potluck string) (string, error) {
	h, _, err := net.SplitHostPort(potluck)
	ip := net.ParseIP(h)
	if err != nil || ip == nil {
		return "", fmt.Errorf("--potluck %q: want <ip>:<port>", potluck)
	}
	id, err := identity.Resolve(realNet(hw.Machine{Root: "/"}), ip)
	if err != nil {
		return "", err
	}
	return id.ID, nil
}

// machineHW adapts hw.Machine to agent.Hardware.
type machineHW struct{ m hw.Machine }

func (h machineHW) GPUs() []hw.GPUInfo                     { return h.m.GPUs() }
func (h machineHW) Backends(g []hw.GPUInfo) []string       { return h.m.Backends(g) }
func (h machineHW) MissingLibs(p string) ([]string, error) { return hw.MissingLibs(p, h.m.Libs()) }

func hostHealthy(ctx context.Context, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func serve(opt agent.Options, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	clock, err := osx.NewBootClock()
	if err != nil {
		log.Error("cannot run here", "err", err)
		return 1
	}
	var rn runner.Runner
	if opt.Install == agentproto.InstallBare {
		s, err := runner.NewSystemd()
		if err != nil {
			log.Error("systemd runner", "err", err)
			return 1
		}
		rn = s
	} else {
		rn = runner.NewChild()
	}
	exe, err := os.Executable()
	if err != nil {
		log.Error("own path", "err", err)
		return 1
	}
	// One library path for the library check and for the launches.
	opt.LDLibraryPath = os.Getenv("LD_LIBRARY_PATH")
	machine := hw.Machine{Root: "/", LDLibraryPath: opt.LDLibraryPath}
	if opt.LeaveMarker == "" {
		opt.LeaveMarker = agent.LeaveMarkerPath
	}
	base := "http://" + opt.Potluck
	httpc := &http.Client{Timeout: 5 * time.Second}
	var a *agent.Agent
	var upd agent.Updater = agent.SelfUpdater{Base: base, HTTP: &http.Client{Timeout: 10 * time.Minute}, Exe: exe, Restart: restartExec,
		Version: agent.ExecVersion, AttemptPath: filepath.Join(opt.StateDir, "update-attempt"), Logger: log,
		Busy: func() bool { return a != nil && a.Launch.Busy() }}
	if opt.Install == agentproto.InstallDocker {
		upd = agent.DockerUpdater{Exit: os.Exit}
	}
	a, err = agent.New(opt, agent.Deps{
		Clock: clock, Runner: rn, Mem: osx.MemInfoFile{}, Sockets: osx.ProcNetSockets{}, Procs: osx.ProcFS{},
		KLog: osx.Kmsg{}, Net: realNet(machine), Hardware: machineHW{machine},
		Mounter: modelsrc.ExecMounter{Cmd: osx.ExecCommander{}}, Prober: modelsrc.ExecProber{Exe: exe},
		DiskFree: osx.StatfsFree, HostHealthy: hostHealthy, Updater: upd, HTTP: httpc, Logger: log,
		Listen: func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
	})
	if err != nil {
		log.Error("start", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run re-adopts an existing launch first, then binds port 50060 (again
	// and again while it is taken: under the starter's Restart=always an exit
	// here would be an endless restart loop), resolves the identity and
	// registers.
	log.Info("potluck-agent starting", "release", opt.Release, "install", opt.Install, "ram_only", opt.RAMOnly)
	a.Run(ctx)
	log.Info(stoppedMessage(rn.Survives()))
	return 0
}

// stoppedMessage is the agent's last log line. A launch outlives the agent
// only where the runner's processes do (systemd launch units); a child
// process of the agent (Docker install) ends with it.
func stoppedMessage(survives bool) string {
	if survives {
		return "potluck-agent stopped; a running launch unit keeps running"
	}
	return "potluck-agent stopped; a running launch ends with the agent (it is a child process of it)"
}
