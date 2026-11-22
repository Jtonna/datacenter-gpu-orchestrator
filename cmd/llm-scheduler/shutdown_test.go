package main

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// Every user of the database stops before the database closes: the API
// server (its catalog build, audit queue and agents table), the pool, the
// scheduler with its learned-need writer, and the benchmark manager. After
// serveUntil returns, nothing writes to the store (a late writer races the
// process exit: a lost or torn write, and in tests "directory not empty" at
// cleanup). Both membership modes.
func TestShutdownClosesTheStoreLast(t *testing.T) {
	for _, yaml := range []string{
		"listen: \"127.0.0.1:0\"\nprofiles: {}\n",
		"listen: \"127.0.0.1:0\"\nlocal_node: host-1\ndefaults: {membership: agents}\nprofiles: {}\n",
	} {
		var mu sync.Mutex
		var steps []string
		var st *bench.Store
		shutdownStep = func(step string, s *bench.Store) {
			mu.Lock()
			defer mu.Unlock()
			steps = append(steps, step)
			if s != nil {
				st = s
			}
		}
		startLog(t, yaml, "")
		shutdownStep = nil
		time.Sleep(200 * time.Millisecond) // a late writer would have written by now
		mu.Lock()
		got := slices.Clone(steps)
		mu.Unlock()
		if len(got) == 0 || got[len(got)-1] != "store closed" || !slices.Contains(got, "api closed") {
			t.Fatalf("shutdown steps %v: the API must be closed before the store, and the store last", got)
		}
		if n := st.LateWrites(); n != 0 {
			t.Fatalf("%d writes reached the store after it was closed", n)
		}
	}
}
