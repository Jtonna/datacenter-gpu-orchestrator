package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// "Compare with…" in the page's words (the lead's look at a copy of live,
// build-92): no literal "null" after the picks; forced zones by their names
// (the dry plans' names, then the zones list; the id only when none); every
// known setting labelled and valued as the profile panel says it (Stage
// "Test" / "Production", Hosted on "a zone" / "the scheduler's machine",
// Memory need with its unit, the context as "32k (32,768)"), an unwritten
// default said as the default; an unknown key stays raw, in mono.
func TestUINextCompareInPageWords(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles/test", "/api/profiles", "/api/models", "/api/status")
	st := fetch["/api/status"]
	const tp, prod = "qwen3-14b-128k", "gemma-4-27b"
	fetch["/api/config/profiles/"+tp] = map[string]any{"revision": 412, "id": tp, "profile": map[string]any{"kind": "llama", "model": "qwen3-14b-Q5_K_M.gguf", "ctx": 131072,
		"stage": "test", "host": "node", "force_nodes": []string{znNamedByPlan, znNoName}, "zz_future": 3, "extra_args": []string{"--ctx-checkpoints", "3"}}}
	fetch["/api/config/profiles/"+prod] = map[string]any{"revision": 412, "id": prod, "profile": map[string]any{"kind": "llama", "model": "gemma-4-27b-it-Q4_K_M.gguf", "ctx": 32768,
		"needs_mib": 5200, "parallel": 1}}
	fetch["POST /api/plan"] = map[string]any{"$match": []any{
		[]any{`"model":"` + tp + `"`, map[string]any{"action": "start", "need_mib": 9216, "need_basis": "estimated from the file size",
			"memory": map[string]any{"nodes": []any{map[string]any{"id": znNamedByPlan, "node": "zone-01", "will_take_mib": 4000}, map[string]any{"id": znNoName, "will_take_mib": 4000}}}}},
		[]any{`"model":"` + prod + `"`, map[string]any{"action": "serve", "need_mib": 18432, "need_basis": "measured at a load"}}}}
	open := "#/test-profiles?q=" + tp + "&profile=" + tp
	compare := []any{"aside.side button[data-act=compare]", map[string]any{"type": "input[data-cmp-search]", "value": prod}, "div.dlg button[data-pick=" + prod + "]"}
	never := []string{"null", "undefined"}
	row := func(k string) string { return "table.cmp-tbl tr[data-key=" + k + "]" }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "compare: the picks line has no null", "route": "#/profiles?profile=" + prod, "status": st,
			"click": []any{"aside.side button[data-act=compare]"},
			"page":  []string{"more: type to narrow"}, "notOnPage": never},
		map[string]any{"name": "compare: forced zones by name, the id only for one with no name", "route": open, "status": st,
			"click":     compare,
			"page":      []string{"Forced zones", "zone-01, " + znNoName},
			"notOnPage": append([]string{znNamedByPlan}, never...)},
		map[string]any{"name": "compare: every known setting in the panel's words", "route": open, "status": st,
			"click": append(append([]any{}, compare...), "div.dlg button.switch"),
			"page": []string{"Hosted on", "a zone (its first forced zone)", "the scheduler's machine", "Stage", "Test", "Production (not marked: the rule)",
				"Memory need", "worked out by the plan", "5.1 GB (5,200 MiB), set by hand", "Context size", "128k (131,072)", "32k (32,768)",
				"Parallel slots", "1 (the default)", "Extra argument --ctx-checkpoints"},
			"notOnPage": append([]string{"host", "stage", "needs_mib", "force_nodes"}, never...),
			"pageHas": []string{row("stage") + "[data-diff=diff]", row("parallel") + "[data-diff=same]", row("zz_future") + " td span.mono[data-raw-key]",
				row("stage") + " td[data-who]"},
			"pageCount": map[string]any{"table.cmp-tbl [data-raw-key]": 1}},
	}})
}

// No known setting of "Compare with…" prints its raw key, written or not,
// and no value reads "null" or "undefined" (core/profilediff.js SETTINGS,
// describe).
func TestUINextCompareSettingsTable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	mod := "file:///" + strings.TrimPrefix(filepath.ToSlash(filepath.Join(filepath.Dir(here), "ui", "next", "core", "profilediff.js")), "/")
	script := `import { SETTINGS, describe } from '` + mod + `';
const sample = { kind: 'command', model: 'm.gguf', ctx: 32768, ctx_above_trained: true, parallel: 2, kv_quant: 'q8_0', flash_attn: true, needs_mib: 5200,
  allow_tiers: [1, 2], force_nodes: ['a'], host: 'node', tensor_split: [1, 1], cmd: ['x', '--y'], check_endpoint: '/health', aliases: ['c'],
  description: 'd', display_name: 'n', generated: true, stage: 'test', ttl: '5m', load_timeout: '10m', stall_timeout: '1m', load_stall_timeout: '2m' };
const out = [];
for (const k of Object.keys(SETTINGS)) {
  if (!(k in sample)) out.push({ k, missing: true });
  const d = describe({ key: k, a: sample[k], b: undefined }, sample, {}, { zone: id => 'zone-' + id });
  out.push({ k, label: d.label, a: d.a, b: d.b, raw: !!d.keyRaw });
}
console.log(JSON.stringify(out));`
	out, err := exec.Command(node, "--input-type=module", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var rows []struct {
		K, Label, A, B string
		Raw, Missing   bool
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(rows) < 20 {
		t.Fatalf("only %d settings", len(rows))
	}
	for _, r := range rows {
		if r.Missing {
			t.Errorf("%s: no sample value in this test", r.K)
			continue
		}
		if r.Raw || r.Label == r.K || r.Label == "" {
			t.Errorf("%s: labelled %q, want the page's words", r.K, r.Label)
		}
		for _, v := range []string{r.A, r.B} {
			if v == "" || strings.Contains(v, "null") || strings.Contains(v, "undefined") || strings.Contains(v, "[object") {
				t.Errorf("%s: value %q", r.K, v)
			}
		}
	}
	// the CSS the narrow layout and the button faces rest on (the Chrome
	// layout test measures them at 390 px: TestUILayoutNoOverlap)
	css, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"button{font-family:var(--sans)", ".btn{font-family:var(--sans)", ".prf-facts dd{white-space:normal", ".side .sf{margin-top:auto;flex:none;display:flex;flex-wrap:wrap",
		".cmp-tbl td{vertical-align:top;overflow-wrap:break-word}", ".cmp-tbl td[data-who]::before{content:attr(data-who)"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("app.css has no %q", want)
		}
	}
}
