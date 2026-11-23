// Package supervise keeps one failing background task from taking the
// scheduler down (background-task isolation, docs/architecture.md "What
// restarts when").
//
// A panic in a supervised goroutine is recovered, logged at error level with
// its stack, counted, recorded as an internalFailure history event and shown
// by Stats (GET /api/system). What happens next is the task's policy:
//
//   - Go with Restart: the task's Rebuild resets the state it owns, and the
//     task runs again after a backoff (100 ms, 1 s, 5 s, then doubling up to
//     30 s; back to 100 ms after a minute without a failure).
//   - Go with Exit: the process ends through the bounded fatal path (Fatal).
//   - Once: a one-shot goroutine body; its onPanic turns the panic into the
//     task's normal failure result. It never runs again.
//   - Guard: an inline call (a subscriber callback, a probe); the caller
//     continues.
//
// A panic that unwound through a critical section (Unlocked, critical.go)
// ends the process whatever the policy: the shared state may be half-written.
//
// The third failure of the same task name within a minute calls Fatal too:
// a fault that repeats is not isolated any more, and a clean restart of the
// process is the honest answer.
//
// It is process-wide and configured once by main (Configure), so that no
// Options struct of the packages it serves has to carry it.
package supervise

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Policy is what happens after a long-lived task (Go) failed.
type Policy int

const (
	// Restart recovers, calls Rebuild (if any), backs off and runs again.
	Restart Policy = iota
	// Exit recovers, reports and calls Fatal: the task never runs again.
	Exit
)

func (p Policy) String() string {
	if p == Exit {
		return "exit"
	}
	return "restart"
}

// Spec describes a long-lived task.
type Spec struct {
	// Name is the stable kind name ("api.catalog", "pool.launch"), not a
	// per-instance one: escalation and Stats are kept by name.
	Name   string
	Policy Policy
	// Rebuild resets the state the task owns before a restart (it gets the
	// panic value). It runs under its own recover: a Rebuild that panics
	// ends the process (Fatal), since the state is then not trustworthy.
	Rebuild func(panicValue any)
	// Done, when set, is called once after the task's final return: a normal
	// return of run, ctx ending, or a failure that is not restarted. It is
	// where a loop's close(done) or wg.Done belongs, never inside run.
	Done func()
}

// Failure is one recovered panic, as it is logged and recorded.
type Failure struct {
	Task            string
	Kind            string // restart, exit, once, guard, report
	Panic           string
	Stack           string
	At              time.Time
	Failures        int // of this task name since start
	FailuresLastMin int
	Restarts        int
	Escalating      bool // the process is ending because of it
}

// Options configure the package (Configure).
type Options struct {
	Logger *slog.Logger
	// Record, when set, gets every failure (main turns it into a history
	// event). It is called on its own goroutine: a failure can be reported
	// from inside a history subscriber, which runs under the store's lock.
	Record func(Failure)
	// Fatal ends the process (main's bounded fatal path). Unset, it logs and
	// calls os.Exit(ExitCode).
	Fatal func(reason string)
	// Now is the clock of the escalation window (default time.Now).
	Now func() time.Time
	// Backoff overrides the restart delay (tests); attempt starts at 0.
	Backoff func(attempt int) time.Duration
}

// ExitCode is the process exit code of the fatal path.
const ExitCode = 3

const (
	// escalateAfter failures of one task within window end the process.
	escalateAfter = 3
	window        = time.Minute
	maxStack      = 16 << 10
)

// TaskStats is one task as GET /api/system lists it.
type TaskStats struct {
	Name   string `json:"name"`
	Policy string `json:"policy"` // restart, exit, once, guard, report
	// State is backoff while any instance of the name waits to restart,
	// else running while any instance is supervised.
	State string `json:"state"` // running, backoff, done, failed, idle
	// Running counts the supervised instances of the name (one per launch
	// for pool.launch), including one waiting in its backoff: an instance
	// leaves the count only when it ends for good.
	Running         int    `json:"running"`
	Restarts        int    `json:"restarts"`
	Failures        int    `json:"failures"`
	FailuresLastMin int    `json:"failures_last_min"`
	LastFailureAt   int64  `json:"last_failure_at,omitempty"` // unix ms
	LastFailure     string `json:"last_failure,omitempty"`
	StartedAt       int64  `json:"started_at,omitempty"` // unix ms
}

type task struct {
	name, policy string
	running      int
	backoff      int
	everLong     bool
	restarts     int
	failures     int
	times        []time.Time
	lastAt       time.Time
	last         string
	startedAt    time.Time
}

var (
	mu    sync.Mutex
	opts  Options
	tasks = map[string]*task{}
	total atomic.Int64
	hook  atomic.Pointer[func(string)]
	// pending counts the Record hand-offs still running (WaitRecords).
	pending atomic.Int64
)

// WaitRecords waits up to d for the failures reported so far to reach
// Record (the fatal path, before it closes the history).
func WaitRecords(d time.Duration) {
	deadline := time.Now().Add(d)
	for pending.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Configure sets the logger, the history hand-off and the fatal path. main
// calls it once, before any task starts. It returns a function that puts the
// previous options back (tests).
func Configure(o Options) (restore func()) {
	prev := swapOptions(o)
	return func() { swapOptions(prev) }
}

func swapOptions(o Options) Options {
	mu.Lock()
	defer mu.Unlock()
	prev := opts
	opts = o
	return prev
}

// locked runs f under the package's lock.
func locked(f func()) {
	mu.Lock()
	defer mu.Unlock()
	f()
}

func current() Options {
	mu.Lock()
	defer mu.Unlock()
	return opts
}

func now() time.Time {
	if f := current().Now; f != nil {
		return f()
	}
	return time.Now()
}

func get(name, policy string) *task {
	t := tasks[name]
	if t == nil {
		t = &task{name: name, policy: policy}
		tasks[name] = t
	}
	return t
}

// call runs f and turns a panic into its value and stack (for a
// *CriticalPanic, the stack where it happened).
func call(f func()) (p any, stack []byte, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			p, stack, panicked = r, debug.Stack(), true
			if cp, ok := r.(*CriticalPanic); ok {
				stack = cp.Stack
			}
		}
	}()
	f()
	return nil, nil, false
}

// Go starts a long-lived task on its own goroutine and returns at once. A
// normal return of run ends the task; restarts stop when ctx ends.
func Go(ctx context.Context, s Spec, run func(ctx context.Context)) {
	locked(func() {
		t := get(s.Name, s.Policy.String())
		t.policy, t.everLong = s.Policy.String(), true
		t.running++
		if t.startedAt.IsZero() {
			t.startedAt = nowLocked()
		}
	})
	go loop(ctx, s, run)
}

func nowLocked() time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now()
}

func loop(ctx context.Context, s Spec, run func(ctx context.Context)) {
	defer func() {
		locked(func() { get(s.Name, s.Policy.String()).running-- })
		if s.Done != nil {
			s.Done()
		}
	}()
	attempt := 0
	var lastFail time.Time
	for {
		p, stack, panicked := call(func() { run(ctx) })
		if !panicked {
			return
		}
		kind := "restart"
		if s.Policy == Exit {
			kind = "exit"
		}
		if fail(s.Name, kind, p, stack, s.Policy == Exit || IsCritical(p)) {
			return
		}
		if s.Rebuild != nil {
			if rp, rstack, bad := call(func() { s.Rebuild(p) }); bad {
				fail(s.Name+".rebuild", "exit", rp, rstack, true)
				return
			}
		}
		at := now()
		if !lastFail.IsZero() && at.Sub(lastFail) > window {
			attempt = 0
		}
		lastFail = at
		if !wait(ctx, s.Name, delay(attempt)) {
			return
		}
		attempt++
		locked(func() { get(s.Name, s.Policy.String()).restarts++ })
	}
}

func delay(attempt int) time.Duration {
	if f := current().Backoff; f != nil {
		return f(attempt)
	}
	switch attempt {
	case 0:
		return 100 * time.Millisecond
	case 1:
		return time.Second
	}
	d := 5 * time.Second
	for i := 2; i < attempt && d < 30*time.Second; i++ {
		d *= 2
	}
	return min(d, 30*time.Second)
}

// wait sleeps d unless ctx ends first (false then).
func wait(ctx context.Context, name string, d time.Duration) bool {
	locked(func() { get(name, "restart").backoff++ })
	defer locked(func() { get(name, "restart").backoff-- })
	if ctx.Err() != nil {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Once runs a one-shot goroutine body: `go supervise.Once(name, run,
// onPanic)`. A panic is reported and onPanic (when set) turns it into the
// task's normal failure result, for example the event its waiter expects.
// It is never run again.
func Once(name string, run func(), onPanic func(p any)) {
	guard(name, "once", run, onPanic)
}

// Guard runs fn inline and recovers a panic in it: it is reported, onPanic
// (when set) runs, and Guard returns true. The caller continues. It is for
// foreign code a loop calls (subscribers) and for inline steps that have a
// natural failure result.
func Guard(name string, fn func(), onPanic func(p any)) (panicked bool) {
	return guard(name, "guard", fn, onPanic)
}

// GuardRead is Guard for a read-only query (the scheduler's plan, status and
// needs): reported and counted, but it never counts toward escalation, since
// it changed nothing and a restart would not help. A panic out of a critical
// section still ends the process.
func GuardRead(name string, fn func(), onPanic func(p any)) (panicked bool) {
	return guardCount(name, "read", fn, onPanic, func() bool { return false })
}

// GuardExit runs fn inline; a panic in it ends the process through Fatal
// (after it is reported). It returns whether fn panicked, so that the caller
// can answer its own caller before the process ends.
func GuardExit(name string, fn func()) (panicked bool) {
	p, stack, bad := call(fn)
	if bad {
		fail(name, "exit", p, stack, true)
	}
	return bad
}

func guard(name, kind string, fn func(), onPanic func(p any)) bool {
	return guardCount(name, kind, fn, onPanic, nil)
}

// guardCount is guard; counts, when set, says whether this failure counts
// toward escalation (a fan-out counts one per operation).
func guardCount(name, kind string, fn func(), onPanic func(p any), counts func() bool) bool {
	p, stack, bad := call(fn)
	if !bad {
		return false
	}
	locked(func() { get(name, kind) })
	escalate := failCount(name, kind, p, stack, IsCritical(p), counts == nil || counts())
	if onPanic != nil {
		if op, ostack, worse := call(func() { onPanic(p) }); worse {
			fail(name+".on_panic", "exit", op, ostack, true)
			return true
		}
	}
	if escalate {
		fatal(reason(name, p))
	}
	return true
}

// Report records a panic that code already recovered on its own (the bench
// store's writer, net/http's handlers). It is logged and counted but does not
// escalate (that code has its own isolation), except a *CriticalPanic, which
// ends the process: the shared state it left may be half-written.
func Report(name string, p any, stack []byte) {
	locked(func() { get(name, "report") })
	if cp, ok := p.(*CriticalPanic); ok {
		stack = cp.Stack
	}
	fail(name, "report", p, stack, IsCritical(p))
}

// ReportRepanic is for a deferred call in code whose panics something else
// already isolates (net/http's per-request recover): it reports the panic
// like Report and panics again with the same value, so that code behaves as
// before. A value pass says yes to (http.ErrAbortHandler) is not reported.
func ReportRepanic(name string, pass func(p any) bool) {
	if p := recover(); p != nil {
		if pass == nil || !pass(p) {
			Report(name, p, debug.Stack())
		}
		panic(p)
	}
}

// fail reports a failure and calls Fatal when it escalates; it says whether
// it did.
func fail(name, kind string, p any, stack []byte, exit bool) bool {
	if failNoFatal(name, kind, p, stack, exit) {
		fatal(reason(name, p))
		return true
	}
	return false
}

func failNoFatal(name, kind string, p any, stack []byte, exit bool) (escalate bool) {
	return failCount(name, kind, p, stack, exit, true)
}

// failCount reports a failure; counts says whether it counts toward the
// escalation window.
func failCount(name, kind string, p any, stack []byte, exit, counts bool) (escalate bool) {
	total.Add(1)
	var f Failure
	var o Options
	locked(func() {
		t := get(name, kind)
		at := nowLocked()
		keep := t.times[:0]
		for _, x := range t.times {
			if at.Sub(x) < window {
				keep = append(keep, x)
			}
		}
		t.times = keep
		if counts {
			t.times = append(t.times, at)
		}
		t.failures++
		t.lastAt, t.last = at, fmt.Sprint(p)
		escalate = exit || (counts && kind != "report" && len(t.times) >= escalateAfter)
		f = Failure{Task: name, Kind: kind, Panic: t.last, Stack: trim(stack), At: at, Failures: t.failures,
			FailuresLastMin: len(t.times), Restarts: t.restarts, Escalating: escalate}
		o = opts
	})
	report(o, f)
	return escalate
}

func trim(stack []byte) string {
	if len(stack) > maxStack {
		stack = stack[:maxStack]
	}
	return string(stack)
}

func reason(name string, p any) string { return name + ": " + fmt.Sprint(p) }

// report logs f and hands it to Record. A reporter that panics falls back
// to stderr; the failure is not lost.
func report(o Options, f Failure) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	if p, _, bad := call(func() {
		log.Error("background task failed", "task", f.Task, "policy", f.Kind, "panic", f.Panic,
			"failures_last_min", f.FailuresLastMin, "restarts", f.Restarts, "escalating", f.Escalating, "stack", f.Stack)
	}); bad {
		fmt.Fprintln(os.Stderr, "background task failed:", f.Task, f.Panic, "(the log itself failed:", p, ")")
		fmt.Fprintln(os.Stderr, f.Stack)
	}
	if rec := o.Record; rec != nil {
		pending.Add(1)
		go func() { // never on the caller: it may hold the history store's lock
			defer pending.Add(-1)
			if p, _, bad := call(func() { rec(f) }); bad {
				fmt.Fprintln(os.Stderr, "recording a background task failure failed:", p)
			}
		}()
	}
}

func fatal(reason string) {
	o := current()
	if o.Fatal != nil {
		o.Fatal(reason)
		return
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Error("internal failure: the process ends", "reason", reason)
	os.Exit(ExitCode)
}

// Total is how many failures were recovered since start.
func Total() int64 { return total.Load() }

// Stats lists the long-lived tasks and every task name that ever failed,
// sorted by name. One-shot tasks are aggregated by name.
func Stats() []TaskStats {
	mu.Lock()
	defer mu.Unlock()
	at := nowLocked()
	out := make([]TaskStats, 0, len(tasks))
	for _, t := range tasks {
		if !t.everLong && t.failures == 0 {
			continue
		}
		ts := TaskStats{Name: t.name, Policy: t.policy, Running: t.running, Restarts: t.restarts, Failures: t.failures, LastFailure: t.last}
		for _, x := range t.times {
			if at.Sub(x) < window {
				ts.FailuresLastMin++
			}
		}
		if !t.lastAt.IsZero() {
			ts.LastFailureAt = t.lastAt.UnixMilli()
		}
		if !t.startedAt.IsZero() {
			ts.StartedAt = t.startedAt.UnixMilli()
		}
		switch {
		case t.backoff > 0:
			ts.State = "backoff"
		case t.running > 0:
			ts.State = "running"
		case !t.everLong:
			ts.State = "idle"
		case t.failures > 0 && t.policy == "exit":
			ts.State = "failed"
		default:
			ts.State = "done"
		}
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Point is a test hook site in instance inst (the object the code runs for:
// a pool launch, a scheduler, a child). A no-op in production (one atomic
// load), it calls the hook a test installed (SetHookForTest) with
// PointName(name, inst), which may panic there.
func Point(name string, inst any) {
	if h := hook.Load(); h != nil {
		(*h)(PointName(name, inst))
	}
}

// SetHookForTest installs h at every Point. Only tests may call it (the
// source scan in lint_test.go enforces that).
func SetHookForTest(h func(point string)) (restore func()) {
	prev := hook.Swap(&h)
	return func() { hook.Store(prev) }
}

// PanicAtForTest returns a hook that panics at point the first n times it
// is reached (n < 0: every time), and a function that says how often it did.
// point is PointName(name, inst), aimed at the test's own instance; a bare
// name (no "/") matches every instance, for a test process that has only one.
func PanicAtForTest(point string, n int) (h func(string), fired func() int) {
	var count atomic.Int64
	return func(p string) {
			if p != point && (strings.Contains(point, "/") || !strings.HasPrefix(p, point+"/")) {
				return
			}
			c := count.Add(1)
			if n < 0 || c <= int64(n) {
				panic(fmt.Sprintf("injected panic at %s (%d)", point, c))
			}
		}, func() int {
			c := int(count.Load())
			if n >= 0 && c > n {
				return n
			}
			return c
		}
}

// ResetForTest forgets every task and failure.
func ResetForTest() {
	locked(func() { tasks = map[string]*task{} })
	total.Store(0)
}
