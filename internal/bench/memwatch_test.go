package bench

import (
	"context"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

// A step records, per node that took part, the lowest available memory while
// the model loaded and over the whole step; the roles (host and workers, with
// each worker's share of the model's memory), the load mode and the plan's
// reload-from-cache figures. Nodes that took no part are left out.
func TestStepRecordsLowestMemoryRolesAndLoadMode(t *testing.T) {
	// The load takes long enough that the first load poll sees it starting.
	api := newFake().prof("gemma", fakeProf{loadDelay: 250 * time.Millisecond})
	api.loadMode = LoadModeFileRead
	pct := 63.0
	api.reload = []NodeReload{{ID: "n1", Verdict: "partial", CachedPct: &pct, Shared: true, FileMiB: 6676, ModelMiB: 7153}}
	api.mem = func(id string, loading bool, chats int) *int64 {
		switch id {
		case "n0":
			return i64(20000) // the host
		case "n1":
			switch {
			case loading:
				return i64(2000)
			case chats > 0:
				return i64(1500) // lower while serving than while loading
			}
			return i64(9000)
		case "n2":
			return i64(100) // took no part: left out
		}
		return nil // n3: no figure
	}
	// Background sampling off: only the load polls and the phase ends sample,
	// so the figures do not depend on timing.
	tm := newTestMgr(t, api, func(o *ManagerOptions) { o.MemSample = -1 })
	tm.bench(gemmaBench)
	tm.suite(`{"id":"s","benchmarks":["gemma"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.State != RunCompleted {
		t.Fatalf("run %s: %s", r.State, r.Error)
	}
	st := r.Steps[0]
	if len(st.MemLow) != 2 {
		t.Fatalf("mem_low %+v", st.MemLow)
	}
	host, worker := st.MemLow[0], st.MemLow[1]
	if host.ID != "n0" || host.Node != "host-1" || *host.StepMiB != 20000 || host.Samples == 0 {
		t.Fatalf("host %+v", host)
	}
	if worker.ID != "n1" || worker.Node != "ps5-a" || worker.LoadMiB == nil || *worker.LoadMiB != 2000 || *worker.StepMiB != 1500 {
		t.Fatalf("worker %+v (load %v step %v)", worker, worker.LoadMiB, worker.StepMiB)
	}
	if low := LowestMiB(&st); low == nil || *low != 1500 {
		t.Fatalf("lowest %v", low)
	}
	if st.Load == nil || st.Load.LoadMode != LoadModeFileRead || len(st.Phases) == 0 || st.Phases[0].Load == nil || st.Phases[0].Load.LoadMode != LoadModeFileRead {
		t.Fatalf("load %+v", st.Load)
	}
	if len(st.Roles) != 2 || st.Roles[0] != (NodeRole{ID: "n0", Node: "host-1", Role: RoleHost}) || st.Roles[1].Node != "ps5-a" ||
		st.Roles[1].Role != RoleWorker || st.Roles[1].MemoryMiB == nil || *st.Roles[1].MemoryMiB != 7900 {
		t.Fatalf("roles %+v", st.Roles)
	}
	if len(st.Reload) != 1 || st.Reload[0].Node != "ps5-a" || st.Reload[0].Verdict != "partial" || *st.Reload[0].CachedPct != 63 {
		t.Fatalf("reload %+v", st.Reload)
	}
	// The same figures come back from the store and in the benchmark's results.
	res, err := tm.store.BenchmarkResults("gemma", 10)
	if err != nil || len(res) != 1 || res[0].LowestMiB == nil || *res[0].LowestMiB != 1500 || res[0].Host != "host-1" ||
		len(res[0].Workers) != 1 || res[0].LoadMode != LoadModeFileRead {
		t.Fatalf("results %+v %v", res, err)
	}
}

// The background sampler records figures on its own.
func TestMemWatchBackgroundSamples(t *testing.T) {
	w := newMemWatch()
	calls := 0
	w.start(time.Millisecond, func(context.Context) (*StatusInfo, error) {
		calls++
		return &StatusInfo{Workers: []WorkerInfo{{ID: "a", Hostname: "ps5-a", AvailMiB: i64(int64(5000 - calls))}}}, nil
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		a := w.low["a"]
		n := 0
		if a != nil {
			n = a.samples
		}
		w.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no background samples")
		}
		time.Sleep(time.Millisecond)
	}
	w.stop()
	got := w.result([]string{"a", "missing"}, nil)
	if len(got) != 1 || got[0].Node != "ps5-a" || got[0].LoadMiB != nil || *got[0].StepMiB >= 5000 {
		t.Fatalf("%+v", got)
	}
}
