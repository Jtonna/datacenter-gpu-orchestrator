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
	lo, pct, mem := int64(1), 63.0, 1.0
	step := jsonKeys(t, bench.Step{
		Load:   &bench.LoadResult{LoadMode: bench.LoadModeFileRead},
		MemLow: []bench.NodeMemLow{{ID: "i", Node: "n", LoadMiB: &lo, StepMiB: &lo}},
		Roles:  []bench.NodeRole{{ID: "i", Node: "n", Role: bench.RoleHost, MemoryMiB: &mem}},
		Reload: []bench.NodeReload{{ID: "i", Node: "n", Verdict: "partial", CachedPct: &pct}},
	})
	res := jsonKeys(t, bench.BenchmarkResult{LowestMiB: &lo, LoadMode: "pushed", Host: "h", Workers: []string{"w"},
		MemLow: []bench.NodeMemLow{{StepMiB: &lo}}, Reload: []bench.NodeReload{{Verdict: "full"}}})
	mode := "pushed"
	agg := jsonKeys(t, bench.AggregatesDoc{ByModelLoadMode: []bench.ModelAgg{{ModelFile: "m", LoadMode: &mode}}})
	suite := jsonKeys(t, benchspec.Suite{ID: "s", Status: benchspec.SuitePlanned, Reason: "r"})

	for _, c := range []struct {
		doc  string
		file string
		keys []string
	}{
		{step, "views/benchmarks/runs.js", []string{"load_mode", "mem_low", "load_mib", "step_mib", "roles", "role", "memory_mib", "reload_from_cache"}},
		{res, "views/benchmarks/pages.js", []string{"lowest_available_mib", "load_mode", "host", "workers", "mem_low", "step_mib", "reload_from_cache"}},
		{agg, "views/benchmarks/aggregates.js", []string{"by_model_load_mode", "load_mode", "model_file"}},
		{suite, "views/benchmarks/common.js", []string{"status", "reason"}},
		{step, "core/stepfacts.js", []string{"verdict", "cached_pct"}},
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
		"Reload from cache: ${w}, about ${num(r.cached_pct, 0)}% of the file", // "Reload from cache: partial, about 85% of the file"
		`RELOAD_TIP = 'Whether the model file stays in the node\'s file cache after loading`,
		"file_read: ['File read'", "pushed: ['Pushed'", "local: ['Local'", "not_recorded: ['Not recorded'",
		"`Host: ${host}`", "`Workers: ${ws.length} (${ws.join(', ')})`",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("core/stepfacts.js lacks %q", want)
		}
	}
	for file, wants := range map[string][]string{
		"views/benchmarks/runs.js": {"setupBlock(step)", "'Lowest available, load'", "'Lowest available, step'", "'Not recorded'",
			"'Load mode'", "loadModeNode(l.load_mode)", "title: RELOAD_TIP", "stepRoles(step)"},
		"views/benchmarks/pages.js":      {"label: 'Lowest available'", "label: 'Load mode'", "rolesText(r.host, r.workers)", "label: 'Reload from cache'"},
		"views/benchmarks/aggregates.js": {"h('h2', 'By load mode')", "loadModeCard(agg)"},
		"views/benchmarks/common.js":     {"key: 'lowest_available_mib'"},
		"views/models/common.js":         {"reloadLine(plan.reload_from_cache"},
	} {
		text := uiText(t, file)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
}

// A planned or blocked suite is listed with a badge and its reason, and its
// Run buttons are off with the reason as the tooltip; the run dialog refuses
// it too, and a 409 from the server is shown with its reason.
func TestUIPlannedSuites(t *testing.T) {
	for file, wants := range map[string][]string{
		"views/benchmarks/common.js":     {"planned: ['Planned'", "blocked: ['Blocked'", "export const suiteNotRunnable", "cannot be run yet: ${s.reason"},
		"views/benchmarks/overview.js":   {"suiteStatusBadge(s)", "s.reason ? h('span.sub', s.reason)", "disabled: !!cache.off || !!suiteNotRunnable(s)", "title: suiteNotRunnable(s) ||"},
		"views/benchmarks/pages.js":      {"suiteStatusBadge(s)", "disabled: !!cache.off || !!suiteNotRunnable(s)", "It cannot be run until its status is removed."},
		"views/benchmarks/runconfirm.js": {"const why = suiteNotRunnable(suite);", "e.status === 409 ? 'The suite cannot be run: '"},
		"views/benchmarks/editor.js":     {"if (!su.benchmarks.length && !su.status)"},
	} {
		text := uiText(t, file)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
}
