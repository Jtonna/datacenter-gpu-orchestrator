package api

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// /api/profiles carries the cache figures of a node that reports its file
// cache, next to the verdict, and leaves them out for one that does not.
func TestReloadEntriesCacheFigures(t *testing.T) {
	now, room := 6959, -124
	got := reloadEntries([]scheduler.NodeReload{
		{ID: "a", Verdict: scheduler.ReloadFull, FileMiB: 4512, CachedNowMiB: &now, CacheRoomMiB: &room, CacheOccupied: true,
			CacheLastFile: "other.gguf", CacheNote: "last load on this node was other.gguf"},
		{ID: "b", Verdict: scheduler.ReloadFull, FileMiB: 4512},
	}, map[string]string{"a": "ps5-a"})
	a, b := got[0], got[1]
	if a["node"] != "ps5-a" || a["verdict"] != "full" || a["cached_now_mib"] != 6959 || a["cache_room_mib"] != -124 || a["cache_occupied"] != true ||
		a["cache_last_file"] != "other.gguf" || a["cache_note"] != "last load on this node was other.gguf" {
		t.Fatalf("a %v", a)
	}
	for _, k := range []string{"cached_now_mib", "cache_room_mib", "cache_occupied", "cache_last_file", "cache_note"} {
		if _, ok := b[k]; ok {
			t.Fatalf("b has %s: %v", k, b)
		}
	}
}
