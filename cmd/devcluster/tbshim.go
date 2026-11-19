package main

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

// tbShim lets the test fakes (fakefleet.New wants a testing.TB) run inside a
// normal program. The embedded interface is nil: only the methods below are
// implemented, and the fakes call nothing else.
type tbShim struct {
	testing.TB
	log *slog.Logger

	mu       sync.Mutex
	cleanups []func()
}

func (t *tbShim) Helper()                   {}
func (t *tbShim) Name() string              { return "devcluster" }
func (t *tbShim) Logf(f string, a ...any)   { t.log.Debug(fmt.Sprintf(f, a...)) }
func (t *tbShim) Log(a ...any)              { t.log.Debug(fmt.Sprint(a...)) }
func (t *tbShim) Errorf(f string, a ...any) { t.log.Error(fmt.Sprintf(f, a...)) }
func (t *tbShim) Error(a ...any)            { t.log.Error(fmt.Sprint(a...)) }
func (t *tbShim) Fatalf(f string, a ...any) { panic(fmt.Sprintf(f, a...)) }
func (t *tbShim) Fatal(a ...any)            { panic(fmt.Sprint(a...)) }
func (t *tbShim) Failed() bool              { return false }
func (t *tbShim) Skipf(f string, a ...any)  { panic(fmt.Sprintf("skip: "+f, a...)) }
func (t *tbShim) Setenv(k, v string)        { panic("Setenv is not supported by devcluster") }
func (t *tbShim) Cleanup(f func())          { t.mu.Lock(); t.cleanups = append(t.cleanups, f); t.mu.Unlock() }
func (t *tbShim) runCleanups() {
	t.mu.Lock()
	cs := t.cleanups
	t.cleanups = nil
	t.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}
