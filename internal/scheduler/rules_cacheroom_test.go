package scheduler

import (
	"testing"

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
