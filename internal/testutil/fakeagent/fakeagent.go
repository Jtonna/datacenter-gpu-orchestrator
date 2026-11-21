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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
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
	Backends   []string
	GPUs       []agentproto.GPU
	Devices    []agentproto.Device
	MemAvail   int64
	Foreign    []agentproto.ForeignProcess
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
	// WatchdogFloorMiB, when set, is sent as watchdog_floor_mib in the
	// registration and the heartbeat (CR-9); 0 sends none, as release 0.1.0.
	// The would_not_fit check uses it, or 1000.
	WatchdogFloorMiB int64

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
}

type launchRun struct {
	req     agentproto.LaunchRequest
	state   string
	stopWhy string // stop_reason while stopping (CR-9)
	stopMsg string
	exit    *agentproto.ExitInfo
	cmd     *exec.Cmd
	ln      net.Listener
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

// Start serves the agent API and starts registering and heartbeating.
func (a *Agent) Start() error {
	if a.Release == "" {
		a.Release = "0.1.0"
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
	req := agentproto.RegisterRequest{ID: a.ID, Name: a.Name, AgentRelease: a.Release, AgentStartID: a.StartID,
		OS: "linux", Arch: "amd64", Install: a.Install, Addr: a.Addr, GPUs: a.GPUs, Backends: a.Backends,
		MemTotalMiB: 16000, ModelSource: agentproto.ModelSource{Kind: agentproto.ModelSourceShare, Target: "192.0.2.1:/"},
		LaunchID: lid, ForeignProcesses: a.Foreign, WatchdogFloorMiB: a.WatchdogFloorMiB}
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
	hb := map[string]any{"agent_start_id": a.StartID, "seq": time.Now().UnixNano(), "uptime_s": 1.0,
		"role": agentproto.RoleIdle, "launch_id": nil, "programs_ready": true, "programs_error": nil,
		"model_source": map[string]any{"kind": "share", "reachable": true, "checked_age_s": 1.0},
		"devices":      a.Devices, "mem_available_mib": a.MemAvail}
	if a.Devices == nil {
		hb["devices"] = []agentproto.Device{}
	}
	if a.cur != nil && a.cur.state != agentproto.StateExited {
		hb["role"], hb["launch_id"] = agentproto.RoleWorker, a.cur.req.LaunchID
	}
	if a.Stats != nil {
		hb["stats"] = a.Stats
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
			if force && a.IgnoreForce {
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

// wouldNotFitLocked is the agent's pre-launch memory check (CR-9), on the
// first device: without expect_mib nothing is checked.
func (a *Agent) wouldNotFitLocked(req agentproto.LaunchRequest) *agentproto.LaunchRefusal {
	if req.Worker == nil || req.Worker.ExpectMiB <= 0 || len(a.Devices) == 0 {
		return nil
	}
	floor, expect, d := a.WatchdogFloorMiB, req.Worker.ExpectMiB, a.Devices[0]
	if floor == 0 {
		floor = 1000
	}
	switch {
	case d.SharesSystemMemory && a.MemAvail-expect < floor:
		return &agentproto.LaunchRefusal{Reason: agentproto.ReasonWouldNotFit, Message: fmt.Sprintf(
			"MemAvailable %d MiB minus the expected %d MiB leaves less than the watchdog floor of %d MiB", a.MemAvail, expect, floor)}
	case !d.SharesSystemMemory && d.FreeMiB-expect < 0:
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
