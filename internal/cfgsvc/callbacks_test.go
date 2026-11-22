package cfgsvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// Apply and OnChange run after the write lock is released, so they may read
// the service; concurrent changes still reach them in revision order.
func TestCallbacksMayReadTheServiceAndKeepOrder(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var applied, changed []int64
	e.svc.o.Apply = func(c *config.Config) {
		if got := e.svc.Revision(); got < c.Revision {
			t.Errorf("Apply of revision %d sees revision %d", c.Revision, got)
		}
		mu.Lock()
		applied = append(applied, c.Revision)
		mu.Unlock()
	}
	e.svc.o.OnChange = func(ev ChangeEvent) {
		_ = e.svc.View()
		mu.Lock()
		changed = append(changed, ev.Revision)
		mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, id := range []string{"a", "b", "c", "d"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.svc.CreateProfile(context.Background(), ui("x"), id, llama(id+".gguf", 1024)); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a callback that reads the service deadlocked")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, want := range []int64{1, 2, 3, 4} {
		if len(applied) != 4 || applied[i] != want || len(changed) != 4 || changed[i] != want {
			t.Fatalf("applied %v, changed %v", applied, changed)
		}
	}
}
