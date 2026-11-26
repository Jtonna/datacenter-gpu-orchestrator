package api

import (
	"testing"
)

// GET /api/system carries the process's own memory figures (runtime), so a
// memory question is one read: heap in use, live heap objects, goroutines,
// the last garbage collection and how many ran. They are read at most once
// per runtimeEvery (ReadMemStats stops the world for a moment) and served
// from that copy meanwhile; read_at says when.
func TestSystemRuntimeFigures(t *testing.T) {
	e := newEnv(t, chatOK)
	rt := func() map[string]any {
		m, ok := jsonOf(t, body(t, e.get("/api/system")))["runtime"].(map[string]any)
		if !ok {
			t.Fatal("GET /api/system has no runtime block")
		}
		return m
	}
	a := rt()
	for _, k := range []string{"heap_inuse_bytes", "heap_alloc_bytes", "heap_objects", "sys_bytes", "goroutines", "gc_cycles", "last_gc_at", "read_at"} {
		v, ok := a[k].(float64)
		if !ok {
			t.Fatalf("runtime.%s missing or not a number: %v", k, a)
		}
		if v <= 0 && k != "gc_cycles" && k != "last_gc_at" {
			t.Fatalf("runtime.%s = %v, want above 0", k, v)
		}
	}
	if b := rt(); b["read_at"] != a["read_at"] {
		t.Fatalf("two reads in a row read the runtime twice (read_at %v then %v): want the cached copy", a["read_at"], b["read_at"])
	}
}
