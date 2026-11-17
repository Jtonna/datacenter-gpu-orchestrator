// Package scheduler is the state machine that owns instances, claims, the
// queue and timers (design section 8, and the stateful parts of 7).
//
// # Structure
//
// All state lives in schedCore and is changed only by its step function,
// handle(event): apply the event, then the time-based rules (load
// timeouts, node loss, idle unload, settling), then the queue pass of 8.3.
// handle never blocks. Slow work (pre-flight probes, starting, health
// checks and stopping of children, the post-settle node fetch) is emitted
// as effects; whoever drives the core performs them and reports back with
// events. Time comes from an injected Clock, and the core reports its next
// deadline (nextDeadline) instead of owning timers, so tests drive it with
// a fake clock, one event at a time, with no goroutines.
//
// Scheduler wraps the core with one goroutine (Run) that feeds it events
// from a channel, wakes it at its deadlines and on fleet view changes, and
// runs the effects in helper goroutines. Its exported methods are safe for
// concurrent use.
//
// Files: state.go (types, node figures, eligibility), step.go (events,
// effects, handle), request.go (8.2), queue.go (8.3), lifecycle.go
// (instances, 7.5), failures.go (8.4, view bookkeeping, timers),
// admin.go (unload, 8.5), deadlines.go, snapshot.go (read-only views,
// dry-run plan), invariants.go (design 11), runner.go (goroutine, API).
//
// # Model names
//
// Acquire, Load, Unload, Plan and Upstream accept a profile name or an
// alias and resolve it here (config.Resolve). Lease.Alias reports alias
// use so the HTTP layer can log it with the caller's address (8.2 step 1).
//
// # Interpretations
//
// Where the design is silent or reads two ways, the code says so in a
// comment starting with "DESIGN:".
package scheduler
