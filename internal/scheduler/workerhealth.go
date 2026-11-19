package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

// Worker health detection (design 8.4a). Detection and reporting only: a
// worker that hangs is named, its node is quarantined, and nothing else is
// done to it. Killing instances stays with node loss, load stall and the
// load timeout.
//
// Every remote worker that backs an instance (loading or ready) is watched on
// three channels: (A) upload progress, (B) the fleet heartbeat, (C) a plain
// TCP connect to its rpc address. A and B are always on. C is OFF by default
// (worker_probe_interval 0): an rpc-server accepts one client and does not
// call accept() again until it leaves, so extra connections land in the kernel
// listen backlog, "succeed" whatever the server's state, and fill the backlog
// until every connect times out. C stays for workers that accept parallel
// connections. The connect runs off the event loop (effWorkerProbe) and comes
// back as evWorkerProbe, like every other slow job.

// Worker health states.
const (
	HealthOK      = "ok"
	HealthSuspect = "suspect"
	HealthHung    = "hung"
)

// Channel states.
const (
	ChanOK      = "ok"
	ChanSilent  = "silent"
	ChanUnknown = "unknown"
)

const (
	workerProbeTimeout  = 2 * time.Second // a connect that takes longer is a failure
	probeFailuresSilent = 3               // consecutive failures that make channel C silent
	probeOnlyFactor     = 3               // C alone must be silent for this many worker_silence to be hung
	heartbeatOnlyFactor = 3               // B alone (no channel known ok) must be silent this many worker_silence to be hung
)

// Channel indexes.
const (
	chUpload = iota
	chHeartbeat
	chProbe
	nChannels
)

var channelNames = [nChannels]string{"upload", "heartbeat", "probe"}

// WorkerProber makes the connect probe. It must not send anything. Connect
// returns nil when the TCP connection was made (it is closed again at once).
type WorkerProber interface {
	Connect(ctx context.Context, addr string, timeout time.Duration) error
}

// TCPConnectProber is the production WorkerProber: dial, close, nothing sent.
type TCPConnectProber struct{}

// Connect implements WorkerProber.
func (TCPConnectProber) Connect(ctx context.Context, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// workerHealth is the scheduler's record of one watched worker.
type workerHealth struct {
	addr  string
	state string
	since time.Time // when the state was entered

	// lost: node loss killed the instance that used this worker while it was
	// offline or absent in a live view. It is then kept as hung, with no instance
	// using it, until it is seen online in a live view again (keepLostNodeHung).
	lost bool

	// The last evaluation, for status.
	ch       [nChannels]string
	silentAt [nChannels]time.Time // when the channel became silent; zero unless silent

	// Channel C.
	gen       uint64
	probing   bool
	nextProbe time.Time
	probed    bool      // at least one result
	streak    int       // consecutive failures
	probeAt   time.Time // when the streak reached probeFailuresSilent; zero when below
}

// workerUse is a remote worker that backs at least one instance.
type workerUse struct {
	addr  string
	insts []*inst
}

// probeOn says the connect probe channel is enabled (worker_probe_interval > 0).
func (c *schedCore) probeOn() bool { return c.cfg.Defaults.WorkerProbeInterval > 0 }

// workersInUse maps every remote node that backs an instance that is loading
// (child running) or ready to its address and instances. A worker no instance
// uses is never in the map, so it is never probed.
func (c *schedCore) workersInUse() map[core.NodeID]*workerUse {
	out := map[core.NodeID]*workerUse{}
	for _, in := range c.sortedInsts() {
		switch {
		case in.state == stStarting && in.phase >= phLoading, in.state == stReady, in.state == stDraining:
		default:
			continue
		}
		for _, pn := range in.placement.Nodes {
			if pn.Local || pn.Addr == "" {
				continue
			}
			u := out[pn.ID]
			if u == nil {
				u = &workerUse{addr: pn.Addr}
				out[pn.ID] = u
			}
			u.insts = append(u.insts, in)
		}
	}
	return out
}

// uploadState is the worker's upload state for a loading instance, and
// whether it is known. It is not known when the instance is not loading a
// remote model, or when the byte count is only estimated.
func (c *schedCore) uploadState(in *inst, addr string) (string, bool) {
	if in.state != stStarting || in.phase != phLoading || in.remoteBytes <= 0 {
		return "", false
	}
	if !in.measured || !in.peerKnown {
		return "", false
	}
	for _, w := range c.loadWorkers(in, true) {
		if w.Addr == addr {
			return w.State, true
		}
	}
	return "", false
}

// probeBlocked says the connect probe must not run now: the worker is being
// uploaded to (or might be: the upload is not measured), or an instance that
// uses it has a request in flight and worker_probe_during_inference is off.
// A worker serves one client at a time and it is not known whether an extra
// connection disturbs a live session.
func (c *schedCore) probeBlocked(u *workerUse) bool {
	if !c.probeOn() {
		return true
	}
	for _, in := range u.insts {
		if in.state == stStarting && in.phase == phLoading && in.remoteBytes > 0 {
			if st, known := c.uploadState(in, u.addr); !known || st == WorkerReceiving {
				return true
			}
		}
		if in.inFlight > 0 && !c.cfg.Defaults.WorkerProbeDuringInference {
			return true
		}
	}
	return false
}

// workerProbed takes the result of a connect probe.
func (c *schedCore) workerProbed(id core.NodeID, gen uint64, err error) {
	h := c.health[id]
	if h == nil || h.gen != gen || !h.probing {
		return
	}
	h.probing = false
	h.probed = true
	if err == nil {
		h.streak, h.probeAt = 0, timeZero
		return
	}
	h.streak++
	if h.streak >= probeFailuresSilent && h.probeAt.IsZero() {
		h.probeAt = c.now
	}
	c.log.Debug("worker connect probe failed", "node", id, "addr", h.addr, "streak", h.streak, "err", err)
}

// evalChannels sets the three channel states of h from what is known now.
func (c *schedCore) evalChannels(id core.NodeID, h *workerHealth, u *workerUse) {
	silence := c.cfg.Defaults.WorkerSilence
	h.ch = [nChannels]string{ChanUnknown, ChanUnknown, ChanUnknown}
	h.silentAt = [nChannels]time.Time{}

	// A: upload progress, only while this worker is receiving.
	for _, in := range u.insts {
		if st, known := c.uploadState(in, u.addr); known && st == WorkerReceiving {
			h.ch[chUpload] = ChanOK
			if g := in.peerGrowth[u.addr]; !g.IsZero() && !c.now.Before(g.Add(silence)) {
				h.ch[chUpload], h.silentAt[chUpload] = ChanSilent, g.Add(silence)
			}
			break
		}
	}

	// B: the fleet heartbeat; unknown while the view is stale.
	if c.fresh {
		h.ch[chHeartbeat] = ChanOK
		if ns := c.nodes[id]; ns != nil && !ns.missingSince.IsZero() && !c.now.Before(ns.missingSince.Add(silence)) {
			h.ch[chHeartbeat], h.silentAt[chHeartbeat] = ChanSilent, ns.missingSince.Add(silence)
		}
	}

	// C: the connect probe; unknown when it has not run (or may not run now).
	if c.probeOn() && h.probed && !c.probeBlocked(u) {
		h.ch[chProbe] = ChanOK
		if h.streak >= probeFailuresSilent {
			h.ch[chProbe], h.silentAt[chProbe] = ChanSilent, h.probeAt
		}
	}
}

// healthState is the state the channels give, given the state it is in: hung
// is left only when no channel is silent and at least one is known to be ok.
func (c *schedCore) healthState(h *workerHealth) string {
	silent, ok := 0, 0
	for _, s := range h.ch {
		switch s {
		case ChanSilent:
			silent++
		case ChanOK:
			ok++
		}
	}
	probeOnly := silent == 1 && h.ch[chProbe] == ChanSilent && h.ch[chHeartbeat] == ChanUnknown &&
		c.now.Sub(h.silentAt[chProbe]) >= probeOnlyFactor*c.cfg.Defaults.WorkerSilence
	// B alone, no channel known ok: an idle loaded model whose node stopped
	// reporting (upload is only known during an upload, the probe may be off).
	hbOnly := silent == 1 && h.ch[chHeartbeat] == ChanSilent && ok == 0 &&
		c.now.Sub(h.silentAt[chHeartbeat]) >= heartbeatOnlyFactor*c.cfg.Defaults.WorkerSilence
	switch {
	case silent >= 2 || probeOnly || hbOnly:
		return HealthHung
	case h.state == HealthHung && (silent > 0 || ok == 0):
		return HealthHung // recovery needs every known channel healthy again
	case silent == 1:
		return HealthSuspect
	}
	return HealthOK
}

// silentText is "heartbeat=30s,probe=24s" for the log.
func (c *schedCore) silentText(h *workerHealth) string {
	var parts []string
	for i, s := range h.ch {
		if s == ChanSilent {
			parts = append(parts, fmt.Sprintf("%s=%v", channelNames[i], c.now.Sub(h.silentAt[i]).Round(time.Second)))
		}
	}
	return strings.Join(parts, ",")
}

func instNames(u *workerUse) string {
	names := make([]string, 0, len(u.insts))
	for _, in := range u.insts {
		names = append(names, in.prof.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// healthStatus is the status object of h; silent_for_s counts from the moment
// the channel became silent.
func (c *schedCore) healthStatus(h *workerHealth) *WorkerHealthStatus {
	s := &WorkerHealthStatus{State: h.state, Since: h.since.UnixMilli(),
		Channels:   WorkerChannels{Upload: h.ch[chUpload], Heartbeat: h.ch[chHeartbeat], Probe: h.ch[chProbe]},
		SilentForS: map[string]float64{}}
	for i, st := range h.ch {
		if st == ChanSilent {
			s.SilentForS[channelNames[i]] = round(c.now.Sub(h.silentAt[i]).Seconds(), 1)
		}
	}
	return s
}

// workerHealthStep runs the whole feature for one step: forget workers no
// instance uses, start due probes, evaluate the channels and act on a change.
func (c *schedCore) workerHealthStep() {
	use := c.workersInUse()
	var watched []core.NodeID
	for id := range c.health {
		watched = append(watched, id)
	}
	sort.Slice(watched, func(i, j int) bool { return watched[i] < watched[j] })
	// A lost node is back when a live view shows it online. It stays in the
	// table for this step (so its recovery episode ends with the right reason).
	returned := map[core.NodeID]bool{}
	for _, id := range watched {
		if h := c.health[id]; h.lost && c.fresh {
			if n, ok := c.raw[id]; ok && n.Online {
				h.lost = false
				returned[id] = true
			}
		}
	}
	for _, id := range watched {
		if h := c.health[id]; use[id] == nil && !h.lost && !returned[id] {
			c.dropHealth(id, h)
		}
	}
	for _, id := range watched {
		if returned[id] && use[id] == nil {
			c.setHealth(id, c.health[id], nil, HealthOK, timeZero)
		}
	}
	for _, id := range c.sortedNodeIDs() {
		u := use[id]
		if u == nil {
			continue
		}
		if h := c.health[id]; h != nil && h.lost {
			continue // killed off by node loss and not back yet: nothing to evaluate
		}
		h := c.health[id]
		if h == nil || h.addr != u.addr {
			h = &workerHealth{addr: u.addr, state: HealthOK, since: c.now, nextProbe: c.now}
			c.health[id] = h
		}
		if c.probeOn() && !h.probing && !c.now.Before(h.nextProbe) && !c.probeBlocked(u) {
			c.probeGen++
			h.gen, h.probing = c.probeGen, true
			h.nextProbe = c.now.Add(c.cfg.Defaults.WorkerProbeInterval)
			c.effects = append(c.effects, effWorkerProbe{id: id, addr: u.addr, gen: h.gen})
		}
		c.evalChannels(id, h, u)
		if next := c.healthState(h); next != h.state {
			c.setHealth(id, h, u, next, timeZero)
		}
	}
}

// keepLostNodeHung is design 8.4a's rule for a node that node loss has just
// killed an instance on: it was offline or absent for nodeLossAfter, so its
// heartbeat is silent, and with no instance using it the normal tracking would
// forget it before it could ever be called hung. It is kept as hung (heartbeat
// silent since it went missing), quarantined, until a live view shows it online
// again; recovery requests, when enabled, run on that episode as for any hung node.
func (c *schedCore) keepLostNodeHung(id core.NodeID, in *inst) {
	missingSince := c.ns(id).missingSince
	addr := ""
	for _, pn := range in.placement.Nodes {
		if pn.ID == id {
			addr = pn.Addr
		}
	}
	h := c.health[id]
	if h == nil {
		h = &workerHealth{addr: addr, state: HealthOK, since: c.now}
		c.health[id] = h
	}
	h.lost = true
	if h.state == HealthHung {
		return // already hung (and quarantined) with its own episode start
	}
	h.ch = [nChannels]string{ChanUnknown, ChanSilent, ChanUnknown}
	h.silentAt = [nChannels]time.Time{}
	h.silentAt[chHeartbeat] = missingSince
	c.setHealth(id, h, &workerUse{addr: addr, insts: []*inst{in}}, HealthHung, missingSince)
}

// setHealth enters a new state. since is when it began (zero: now); a node
// kept hung after node loss began its episode when it went missing.
func (c *schedCore) setHealth(id core.NodeID, h *workerHealth, u *workerUse, next string, since time.Time) {
	prev, prevSince := h.state, h.since
	if since.IsZero() {
		since = c.now
	}
	h.state, h.since = next, since
	c.changed = true
	c.emit(FeedWorkerHealth, "", func(e *FeedEvent) {
		e.Worker = &WorkerHealthEvent{Node: id, Addr: h.addr, Health: c.healthStatus(h)}
	})
	insts := ""
	if u != nil {
		insts = instNames(u)
	}
	name := c.nodeName(id)
	data := map[string]any{"state": next, "previous": prev, "addr": h.addr, "instances": insts,
		"channels": map[string]any{"upload": h.ch[chUpload], "heartbeat": h.ch[chHeartbeat], "probe": h.ch[chProbe]},
		"silent":   c.silentText(h), "node_lost": h.lost}
	switch {
	case next == HealthHung:
		c.log.Error("worker hung", "node", id, "addr", h.addr, "silent", c.silentText(h), "instances", insts)
		c.record(history.TWorkerHealth, history.Error, "",
			fmt.Sprintf("Worker %s is hung (%s); its node is quarantined.", name, c.silentText(h)), []string{name}, "", data)
		c.quarantine(id, errWorkerHung)
	case prev == HealthHung:
		hung := c.now.Sub(prevSince)
		c.log.Info("worker recovered", "node", id, "addr", h.addr, "hung_for", hung.Round(time.Second))
		data["hung_for_s"] = secs(hung)
		c.record(history.TWorkerHealth, history.Info, "",
			fmt.Sprintf("Worker %s recovered after being hung for %s.", name, hung.Round(time.Second)), []string{name}, "", data)
	default:
		sev := history.Info
		if next == HealthSuspect {
			sev = history.Warn
		}
		c.record(history.TWorkerHealth, sev, "",
			fmt.Sprintf("Worker %s health is now %s (was %s).", name, next, prev), []string{name}, "", data)
	}
}

var errWorkerHung = errors.New("worker hung")

// dropHealth forgets a worker no instance uses any more. If it was not ok,
// clients are told with an event that carries no health.
func (c *schedCore) dropHealth(id core.NodeID, h *workerHealth) {
	delete(c.health, id)
	if h.state == HealthOK {
		return
	}
	if h.state == HealthHung {
		c.log.Info("worker health no longer tracked", "node", id, "addr", h.addr,
			"was", h.state, "for", c.now.Sub(h.since).Round(time.Second), "reason", "no instance uses the worker")
	}
	c.emit(FeedWorkerHealth, "", func(e *FeedEvent) { e.Worker = &WorkerHealthEvent{Node: id, Addr: h.addr} })
}

// healthDeadlines adds the times at which a health rule may change something.
func (c *schedCore) healthDeadlines(add func(time.Time)) {
	silence := c.cfg.Defaults.WorkerSilence
	for id, h := range c.health {
		if c.probeOn() && !h.probing {
			add(h.nextProbe)
		}
		if ns := c.nodes[id]; ns != nil && !ns.missingSince.IsZero() {
			add(ns.missingSince.Add(silence))
			add(ns.missingSince.Add((1 + heartbeatOnlyFactor) * silence))
		}
		if !h.probeAt.IsZero() {
			add(h.probeAt.Add(probeOnlyFactor * silence))
		}
		for _, in := range c.insts {
			if g := in.peerGrowth[h.addr]; !g.IsZero() && h.ch[chUpload] == ChanOK {
				add(g.Add(silence))
			}
		}
	}
}
