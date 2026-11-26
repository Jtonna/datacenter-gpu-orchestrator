package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// The step facts (lowest available memory, load mode, roles, reload from
// cache) and planned or blocked suites in the web UI. The UI has no
// JavaScript test runner: these tests read the embedded files, and check that
// every key the UI reads is one the server writes.

func jsonKeys(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUIBenchStepFactsKeysMatchTheServer(t *testing.T) {
	lo, pct, mem, room := int64(1), 63.0, 1.0, 1
	step := jsonKeys(t, bench.Step{
		Load:   &bench.LoadResult{LoadMode: bench.LoadModeFileRead},
		MemLow: []bench.NodeMemLow{{ID: "i", Node: "n", LoadMiB: &lo, StepMiB: &lo, Kind: bench.MemKindGPU, Note: "n"}},
		Roles:  []bench.NodeRole{{ID: "i", Node: "n", Role: bench.RoleHost, MemoryMiB: &mem}},
		Reload: []bench.NodeReload{{ID: "i", Node: "n", Verdict: "partial", CachedPct: &pct, CachedNowMiB: &room, CacheRoomMiB: &room, CacheOccupied: true, CacheLastFile: "f.gguf", CacheNote: "n"}},
	})
	suite := jsonKeys(t, benchspec.Suite{ID: "s", Status: benchspec.SuitePlanned, Reason: "r"})

	for _, c := range []struct {
		doc  string
		file string
		keys []string
	}{
		// the classic run, benchmark and aggregates pages that drew the other
		// facts were dropped (25 Nov); the server still writes them (above)
		{suite, uiNextDir + "views/bench/list.js", []string{"status", "reason"}},
		{step, "core/stepfacts.js", []string{"verdict", "cached_pct", "cache_occupied", "cached_now_mib", "cache_last_file"}},
	} {
		text := uiText(t, c.file)
		for _, k := range c.keys {
			if !strings.Contains(c.doc, `"`+k+`"`) {
				t.Errorf("the server does not write %q (%s)", k, c.doc)
			}
			if !strings.Contains(text, k) {
				t.Errorf("%s does not read %q", c.file, k)
			}
		}
	}
}

func TestUIBenchStepFactsWords(t *testing.T) {
	facts := uiText(t, "core/stepfacts.js")
	for _, want := range []string{
		"const RELOAD_PREFIX = 'Can stay in the file cache: '", "full: 'all of it'", "none: 'none'",
		"`about ${num(r.cached_pct, 0)}%`", // "Can stay in the file cache: about 85%"
		`RELOAD_TIP = 'How much of the model file can stay in the node\'s file cache next to the loaded model`,
		"After other large models the first reloads still read part of the file again.",
		"`The file cache on ${name} holds other files now (last loaded there: ${r.cache_last_file}); expect the first reloads to be slower.`",
		"`The file cache on ${name} is nearly full now (${num(r && r.cached_now_mib, 0)} MiB); if that is other models' files, the first reloads will be slower.`",
		"file_read: ['File read'", "pushed: ['Pushed'", "local: ['Local'", "not_recorded: ['Not recorded'",
		"`Host: ${host}`", "`Workers: ${ws.length} (${ws.join(', ')})`",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("core/stepfacts.js lacks %q", want)
		}
	}
	for file, wants := range map[string][]string{
		"views/models/common.js": {"reloadLine(plan.reload_from_cache"},
	} {
		text := uiText(t, file)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
}

// A planned or blocked group is listed with its status and reason, and its
// panel says it cannot run until that is settled (the run buttons are not
// on these pages yet: slice 6).
func TestUIPlannedSuites(t *testing.T) {
	for file, wants := range map[string][]string{
		uiNextDir + "views/bench/list.js": {"g.status === 'planned' ? 'planned: ' + (g.reason || 'not ready')", "'blocked: ' + (g.reason || 'cannot run')",
			"`This group is ${g.status}: ${g.reason || 'no reason given'}. It cannot run until that is settled.`"},
	} {
		text := uiText(t, file)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
}
