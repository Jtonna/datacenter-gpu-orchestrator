package main

import (
	"math"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Scenarios: what the status side shows, and for bench-* scenarios what the
// Benchmarks screens show. Every scenario except live builds a
// scheduler.Status (DEV-ONLY injection into /api/status, /api/profiles,
// /api/plan, /api/metrics/activity and the live channel's status frames); the
// profile list always comes from the real configuration.

// A scenario is a name, a description, a status builder and an optional
// frozen bench world. To add one, put it in a file of its own and register it
// from init:
//
//	func init() {
//		registerScenario(scenario{Name: "my-case", Description: "...", Status: scMyCase})
//	}
//
// Status gets the render time and how long the scenario has been selected
// (for creeping figures); nil means live (the real scheduler answers).
//
// Bench, when set, makes it a frozen bench scenario (bench_frozen.go): the real
// database is snapshotted, Prepare may change the snapshot, and a real
// benchmark manager runs Suite on it against the scripted scheduler until the
// FreezeLoad or FreezeChat point; /api/bench/... and the bench frames then come
// from that world. Example, a run held in the load of the 3-worker Gemma:
//
//	Bench: &frozenSpec{Suite: "load-scaling", FreezeLoad: map[string]float64{"bench-gemma-12b-3w": 0.4}},
//
// Without Bench, the Benchmarks screens show the real server's data.
type scenario struct {
	Name        string
	Description string
	StartupOnly bool // selectable only with --scenario at startup
	Status      func(now time.Time, t time.Duration) *statusBuilder
	// Decorate, when set, changes the scripted status document after it was
	// built (facts the scheduler.Status struct does not carry: agents_scenario.go).
	Decorate func(v map[string]any)
	Bench    *frozenSpec
	// Real marks a scenario that replays the recording of a real scheduler
	// (real.go); Raw is its status document.
	Real bool
	Raw  func() (map[string]any, error)
}

// registerScenario adds a scenario; a duplicate name panics at startup.
func registerScenario(sc scenario) {
	if _, dup := scenarioByName(sc.Name); dup || sc.Name == "" {
		panic("devcluster: duplicate or empty scenario name " + sc.Name)
	}
	scenarios = append(scenarios, sc)
}

var scenarios = []scenario{
	{Name: "demo", Description: "Default. qwen3.8-27b on two PS5s with 2 in flight, qwen3.5-4b on host-1, gemma-4-12b loading on the third PS5 (about 55%), devstral-24b queued.", Status: scDemo},
	{Name: "empty", Description: "Nothing loaded, empty queue, all three PS5s online and free.", Status: scEmpty},
	{Name: "ready", Description: "gemma-4-12b ready on console-1 for 38 min, idle, 212 requests served, last request 14.2 tok/s.", Status: scReady},
	{Name: "split", Description: "qwen3.8-27b split across all three PS5s with 2 in flight; qwen3.5-4b ready on host-1.", Status: scSplit},
	{Name: "twins", Description: "devstral-24b on console-1 and bench-devstral-24b on console-2: one model file, two instances; third PS5 free.", Status: scTwins},
	{Name: "loading", Description: "kimi-dev-72b-iq2s loading across three PS5s, measured progress about 40% (creeping), workers done / receiving / waiting, 1 waiter.", Status: scLoading},
	{Name: "stalled", Description: "The kimi-dev-72b-iq2s load stalled (stalled_s grows from 48), second worker stuck, its node suspect with upload silent.", Status: scStalled},
	{Name: "queued", Description: "qwen3.8-27b on three PS5s with 1 in flight; devstral-24b waiting unclaimed, gemma-4-12b claimed with qwen3.8-27b as victim; a backoff and a quarantine.", Status: scQueued},
	{Name: "refused", Description: "gemma-4-12b ready; a failed kimi-dev-72b-iq2s load 50 s ago with a backoff; fresh requestRefused and loadFinished failed history events.", Status: scRefused},
	{Name: "hung", Description: "qwen3.8-27b on three PS5s; console-2 hung and quarantined with a pending recovery request; console-3 offline.", Status: scHung},
	{Name: "stale", Description: "Like ready, but the fleet view is stale and disconnected (age 95 s).", Status: scStale},
	{Name: "bare", Description: "First start on an empty database (nothing imported): the real configured false and revision 0, no profiles, no tiers (no node eligible), history off, no benchmarks, empty audit. Only with --scenario bare at startup (history cannot be switched off at runtime).", StartupOnly: true, Status: scBare},
	{Name: "live", Description: "The real scheduler, nothing scripted: a chat request to /v1/chat/completions really loads a model through fakellama."},
	{Name: "bench-load", Description: "Frozen nightly run: step 7 kimi-72b-load-3w in its load phase at 40% (workers done / receiving / waiting).", Status: scBenchLoad,
		Bench: &frozenSpec{Suite: "nightly", FreezeLoad: map[string]float64{"bench-kimi-72b-3w": 0.40}, Lead: 270 * time.Second}},
	{Name: "bench-conversation", Description: "Frozen nightly run: gemma-conversation on bench-gemma-12b-2w, turn 3 of 5 streaming.", Status: scBenchConversation,
		Bench: &frozenSpec{Suite: "nightly", FreezeChat: map[string]int{"bench-gemma-12b-2w": 3}, Lead: 210 * time.Second}},
	{Name: "bench-last", Description: "Frozen nightly run: the last benchmark (8 of 8) devstral-load-requests in its requests phase, 4 of 6 requests done.", Status: scBenchLast,
		Bench: &frozenSpec{Suite: "nightly", FreezeChat: map[string]int{"bench-devstral-24b": 5}, Lead: 9 * time.Minute}},
	{Name: "bench-hold", Description: "Frozen phase-tour run: tour-every-phase in its hold phase (lengthened to 15 min in the snapshot, so it creeps).", Status: scBenchHold,
		Bench: &frozenSpec{Suite: "phase-tour", Prepare: func(st *bench.Store) error { return setHold(st, "tour-every-phase", 900) }, FreezeChat: map[string]int{"qwen3.5-4b": 7}, Lead: 40 * time.Second}},
	{Name: "bench-concurrent", Description: "Frozen nightly run: qwen-simultaneous, four requests at once, 1 of 4 done, three streaming.", Status: scBenchConcurrent,
		Bench: &frozenSpec{Suite: "nightly", FreezeChat: map[string]int{"qwen3.5-4b": 12}, Lead: 270 * time.Second}},
	{Name: "bench-parallel", Description: "Frozen side-by-side run: the parallel step qwen-and-gemma, Qwen 4B in its requests and the Gemma load at 60% (detail.members).", Status: scBenchParallel,
		Bench: &frozenSpec{Suite: "side-by-side", FreezeLoad: map[string]float64{"bench-gemma-12b-1w": 0.60}, FreezeChat: map[string]int{"qwen3.5-4b": 4}, Lead: 35 * time.Second}},
}

func scenarioByName(n string) (scenario, bool) {
	for _, s := range scenarios {
		if s.Name == n {
			return s, true
		}
	}
	return scenario{}, false
}

var (
	ps3 = []string{consoleA.Host, consoleB.Host, consoleC.Host}
)

func scEmpty(now time.Time, t time.Duration) *statusBuilder { return newStatus(now) }

func scReady(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("gemma-4-12b", []string{consoleA.Host}, instOpts{readyAgo: 38 * time.Minute, served: 212, lastTPS: 14.2, lastAgo: 4*time.Minute + 12*time.Second})
	b.learned("gemma-4-12b", []string{consoleA.Host})
	return b
}

func scStale(now time.Time, t time.Duration) *statusBuilder {
	b := scReady(now, t)
	b.st.View.Stale, b.st.View.Connected, b.st.View.Age = true, false, 95*time.Second
	return b
}

func scSplit(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.8-27b", ps3, instOpts{readyAgo: 1*time.Hour + 12*time.Minute, served: 87, inFlight: 2, lastTPS: 9.8, lastAgo: 41 * time.Second})
	b.ready("qwen3.5-4b", []string{hostNode.Host}, instOpts{readyAgo: 22 * time.Minute, served: 46, lastTPS: 61.3, lastAgo: 2 * time.Minute})
	b.learned("qwen3.8-27b", ps3)
	return b
}

func scTwins(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("devstral-24b", []string{consoleA.Host}, instOpts{readyAgo: 51 * time.Minute, served: 133, lastTPS: 7.9, lastAgo: 3 * time.Minute})
	b.ready("bench-devstral-24b", []string{consoleB.Host}, instOpts{readyAgo: 6 * time.Minute, served: 4, lastTPS: 8.1, lastAgo: 40 * time.Second})
	b.staleProfile = map[string]bool{"bench-devstral-24b": true} // edited after it loaded
	return b
}

// creep moves a progress figure forward 0.1% per second from start and wraps
// inside [start, start+span).
func creep(start, span float64, t time.Duration) float64 {
	return start + math.Mod(t.Seconds()*0.001, span)
}

func scLoading(now time.Time, t time.Duration) *statusBuilder {
	return loadingOf("kimi-dev-72b-iq2s", now, t, false)
}

func loadingOf(profile string, now time.Time, t time.Duration, stalled bool) *statusBuilder {
	b := newStatus(now)
	o := loadOpts{startedAgo: 130*time.Second + t, progress: creep(0.40, 0.25, t), waiters: 1, rateMBs: 80}
	if stalled {
		o.progress = 0.40
		o.stalledS = 48 + t.Seconds()
	}
	b.loading(profile, ps3, o)
	if stalled {
		since := now.Add(-time.Duration(o.stalledS * float64(time.Second)))
		b.health(consoleB.Host, "suspect", since.Add(30*time.Second), "silent", "ok", map[string]float64{"upload": round1(o.stalledS)})
	}
	return b
}

func scStalled(now time.Time, t time.Duration) *statusBuilder {
	return loadingOf("kimi-dev-72b-iq2s", now, t, true)
}

func scQueued(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.8-27b", ps3, instOpts{readyAgo: 47 * time.Minute, served: 61, inFlight: 1, lastTPS: 9.6, lastAgo: 50 * time.Second, victimOf: "gemma-4-12b", state: "draining"})
	b.learned("qwen3.8-27b", ps3)
	b.queue(scheduler.EntryStatus{Profile: "devstral-24b", Created: now.Add(-125 * time.Second), Waiters: 2,
		WaitReason: "placer: need 12100 MiB (requires 12100 MiB) does not fit the available nodes"})
	b.queue(scheduler.EntryStatus{Profile: "gemma-4-12b", Created: now.Add(-40 * time.Second), Waiters: 1, Claimed: true,
		Claim: []core.NodeID{core.NodeID(consoleC.ID)}, Victims: []string{"qwen3.8-27b"}})
	b.st.Backoffs = append(b.st.Backoffs, scheduler.BackoffStatus{Profile: "kimi-dev-72b-iq2s", Until: now.Add(4 * time.Minute), Level: 2})
	q := now.Add(90 * time.Second)
	b.st.Quarantines = append(b.st.Quarantines, scheduler.QuarantineStatus{Node: core.NodeID(hostNode.ID), Until: q, Level: 1})
	b.nodes[hostNode.Host].Quarantined = q
	return b
}

func scRefused(now time.Time, t time.Duration) *statusBuilder {
	b := scReady(now, t)
	fin := now.Add(-50 * time.Second)
	b.st.RecentLoads = append(b.st.RecentLoads, scheduler.LoadOutcome{SessionID: sessionID(fin.Add(-71*time.Second), 12), Model: "kimi-dev-72b-iq2s",
		Workers: []string{consoleA.Addr(), consoleB.Addr(), consoleC.Addr()}, Outcome: scheduler.LoadFailed,
		Reason: "load stalled: no new bytes acknowledged for 180 s (8.5 of 25.5 GB sent); the child was stopped", FinishedAt: fin.UnixMilli(), LoadS: 71.4})
	b.st.Backoffs = append(b.st.Backoffs, scheduler.BackoffStatus{Profile: "kimi-dev-72b-iq2s", Until: now.Add(2*time.Minute + 10*time.Second), Level: 1})
	return b
}

func scHung(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.8-27b", ps3, instOpts{readyAgo: 2*time.Hour + 4*time.Minute, served: 190, lastTPS: 9.1, lastAgo: 3*time.Minute + 30*time.Second})
	b.learned("qwen3.8-27b", ps3)
	hungSince := now.Add(-62 * time.Second)
	b.health(consoleB.Host, "hung", hungSince, "silent", "silent", map[string]float64{"heartbeat": 92, "upload": 124})
	q := now.Add(4*time.Minute + 30*time.Second)
	b.nodes[consoleB.Host].Quarantined = q
	b.st.Quarantines = append(b.st.Quarantines, scheduler.QuarantineStatus{Node: core.NodeID(consoleB.ID), Until: q, Level: 1})
	ep := hungSince.UnixMilli()
	b.nodes[consoleB.Host].Recovery = &scheduler.RecoveryStatus{Step: 1, EpisodeStarted: &ep,
		LastRequest: &scheduler.RecoveryRequestStatus{Action: "restart_rpc_server", Nonce: "rq-7f3a91c2", SentAt: now.Add(-48 * time.Second).UnixMilli(), Status: "pending"}}
	c := b.nodes[consoleC.Host]
	c.Online = false
	c.MissingSince = now.Add(-25 * time.Second)
	b.health(consoleC.Host, "suspect", now.Add(-25*time.Second), "unknown", "silent", map[string]float64{"heartbeat": 25})
	return b
}

func scDemo(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	two := []string{consoleA.Host, consoleB.Host}
	b.ready("qwen3.8-27b", two, instOpts{readyAgo: 1*time.Hour + 3*time.Minute, served: 148, inFlight: 2, lastTPS: 10.4, lastAgo: 12 * time.Second})
	b.ready("qwen3.5-4b", []string{hostNode.Host}, instOpts{readyAgo: 19 * time.Minute, served: 57, lastTPS: 62.7, lastAgo: 75 * time.Second})
	b.loading("gemma-4-12b", []string{consoleC.Host}, loadOpts{startedAgo: 41*time.Second + t, progress: creep(0.55, 0.3, t), waiters: 2, rateMBs: 101})
	b.queue(scheduler.EntryStatus{Profile: "devstral-24b", Created: now.Add(-95 * time.Second), Waiters: 1,
		WaitReason: "placer: need 12100 MiB (requires 12100 MiB) does not fit the available nodes"})
	b.learned("qwen3.8-27b", two)
	b.learned("qwen3.5-4b", []string{hostNode.Host})
	return b
}

func scBenchLoad(now time.Time, t time.Duration) *statusBuilder {
	return loadingOf("bench-kimi-72b-3w", now, t, false)
}

func scBenchConversation(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("bench-gemma-12b-2w", []string{consoleA.Host, consoleB.Host}, instOpts{readyAgo: 50 * time.Second, served: 3, inFlight: 1, lastTPS: 11.8, lastAgo: 6 * time.Second})
	return b
}

func scBenchHold(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.5-4b", []string{consoleA.Host}, instOpts{readyAgo: 70 * time.Second, served: 7, lastTPS: 30.8, lastAgo: 20 * time.Second})
	return b
}

func scBenchConcurrent(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.5-4b", []string{consoleA.Host}, instOpts{readyAgo: 40 * time.Second, served: 11, inFlight: 3, lastTPS: 18.2, lastAgo: 2 * time.Second})
	return b
}

func scBenchParallel(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.5-4b", []string{consoleB.Host}, instOpts{readyAgo: 30 * time.Second, served: 3, inFlight: 1, lastTPS: 31.2, lastAgo: 3 * time.Second})
	b.loading("bench-gemma-12b-1w", []string{consoleA.Host}, loadOpts{startedAgo: 45*time.Second + t, progress: 0.6, rateMBs: 101})
	return b
}

func scBenchLast(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("bench-devstral-24b", []string{consoleB.Host}, instOpts{readyAgo: 95 * time.Second, served: 6, inFlight: 1, lastTPS: 8.0, lastAgo: 4 * time.Second})
	return b
}

// scBare is a first start: no configuration, so no tier matches any node
// (none is eligible) and nothing is excluded or overridden.
func scBare(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	for _, ns := range b.nodes {
		ns.Eligible, ns.Excluded, ns.Priority, ns.PlanBudgetMiB = false, false, 0, 0
	}
	return b
}
