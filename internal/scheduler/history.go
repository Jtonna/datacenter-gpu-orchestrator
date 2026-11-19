package scheduler

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/fleet"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

// Event history (design 9.1). The core never writes anything: at the points
// where it already logs a safety or lifecycle fact it appends a history.Event
// to c.hist, and the runner hands the batch to the HistorySink after the step,
// like the feed. The sink must not block (history.Store.Record never does);
// the file writing happens off the event loop in its own goroutine.

// HistorySink receives the history events of every step, in order. Record
// must never block or fail.
type HistorySink interface{ Record(history.Event) }

const (
	memorySampleEvery = 60 * time.Second // nodeMemory while a node backs an instance
	refuseDedupe      = 30 * time.Second // the same refusal for one profile is recorded this often at most
)

// takeHistory returns and clears the history outbox.
func (c *schedCore) takeHistory() []history.Event {
	out := c.hist
	c.hist = nil
	return out
}

// record appends an event stamped with the step's time.
func (c *schedCore) record(typ, sev, profile, summary string, nodes []string, session string, data map[string]any) {
	e := history.Event{Ts: c.now.UnixMilli(), Type: typ, Severity: sev, Profile: profile, Nodes: nodes,
		SessionID: session, Summary: summary, Data: data}
	if p := c.cfg.Profiles[profile]; p != nil {
		e.ModelFile = p.Model
	}
	if e.Nodes == nil {
		e.Nodes = []string{}
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	c.hist = append(c.hist, e)
}

// recordInst is record for an event about an instance: its profile, the
// nodes of its placement and its load session.
func (c *schedCore) recordInst(in *inst, typ, sev, summary string, data map[string]any) {
	if in.benchRun != "" {
		if data == nil {
			data = map[string]any{}
		}
		data["bench_run"] = in.benchRun
	}
	c.record(typ, sev, in.prof.Name, summary, c.placementNames(in), in.sessionID, data)
}

// nodeName is the hostname of a node, else its id.
func (c *schedCore) nodeName(id core.NodeID) string {
	if n, ok := c.raw[id]; ok {
		return nodeLabel(id, n)
	}
	return string(id)
}

func (c *schedCore) placementNames(in *inst) []string {
	out := make([]string, len(in.placement.Nodes))
	for i, pn := range in.placement.Nodes {
		out[i] = c.nodeName(pn.ID)
	}
	return out
}

func joinNames(n []string) string {
	if len(n) == 0 {
		return "no node"
	}
	return strings.Join(n, ", ")
}

func secs(d time.Duration) float64 { return round(d.Seconds(), 1) }

// ---- loads ----

func (c *schedCore) historyLoadStarted(in *inst) {
	data := map[string]any{"workers": append([]string{}, in.peers...), "bytes_total": in.remoteBytes}
	if info, err := c.need(in.prof); err == nil {
		data["need_mib"] = info.Need.MiB
		data["need_basis"] = c.needBasis(info)
	}
	c.recordInst(in, history.TLoadStarted, history.Info,
		fmt.Sprintf("Loading %s onto %s.", in.prof.Name, joinNames(c.placementNames(in))), data)
	c.sampleLoadMemory(in, "load_start")
}

// loadFigures are the per-node bytes and measured memory of a load.
func (c *schedCore) loadFigures(in *inst) []map[string]any {
	idx := map[string]int{}
	for i, pn := range in.placement.Nodes {
		if pn.Local {
			idx[c.build.LocalDevice] = i
		} else {
			idx[pn.Addr] = i
		}
	}
	mem := make([]float64, len(in.placement.Nodes))
	haveMem := make([]bool, len(mem))
	if in.child != nil {
		for _, d := range in.child.Load().Devices {
			if d.Host {
				continue
			}
			key := endpointOf(d.Name)
			if key == "" {
				key = d.Name
			}
			if i, ok := idx[key]; ok {
				mem[i] += d.TotalMiB()
				haveMem[i] = true
			}
		}
	}
	out := make([]map[string]any, 0, len(in.placement.Nodes))
	for i, pn := range in.placement.Nodes {
		f := map[string]any{"node": c.nodeName(pn.ID)}
		if b, ok := in.peerSent[pn.Addr]; ok && !pn.Local {
			f["bytes"] = b
		}
		if haveMem[i] {
			f["memory_mib"] = math.Round(mem[i]*10) / 10
		}
		out = append(out, f)
	}
	return out
}

func (c *schedCore) historyLoadFinished(in *inst, o LoadOutcome) {
	bytes, measured := in.sentBytes, in.sentBytes > 0
	if !measured && o.Outcome == LoadReady {
		bytes = in.remoteBytes
	}
	data := map[string]any{"outcome": o.Outcome, "reason": o.Reason, "load_s": o.LoadS, "bytes": bytes,
		"bytes_measured": measured, "nodes_detail": c.loadFigures(in)}
	if bytes > 0 && o.LoadS > 0 {
		data["avg_mb_s"] = round(float64(bytes)/1e6/o.LoadS, 1)
	}
	var sev, summary string
	switch o.Outcome {
	case LoadReady:
		sev = history.Info
		summary = fmt.Sprintf("%s finished loading in %.1f s", in.prof.Name, o.LoadS)
		if v, ok := data["avg_mb_s"].(float64); ok {
			summary += fmt.Sprintf(" (%.1f MB/s)", v)
		}
		summary += "."
	case LoadFailed:
		sev = history.Error
		summary = fmt.Sprintf("Loading %s failed after %.1f s: %s.", in.prof.Name, o.LoadS, trimDot(o.Reason))
	default:
		sev = history.Warn
		summary = fmt.Sprintf("Loading %s was cancelled after %.1f s: %s.", in.prof.Name, o.LoadS, trimDot(o.Reason))
	}
	c.recordInst(in, history.TLoadFinished, sev, summary, data)
	c.sampleLoadMemory(in, "load_end")
}

func trimDot(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") }

// ---- memory samples ----

// sampleMemory records one compact nodeMemory event for a shared-memory node:
// what the fleet reports available, the used figure and the live budget.
func (c *schedCore) sampleMemory(id core.NodeID, profile, why string) {
	n, ok := c.raw[id]
	if !ok || !n.SharedMemory || n.Local || !n.Online || n.FreeUnknown {
		return
	}
	data := map[string]any{"reason": why, "available_mib": n.FreeMiB, "total_mib": n.TotalMiB,
		"used_mib": max(n.TotalMiB-n.FreeMiB, 0), "budget_mib": c.liveBudget(id, n)}
	name := nodeLabel(id, n)
	c.record(history.TNodeMemory, history.Info, profile,
		fmt.Sprintf("Memory on %s: %d MiB available of %d MiB (%s).", name, n.FreeMiB, n.TotalMiB, strings.ReplaceAll(why, "_", " ")),
		[]string{name}, "", data)
	c.memNext[id] = c.now.Add(memorySampleEvery)
}

func (c *schedCore) sampleLoadMemory(in *inst, why string) {
	for _, pn := range in.placement.Nodes {
		c.sampleMemory(pn.ID, in.prof.Name, why)
	}
}

// memorySamples is the periodic part: a sample every minute for every
// shared-memory node that backs an instance; forgets the others.
func (c *schedCore) memorySamples() {
	backing := map[core.NodeID]string{}
	for _, in := range c.sortedInsts() {
		for _, pn := range in.placement.Nodes {
			if _, ok := backing[pn.ID]; !ok {
				backing[pn.ID] = in.prof.Name
			}
		}
	}
	for id := range c.memNext {
		if _, ok := backing[id]; !ok {
			delete(c.memNext, id)
		}
	}
	for _, id := range c.sortedNodeIDs() {
		prof, ok := backing[id]
		if !ok {
			continue
		}
		if next, seen := c.memNext[id]; !seen {
			c.memNext[id] = c.now.Add(memorySampleEvery)
		} else if !c.now.Before(next) {
			c.sampleMemory(id, prof, "periodic")
		}
	}
}

func (c *schedCore) memoryDeadlines(add func(time.Time)) {
	for _, t := range c.memNext {
		add(t)
	}
}

// ---- refused requests ----

// refusalReasons are the answers that are a decision about the cluster (the
// others are a bad name, a failed load that has its own event, a shutdown or a
// client that left).
func refusalRecorded(r Reason) bool {
	switch r {
	case ReasonUnknownModel, ReasonLoadFailed, ReasonShuttingDown, ReasonUnloaded, ReasonClientGone:
		return false
	}
	return true
}

// noteRefusal records that requests for a profile were answered without being
// served. The same profile and reason is recorded at most every refuseDedupe;
// the next record says how many were left out.
func (c *schedCore) noteRefusal(profile string, err *Error, waiters int, benchRun string) {
	if err == nil || !refusalRecorded(err.Reason) {
		return
	}
	key := profile + "\x00" + string(err.Reason)
	if r, ok := c.refused[key]; ok && c.now.Sub(r.at) < refuseDedupe {
		r.suppressed += waiters
		return
	}
	suppressed := 0
	if r, ok := c.refused[key]; ok {
		suppressed = r.suppressed
	}
	c.refused[key] = &refusal{at: c.now}
	data := map[string]any{"reason": string(err.Reason), "status": err.Status, "message": err.Message, "waiters": waiters}
	if suppressed > 0 {
		data["suppressed_since_last"] = suppressed
	}
	if err.Reason == ReasonDoesNotFit {
		data["need_mib"], data["required_mib"], data["capacity_mib"] = err.NeedMiB, err.RequiredMiB, err.CapacityMiB
	}
	if err.RetryAfter > 0 {
		data["retry_after_s"] = secs(err.RetryAfter)
	}
	if benchRun != "" {
		data["bench_run"] = benchRun
	}
	c.record(history.TRequestRefused, history.Warn, profile,
		fmt.Sprintf("Request for %s refused before start (%s): %s.", profile, err.Reason, trimDot(err.Message)),
		nil, "", data)
}

type refusal struct {
	at         time.Time
	suppressed int
}

// ---- unloads ----

// unloadReason puts the recorded stop reason in the words of the history:
// ttl, api, eviction, shutdown, node loss, load failed, child exited.
func (c *schedCore) unloadReason(in *inst) string {
	switch r := c.stopReasonOf(in); {
	case r == "admin":
		return "api"
	case strings.HasPrefix(r, "evicted for"):
		return "eviction"
	default:
		return r
	}
}

func (c *schedCore) historyUnloaded(in *inst) {
	reason := c.unloadReason(in)
	loadedFor := c.now.Sub(in.readyAt)
	sev := history.Info
	switch reason {
	case "node loss", "child exited", "load failed":
		sev = history.Warn
	}
	data := map[string]any{"reason": reason, "loaded_for_s": secs(loadedFor), "requests_served": in.served}
	if in.exit != "" {
		data["exit"] = in.exit
	}
	c.recordInst(in, history.TInstanceUnloaded, sev,
		fmt.Sprintf("%s was unloaded (%s) after %s loaded and %d requests served.", in.prof.Name, reason,
			loadedFor.Round(time.Second), in.served), data)
}

// ---- fleet view ----

// trackView records the fleet view going stale or coming back and the feed
// disconnecting or reconnecting. Called every step by refreshView.
func (c *schedCore) trackView() {
	connected := c.view.Connected()
	if !c.viewSeen {
		c.viewSeen = true
		c.viewFresh, c.viewConn = true, true // assume fine until the first look says otherwise
	}
	if c.fresh != c.viewFresh {
		if !c.fresh {
			c.staleAt = c.now
			c.record(history.TFleetViewStale, history.Warn, "",
				fmt.Sprintf("The fleet view is stale (%s, stale_after is %s); remote nodes are not used until it is live again.",
					c.ageText(), c.cfg.Fleet.StaleAfter),
				nil, "", map[string]any{"age_s": c.ageSeconds(), "stale_after_s": secs(c.cfg.Fleet.StaleAfter), "connected": connected})
		} else {
			c.record(history.TFleetViewRecovered, history.Info, "",
				fmt.Sprintf("The fleet view is live again after %s.", c.now.Sub(c.staleAt).Round(time.Second)),
				nil, "", map[string]any{"stale_for_s": secs(c.now.Sub(c.staleAt))})
		}
		c.viewFresh = c.fresh
	}
	if connected != c.viewConn {
		if !connected {
			c.downAt = c.now
			c.record(history.TFleetFeedDown, history.Warn, "",
				"The fleet event stream disconnected; the scheduler polls until it is back.", nil, "", nil)
		} else {
			c.record(history.TFleetFeedRecovered, history.Info, "",
				fmt.Sprintf("The fleet event stream reconnected after %s.", c.now.Sub(c.downAt).Round(time.Second)),
				nil, "", map[string]any{"down_for_s": secs(c.now.Sub(c.downAt))})
		}
		c.viewConn = connected
	}
}

func (c *schedCore) ageSeconds() any {
	if c.age < 0 || c.age == fleet.NeverReceived {
		return nil
	}
	return secs(c.age)
}

func (c *schedCore) ageText() string {
	if c.age < 0 || c.age == fleet.NeverReceived {
		return "no data has arrived yet"
	}
	return "nothing received for " + c.age.Round(time.Second).String()
}
