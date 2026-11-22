package scheduler

import (
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// The second reload line (design, "Reload from cache"): the room the rule
// leaves for the file share, minus what the node's file cache holds now. Not
// part of the verdict, which says what CAN stay cached.

// Measured on consoles: memory 14435 MiB, margin 2024 MiB, model share 5576
// MiB, file share 4512 MiB (verdict full). One console's cache held 6959 MiB
// of other files: the first reloads took 44.2 s, 24.6 s and 13.6 s. Another's
// held 1184 MiB: warm from the second load on.
func TestCacheRoomMeasuredCases(t *testing.T) {
	in := ReloadInput{FileRead: true, Shared: true, FileMiB: 4512, ModelMiB: 5576, MemoryMiB: 14435, MarginMiB: 2024}
	if v, _ := ReloadFromCache(in); v != ReloadFull {
		t.Fatalf("verdict %s, want full", v)
	}
	cases := []struct {
		name     string
		in       ReloadInput
		cached   int
		room     int
		occupied bool
		ok       bool
	}{
		{"cache full of other files", in, 6959, -124, true, true},
		{"after the first reload", in, 8708, -1873, true, true},
		{"almost empty cache", in, 1184, 5651, false, true},
		{"empty cache", in, 0, 6835, false, true},
		{"exactly the file share left", in, 6835 - 4512, 4512, false, true},
		{"one MiB short", in, 6835 - 4511, 4511, true, true},
		{"dedicated GPU: the model is not in system memory", ReloadInput{FileRead: true, FileMiB: 4512, ModelMiB: 5576, MemoryMiB: 16000, MarginMiB: 1000},
			6000, 9000, false, true},
		{"pushed: the node does not read the file", ReloadInput{Shared: true, FileMiB: 4512, ModelMiB: 5576, MemoryMiB: 14435, MarginMiB: 2024}, 1184, 0, false, false},
		{"memory unknown", ReloadInput{FileRead: true, Shared: true, FileMiB: 4512, ModelMiB: 5576}, 1184, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			room, occ, ok := CacheRoom(tc.in, tc.cached)
			if ok != tc.ok || (ok && (room != tc.room || occ != tc.occupied)) {
				t.Fatalf("room %d occupied %v ok %v, want %d %v %v", room, occ, ok, tc.room, tc.occupied, tc.ok)
			}
		})
	}
}

// The plan carries the figures for a node that reports its cache, and none for
// one that does not; the verdict is the same either way.
func TestReloadPlanCacheOccupied(t *testing.T) {
	for _, tc := range []struct {
		name     string
		known    bool
		cached   int
		occupied bool
	}{
		{"occupied", true, 6959, true},
		{"not occupied", true, 1184, false},
		{"no cache figure", false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := reloadConsole(t, true, 2.7, 2.9)
			h.view.modify("a", func(n *core.Node) { n.CachedMiB, n.CachedKnown = tc.cached, tc.known })
			h.viewChanged()
			r := h.c.dryPlan("m").ReloadFromCache[0]
			if r.Verdict != ReloadFull || r.CacheOccupied != tc.occupied {
				t.Fatalf("reload %+v", r)
			}
			if !tc.known {
				if r.CachedNowMiB != nil || r.CacheRoomMiB != nil {
					t.Fatalf("figures without a cache figure: %+v", r)
				}
				return
			}
			want := r.MemoryMiB - r.MarginMiB - r.ModelMiB - tc.cached
			if r.CachedNowMiB == nil || *r.CachedNowMiB != tc.cached || r.CacheRoomMiB == nil || *r.CacheRoomMiB != want {
				t.Fatalf("reload %+v", r)
			}
		})
	}
}

// Whether a short room is flagged depends on the last model file that became
// ready on the node: the profile's own file is not flagged (the cache most
// likely holds it), another file is flagged and named, and an unknown one
// (nothing loaded since the scheduler started) is flagged with a note. The
// three measured cases, with each history.
func TestCacheFlagByLastFile(t *testing.T) {
	in := ReloadInput{FileRead: true, Shared: true, FileMiB: 4512, ModelMiB: 5576, MemoryMiB: 14435, MarginMiB: 2024}
	for _, tc := range []struct {
		name   string
		cached int
		last   string
		flag   bool
		note   string
	}{
		{"other files, last load another model", 6959, "other.gguf", true, "last load on this node was other.gguf"},
		{"other files, nothing loaded since start", 6959, "", true, CacheNoteUnknown},
		{"other files, last load this model", 6959, "m.gguf", false, CacheNoteSameFile},
		{"after one reload, last load this model", 8708, "m.gguf", false, CacheNoteSameFile},
		{"after one reload, last load another model", 8708, "other.gguf", true, "last load on this node was other.gguf"},
		// The third console: 5640 MiB cached right after it read this model's
		// file once; room 1195 < 4512, but it reloaded warm.
		{"third console after its first read", 5640, "m.gguf", false, CacheNoteSameFile},
		{"almost empty cache, any history", 1184, "other.gguf", false, ""},
		{"almost empty cache, unknown", 1184, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, short, ok := CacheRoom(in, tc.cached)
			if !ok {
				t.Fatal("not ok")
			}
			flag, note := CacheFlag(short, tc.last, "m.gguf")
			if flag != tc.flag || note != tc.note {
				t.Fatalf("flag %v note %q, want %v %q", flag, note, tc.flag, tc.note)
			}
		})
	}
}

// The scheduler keeps, per node, the model file of the last load that became
// ready there; the plan uses it.
func TestReloadPlanCacheLastFile(t *testing.T) {
	h := reloadConsole(t, true, 2.7, 2.9)
	h.llama("other", 3000)
	h.view.modify("a", func(n *core.Node) { n.CachedMiB, n.CachedKnown = 6959, true })
	h.viewChanged()
	plan := func() NodeReload { h.t.Helper(); return h.c.dryPlan("m").ReloadFromCache[0] }
	load := func(profile string) {
		h.t.Helper()
		w := h.request(profile)
		h.launched(WorkerResult{LaunchIDs: map[core.NodeID]string{"a": "l-" + profile}})
		h.healthy(profile)
		h.release(h.granted(w))
		if err := h.c.unload(profile, false); err != nil {
			t.Fatal(err)
		}
		h.step(evTick{})
		h.wantState(profile, "absent")
		h.advance(4 * time.Second) // settle
	}

	// Unknown: nothing loaded since the scheduler started.
	if r := plan(); !r.CacheOccupied || r.CacheLastFile != "" || r.CacheNote != CacheNoteUnknown {
		t.Fatalf("unknown: %+v", r)
	}
	// Another model was the last ready load: flagged, named.
	load("other")
	if r := plan(); !r.CacheOccupied || r.CacheLastFile != "other.gguf" || r.CacheNote != "last load on this node was other.gguf" {
		t.Fatalf("other: %+v", r)
	}
	// This model was: not flagged, the numbers stay.
	load("m")
	if r := plan(); r.CacheOccupied || r.CacheLastFile != "m.gguf" || r.CacheNote != CacheNoteSameFile || r.CacheRoomMiB == nil || *r.CacheRoomMiB >= r.FileMiB {
		t.Fatalf("same: %+v", r)
	}
}
