package bench

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMemSample is how often a step samples the nodes' available memory.
const DefaultMemSample = 500 * time.Millisecond

// memWatch keeps the lowest available memory of every node seen while a step
// runs. Samples come from the background sampler (about twice a second) and
// from every status the step reads anyway (the load polls, the end of each
// phase). Only the nodes that took part are kept when the step finishes.
type memWatch struct {
	loading atomic.Bool // the step is loading its model

	mu   sync.Mutex
	low  map[string]*memLowAcc // by node id
	stop func()
}

type memLowAcc struct {
	name       string
	load, step *int64
	samples    int
	kind, note string
}

func newMemWatch() *memWatch { return &memWatch{low: map[string]*memLowAcc{}} }

// observe records one status: every node with a known figure.
func (w *memWatch) observe(s *StatusInfo) {
	if w == nil || s == nil {
		return
	}
	loading := w.loading.Load()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range s.Workers {
		if n.AvailMiB == nil {
			continue
		}
		v := *n.AvailMiB
		kind := n.AvailKind
		if kind == "" {
			kind = MemKindSystem
		}
		a := w.low[n.ID]
		if a == nil {
			a = &memLowAcc{kind: kind}
			w.low[n.ID] = a
		}
		// One kind per node and step: a GPU figure wins (it is the memory the
		// model uses); system samples taken before it are dropped, and system
		// samples after it are ignored, so the two are never mixed.
		switch {
		case kind == a.kind:
		case kind == MemKindGPU:
			*a = memLowAcc{name: a.name, kind: kind}
		default:
			continue
		}
		if n.AvailNote != "" {
			a.note = n.AvailNote
		}
		if n.Hostname != "" {
			a.name = n.Hostname
		}
		a.samples++
		if a.step == nil || v < *a.step {
			a.step = &v
		}
		if loading && (a.load == nil || v < *a.load) {
			lv := v
			a.load = &lv
		}
	}
}

// start samples in the background every d until stopped (d <= 0: never).
func (w *memWatch) start(d time.Duration, status func(context.Context) (*StatusInfo, error)) {
	if d <= 0 {
		w.stop = func() {}
		return
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			s, err := status(ctx)
			cancel()
			if err == nil {
				w.observe(s)
			}
		}
	}()
	var once sync.Once
	w.stop = func() { once.Do(func() { close(done); wg.Wait() }) }
}

// result is the lowest figures of the given nodes (ids, in order); a node
// never sampled is left out. names fills in a name the samples lacked.
func (w *memWatch) result(ids []string, names map[string]string) []NodeMemLow {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []NodeMemLow
	seen := map[string]bool{}
	for _, id := range ids {
		a := w.low[id]
		if a == nil || seen[id] {
			continue
		}
		seen[id] = true
		name := a.name
		if name == "" {
			name = names[id]
		}
		if name == "" {
			name = id
		}
		out = append(out, NodeMemLow{ID: id, Node: name, LoadMiB: a.load, StepMiB: a.step, Samples: a.samples, Kind: a.kind, Note: a.note})
	}
	return out
}
