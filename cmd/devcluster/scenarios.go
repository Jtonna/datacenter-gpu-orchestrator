package main

import (
	"math"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/scheduler"
)

// Scenarios: what the status side shows. Every scenario except live is a Go
// function that builds a scheduler.Status; bench-* scenarios also start the
// mock benchmark engine in a frozen run (mock_bench_engine.go).

// A scenario is a name, a description, a status builder and a bench preset.
// To add one, put it in a file of its own and register it from init:
//
//	func init() {
//		registerScenario(scenario{Name: "my-case", Description: "...", Status: scMyCase})
//	}
//
// Status gets the render time and how long the scenario has been selected
// (for creeping figures); nil means live (the real scheduler answers).
// Bench is the frozen benchmark run to show: "", "load", "conversation" or
// "last" (see freeze in mock_bench_engine.go).
//
// BenchSetup, when set, runs after the bench mock is reseeded (and after
// the frozen run, if any) and before the bench state frame goes out. It may
// change the seeded data in place: take e.store.mu while touching
// e.store.benchmarks, e.store.suites or e.store.runs (types in
// mock_bench.go; fillStep, newSteps and benchEnv in mock_bench_seed.go
// build runs), and call e.enqueue(suiteID, "", note, "manual") without any
// lock held to start or queue runs. Example, no suites at all:
//
//	BenchSetup: func(e *benchEngine, now time.Time) {
//		e.store.mu.Lock()
//		defer e.store.mu.Unlock()
//		e.store.suites, e.store.runs = map[string]map[string]any{}, nil
//	},
type scenario struct {
	Name        string
	Description string
	StartupOnly bool // selectable only with --scenario at startup
	Status      func(now time.Time, t time.Duration) *statusBuilder
	Bench       string
	BenchSetup  func(e *benchEngine, now time.Time)
}

// registerScenario adds a scenario; a duplicate name panics at startup.
func registerScenario(sc scenario) {
	if _, dup := scenarioByName(sc.Name); dup || sc.Name == "" {
		panic("devcluster: duplicate or empty scenario name " + sc.Name)
	}
	scenarios = append(scenarios, sc)
}

var scenarios = []scenario{
	{Name: "demo", Description: "Default. qwen3.8-27b on two PS5s with 2 in flight, qwen3.5-4b on omen-runner, gemma-4-12b loading on the third PS5 (about 55%), devstral-24b queued.", Status: scDemo},
	{Name: "empty", Description: "Nothing loaded, empty queue, all three PS5s online and free.", Status: scEmpty},
	{Name: "ready", Description: "gemma-4-12b ready on ps5-9032a330 for 38 min, idle, 212 requests served, last request 14.2 tok/s.", Status: scReady},
	{Name: "split", Description: "qwen3.8-27b split across all three PS5s with 2 in flight; qwen3.5-4b ready on omen-runner.", Status: scSplit},
	{Name: "twins", Description: "devstral-24b on ps5-9032a330 and bench-devstral-24b on ps5-31bd9dc4: one model file, two instances; third PS5 free.", Status: scTwins},
	{Name: "loading", Description: "kimi-dev-72b-iq2s loading across three PS5s, measured progress about 40% (creeping), workers done / receiving / waiting, 1 waiter.", Status: scLoading},
	{Name: "stalled", Description: "The kimi-dev-72b-iq2s load stalled (stalled_s grows from 48), second worker stuck, its node suspect with upload silent.", Status: scStalled},
	{Name: "queued", Description: "qwen3.8-27b on three PS5s with 1 in flight; devstral-24b waiting unclaimed, gemma-4-12b claimed with qwen3.8-27b as victim; a backoff and a quarantine.", Status: scQueued},
	{Name: "refused", Description: "gemma-4-12b ready; a failed kimi-dev-72b-iq2s load 50 s ago with a backoff; fresh requestRefused and loadFinished failed history events.", Status: scRefused},
	{Name: "hung", Description: "qwen3.8-27b on three PS5s; ps5-31bd9dc4 hung and quarantined with a pending recovery request; ps5-407c8925 offline.", Status: scHung},
	{Name: "stale", Description: "Like ready, but the fleet view is stale and disconnected (age 95 s).", Status: scStale},
	{Name: "bare", Description: "First start: configured false, config revision 0, no profiles, no tiers (no node eligible), history off, no benchmarks, empty audit. Only with --scenario bare at startup (history cannot be switched off at runtime).", StartupOnly: true, Status: scBare},
	{Name: "live", Description: "The real scheduler, nothing scripted: a chat request to /v1/chat/completions really loads a model through fakellama."},
	{Name: "bench-load", Description: "nightly benchmark run at about 75%: step 7 kimi-72b-load-3w in its load phase at 40% (workers done / receiving / waiting).", Status: scBenchLoad, Bench: "load"},
	{Name: "bench-conversation", Description: "nightly benchmark run at about 45%: a conversation benchmark on bench-gemma-12b-2w, turn 3 of 5, streaming.", Status: scBenchConversation, Bench: "conversation"},
	{Name: "bench-last", Description: "nightly benchmark run at about 93%: the last benchmark (8 of 8) in a requests phase, 4 of 6 requests done.", Status: scBenchLast, Bench: "last"},
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
	ps3 = []string{ps5A.Host, ps5B.Host, ps5C.Host}
)

func scEmpty(now time.Time, t time.Duration) *statusBuilder { return newStatus(now) }

func scReady(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("gemma-4-12b", []string{ps5A.Host}, instOpts{readyAgo: 38 * time.Minute, served: 212, lastTPS: 14.2, lastAgo: 4*time.Minute + 12*time.Second})
	b.learned("gemma-4-12b", []string{ps5A.Host})
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
	b.ready("qwen3.5-4b", []string{omen.Host}, instOpts{readyAgo: 22 * time.Minute, served: 46, lastTPS: 61.3, lastAgo: 2 * time.Minute})
	b.learned("qwen3.8-27b", ps3)
	return b
}

func scTwins(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("devstral-24b", []string{ps5A.Host}, instOpts{readyAgo: 51 * time.Minute, served: 133, lastTPS: 7.9, lastAgo: 3 * time.Minute})
	b.ready("bench-devstral-24b", []string{ps5B.Host}, instOpts{readyAgo: 6 * time.Minute, served: 4, lastTPS: 8.1, lastAgo: 40 * time.Second})
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
		b.health(ps5B.Host, "suspect", since.Add(30*time.Second), "silent", "ok", map[string]float64{"upload": round1(o.stalledS)})
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
		Claim: []core.NodeID{core.NodeID(ps5C.ID)}, Victims: []string{"qwen3.8-27b"}})
	b.st.Backoffs = append(b.st.Backoffs, scheduler.BackoffStatus{Profile: "kimi-dev-72b-iq2s", Until: now.Add(4 * time.Minute), Level: 2})
	q := now.Add(90 * time.Second)
	b.st.Quarantines = append(b.st.Quarantines, scheduler.QuarantineStatus{Node: core.NodeID(omen.ID), Until: q, Level: 1})
	b.nodes[omen.Host].Quarantined = q
	return b
}

func scRefused(now time.Time, t time.Duration) *statusBuilder {
	b := scReady(now, t)
	fin := now.Add(-50 * time.Second)
	b.st.RecentLoads = append(b.st.RecentLoads, scheduler.LoadOutcome{SessionID: sessionID(fin.Add(-71*time.Second), 12), Model: "kimi-dev-72b-iq2s",
		Workers: []string{ps5A.Addr(), ps5B.Addr(), ps5C.Addr()}, Outcome: scheduler.LoadFailed,
		Reason: "load stalled: no new bytes acknowledged for 180 s (8.5 of 25.5 GB sent); the child was stopped", FinishedAt: fin.UnixMilli(), LoadS: 71.4})
	b.st.Backoffs = append(b.st.Backoffs, scheduler.BackoffStatus{Profile: "kimi-dev-72b-iq2s", Until: now.Add(2*time.Minute + 10*time.Second), Level: 1})
	return b
}

func scHung(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("qwen3.8-27b", ps3, instOpts{readyAgo: 2*time.Hour + 4*time.Minute, served: 190, lastTPS: 9.1, lastAgo: 3*time.Minute + 30*time.Second})
	b.learned("qwen3.8-27b", ps3)
	hungSince := now.Add(-62 * time.Second)
	b.health(ps5B.Host, "hung", hungSince, "silent", "silent", map[string]float64{"heartbeat": 92, "upload": 124})
	q := now.Add(4*time.Minute + 30*time.Second)
	b.nodes[ps5B.Host].Quarantined = q
	b.st.Quarantines = append(b.st.Quarantines, scheduler.QuarantineStatus{Node: core.NodeID(ps5B.ID), Until: q, Level: 1})
	ep := hungSince.UnixMilli()
	b.nodes[ps5B.Host].Recovery = &scheduler.RecoveryStatus{Step: 1, EpisodeStarted: &ep,
		LastRequest: &scheduler.RecoveryRequestStatus{Action: "restart_rpc_server", Nonce: "rq-7f3a91c2", SentAt: now.Add(-48 * time.Second).UnixMilli(), Status: "pending"}}
	c := b.nodes[ps5C.Host]
	c.Online = false
	c.MissingSince = now.Add(-25 * time.Second)
	b.health(ps5C.Host, "suspect", now.Add(-25*time.Second), "unknown", "silent", map[string]float64{"heartbeat": 25})
	return b
}

func scDemo(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	two := []string{ps5A.Host, ps5B.Host}
	b.ready("qwen3.8-27b", two, instOpts{readyAgo: 1*time.Hour + 3*time.Minute, served: 148, inFlight: 2, lastTPS: 10.4, lastAgo: 12 * time.Second})
	b.ready("qwen3.5-4b", []string{omen.Host}, instOpts{readyAgo: 19 * time.Minute, served: 57, lastTPS: 62.7, lastAgo: 75 * time.Second})
	b.loading("gemma-4-12b", []string{ps5C.Host}, loadOpts{startedAgo: 41*time.Second + t, progress: creep(0.55, 0.3, t), waiters: 2, rateMBs: 101})
	b.queue(scheduler.EntryStatus{Profile: "devstral-24b", Created: now.Add(-95 * time.Second), Waiters: 1,
		WaitReason: "placer: need 12100 MiB (requires 12100 MiB) does not fit the available nodes"})
	b.learned("qwen3.8-27b", two)
	b.learned("qwen3.5-4b", []string{omen.Host})
	return b
}

func scBenchLoad(now time.Time, t time.Duration) *statusBuilder {
	return loadingOf("bench-kimi-72b-3w", now, t, false)
}

func scBenchConversation(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("bench-gemma-12b-2w", []string{ps5A.Host, ps5B.Host}, instOpts{readyAgo: 50 * time.Second, served: 3, inFlight: 1, lastTPS: 11.8, lastAgo: 6 * time.Second})
	return b
}

func scBenchLast(now time.Time, t time.Duration) *statusBuilder {
	b := newStatus(now)
	b.ready("bench-devstral-24b", []string{ps5B.Host}, instOpts{readyAgo: 95 * time.Second, served: 6, inFlight: 1, lastTPS: 8.0, lastAgo: 4 * time.Second})
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
