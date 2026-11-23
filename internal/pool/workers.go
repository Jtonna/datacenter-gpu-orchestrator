package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Worker launches (agent contract 6, 11 "Phase 4 loads"). Every launch has a
// supervisor goroutine from the moment it is sent until it is stopped: it
// renews the lease every 5 s, reads the launch state (4 Hz while it starts and
// while its load is in progress, 1 Hz after: contract 6.3), copies the launch
// log, and reports load progress and an exit.

var _ scheduler.WorkerLauncher = (*Pool)(nil)

// How long a worker launch may stay starting is the setting
// worker_start_timeout (default 15 s, answered by the PS5 side to the
// contract question); the load then fails with worker_start_timeout.

// ReasonWorkerStartTimeout fails a load whose worker did not become ready.
const ReasonWorkerStartTimeout = "worker_start_timeout"

type launch struct {
	p       *Pool
	iid     uint64
	id      string
	node    core.NodeID
	name    string
	profile string // the profile the load is for (CR-10: a trusted field of the launch's log lines)
	base    string // http://addr:50060
	rpcAddr string
	// feed, set for a host launch (hostlaunch.go), takes the launch log's
	// new bytes as they are copied: the remote child reads its output there.
	feed func([]byte)

	mu       sync.Mutex
	st       agentproto.LaunchState
	have     bool
	goneWhy  string // set when the launch is known to be gone (agent restart)
	goneMsg  string
	report   func(scheduler.WorkerReport)
	reported bool // the exit was reported
	loaded   bool // the scheduler said the load is over (LoadDone)
	lastLoad string
	logOff   int64
	changed  chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}
}

func (l *launch) stateName() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.goneWhy != "" {
		return agentproto.StateExited
	}
	if !l.have {
		return agentproto.StateStarting
	}
	return l.st.State
}

// gone marks the launch as ended without the agent saying so (called with
// p.mu held).
func (l *launch) gone(reason, msg string) {
	l.mu.Lock()
	if l.goneWhy == "" {
		l.goneWhy, l.goneMsg = reason, msg
	}
	l.mu.Unlock()
	l.wake()
}

func (l *launch) wake() {
	select {
	case l.changed <- struct{}{}:
	default:
	}
}

// How often GET /launch is read (6.3).
const (
	pollLoading   = 250 * time.Millisecond
	pollAfterLoad = time.Second
)

// loading: a load is in progress on the launch's node, as far as is known. A
// worker is ready before the host connects and the load starts, so ready does
// not end it. It ends when the scheduler says the load is over (LoadDone),
// when the agent reports the node's part of the load done, or with the launch.
func (l *launch) loading() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loaded || l.goneWhy != "" {
		return false
	}
	if !l.have {
		return true
	}
	if l.st.State == agentproto.StateExited || l.st.State == agentproto.StateStopping {
		return false
	}
	return l.st.Load == nil || l.st.Load.State != agentproto.LoadDone
}

// LoadDone is scheduler.WorkerLauncher.LoadDone.
func (p *Pool) LoadDone(iid uint64) {
	p.mu.Lock()
	ls := append([]*launch(nil), p.byIID[iid]...)
	p.unlock()
	for _, l := range ls {
		l.mu.Lock()
		l.loaded = true
		l.mu.Unlock()
	}
}

// exitInfo is the launch's exit, or nil while it runs.
func (l *launch) exitInfo() *agentproto.ExitInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.goneWhy != "" {
		return &agentproto.ExitInfo{Reason: l.goneWhy, Message: l.goneMsg, LogTail: []string{}}
	}
	if l.have && l.st.State == agentproto.StateExited {
		if l.st.Exit != nil {
			return l.st.Exit
		}
		return &agentproto.ExitInfo{Reason: agentproto.ExitCrashed, Message: "the agent reported exited without an exit object", LogTail: []string{}}
	}
	return nil
}

// Launch is scheduler.WorkerLauncher.Launch.
func (p *Pool) Launch(ctx context.Context, spec scheduler.WorkerLaunchSpec, report func(scheduler.WorkerReport)) scheduler.WorkerResult {
	cfg := p.o.Config()
	p.mu.Lock()
	if p.stopped[spec.IID] {
		p.unlock()
		return scheduler.WorkerResult{Reason: ReasonAgentUnreachable, Message: "stopped before the launches were sent"}
	}
	var ls []*launch
	for _, w := range spec.Workers {
		a := p.agents[string(w.Node)]
		if a == nil || a.left || a.lost {
			p.unlock()
			p.dropLaunches(spec.IID, ls)
			return scheduler.WorkerResult{Node: w.Node, Reason: ReasonAgentUnreachable, Message: "the node's agent is not registered or is lost"}
		}
		p.seq++
		l := &launch{p: p, iid: spec.IID, node: w.Node, name: a.reg.Name, rpcAddr: w.Addr, profile: spec.Profile,
			id:      fmt.Sprintf("ln-%d-%d-%d", p.o.Clock().UnixMilli(), spec.IID, p.seq),
			base:    p.agentBase(a), // the address the call-back check reached
			changed: make(chan struct{}, 1), done: make(chan struct{})}
		ls = append(ls, l)
		p.launches[l.id] = l
	}
	p.byIID[spec.IID] = ls
	p.publishLogSnapLocked()
	p.unlock()

	// Send every launch at once.
	type sent struct {
		ref *agentproto.LaunchRefusal
		err error
	}
	res := make([]sent, len(ls))
	var wg sync.WaitGroup
	for i, l := range ls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := agentproto.LaunchRequest{LaunchID: l.id, Role: agentproto.RoleWorker, Release: p.o.PotluckRelease,
				Label: spec.Label, LeaseTTLS: LeaseTTL.Seconds(), MinMemAvailableMiB: spec.Workers[i].MinMemAvailableMiB,
				Worker: &agentproto.WorkerSpec{ModelDir: cfg.Defaults.WorkerModelDir, ExtraArgs: []string{},
					ExpectMiB: spec.Workers[i].ExpectMiB}}
			st, ref, err := p.postLaunch(ctx, l.base, req)
			if err == nil && ref == nil {
				l.mu.Lock()
				l.st, l.have = st, true
				l.mu.Unlock()
				p.mu.Lock()
				stopped := p.stopped[spec.IID]
				p.unlock()
				if stopped {
					// The load was stopped while this launch was on its way.
					go p.stopLaunch(l, false, false)
				} else {
					p.supervise(l)
				}
			}
			res[i] = sent{ref, err}
		}()
	}
	wg.Wait()
	ids := map[core.NodeID]string{}
	for i, l := range ls {
		ids[l.node] = l.id
		r := res[i]
		switch {
		case r.ref != nil:
			p.forget(l, true)
			return scheduler.WorkerResult{LaunchIDs: ids, Node: l.node, Reason: r.ref.Reason, Message: r.ref.Message}
		case r.err != nil:
			p.forget(l, false)
			var ue *url.Error
			if errors.As(r.err, &ue) && ctx.Err() == nil { // no answer at all, and not our own stop
				p.agentUnreachable(l.node, r.err)
			}
			return scheduler.WorkerResult{LaunchIDs: ids, Node: l.node, Reason: ReasonAgentUnreachable, Message: r.err.Error()}
		}
	}
	p.log.Info("worker launches sent", "profile", spec.Profile, "launches", ids)

	// Wait until every launch is ready.
	limit := p.o.Config().Defaults.WorkerStartTimeout
	if limit <= 0 {
		limit = config.DefaultDefaults().WorkerStartTimeout
	}
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		ready := 0
		for _, l := range ls {
			if ex := l.exitInfo(); ex != nil {
				return scheduler.WorkerResult{LaunchIDs: ids, Node: l.node, Reason: scheduler.ReasonWorkerExited,
					Message: fmt.Sprintf("%s: %s", ex.Reason, ex.Message), Exit: ex}
			}
			if l.stateName() == agentproto.StateReady {
				ready++
			}
		}
		if ready == len(ls) {
			break
		}
		select {
		case <-ctx.Done():
			return scheduler.WorkerResult{LaunchIDs: ids, Reason: ReasonAgentUnreachable, Message: "the scheduler is stopping"}
		case <-deadline.C:
			late := ls[0]
			for _, l := range ls {
				if l.stateName() != agentproto.StateReady {
					late = l
					break
				}
			}
			return scheduler.WorkerResult{LaunchIDs: ids, Node: late.node, Reason: ReasonWorkerStartTimeout,
				Message: fmt.Sprintf("the worker launch on %s was not ready within %v (worker_start_timeout)", late.node, limit)}
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, l := range ls {
		l.mu.Lock()
		l.report = report
		l.mu.Unlock()
		l.wake() // the supervisor reports what is already known
	}
	return scheduler.WorkerResult{LaunchIDs: ids}
}

func (p *Pool) dropLaunches(iid uint64, ls []*launch) {
	p.mu.Lock()
	defer p.unlock()
	for _, l := range ls {
		p.endLaunchLocked(l, true) // never sent
	}
	delete(p.byIID, iid)
	p.publishLogSnapLocked()
}

// forget drops a launch that the agent refused (nothing runs to stop), or
// whose launch call got no answer (refused false: it may have started).
func (p *Pool) forget(l *launch, refused bool) {
	p.mu.Lock()
	defer p.unlock()
	p.endLaunchLocked(l, refused)
	p.publishLogSnapLocked()
	ls := p.byIID[l.iid]
	for i, x := range ls {
		if x == l {
			p.byIID[l.iid] = append(ls[:i:i], ls[i+1:]...)
			break
		}
	}
}

// supervise starts the launch's supervisor.
func (p *Pool) supervise(l *launch) {
	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	l.cancel = cancel
	l.mu.Unlock()
	go func() {
		defer close(l.done)
		lease := time.NewTicker(p.o.LeaseEvery)
		defer lease.Stop()
		logs := time.NewTicker(2 * time.Second)
		defer logs.Stop()
		// The state is read 4 times a second while a load is in progress on
		// the node, then once a second (6.3).
		poll := time.NewTicker(pollLoading)
		defer poll.Stop()
		fast := true
		for {
			select {
			case <-ctx.Done():
				return
			case <-lease.C:
				p.renew(ctx, l)
			case <-logs.C:
				p.copyLog(ctx, l)
			case <-poll.C:
				p.poll(ctx, l)
				p.reportIfNeeded(l)
				if fast && !l.loading() {
					fast = false
					poll.Reset(pollAfterLoad)
				}
			case <-l.changed:
				p.reportIfNeeded(l)
			}
		}
	}()
}

func (p *Pool) renew(ctx context.Context, l *launch) {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, l.base+agentproto.PathLaunch+"/"+l.id+"/lease", nil)
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		p.log.Debug("lease renewal failed", "launch", l.id, "node", l.node, "err", err)
		return // the lease decides; the next renewal is on time
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		p.poll(ctx, l) // not the current launch any more: read what it says
	}
}

func (p *Pool) poll(ctx context.Context, l *launch) {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, l.base+agentproto.PathLaunch, nil)
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		if ctx.Err() == nil { // not the launch's own stop
			p.agentUnreachable(l.node, err)
		}
		return
	}
	defer resp.Body.Close()
	var st agentproto.LaunchState
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&st) != nil {
		return
	}
	l.mu.Lock()
	switch {
	case st.State == agentproto.StateNone || (st.LaunchID != "" && st.LaunchID != l.id):
		// The agent no longer has this launch (it restarted, 6.1).
		if l.goneWhy == "" {
			l.goneWhy = agentproto.InstanceReasonAgentRestarted
			l.goneMsg = fmt.Sprintf("the agent reports launch %q (%s), not %s", st.LaunchID, st.State, l.id)
		}
	default:
		l.st, l.have = st, true
	}
	l.mu.Unlock()
}

// reportIfNeeded sends the launch's load progress and exit once Launch has
// handed over (report set).
func (p *Pool) reportIfNeeded(l *launch) {
	l.mu.Lock()
	rep := l.report
	var load *agentproto.LoadProgress
	if l.have && l.st.Load != nil {
		b, _ := json.Marshal(l.st.Load)
		if string(b) != l.lastLoad {
			l.lastLoad = string(b)
			load = l.st.Load
		}
	}
	reported := l.reported
	l.mu.Unlock()
	if rep == nil {
		return
	}
	if load != nil {
		rep(scheduler.WorkerReport{Node: l.node, Addr: l.rpcAddr, Load: load})
	}
	if ex := l.exitInfo(); ex != nil && !reported {
		l.mu.Lock()
		l.reported = true
		l.mu.Unlock()
		p.copyLog(context.Background(), l)
		p.log.Warn("worker launch exited", "launch", l.id, "node", l.node, "reason", ex.Reason, "message", ex.Message)
		rep(scheduler.WorkerReport{Node: l.node, Addr: l.rpcAddr, Exit: ex})
	}
}

// copyLog appends the launch's new log bytes to <LogDir>/<launch id>.log.
func (p *Pool) copyLog(ctx context.Context, l *launch) {
	if p.o.LogDir == "" && l.feed == nil {
		return
	}
	l.mu.Lock()
	off := l.logOff
	l.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, l.base+agentproto.PathLaunchLog+"?since="+strconv.FormatInt(off, 10), nil)
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return
	}
	next, err := strconv.ParseInt(resp.Header.Get(agentproto.HeaderNextOffset), 10, 64)
	if err != nil {
		next = off + int64(len(data))
	}
	if len(data) > 0 && l.feed != nil {
		l.feed(data)
	}
	if len(data) > 0 && p.o.LogDir != "" {
		role := "worker-"
		if l.feed != nil {
			role = "host-"
		}
		f, err := os.OpenFile(filepath.Join(p.o.LogDir, role+l.name+"-"+l.id+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		if d := resp.Header.Get(agentproto.HeaderDroppedBytes); d != "" {
			fmt.Fprintf(f, "[potluck: %s bytes of the launch log were dropped on the node]\n", d)
		}
		f.Write(data)
		f.Close()
	}
	l.mu.Lock()
	if next > l.logOff {
		l.logOff = next
	}
	l.mu.Unlock()
}

var _ scheduler.WorkerExitReader = (*Pool)(nil)

// Exits is scheduler.WorkerExitReader: the launches of load iid that ended by
// themselves (watchdog, oom_kill, lease_expired, start_failed, crashed ...).
// The scheduler calls it when the load's host program ended without being
// stopped: the host aborts within milliseconds of losing a worker, so the
// worker's own exit is often not read yet, and the agent may still report
// the launch as stopping; a stopping launch whose stop_reason is watchdog,
// oom_kill or lease_expired is taken as that cause at once (CR-9). Every member is read in parallel, again every
// 100 ms, until one has exited by itself or ExitReadWait has passed.
func (p *Pool) Exits(ctx context.Context, iid uint64) []scheduler.WorkerReport {
	p.mu.Lock()
	ls := append([]*launch(nil), p.byIID[iid]...)
	p.unlock()
	if len(ls) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.o.ExitReadWait)
	defer cancel()
	out := make([]*agentproto.ExitInfo, len(ls))
	var wg sync.WaitGroup
	for i, l := range ls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out[i] = p.ownExit(ctx, l); out[i] != nil {
				cancel() // one explains the exit; the others are not waited for
			}
		}()
	}
	wg.Wait()
	var rs []scheduler.WorkerReport
	for i, l := range ls {
		if out[i] != nil {
			p.log.Info("worker launch had ended by itself when the host program ended", "launch", l.id, "node", l.node,
				"reason", out[i].Reason, "message", out[i].Message)
			rs = append(rs, scheduler.WorkerReport{IID: iid, Node: l.node, Addr: l.rpcAddr, Exit: out[i]})
		}
	}
	return rs
}

// ownExit reads the launch until it has exited, is stopping for a cause of
// its own, or ctx ends. It returns the exit when the launch ended by itself, nil when it was stopped on request,
// is another launch, or did not exit in time.
func (p *Pool) ownExit(ctx context.Context, l *launch) *agentproto.ExitInfo {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.base+agentproto.PathLaunch, nil)
		if resp, err := p.o.HTTP.Do(req); err == nil {
			var st agentproto.LaunchState
			derr := json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			switch {
			case derr != nil || resp.StatusCode != http.StatusOK:
			case st.State == agentproto.StateNone || st.LaunchID != l.id:
				return nil
			case st.State == agentproto.StateExited:
				if st.Exit == nil {
					return nil
				}
				switch st.Exit.Reason {
				case agentproto.ExitStopped, agentproto.ExitForced, agentproto.ExitReset:
					return nil
				}
				return st.Exit
			case st.State == agentproto.StateStopping:
				// CR-9: the agent has decided to stop the launch for a cause of
				// its own and says so before the process has gone.
				switch st.StopReason {
				case agentproto.ExitWatchdog, agentproto.ExitOOMKill, agentproto.ExitLeaseExpired:
					return &agentproto.ExitInfo{Reason: st.StopReason, Message: st.StopMessage, LogTail: []string{}}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop is scheduler.WorkerLauncher.Stop: every launch of load iid is
// stopped (force for the nodes in force), off the caller's goroutine.
func (p *Pool) Stop(iid uint64, force []core.NodeID) {
	p.mu.Lock()
	p.stopped[iid] = true
	ls := p.byIID[iid]
	delete(p.byIID, iid)
	type job struct {
		l     *launch
		force bool
		lost  bool
	}
	var jobs []job
	for _, l := range ls {
		f := false
		for _, n := range force {
			f = f || n == l.node
		}
		a := p.agents[string(l.node)]
		jobs = append(jobs, job{l, f, a == nil || a.lost || a.left})
	}
	p.unlock()
	p.stops.Add(1)
	go func() {
		defer p.stops.Done()
		var wg sync.WaitGroup
		for _, j := range jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.stopLaunch(j.l, j.force, j.lost)
			}()
		}
		wg.Wait()
	}()
}

// stopExitWait is how long a plain stop waits for the launch to report
// exited (a variable for the tests).
var stopExitWait = 15 * time.Second

// stopLaunch stops one launch: DELETE, or forced DELETE then /reset after 5 s
// (7.2). A lost node is not waited for (11).
func (p *Pool) stopLaunch(l *launch, force, lost bool) {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel() // no more lease renewals
		<-l.done
	}
	confirmed := false // the agent confirmed the launch no longer runs (TS1)
	defer func() {
		p.mu.Lock()
		p.endLaunchLocked(l, confirmed)
		p.publishLogSnapLocked()
		p.unlock()
	}()
	if lost {
		p.log.Info("worker launch not stopped: its node is lost", "launch", l.id, "node", l.node)
		return
	}
	l.mu.Lock()
	gone := l.goneWhy
	l.mu.Unlock()
	if gone != "" {
		// The agent restarted and no longer has this launch: a stop would be
		// answered 409 (its current launch is another one) or find none.
		p.log.Debug("worker launch not stopped: it ended with its agent", "launch", l.id, "node", l.node, "reason", gone)
		confirmed = true
		return
	}
	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	p.log.Info("stopping worker launch", "launch", l.id, "node", l.node, "force", force)
	if err := p.deleteLaunch(ctx, l.base, l.id, force); err != nil {
		p.log.Warn("stopping worker launch failed", "launch", l.id, "node", l.node, "err", err)
	}
	if force {
		confirmed = p.forceTail(ctx, l.base, l.id, string(l.node))
	} else {
		confirmed = p.waitExited(ctx, l.base, l.id, stopExitWait)
	}
	p.copyLog(ctx, l)
}

// forceTail waits up to 5 s for the launch to report exited, then resets the
// agent. It reports whether the launch is known to have ended (exited, or the
// reset was answered).
func (p *Pool) forceTail(ctx context.Context, base, id, node string) bool {
	if p.waitExited(ctx, base, id, ForceResetWait) {
		return true
	}
	p.log.Warn("forced stop did not end the launch within 5 s; resetting the agent", "launch", id, "node", node)
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, base+agentproto.PathReset, nil)
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		p.log.Warn("reset failed", "node", node, "err", err)
		return false
	}
	var ra agentproto.ResetAnswer
	json.NewDecoder(resp.Body).Decode(&ra)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		p.log.Warn("reset failed", "node", node, "status", resp.StatusCode)
		return false
	}
	p.log.Info("agent reset", "node", node, "killed", len(ra.Killed))
	p.mu.Lock()
	if a := p.agents[node]; a != nil {
		p.recordLocked(history.TNodeAdmin, history.Warn, a, fmt.Sprintf("Node %s was reset after a forced stop did not end launch %s within 5 s.", a.reg.Name, id),
			map[string]any{"launch_id": id, "killed": len(ra.Killed)})
	}
	p.unlock()
	return true
}

// waitExited polls GET /launch until the launch is exited (or another launch
// or none is current), for at most d.
func (p *Pool) waitExited(ctx context.Context, base, id string, d time.Duration) bool {
	end := time.Now().Add(d)
	for {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, base+agentproto.PathLaunch, nil)
		resp, err := p.o.HTTP.Do(req)
		if err == nil {
			var st agentproto.LaunchState
			derr := json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			if derr == nil && (st.State == agentproto.StateExited || st.State == agentproto.StateNone || st.LaunchID != id) {
				cancel()
				return true
			}
		}
		cancel()
		if time.Now().After(end) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ForceNode is scheduler.WorkerLauncher.ForceNode: the launch the node last
// reported is stopped with force, then the agent is reset if needed.
func (p *Pool) ForceNode(node core.NodeID) {
	p.mu.Lock()
	a := p.agents[string(node)]
	if a == nil || a.lost || a.left || a.hb == nil || a.hb.LaunchID == nil {
		p.unlock()
		return // a lost node is treated as holding nothing (7.2)
	}
	id := *a.hb.LaunchID
	base := "http://" + net.JoinHostPort(a.reg.Addr, strconv.Itoa(p.o.AgentPort))
	p.unlock()
	p.stops.Add(1)
	go func() {
		defer p.stops.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.deleteLaunch(ctx, base, id, true); err != nil {
			p.log.Warn("forced stop failed", "node", node, "launch", id, "err", err)
		}
		p.forceTail(ctx, base, id, string(node))
	}()
}

// ---- agent HTTP ----

// postLaunch sends POST /launch. A refusal comes back as ref; err is a
// transport failure or an answer that is neither.
func (p *Pool) postLaunch(ctx context.Context, base string, lr agentproto.LaunchRequest) (agentproto.LaunchState, *agentproto.LaunchRefusal, error) {
	b, _ := json.Marshal(lr)
	// The agent may wait for a device read and a port, so the answer may take
	// long; a connection that cannot be made fails after ConnectTimeout (the
	// HTTP client's dialer).
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, base+agentproto.PathLaunch, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		return agentproto.LaunchState{}, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		var st agentproto.LaunchState
		if err := json.Unmarshal(body, &st); err != nil {
			return st, nil, fmt.Errorf("launch answer: %v", err)
		}
		return st, nil, nil
	}
	var ref agentproto.LaunchRefusal
	if json.Unmarshal(body, &ref) == nil && ref.Reason != "" {
		return agentproto.LaunchState{}, &ref, nil
	}
	return agentproto.LaunchState{}, nil, fmt.Errorf("launch answered %d: %s", resp.StatusCode, clip(string(body)))
}

func (p *Pool) deleteLaunch(ctx context.Context, base, id string, force bool) error {
	u := base + agentproto.PathLaunch + "/" + id
	if force {
		u += "?force=1"
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodDelete, u, nil)
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return errors.New("the agent's current launch is another one (409); nothing stopped")
	}
	return fmt.Errorf("stop answered %d", resp.StatusCode)
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// ReasonAgentUnreachable is scheduler.ReasonAgentUnreachable.
const ReasonAgentUnreachable = scheduler.ReasonAgentUnreachable

// HeldLaunches lists the launches this run holds, for tests and status.
func (p *Pool) HeldLaunches() []string {
	p.mu.Lock()
	defer p.unlock()
	var out []string
	for id := range p.launches {
		out = append(out, id)
	}
	return out
}
