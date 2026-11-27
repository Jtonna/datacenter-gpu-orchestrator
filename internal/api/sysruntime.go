package api

import (
	"runtime"
	"sync"
	"time"
)

// runtimeEvery is the least time between two reads of the process's memory
// figures: runtime.ReadMemStats stops the world for a moment, and GET
// /api/system may be polled.
const runtimeEvery = 5 * time.Second

// runtimeCache holds the last read of the process's memory figures.
type runtimeCache struct {
	mu  sync.Mutex
	at  time.Time
	doc map[string]any
}

// runtimeFigures is the "runtime" block of GET /api/system: heap in use
// (spans holding objects), heap allocated (live and not yet swept objects),
// live heap objects, memory obtained from the OS, goroutines, garbage
// collections so far and when the last one ended (unix ms, 0 before the
// first), and read_at (unix ms of the read).
func (c *runtimeCache) figures(now time.Time) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.doc != nil && now.Sub(c.at) < runtimeEvery && !now.Before(c.at) {
		return c.doc
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	last := int64(0)
	if m.LastGC > 0 {
		last = int64(m.LastGC / uint64(time.Millisecond))
	}
	c.at = now
	c.doc = map[string]any{"heap_inuse_bytes": m.HeapInuse, "heap_alloc_bytes": m.HeapAlloc, "heap_objects": m.HeapObjects,
		"sys_bytes": m.Sys, "goroutines": runtime.NumGoroutine(), "gc_cycles": m.NumGC, "last_gc_at": last, "read_at": now.UnixMilli()}
	return c.doc
}
