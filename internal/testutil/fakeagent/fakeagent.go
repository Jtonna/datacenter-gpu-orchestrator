// Package fakeagent is a fake potluck-agent for the scheduler's tests: it
// registers with Potluck, sends heartbeats and serves the launch API of the
// agent contract (agent contract 4 and 6) from the agentproto types. It is
// not the real agent: it models only what Potluck can observe, and its
// behaviour is scripted by the test.
//
// A worker launch runs a real child process when RPCServer is set (the fake
// rpc-server of internal/testutil/fakeprog, listening on the worker port);
// otherwise an in-process listener stands in for it.
package fakeagent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Agent is one fake agent.
type Agent struct {
	ID, Name   string
	Addr       string // IP the agent and its worker listen on
	AgentPort  int
	WorkerPort int
	Potluck    string // base URL, http://host:port
	Release    string
	Install    string
	StartID    string
	// OS and Arch are the registration's; empty sends linux and amd64.
	OS, Arch string
	Backends []string
	GPUs     []agentproto.GPU
	Devices  []agentproto.Device
	MemAvail int64
	// MemTotalMiB is the registration's mem_total_mib; 0 sends 16000.
	MemTotalMiB int64
	Foreign     []agentproto.ForeignProcess
	// RegLaunchID is sent as the registration's launch_id (a launch the node
	// still runs from before).
	RegLaunchID *string
	// Stats is the heartbeat's stats object as raw JSON; nil sends none.
	Stats json.RawMessage
	// HeartbeatEvery defaults to 100 ms.
	HeartbeatEvery time.Duration
	// RPCServer is the program run as rpc-server; empty uses an in-process listener.
	RPCServer string
	// ReadyAfter is how long a launch stays starting.
	ReadyAfter time.Duration
	// Refuse, when set, may refuse a launch.
	Refuse func(agentproto.LaunchRequest) *agentproto.LaunchRefusal
	// IgnoreForce: a forced stop leaves the launch stopping (only /reset ends it).
	IgnoreForce bool
	// IgnoreStop: a plain stop leaves the launch stopping (a worker slow to exit).
	IgnoreStop bool
	// WatchdogFloorMiB, when set, is sent as watchdog_floor_mib in the
	// registration and the heartbeat (CR-9); 0 sends none, as release 0.1.0.
	// The would_not_fit check uses it, or 1000.
	WatchdogFloorMiB int64
	// Capabilities is sent as the registration's capabilities (6.13).
	// CacheEvict answers POST /cache/evict: the status and the body; nil
	// answers 404 (an agent without the request).
	Capabilities []string
	CacheEvict   func(agentproto.CacheEvictRequest) (int, any)
	// HostPort is where a host launch's fake llama-server listens (phase 6);
	// a host launch is taken only when Capabilities holds host_launch and
	// HostPort is set, else it is refused with invalid_request as an agent
	// of release 0.1.6 would. AutoPort: the launch binds port 0 and the
	// launch state reports the port it got (HostPortNow): no port is picked
	// first and bound later, so no other test can take it in between.
	HostPort int

	mu         sync.Mutex
	srv        *http.Server
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	hbOn       bool
	paused     bool
	answer     *agentproto.RegisterAnswer
	refusals   []agentproto.RegisterAnswer
	hbStatus   []int
	calls      []string
	cur        *launchRun
	stopLaunch []string
	launches   []agentproto.LaunchRequest // every accepted launch request
	hostBound  int                        // the port a host launch with AutoPort bound
	hostAuth   []string                   // the Authorization header of each request to a host launch
	silentTo   time.Time                  // no heartbeat is sent before this (SilentFor)
	kick       chan struct{}              // sends a heartbeat at once (the end of a silence)
}

type launchRun struct {
	req     agentproto.LaunchRequest
	state   string
	stopWhy string // stop_reason while stopping (CR-9)
	stopMsg string
	exit    *agentproto.ExitInfo
	cmd     *exec.Cmd
	ln      net.Listener
	hsrv    *http.Server // a host launch's fake llama-server
	leases  int
	started time.Time
	log     syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

func (s *syncBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func (a *Agent) memTotal() int64 {
	if a.MemTotalMiB > 0 {
		return a.MemTotalMiB
	}
	return 16000
}

// Start serves the agent API and starts registering and heartbeating.
func (a *Agent) Start() error {
	if a.Release == "" {
		a.Release = agentproto.Release
	}
	if a.Install == "" {
		a.Install = agentproto.InstallBare
	}
	if a.StartID == "" {
		a.StartID = fmt.Sprintf("s%d", time.Now().UnixNano())
	}
	if a.Backends == nil {
		a.Backends = []string{agentproto.BackendVulkan}
	}
	if a.HeartbeatEvery == 0 {
		a.HeartbeatEvery = 100 * time.Millisecond
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(a.Addr, strconv.Itoa(a.AgentPort)))
	if err != nil {
		return err
	}
	a.srv = &http.Server{Handler: a.handler()}
	go a.srv.Serve(ln)
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Lock()
	a.hbOn = true
	a.mu.Unlock()
	a.wg.Add(1)
	go a.loop(ctx)
	return nil
}

// Close stops everything, the launch's child included.
func (a *Agent) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	if a.srv != nil {
		a.srv.Close()
	}
	a.mu.Lock()
	if a.cur != nil {
		a.endLocked(agentproto.ExitStopped)
	}
	a.mu.Unlock()
}

// SetHeartbeats switches the heartbeats on or off (off: the node goes lost).
func (a *Agent) SetHeartbeats(on bool) { a.mu.Lock(); a.hbOn = on; a.mu.Unlock() }

// Answer is the last accepted registration answer, or nil.
func (a *Agent) Answer() *agentproto.RegisterAnswer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.answer
}

// Refusals are the refused registration answers so far.
func (a *Agent) Refusals() []agentproto.RegisterAnswer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agentproto.RegisterAnswer(nil), a.refusals...)
}

// Paused is what the last heartbeat answer said.
func (a *Agent) Paused() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.paused }

// Calls lists the launch API calls received ("POST /launch", "DELETE /launch/<id>?force=1", ...).
func (a *Agent) Calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// Has reports whether a call starting with prefix was received.
func (a *Agent) Has(prefix string) bool {
	for _, c := range a.Calls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// StopLaunches are the stop_launch ids the registrations returned.
func (a *Agent) StopLaunches() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.stopLaunch...)
}

// LaunchState is the current launch state ("none" without one).
func (a *Agent) LaunchState() agentproto.LaunchState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

// Leases is how many lease renewals the current launch got.
func (a *Agent) Leases() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return 0
	}
	return a.cur.leases
}

// ExitLaunch ends the current launch on its own with reason (watchdog, crashed ...).
func (a *Agent) ExitLaunch(reason, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur != nil && a.cur.state != agentproto.StateExited {
		a.endLocked(reason)
		a.cur.exit.Message = msg
	}
}

// Deregister sends POST /api/pool/nodes/{id}/deregister.
func (a *Agent) Deregister() (int, error) {
	resp, err := http.Post(a.Potluck+fmt.Sprintf(agentproto.PathDeregisterFmt, a.ID), "application/json", nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// HeartbeatStatuses are the HTTP statuses of the heartbeats sent.
func (a *Agent) HeartbeatStatuses() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int(nil), a.hbStatus...)
}

func (a *Agent) loop(ctx context.Context) {
	defer a.wg.Done()
	registered := false
	t := time.NewTicker(a.HeartbeatEvery)
	defer t.Stop()
	for {
		a.mu.Lock()
		on := a.hbOn
		a.mu.Unlock()
		if on {
			if !registered {
				registered = a.register(ctx)
			} else if st := a.heartbeat(ctx); st == http.StatusGone {
				registered = false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kickC(): // a withheld report ends: report at once (SilentFor)
		}
	}
}

func (a *Agent) register(ctx context.Context) bool {
	a.mu.Lock()
	lid := a.RegLaunchID
	if a.cur != nil && a.cur.state != agentproto.StateExited {
		id := a.cur.req.LaunchID
		lid = &id
	}
	// the slices are copied under a.mu: the request is marshalled after the
	// lock is released (post), and a setter may change the agent meanwhile
	req := agentproto.RegisterRequest{ID: a.ID, Name: a.Name, AgentRelease: a.Release, AgentStartID: a.StartID,
		OS: cmp.Or(a.OS, "linux"), Arch: cmp.Or(a.Arch, "amd64"), Install: a.Install, Addr: a.Addr, GPUs: slices.Clone(a.GPUs), Backends: slices.Clone(a.Backends),
		MemTotalMiB: a.memTotal(), ModelSource: agentproto.ModelSource{Kind: agentproto.ModelSourceShare, Target: "192.0.2.1:/"},
		LaunchID: lid, ForeignProcesses: slices.Clone(a.Foreign), WatchdogFloorMiB: a.WatchdogFloorMiB, Capabilities: slices.Clone(a.Capabilities)}
	a.mu.Unlock()
	req.FillEmpty()
	var ans agentproto.RegisterAnswer
	if code, err := a.post(ctx, agentproto.PathRegister, req, &ans); err != nil || code != http.StatusOK {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !ans.Accepted {
		a.refusals = append(a.refusals, ans)
		return false
	}
	a.answer, a.paused = &ans, ans.Paused
	if ans.StopLaunch != nil {
		a.stopLaunch = append(a.stopLaunch, *ans.StopLaunch)
		if a.cur != nil && a.cur.req.LaunchID == *ans.StopLaunch {
			a.endLocked(agentproto.ExitStopped)
		}
		a.RegLaunchID = nil
	}
	return true
}

func (a *Agent) heartbeat(ctx context.Context) int {
	a.mu.Lock()
	if time.Now().Before(a.silentTo) { // a withheld report (SilentFor)
		a.mu.Unlock()
		return http.StatusOK
	}
	// Everything the body holds is copied under a.mu: json.Marshal runs in
	// post, after the lock is released, while SetMemAvail may write
	// a.Devices[i].FreeMiB (the data race the Linux race run found when the
	// body held a.Devices itself).
	devices := slices.Clone(a.Devices)
	if devices == nil {
		devices = []agentproto.Device{}
	}
	hb := map[string]any{"agent_start_id": a.StartID, "seq": time.Now().UnixNano(), "uptime_s": 1.0,
		"role": agentproto.RoleIdle, "launch_id": nil, "programs_ready": true, "programs_error": nil,
		"model_source": map[string]any{"kind": "share", "reachable": true, "checked_age_s": 1.0},
		"devices":      devices, "mem_available_mib": a.MemAvail}
	if a.cur != nil && a.cur.state != agentproto.StateExited {
		hb["role"], hb["launch_id"] = roleOf(a.cur.req), a.cur.req.LaunchID
	}
	if a.Stats != nil {
		hb["stats"] = slices.Clone(a.Stats)
	}
	if a.WatchdogFloorMiB > 0 {
		hb["watchdog_floor_mib"] = a.WatchdogFloorMiB
	}
	if a.cur != nil && a.cur.state == agentproto.StateStopping && a.cur.stopWhy != "" {
		hb["stop_reason"], hb["stop_message"] = a.cur.stopWhy, a.cur.stopMsg
	}
	a.mu.Unlock()
	var ans agentproto.HeartbeatAnswer
	code, err := a.post(ctx, fmt.Sprintf(agentproto.PathHeartbeatFmt, a.ID), hb, &ans)
	if err != nil {
		return 0
	}
	a.mu.Lock()
	a.hbStatus = append(a.hbStatus, code)
	if code == http.StatusOK {
		a.paused = ans.Paused
	}
	a.mu.Unlock()
	return code
}

func (a *Agent) post(ctx context.Context, path string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, a.Potluck+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// ---- launch API ----

func (a *Agent) handler() http.Handler {
	mux := http.NewServeMux()
	note := func(r *http.Request) {
		a.mu.Lock()
		c := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			c += "?" + r.URL.RawQuery
		}
		a.calls = append(a.calls, c)
		a.mu.Unlock()
	}
	mux.HandleFunc("POST /launch", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		var req agentproto.LaunchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			refuse(w, agentproto.ReasonInvalidRequest, err.Error())
			return
		}
		if a.Refuse != nil {
			if ref := a.Refuse(req); ref != nil {
				writeJSON(w, agentproto.LaunchRefusalStatus[ref.Reason], ref)
				return
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.cur != nil && a.cur.state != agentproto.StateExited {
			writeJSON(w, 409, agentproto.LaunchRefusal{Reason: agentproto.ReasonBusy, Message: "busy", LaunchID: a.cur.req.LaunchID})
			return
		}
		if a.paused {
			writeJSON(w, 409, agentproto.LaunchRefusal{Reason: agentproto.ReasonPaused, Message: "the node is paused"})
			return
		}
		if req.MinMemAvailableMiB > a.MemAvail {
			writeJSON(w, 507, agentproto.LaunchRefusal{Reason: agentproto.ReasonLowMemory,
				Message: fmt.Sprintf("MemAvailable %d MiB is below the required %d MiB", a.MemAvail, req.MinMemAvailableMiB)})
			return
		}
		if ref := a.wouldNotFitLocked(req); ref != nil {
			writeJSON(w, agentproto.LaunchRefusalStatus[ref.Reason], ref)
			return
		}
		if req.Role == agentproto.RoleHost {
			if !slices.Contains(a.Capabilities, agentproto.CapHostLaunch) || a.HostPort == 0 || req.Host == nil {
				refuse(w, agentproto.ReasonInvalidRequest, "this agent takes worker launches only")
				return
			}
			a.startHostLocked(w, req)
			return
		}
		run := &launchRun{req: req, state: agentproto.StateStarting, started: time.Now()}
		addr := net.JoinHostPort(a.Addr, strconv.Itoa(a.WorkerPort))
		if a.RPCServer != "" {
			run.cmd = exec.Command(a.RPCServer, "--host", a.Addr, "--port", strconv.Itoa(a.WorkerPort))
			run.cmd.Stdout, run.cmd.Stderr = &run.log, &run.log
			if err := run.cmd.Start(); err != nil {
				writeJSON(w, 409, agentproto.LaunchRefusal{Reason: agentproto.ReasonPortInUse, Message: err.Error()})
				return
			}
		} else {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				writeJSON(w, 409, agentproto.LaunchRefusal{Reason: agentproto.ReasonPortInUse, Message: err.Error()})
				return
			}
			run.ln = ln
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func() { io.Copy(io.Discard, c); c.Close() }()
				}
			}()
		}
		fmt.Fprintf(&run.log, "fake agent: worker launch %s for %s\n", req.LaunchID, req.Label)
		a.cur = run
		a.launches = append(a.launches, req)
		go a.becomeReady(run, addr)
		writeJSON(w, http.StatusAccepted, a.stateLocked())
	})
	mux.HandleFunc("GET /launch", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		a.mu.Lock()
		defer a.mu.Unlock()
		writeJSON(w, 200, a.stateLocked())
	})
	mux.HandleFunc("POST /launch/{id}/lease", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.cur == nil || a.cur.req.LaunchID != r.PathValue("id") || a.cur.state == agentproto.StateExited {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		a.cur.leases++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /launch/{id}", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.cur == nil || a.cur.req.LaunchID != r.PathValue("id") {
			writeJSON(w, 409, agentproto.LaunchRefusal{Reason: "not_current", Message: "not the current launch"})
			return
		}
		if a.cur.state != agentproto.StateExited {
			force := r.URL.Query().Get("force") == "1"
			if (force && a.IgnoreForce) || (!force && a.IgnoreStop) {
				a.cur.state = agentproto.StateStopping
			} else if force {
				a.endLocked(agentproto.ExitForced)
			} else {
				a.endLocked(agentproto.ExitStopped)
			}
		}
		writeJSON(w, 200, a.stateLocked())
	})
	mux.HandleFunc("GET /launch/log", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var data []byte
		if a.cur != nil {
			data = a.cur.log.Bytes()
		}
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		if since > len(data) {
			since = len(data)
		}
		w.Header().Set(agentproto.HeaderNextOffset, strconv.Itoa(len(data)))
		w.Write(data[since:])
	})
	mux.HandleFunc("POST "+agentproto.PathCacheEvict, func(w http.ResponseWriter, r *http.Request) {
		note(r)
		if a.CacheEvict == nil {
			w.WriteHeader(404)
			return
		}
		var req agentproto.CacheEvictRequest
		json.NewDecoder(r.Body).Decode(&req)
		code, body := a.CacheEvict(req)
		writeJSON(w, code, body)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		a.mu.Lock()
		defer a.mu.Unlock()
		ans := agentproto.ResetAnswer{Killed: []agentproto.KilledProcess{}}
		if a.cur != nil && a.cur.state != agentproto.StateExited {
			ans.Killed = append(ans.Killed, agentproto.KilledProcess{LaunchID: a.cur.req.LaunchID})
			a.endLocked(agentproto.ExitReset)
		}
		writeJSON(w, 200, ans)
	})
	return mux
}

// becomeReady turns the launch ready once its worker port accepts a
// connection (closed at once, as a probe must) and ReadyAfter has passed.
func (a *Agent) becomeReady(run *launchRun, addr string) {
	time.Sleep(a.ReadyAfter)
	for i := 0; i < 200; i++ {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == run && run.state == agentproto.StateStarting {
		run.state = agentproto.StateReady
	}
}

// wouldNotFitLocked is the agent's pre-launch memory check (CR-9, with the
// review's rule), on the first device: without expect_mib nothing is
// checked. Every test that applies must pass: the device's free figure when a
// device is known, and MemAvailable minus the floor unless the device is a
// CUDA device that does not share system memory (no device: MemAvailable).
func (a *Agent) wouldNotFitLocked(req agentproto.LaunchRequest) *agentproto.LaunchRefusal {
	var expect int64
	switch {
	case req.Worker != nil:
		expect = req.Worker.ExpectMiB
	case req.Host != nil:
		expect = req.Host.ExpectMiB
	}
	if expect <= 0 {
		return nil
	}
	floor := a.WatchdogFloorMiB
	if floor == 0 {
		floor = 1000
	}
	var d agentproto.Device
	haveDev := len(a.Devices) > 0
	if haveDev {
		d = a.Devices[0]
	}
	memTest := !haveDev || d.SharesSystemMemory || !strings.EqualFold(d.Backend, agentproto.BackendCUDA)
	switch {
	case memTest && a.MemAvail-expect < floor:
		return &agentproto.LaunchRefusal{Reason: agentproto.ReasonWouldNotFit, Message: fmt.Sprintf(
			"MemAvailable %d MiB minus the expected %d MiB leaves less than the watchdog floor of %d MiB", a.MemAvail, expect, floor)}
	case haveDev && d.FreeMiB-expect < 0:
		return &agentproto.LaunchRefusal{Reason: agentproto.ReasonWouldNotFit, Message: fmt.Sprintf(
			"device %s has %d MiB free, less than the expected %d MiB", d.Name, d.FreeMiB, expect)}
	}
	return nil
}

// BeginStop puts the current launch into stopping with a stop_reason, as an
// agent that has decided to stop it and whose process has not gone yet (CR-9).
func (a *Agent) BeginStop(reason, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur != nil && a.cur.state != agentproto.StateExited {
		a.cur.state, a.cur.stopWhy, a.cur.stopMsg = agentproto.StateStopping, reason, msg
	}
}

func (a *Agent) endLocked(reason string) {
	run := a.cur
	if run.cmd != nil && run.cmd.Process != nil {
		run.cmd.Process.Kill()
		run.cmd.Wait()
	}
	if run.ln != nil {
		run.ln.Close()
	}
	if run.hsrv != nil {
		run.hsrv.Close()
	}
	code := 0
	run.state = agentproto.StateExited
	run.exit = &agentproto.ExitInfo{Code: &code, Reason: reason, LogTail: []string{}}
}

func (a *Agent) stateLocked() agentproto.LaunchState {
	if a.cur == nil {
		return agentproto.LaunchState{State: agentproto.StateNone}
	}
	r := a.cur
	st := agentproto.LaunchState{LaunchID: r.req.LaunchID, Role: agentproto.RoleWorker, State: r.state,
		Port: a.WorkerPort, Command: []string{"rpc-server"}, Exit: r.exit, MemAvailableMiB: a.MemAvail}
	if r.req.Role == agentproto.RoleHost {
		port := a.hostPortLocked()
		st.Role, st.Port, st.Command = agentproto.RoleHost, port, []string{"llama-server", "--port", strconv.Itoa(port)}
	}
	if r.state != agentproto.StateExited {
		ttl := r.req.LeaseTTLS
		st.LeaseExpiresInS = &ttl
	}
	if r.state == agentproto.StateStopping {
		st.StopReason, st.StopMessage = r.stopWhy, r.stopMsg
	}
	return st
}

func refuse(w http.ResponseWriter, reason, msg string) {
	writeJSON(w, agentproto.LaunchRefusalStatus[reason], agentproto.LaunchRefusal{Reason: reason, Message: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ---- host launches (phase 6) ----

func roleOf(req agentproto.LaunchRequest) string {
	if req.Role == agentproto.RoleHost {
		return agentproto.RoleHost
	}
	return agentproto.RoleWorker
}

// startHostLocked starts a host launch: a fake llama-server on HostPort that
// answers /health without a key and everything else only with the launch's
// key, and writes the buffer lines a real host writes at -lv 4.
func (a *Agent) startHostLocked(w http.ResponseWriter, req agentproto.LaunchRequest) {
	port := a.HostPort
	if port == AutoPort {
		port = 0
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(a.Addr, strconv.Itoa(port)))
	if err != nil {
		writeJSON(w, 409, agentproto.LaunchRefusal{Reason: agentproto.ReasonPortInUse, Message: err.Error()})
		return
	}
	a.hostBound = ln.Addr().(*net.TCPAddr).Port
	addr := net.JoinHostPort(a.Addr, strconv.Itoa(a.hostBound))
	key := req.Host.APIKey
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		a.mu.Lock()
		a.hostAuth = append(a.hostAuth, got)
		a.mu.Unlock()
		if key != "" && got != "Bearer "+key {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "Invalid API Key", "type": "authentication_error"}})
			return
		}
		writeJSON(w, 200, map[string]any{"object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "hello"}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1}})
	})
	run := &launchRun{req: req, state: agentproto.StateStarting, started: time.Now(), ln: ln, hsrv: &http.Server{Handler: mux}}
	go run.hsrv.Serve(ln)
	fmt.Fprintf(&run.log, "fake agent: host launch %s for %s\n", req.LaunchID, req.Label)
	fmt.Fprintf(&run.log, "load_tensors:      Vulkan0 model buffer size =  2000.00 MiB\n")
	for i, wk := range req.Host.Workers {
		fmt.Fprintf(&run.log, "load_tensors: RPC%d[%s] model buffer size =  1000.00 MiB\n", i, wk)
	}
	fmt.Fprintf(&run.log, "llama_kv_cache:    Vulkan0 KV buffer size =   256.00 MiB\n")
	a.cur = run
	a.launches = append(a.launches, req)
	go a.becomeReady(run, addr)
	writeJSON(w, http.StatusAccepted, a.stateLocked())
}

// AutoPort as Agent.HostPort: a host launch binds port 0 (see HostPort).
const AutoPort = -1

// HostPortNow is the port of the host launch: the one bound under
// AutoPort (0 before any host launch), else HostPort.
func (a *Agent) HostPortNow() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hostPortLocked()
}

func (a *Agent) hostPortLocked() int {
	if a.HostPort == AutoPort {
		return a.hostBound
	}
	return a.HostPort
}

// Launches are the launch requests the agent accepted, in order.
func (a *Agent) Launches() []agentproto.LaunchRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agentproto.LaunchRequest(nil), a.launches...)
}

// HostAuth are the Authorization headers of the requests a host launch got
// (/health excluded), in order.
func (a *Agent) HostAuth() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.hostAuth...)
}

// SetMemAvail changes the agent's MemAvailable from the next heartbeat on,
// with the free figure of its devices that share system memory, and the
// stats object when stats is not nil (a scripted memory drop). It writes
// under a.mu, into a new Devices slice (never the array a caller or a
// body being sent may hold), and keeps its own copy of stats.
func (a *Agent) SetMemAvail(mib int64, stats json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.MemAvail = mib
	devices := slices.Clone(a.Devices)
	for i := range devices {
		if devices[i].SharesSystemMemory {
			devices[i].FreeMiB = mib
		}
	}
	a.Devices = devices
	if stats != nil {
		a.Stats = slices.Clone(stats)
	}
}

// SilentFor withholds the agent's heartbeats for d from now (its memory
// report is then as old as its last heartbeat), as an agent that misses a
// report does, and sends one at once when the silence ends, so the gap
// the scheduler sees is d plus the time since the last report. Kept short
// of the pool's LostAfter by the caller.
func (a *Agent) SilentFor(d time.Duration) {
	a.mu.Lock()
	a.silentTo = time.Now().Add(d)
	a.mu.Unlock()
	k := a.kickC()
	time.AfterFunc(d, func() {
		select {
		case k <- struct{}{}:
		default:
		}
	})
}

func (a *Agent) kickC() chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kick == nil {
		a.kick = make(chan struct{}, 1)
	}
	return a.kick
}
