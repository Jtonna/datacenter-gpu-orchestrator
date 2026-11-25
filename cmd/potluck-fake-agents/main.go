// Command potluck-fake-agents registers fake console agents with a scheduler,
// for a release readiness test on a throwaway scheduler (a copy of the live
// database on an isolated network) where no real agent can register. The
// fake agents register and heartbeat as agents of the scheduler's release do
// (agent contract 4), so the scheduler lists them active and placeable and
// its plans place on them. They never run anything: a launch is refused,
// unless --accept-launches is given, which reports a fake worker (a port
// that accepts connections and discards them) so a load gets as far as the
// scheduler's own host program.
//
//	potluck-fake-agents --scheduler URL (--nodes FILE | --from-status URL-or-FILE)
//	    [--addr-base 127.0.0.2] [--agent-port 50060] [--worker-port 50052]
//	    [--release auto] [--accept-launches] [--heartbeat 2s] [--print]
//
// The scheduler calls every agent back on one agent port (50060), so each fake
// agent needs an address of its own: agent i listens on --addr-base + i (a
// loopback address by default). Run the tool in the scheduler's network
// namespace (docker run --network container:<scheduler>) so the scheduler
// reaches those addresses; a node in --nodes may name its own "addr" instead.
//
// --from-status reads a scheduler's GET /api/status (its agents) or GET
// /api/nodes (its nodes, the local node left out) and mirrors ids, names,
// devices, free memory, capabilities, watchdog floors and the headline
// temperature of each node's stats. Addresses are never copied. --print
// writes the node list it would use as JSON and exits.
//
// Each agent sends a stats object in the shape real agents send (stats
// contract, stats_version 1) with headline_temp_c: the mirrored one, the
// node's "headline_temp_c" in --nodes, or 50.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakeagent"
)

// Node is one fake agent, as nodes.json lists it.
type Node struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Addr             string              `json:"addr,omitempty"` // default: --addr-base + index
	MemTotalMiB      int64               `json:"mem_total_mib,omitempty"`
	MemAvailableMiB  int64               `json:"mem_available_mib"`
	Devices          []agentproto.Device `json:"devices,omitempty"` // default: one shared Vulkan0 device from the memory figures
	GPUs             []agentproto.GPU    `json:"gpus,omitempty"`    // default: one integrated AMD GPU
	Capabilities     *[]string           `json:"capabilities,omitempty"`
	WatchdogFloorMiB int64               `json:"watchdog_floor_mib,omitempty"` // default 1000

	// HeadlineTempC is the temperature its stats report (stats contract
	// 4.6); default 50, mirrored by --from-status.
	HeadlineTempC *float64 `json:"headline_temp_c,omitempty"`
}

// DefaultTempC is the headline temperature of a node that gives none.
const DefaultTempC = 50.0

// statsOf is the stats object a real agent sends (stats contract,
// stats_version 1): the node exporter ok, CPU, memory from the node's
// figures, load, one temperature sensor at the headline temperature, and
// no GPU entries (a console's GPU is not drawn from them).
func statsOf(n Node) json.RawMessage {
	temp := DefaultTempC
	if n.HeadlineTempC != nil {
		temp = *n.HeadlineTempC
	}
	s := map[string]any{
		"stats_version":   1,
		"sources":         map[string]any{"node": map[string]any{"status": "ok", "age_ms": 500, "scrape_ms": 20}},
		"cpu":             map[string]any{"util_percent": 2.0, "logical_cpus": 8},
		"memory":          map[string]any{"total_bytes": n.MemTotalMiB << 20, "available_bytes": n.MemAvailableMiB << 20},
		"load":            map[string]any{"load1": 0.1},
		"net":             []any{},
		"temps":           []any{map[string]any{"id": "fake/temp1", "label": "fake", "celsius": temp}},
		"headline_temp_c": temp,
		"fans":            []any{},
		"gpus":            []any{},
	}
	b, _ := json.Marshal(s)
	return b
}

// Options are the command line.
type Options struct {
	Scheduler      string
	Release        string // "auto": GET /api/pool/release of the scheduler
	AddrBase       string
	AgentPort      int
	WorkerPort     int
	AcceptLaunches bool
	Heartbeat      time.Duration
	// MemFile, when set, is read every heartbeat: a JSON object of node id or
	// name to MemAvailable MiB; a listed node reports that figure from then
	// on (a scripted memory drop for a context fill's stop line).
	MemFile string
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("potluck-fake-agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o Options
	var nodesFile, fromStatus string
	var printOnly bool
	fs.StringVar(&o.Scheduler, "scheduler", "", "the scheduler's base URL, e.g. http://127.0.0.1:8080 (required)")
	fs.StringVar(&nodesFile, "nodes", "", "nodes.json: the fake agents")
	fs.StringVar(&fromStatus, "from-status", "", "mirror the agents of a scheduler's GET /api/status or GET /api/nodes (a URL or a saved file)")
	fs.StringVar(&o.AddrBase, "addr-base", "127.0.0.2", "address of the first agent without an addr; the next agents count up from it")
	fs.IntVar(&o.AgentPort, "agent-port", agentproto.PortAgent, "agent API port (the scheduler's, the same for every agent)")
	fs.IntVar(&o.WorkerPort, "worker-port", agentproto.PortWorker, "worker port (used with --accept-launches)")
	fs.StringVar(&o.Release, "release", "auto", "agent release stamp; auto asks the scheduler (it accepts only its own release)")
	fs.BoolVar(&o.AcceptLaunches, "accept-launches", false, "accept launches and report a fake running worker (default: refuse every launch)")
	fs.DurationVar(&o.Heartbeat, "heartbeat", 2*time.Second, "heartbeat interval (the scheduler asks for 2 s)")
	fs.BoolVar(&printOnly, "print", false, "print the node list as JSON and exit")
	fs.StringVar(&o.MemFile, "mem-file", "", "a JSON file of node id or name to MemAvailable MiB, read every heartbeat: a listed node reports that figure (scripted memory drop); a key silent:<id or name> with N withholds that agent's reports for N s (at most 8)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (nodesFile == "") == (fromStatus == "") {
		fmt.Fprintln(stderr, "give exactly one of --nodes and --from-status")
		return 2
	}
	var nodes []Node
	var err error
	if nodesFile != "" {
		nodes, err = readNodes(nodesFile)
	} else {
		nodes, err = nodesFromStatus(fromStatus)
	}
	if err == nil {
		err = assignAddrs(nodes, o.AddrBase)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if printOnly {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"nodes": nodes})
		return 0
	}
	if o.Scheduler == "" {
		fmt.Fprintln(stderr, "--scheduler is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f, err := Start(ctx, o, nodes, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	<-ctx.Done()
	f.Close()
	fmt.Fprintln(stdout, "stopped; the agents deregistered")
	return 0
}

// Fleet is the running fake agents.
type Fleet struct {
	Agents []*fakeagent.Agent
}

// Start registers one fake agent per node and returns once each has an
// answer to its registration (accepted or refused), or after 30 s.
func Start(ctx context.Context, o Options, nodes []Node, out io.Writer) (*Fleet, error) {
	base := strings.TrimRight(o.Scheduler, "/")
	rel := o.Release
	if rel == "" || rel == "auto" {
		r, err := schedulerRelease(ctx, base)
		if err != nil {
			return nil, err
		}
		rel = r
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 2 * time.Second
	}
	f := &Fleet{}
	for _, n := range nodes {
		a := &fakeagent.Agent{ID: n.ID, Name: n.Name, Addr: n.Addr, AgentPort: o.AgentPort, WorkerPort: o.WorkerPort, Potluck: base,
			Release: rel, Install: agentproto.InstallBare, MemAvail: n.MemAvailableMiB, MemTotalMiB: n.MemTotalMiB,
			GPUs: n.GPUs, Devices: n.Devices, WatchdogFloorMiB: n.WatchdogFloorMiB, HeartbeatEvery: o.Heartbeat,
			Stats: statsOf(n)}
		if n.Capabilities != nil {
			a.Capabilities = *n.Capabilities
		}
		a.CacheEvict = func(agentproto.CacheEvictRequest) (int, any) {
			return http.StatusNotImplemented, agentproto.LaunchRefusal{Reason: agentproto.ReasonUnsupported,
				Message: "fake agent (potluck-fake-agents): it holds no files, so there is no cache to drop"}
		}
		if !o.AcceptLaunches {
			a.Refuse = func(agentproto.LaunchRequest) *agentproto.LaunchRefusal {
				return &agentproto.LaunchRefusal{Reason: agentproto.ReasonInvalidRequest,
					Message: "fake agent (potluck-fake-agents): launches are refused; nothing can load here (start it with --accept-launches for a fake worker)"}
			}
		}
		if err := a.Start(); err != nil {
			f.Close()
			return nil, fmt.Errorf("agent %s at %s: %w (with the default loopback addresses, run in the scheduler's network namespace)",
				n.Name, net.JoinHostPort(n.Addr, fmt.Sprint(o.AgentPort)), err)
		}
		f.Agents = append(f.Agents, a)
	}
	if o.MemFile != "" {
		// Read every 100 ms (not once per heartbeat): a key takes effect at most
		// 100 ms after it is written, before the next heartbeat goes out.
		go f.followMemFile(ctx, o.MemFile, min(o.Heartbeat, memFilePoll), nodes, out)
	}
	mode := "launches refused"
	if o.AcceptLaunches {
		mode = "launches accepted (fake workers)"
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		done, ok := 0, 0
		for _, a := range f.Agents {
			if a.Answer() != nil {
				done, ok = done+1, ok+1
			} else if len(a.Refusals()) > 0 {
				done++
			}
		}
		if done == len(f.Agents) || time.Now().After(deadline) || ctx.Err() != nil {
			fmt.Fprintf(out, "%d of %d fake agents registered with %s as release %s; %s\n", ok, len(f.Agents), base, rel, mode)
			for _, a := range f.Agents {
				switch r := a.Refusals(); {
				case a.Answer() != nil:
					fmt.Fprintf(out, "  %-20s %-17s at %s: registered\n", a.Name, a.ID, a.Addr)
				case len(r) > 0:
					fmt.Fprintf(out, "  %-20s %-17s at %s: REFUSED %s: %s\n", a.Name, a.ID, a.Addr, r[len(r)-1].Reason, r[len(r)-1].Message)
				default:
					fmt.Fprintf(out, "  %-20s %-17s at %s: no answer yet (is the scheduler reachable?)\n", a.Name, a.ID, a.Addr)
				}
			}
			return f, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Close deregisters and stops every agent.
func (f *Fleet) Close() {
	for _, a := range f.Agents {
		a.Deregister()
		a.Close()
	}
}

func schedulerRelease(ctx context.Context, base string) (string, error) {
	var info agentproto.ReleaseInfo
	if err := getJSON(ctx, base+agentproto.PathRelease, &info); err != nil {
		return "", fmt.Errorf("the scheduler's release (GET %s): %w; give --release", agentproto.PathRelease, err)
	}
	if info.Release == "" {
		return "", errors.New("the scheduler serves no release; give --release")
	}
	return info.Release, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s answered %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readNodes(file string) ([]Node, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(doc.Nodes) == 0 {
		return nil, fmt.Errorf("%s lists no nodes", file)
	}
	for i := range doc.Nodes {
		fillDefaults(&doc.Nodes[i])
	}
	return doc.Nodes, nil
}

// fillDefaults gives a node what a console agent reports when nodes.json
// leaves it out: one shared Vulkan device holding the memory figures, an
// integrated AMD GPU, the file_cache_evict capability and a 1000 MiB floor.
func fillDefaults(n *Node) {
	if n.MemTotalMiB == 0 {
		for _, d := range n.Devices {
			n.MemTotalMiB = max(n.MemTotalMiB, d.TotalMiB)
		}
	}
	if n.MemTotalMiB == 0 {
		n.MemTotalMiB = 16000
	}
	if n.MemAvailableMiB == 0 {
		n.MemAvailableMiB = n.MemTotalMiB - 1000
	}
	if len(n.Devices) == 0 {
		n.Devices = []agentproto.Device{{Name: "Vulkan0", Backend: agentproto.BackendVulkan, TotalMiB: n.MemTotalMiB,
			FreeMiB: n.MemAvailableMiB, SharesSystemMemory: true}}
	}
	if n.GPUs == nil {
		n.GPUs = []agentproto.GPU{{Vendor: "amd", Integrated: true}}
	}
	if n.Capabilities == nil {
		c := []string{agentproto.CapFileCacheEvict}
		n.Capabilities = &c
	}
	if n.WatchdogFloorMiB == 0 {
		n.WatchdogFloorMiB = 1000
	}
}

// nodesFromStatus mirrors the agents of GET /api/status (agents) or GET
// /api/nodes (nodes, without the local node). Agents that left or were
// removed are skipped. Addresses are not copied.
func nodesFromStatus(src string) ([]Node, error) {
	var raw []byte
	var err error
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		var m json.RawMessage
		err = getJSON(context.Background(), src, &m)
		raw = m
	} else {
		raw, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Agents []json.RawMessage `json:"agents"`
		Nodes  []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	list := doc.Agents
	if list == nil {
		list = doc.Nodes
	}
	var out []Node
	for _, r := range list {
		var a struct {
			ID               string              `json:"id"`
			Name             string              `json:"name"`
			State            string              `json:"state"`
			Local            bool                `json:"local"`
			Devices          []agentproto.Device `json:"devices"`
			MemAvailMiB      *int64              `json:"mem_available_mib"`
			Capabilities     *[]string           `json:"capabilities"`
			WatchdogFloorMiB *int64              `json:"watchdog_floor_mib"`
			Stats            struct {
				HeadlineTempC *float64 `json:"headline_temp_c"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(r, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		if a.Local || a.ID == "" || a.State == "left" || a.State == "removed" {
			continue
		}
		n := Node{ID: a.ID, Name: a.Name, Devices: a.Devices, Capabilities: a.Capabilities, HeadlineTempC: a.Stats.HeadlineTempC}
		if a.MemAvailMiB != nil {
			n.MemAvailableMiB = *a.MemAvailMiB
		}
		if a.WatchdogFloorMiB != nil {
			n.WatchdogFloorMiB = *a.WatchdogFloorMiB
		}
		for i := range n.Devices {
			n.Devices[i].ReadAgeS = 0
		}
		fillDefaults(&n)
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no agent", src)
	}
	return out, nil
}

// assignAddrs gives every node without an addr the next address from base.
func assignAddrs(nodes []Node, base string) error {
	ip := net.ParseIP(base).To4()
	if ip == nil {
		return fmt.Errorf("--addr-base %q is not an IPv4 address", base)
	}
	next := append(net.IP{}, ip...)
	seen := map[string]bool{}
	for i := range nodes {
		if nodes[i].ID == "" || nodes[i].Name == "" {
			return fmt.Errorf("node %d needs an id and a name", i+1)
		}
		if seen[nodes[i].ID] {
			return fmt.Errorf("node id %s is listed twice", nodes[i].ID)
		}
		seen[nodes[i].ID] = true
		if nodes[i].Addr != "" {
			continue
		}
		nodes[i].Addr = next.String()
		for j := 3; j >= 0; j-- {
			if next[j]++; next[j] != 0 {
				break
			}
		}
	}
	return nil
}

// memFilePoll is how often the memory file is read.
const memFilePoll = 100 * time.Millisecond

// followMemFile applies the memory file every interval until ctx ends: a
// node listed by id or name reports that MemAvailable (and the same
// available memory in its stats). A missing or bad file changes nothing.
func (f *Fleet) followMemFile(ctx context.Context, file string, every time.Duration, nodes []Node, out io.Writer) {
	applied := map[string]int64{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if b, err := os.ReadFile(file); err == nil {
			var want map[string]int64
			if json.Unmarshal(b, &want) == nil {
				for _, a := range f.Agents {
					// "silent:<id or name>": N withholds that agent's reports for N s
					// (at most 8 s, short of the pool's LostAfter), once per new N.
					key := "silent:" + a.ID
					s, ok := want[key]
					if !ok {
						key = "silent:" + a.Name
						s, ok = want[key]
					}
					if ok && s > 0 && applied[key] != s {
						d := time.Duration(min(s, 8)) * time.Second
						a.SilentFor(d)
						applied[key] = s
						fmt.Fprintf(out, "  %-20s withholds its reports for %s (%s)\n", a.Name, d, file)
					}
				}
				for i, a := range f.Agents {
					v, ok := want[a.ID]
					if !ok {
						v, ok = want[a.Name]
					}
					if !ok || applied[a.ID] == v {
						continue
					}
					n := nodes[i]
					n.MemAvailableMiB = v
					a.SetMemAvail(v, statsOf(n))
					applied[a.ID] = v
					fmt.Fprintf(out, "  %-20s now reports %d MiB available (%s)\n", a.Name, v, file)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
