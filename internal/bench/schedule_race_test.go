package bench

import (
	"sync"
	"testing"
	"time"
)

// A schedule check reads the suite list before it takes the manager's lock.
// A check whose list was read before a suite was saved (the manager loop's
// check, say) must not throw away the schedule state a newer check made for
// that suite: the state would be made again from "now", and an "at"
// schedule whose time has passed would be due tomorrow, so today's run
// would be missed. This is what made TestScheduleQueuesADueSuiteOnce fail
// about 2 % of the time: the loop's start-up check lands in that window.
func TestScheduleStaleSuiteListKeepsTheDueSlot(t *testing.T) {
	api := newFake().prof("gemma", fakeProf{})
	var now = time.Date(2026, 11, 18, 2, 0, 0, 0, time.Local)
	var mu sync.Mutex
	tm := newTestMgr(t, api, func(o *ManagerOptions) {
		o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})

	// A check that has read the (empty) suite list and is held there.
	listed, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	tm.m.afterList = func() {
		once.Do(func() { close(listed); <-release })
	}
	go func() { defer close(done); tm.m.checkSchedules(true) }()
	<-listed
	tm.m.afterList = nil

	// The suite is saved and a newer check makes its state: due at 03:00.
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"only_when_idle":false,"schedule":{"at":"03:00"}}`)
	if st := tm.m.State(); len(st.NextScheduled) != 1 || time.UnixMilli(st.NextScheduled[0].Next).Hour() != 3 {
		t.Fatalf("%+v", st.NextScheduled)
	}

	// The held check goes on with its old list.
	close(release)
	<-done

	mu.Lock()
	now = time.Date(2026, 11, 18, 3, 0, 5, 0, time.Local)
	mu.Unlock()
	tm.m.checkSchedules(true)
	runs, _ := tm.store.ListRuns(RunFilter{})
	if len(runs) != 1 || runs[0].Trigger != "schedule" {
		t.Fatalf("the 03:00 run was not queued: %+v (next scheduled %+v)", runs, tm.m.State().NextScheduled)
	}
	tm.finished(runs[0].ID)
}
