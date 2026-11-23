package supervise

import (
	"fmt"
	"reflect"
	"runtime/debug"
	"sync/atomic"
)

// Locks and recovered panics (review IB1, design finding 4).
//
// A task that recovers a panic must neither leave a lock held (the process
// would hang where it used to crash and come back) nor go on with state
// that the panic left half-written. Every Lock in the scheduler binary is
// therefore followed at once by its deferred release (the lock scan in
// lockscan_test.go enforces it), and in the packages whose shared state a
// restart cannot rebuild (pool, scheduler, needstore, history, instance,
// cfgsvc, bench) that release is Unlocked:
//
//	p.mu.Lock()
//	defer supervise.Unlocked("pool", p.unlock)
//
// A panic inside such a section releases the lock and continues as a
// *CriticalPanic, which every recovering helper of this package (Go, Once,
// Guard, Report and ReportRepanic) turns into the fatal path, whatever the
// task's own policy. A panic outside any section cannot have half-written
// the shared state (it is only written under its lock), so the task's
// policy (restart, or the failure as its result) holds there.

// CriticalPanic is a panic that left a section of shared state (a lock)
// while unwinding: the state may be half-written.
type CriticalPanic struct {
	Section string // the package whose lock it was
	Value   any    // the original panic value
	Stack   []byte // where it happened
}

func (c *CriticalPanic) Error() string {
	return fmt.Sprintf("%v (inside a %s lock: its state may be half-written)", c.Value, c.Section)
}

func (c *CriticalPanic) String() string { return c.Error() }

// IsCritical says whether p (a recovered panic value) came out of a
// critical section.
func IsCritical(p any) bool {
	_, ok := p.(*CriticalPanic)
	return ok
}

// Unlocked releases a lock in a deferred call: `defer
// supervise.Unlocked(section, mu.Unlock)` right after the Lock. On a normal
// return it only releases. While a panic unwinds through it, it releases the
// lock and panics again with a *CriticalPanic (the innermost section and the
// original stack are kept), so that the recovering task ends the process
// instead of continuing.
func Unlocked(section string, release func()) {
	r := recover()
	if r == nil {
		release()
		return
	}
	cp, ok := r.(*CriticalPanic)
	if !ok {
		cp = &CriticalPanic{Section: section, Value: r, Stack: debug.Stack()}
	}
	func() {
		defer func() { _ = recover() }() // a release that fails must not hide the panic
		release()
	}()
	panic(cp)
}

// Fanout is one operation that runs a member per node at once (probes,
// launches, stops, cache evictions). A bad input fails every member at once,
// so the members' failures count once toward escalation (review IS1): each
// is still reported and counted, and turned into its member's result.
type Fanout struct {
	name    string
	counted atomic.Bool
}

// NewFanout starts one fan-out operation of the task name.
func NewFanout(name string) *Fanout { return &Fanout{name: name} }

// Once runs one member: `go fan.Once(run, onPanic)`, as supervise.Once.
func (f *Fanout) Once(run func(), onPanic func(p any)) {
	guardCount(f.name, "once", run, onPanic, func() bool { return f.counted.CompareAndSwap(false, true) })
}

// PointName is the name a test hook sees at Point(name, inst): the point and
// the instance it fires in (a pointer by address, a value such as an id as
// it prints), so that a test aims at its own instance and never at one
// another test left running.
func PointName(name string, inst any) string {
	switch reflect.ValueOf(inst).Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return fmt.Sprintf("%s/%p", name, inst)
	}
	return fmt.Sprintf("%s/%v", name, inst)
}
