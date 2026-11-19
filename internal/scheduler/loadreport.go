package scheduler

import (
	"fmt"
	"math"
	"time"
)

// The structured load report (design 7.6a): a `load` object on a loading
// instance, and `recent_loads` with the outcomes of finished load attempts.
// A load attempt (a "session") begins when its child starts and ends when the
// instance is ready, the load fails, or it is cancelled.

// RecentLoadRetention is how long a finished load attempt stays in
// recent_loads.
const RecentLoadRetention = 120 * time.Second

// Load attempt outcomes.
const (
	LoadReady     = "ready"
	LoadFailed    = "failed"
	LoadCancelled = "cancelled"
)

// Worker upload states.
const (
	WorkerWaiting   = "waiting"
	WorkerReceiving = "receiving"
	WorkerDone      = "done"
)

const (
	handshakeBytes = 1 << 20 // a worker with less than this received nothing but handshake
	doneFraction   = 0.99    // a worker is done when it has this share of its bytes acknowledged
	mibBytes       = 1 << 20
)

// LoadReport is the `load` object of a loading instance. Pointer fields are
// left out of the JSON when nil.
type LoadReport struct {
	SessionID  string
	StartedAt  int64 // unix ms; the child started
	ElapsedS   float64
	BytesTotal *int64 // whole load; nil for a load with no remote worker
	BytesSent  *int64 // nil when it cannot be measured
	Progress   *float64
	EtaS       *float64
	StalledS   *float64 // seconds since the bytes last rose; only when over 15 and measured
	Source     *string  // "measured" or "estimated"
	Workers    []LoadWorker
}

// LoadWorker is one remote worker of a load. The array is in --rpc order;
// the upload order is llama.cpp's choice and is not guaranteed.
type LoadWorker struct {
	Addr       string
	BytesTotal int64
	BytesSent  *int64 // nil unless the figure is measured
	State      string // waiting, receiving, done
}

// LoadOutcome is one finished load attempt (an entry of recent_loads).
type LoadOutcome struct {
	SessionID  string
	Model      string
	Workers    []string
	Outcome    string // ready, failed, cancelled
	Reason     string // plain words; empty for ready
	FinishedAt int64  // unix ms
	LoadS      float64
}

// LoadStart is the payload of the loadStarted feed event.
type LoadStart struct {
	SessionID string
	Model     string
	StartedAt int64 // unix ms
	Workers   []string
}

// sessionID is unique across restarts: the child's start time in unix ms
// plus the instance id (which restarts from 1 with the scheduler).
func sessionID(at time.Time, iid uint64) string {
	return fmt.Sprintf("ld-%d-%d", at.UnixMilli(), iid)
}

func (in *inst) loadActive() bool {
	return in.sessionID != "" && !in.sessionDone
}

// beginLoad opens the load attempt of an instance whose child just started.
func (c *schedCore) beginLoad(in *inst) {
	in.sessionAt = c.now
	in.sessionID = sessionID(c.now, in.iid)
	in.sessionDone = false
	in.peerSent = map[string]int64{}
	in.peerGrowth = map[string]time.Time{}
	c.emit(FeedLoadStarted, in.prof.Name, func(e *FeedEvent) {
		e.LoadStarted = &LoadStart{SessionID: in.sessionID, Model: in.prof.Name, StartedAt: c.now.UnixMilli(),
			Workers: append([]string{}, in.peers...)}
	})
}

// finishLoad closes the load attempt once and records its outcome.
func (c *schedCore) finishLoad(in *inst, outcome, reason string) {
	if !in.loadActive() {
		return
	}
	in.sessionDone = true
	o := LoadOutcome{SessionID: in.sessionID, Model: in.prof.Name, Workers: append([]string{}, in.peers...),
		Outcome: outcome, Reason: reason, FinishedAt: c.now.UnixMilli(), LoadS: round(c.now.Sub(in.sessionAt).Seconds(), 1)}
	c.recent = append([]LoadOutcome{o}, c.recent...)
	c.pruneRecent()
	c.changed = true
	c.emit(FeedLoadFinished, in.prof.Name, func(e *FeedEvent) { cp := o; e.Outcome = &cp })
}

// cancelReason puts a stop reason into plain words.
func cancelReason(stop string) string {
	switch stop {
	case "admin":
		return "unloaded while loading"
	case "shutdown":
		return "scheduler shutting down"
	case "":
		return "stopped while loading"
	}
	return stop
}

// pruneRecent drops outcomes older than RecentLoadRetention.
func (c *schedCore) pruneRecent() {
	cut := c.now.Add(-RecentLoadRetention).UnixMilli()
	n := len(c.recent)
	for n > 0 && c.recent[n-1].FinishedAt < cut {
		n--
	}
	c.recent = c.recent[:n]
}

// recentLoads is a copy of the outcomes still retained, newest first.
func (c *schedCore) recentLoads() []LoadOutcome {
	c.pruneRecent()
	return append([]LoadOutcome{}, c.recent...)
}

// workerTotals are each worker's bytes: its share of the file by the tensor
// split, replaced by the size the child's log reports once it has printed it
// (the RPC model buffer of that address).
func (c *schedCore) workerTotals(in *inst) []int64 {
	out := append([]int64(nil), in.workerEst...)
	if in.child == nil {
		return out
	}
	for _, d := range in.child.Load().Devices {
		if d.Host || d.ModelMiB <= 0 {
			continue
		}
		addr := endpointOf(d.Name)
		for i, p := range in.peers {
			if p == addr && i < len(out) {
				out[i] = int64(math.Round(d.ModelMiB * mibBytes))
			}
		}
	}
	return out
}

// loadWorkers builds the per-worker view. Without a measurement nothing is
// known about a worker's progress, so every one is waiting and has no sent
// figure.
func (c *schedCore) loadWorkers(in *inst, measured bool) []LoadWorker {
	totals := c.workerTotals(in)
	ws := make([]LoadWorker, len(in.peers))
	sent := make([]int64, len(ws))
	for i, p := range in.peers {
		ws[i] = LoadWorker{Addr: p, BytesTotal: totals[i], State: WorkerWaiting}
		if measured {
			sent[i] = in.peerSent[p]
			s := sent[i]
			ws[i].BytesSent = &s
		}
	}
	if !measured {
		return ws
	}
	// Each worker's state comes from its own bytes only: workers receive one
	// at a time, but llama.cpp picks the order, so nothing is inferred from
	// the other workers.
	for i := range ws {
		switch {
		case totals[i] > 0 && float64(sent[i]) >= doneFraction*float64(totals[i]):
			ws[i].State = WorkerDone
		case sent[i] >= handshakeBytes:
			ws[i].State = WorkerReceiving
		}
	}
	return ws
}

// loadReport is the `load` object of a loading instance, or nil when it has
// no running load attempt.
func (c *schedCore) loadReport(in *inst) *LoadReport {
	if in.state != stStarting || in.phase != phLoading || !in.loadActive() {
		return nil
	}
	v := c.loadView(in)
	r := &LoadReport{SessionID: in.sessionID, StartedAt: in.sessionAt.UnixMilli(),
		ElapsedS:   round(c.now.Sub(in.sessionAt).Seconds(), 1),
		BytesTotal: v.total, BytesSent: v.sent, Progress: v.progress, EtaS: v.eta, StalledS: v.stalled, Source: v.source, Workers: []LoadWorker{}}
	if v.total != nil {
		r.Workers = c.loadWorkers(in, v.sent != nil && in.peerKnown)
	}
	return r
}
