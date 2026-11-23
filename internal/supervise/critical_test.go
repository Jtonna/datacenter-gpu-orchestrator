package supervise

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// A panic inside a critical section releases the lock and ends the process
// (Fatal), whatever the task's policy: a restartable task, a one-shot, a
// guarded call and a reported panic alike. A panic outside the section keeps
// the policy.
func TestPanicInsideALockIsFatalAndFreesIt(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(fn func())
	}{
		{"restart", func(fn func()) {
			done := make(chan struct{})
			Go(context.Background(), Spec{Name: "t.crit", Policy: Restart, Done: func() { close(done) }}, func(context.Context) { fn() })
			<-done
		}},
		{"once", func(fn func()) { Once("t.crit", fn, nil) }},
		{"guard", func(fn func()) { Guard("t.crit", fn, nil) }},
		{"report", func(fn func()) {
			func() {
				defer func() {
					if p := recover(); p != nil {
						Report("t.crit", p, nil)
					}
				}()
				fn()
			}()
		}},
		{"report-repanic", func(fn func()) {
			func() {
				defer func() { _ = recover() }() // net/http's own recover
				defer ReportRepanic("t.crit", nil)
				fn()
			}()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			var mu sync.Mutex
			c.run(func() {
				mu.Lock()
				defer Unlocked("tpkg", mu.Unlock)
				panic("half-written")
			})
			if !mu.TryLock() {
				t.Fatal("the lock is still held after the panic")
			}
			mu.Unlock()
			if f.fatalCount() != 1 {
				t.Fatalf("fatal %d times, want once", f.fatalCount())
			}
			if !strings.Contains(f.fatals[0], "half-written") || !strings.Contains(f.fatals[0], "inside a tpkg lock") {
				t.Fatalf("reason %q", f.fatals[0])
			}
			eventually(t, "the record", func() bool { return f.recordCount() == 1 })
			if r := f.records[0]; !r.Escalating || !strings.Contains(r.Stack, "critical_test.go") {
				t.Fatalf("record %+v", r)
			}
		})
	}
	t.Run("outside the section", func(t *testing.T) {
		f := setup(t)
		var mu sync.Mutex
		Once("t.crit", func() {
			func() {
				mu.Lock()
				defer Unlocked("tpkg", mu.Unlock)
			}()
			panic("after the section")
		}, nil)
		if f.fatalCount() != 0 || !mu.TryLock() {
			t.Fatalf("fatal %d", f.fatalCount())
		}
	})
	t.Run("normal return", func(t *testing.T) {
		var mu sync.Mutex
		func() {
			mu.Lock()
			defer Unlocked("tpkg", mu.Unlock)
		}()
		if !mu.TryLock() {
			t.Fatal("not released")
		}
	})
}

// Nested sections keep the innermost section and the first stack.
func TestNestedSections(t *testing.T) {
	f := setup(t)
	var a, b sync.Mutex
	Once("t.nest", func() {
		a.Lock()
		defer Unlocked("outer", a.Unlock)
		b.Lock()
		defer Unlocked("inner", b.Unlock)
		panic("x")
	}, nil)
	if !a.TryLock() || !b.TryLock() {
		t.Fatal("a lock is still held")
	}
	if f.fatalCount() != 1 || !strings.Contains(f.fatals[0], "inside a inner lock") {
		t.Fatalf("fatals %v", f.fatals)
	}
}

// The members of one fan-out count once toward escalation; each failure is
// still reported and turned into its member's result. Three separate
// operations that fail do escalate.
func TestFanoutCountsOncePerOperation(t *testing.T) {
	f := setup(t)
	fan := NewFanout("t.fan")
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := range 3 {
		wg.Add(1)
		go fan.Once(func() { defer wg.Done(); panic("bad input") }, func(any) { results[i] = "failed" })
	}
	wg.Wait()
	eventually(t, "three records", func() bool { return f.recordCount() == 3 })
	if f.fatalCount() != 0 {
		t.Fatalf("one operation escalated: %v", f.fatals)
	}
	if s, _ := stat("t.fan"); s.Failures != 3 || s.FailuresLastMin != 1 {
		t.Fatalf("stats %+v", s)
	}
	for i, r := range results {
		if r != "failed" {
			t.Fatalf("member %d result %q", i, r)
		}
	}
	for range 2 {
		NewFanout("t.fan").Once(func() { panic("again") }, nil)
	}
	if f.fatalCount() != 1 {
		t.Fatalf("three operations failing in a minute: fatal %d", f.fatalCount())
	}
}
