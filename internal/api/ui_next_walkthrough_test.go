package api

import (
	"encoding/json"
	"testing"
	"time"
)

// The gear opens the cluster's own Settings page, not Profiles (the owner's
// walkthrough, build-92 W4); its menu reads Settings, Profiles, Test
// profiles, Tiers and zones, Placement preview, Audit, About; the old
// addresses of every page under it still open that page.
func TestUINextSettingsGear(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--config", "normal"}, settingsPaths...)
	st := ovStatus(time.Now())
	menu := "nav[aria-label^=Settings] a.nd b"
	order := []string{"Settings", "Profiles", "Test profiles", "Tiers and zones", "Placement preview", "Audit", "About"}
	cases := []any{
		map[string]any{"name": "gear: opens Settings", "route": "#/overview", "status": st, "click": []string{"button.rb[data-section=settings]"},
			"hash":    "#/settings",
			"pageHas": []string{"button.rb[data-section=settings][aria-current=page]", "nav[aria-label^=Settings] a.nd.on[href='#/settings']"},
			"expect":  []string{"Timing"}},
		map[string]any{"name": "gear: the menu's order", "route": "#/settings", "status": st, "pageSeq": map[string]any{menu: order}},
	}
	// every page of the section keeps its address and stays in the gear's section
	for _, r := range []string{"#/settings", "#/profiles", "#/test-profiles", "#/tiers", "#/placement", "#/audit", "#/about"} {
		cases = append(cases, map[string]any{"name": "gear: old address " + r, "route": r, "status": st,
			"hash": r, "pageHas": []string{"button.rb[data-section=settings][aria-current=page]", "nav[aria-label^=Settings] a.nd.on[href='" + r + "']"},
			"notOnPage": []string{"There is no such screen"}})
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": cases})
}

// The owner's "already known" items of the walkthrough (build-92 s1 to s4,
// s7) on the runs of the runcancel fixture: run 51 is a direct run of one
// benchmark, cancelled at its one step; run 52 is a group's run going.
func TestUINextBenchWalkthroughRuns(t *testing.T) {
	f := benchRunCancel(t, "runcancel")
	st := f["/api/status"]
	// as the server sends a direct run: inputs from its first step (bench.directInputs), no recipe
	r51 := map[string]any{}
	for k, v := range f["/api/bench/runs/51"].(map[string]any) {
		r51[k] = v
	}
	r51["inputs"] = map[string]any{"profile": "test-s3-qwen4b-2con", "model": "qwen3.5-4b-q4_k_m.gguf", "node_count": 2}
	listOffset := `^GET /api/bench/runs\?(.*&)?offset=`
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		// s1: a cancelled run's mark is a neutral grey dash, in the list row and its panel's Reports row; never the red cross
		map[string]any{"name": "s1: a cancelled run's row mark", "route": "#/benchmarks/runs", "status": st,
			"has":  []string{"tr[data-run='51'] .bp-ri[data-result=cancelled] [stroke*=cancelled]"},
			"none": []string{"tr[data-run='51'] .bp-ri [fill*=failed]"}},
		map[string]any{"name": "s1: a cancelled step's mark in the panel", "route": "#/benchmarks/runs/51", "status": st,
			"pageHas":  []string{"aside.side a.bp-rep .bp-ri[data-result=cancelled] [stroke*=cancelled]"},
			"pageNone": []string{"aside.side a.bp-rep .bp-ri [fill*=failed]"}},
		// s1: "ran" counts the entries that finished: the cancelled one is "not run"
		map[string]any{"name": "s1: a cancelled entry is not ran", "route": "#/benchmarks/runs/51", "status": st,
			"pageSeq": map[string]any{"aside.side .bp-kpi[data-kpi=ran] b": []string{"0"}, "aside.side .bp-kpi[data-kpi=not-run] b": []string{"1"}}},
		// s2: when the run ends (here: cancelled) the Runs list behind the panel is
		// read again (the control first: the case after it leaves no run going)
		map[string]any{"name": "s2: no run ends, no second read", "route": "#/benchmarks/runs/52", "status": st,
			"fetchedCount": map[string]any{listOffset: 1}},
		map[string]any{"name": "s2: the list is read again when the run ends", "route": "#/benchmarks/runs/52", "status": st,
			"benchRunAfter": nil, "fetchedCount": map[string]any{listOffset: 2}},
		// s3: Run again is offered on a cancelled direct run, which carries inputs as the server sends them
		map[string]any{"name": "s3: Run again on a cancelled direct run", "route": "#/benchmarks/runs/51", "status": st,
			"fetch":    withAnswer(f, "/api/bench/runs/51", r51),
			"pageHas":  []string{"aside.side .bp-acts button.pri[data-act=run-again]:not([disabled])"},
			"pageNone": []string{"aside.side .bp-notyet[data-notyet=recipe]"}},
		// s4: the panel is headed "Run" for one benchmark's run, "Group run" for a group's
		map[string]any{"name": "s4: a single benchmark's run is headed Run", "route": "#/benchmarks/runs/51", "status": st,
			"pageSeq": map[string]any{"aside.side .side-label": []string{"Run"}}},
		map[string]any{"name": "s4: a group's run is headed Group run", "route": "#/benchmarks/runs/52", "status": st,
			"pageSeq": map[string]any{"aside.side .side-label": []string{"Group run"}}},
		// s7: Compare's hover says only that it is not built yet and needs a route
		map[string]any{"name": "s7: Compare's hover", "route": "#/benchmarks/runs/51", "status": st,
			"titles":    []string{"Not available yet: comparing two runs is not built yet; it needs a server route first"},
			"notOnPage": []string{"mockup"}},
	}})
}

// s6: a refused save in the benchmark editor: the sentence under the part
// says its place in the page's words, as the save bar does, never the raw
// path.
func TestUINextBenchWalkthroughRefusedField(t *testing.T) {
	f := benchEditors(t)
	st := f["/api/status"]
	comment := map[string]any{"type": ".ed-savebar input[aria-label=Comment]", "value": "x"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "s6: the prompt's own sentence names the place", "route": "#/benchmarks/new/cold-reload-loaded", "status": st,
			"fetch": withAnswer(f, "POST /api/bench/benchmarks", map[string]any{"$status": 422, "$body": map[string]any{
				"error": map[string]any{"message": "x", "code": 422, "reason": "invalid"},
				"problems": []any{
					map[string]any{"path": "phases[1]", "message": "phases[1] (warmup): count must be 0 to 8"},
					map[string]any{"path": "phases[2].requests[0]", "message": "phases[2].requests[0] (explain): messages or prompt is required"}}}}),
			"click": []any{comment, ".ed-savebar button[data-act=save]"},
			"text": map[string]any{
				".ed-phase[data-phase='1'] [data-problem]": "Phase 2: count must be 0 to 8",
				".ed-phase[data-phase='2'] [data-problem]": `Phase 3, prompt "explain": messages or prompt is required`},
			"absent": []string{"phases[2].requests[0]", "phases[1] (warmup)"}},
	}})
}

// s5: a memory figure under 1 GB is in MB (as the Models page writes a
// small size), 1 GB and over in GB with one decimal; the Placement
// preview's conversation-memory sentences use that one rule (core/format.js gb).
func TestUINextPlacementMemoryUnits(t *testing.T) {
	e := newEnv(t, chatOK)
	var status map[string]any
	json.Unmarshal([]byte(body(t, e.get("/api/status"))), &status)
	plan := func(ck, pc float64) map[string]any {
		var p map[string]any
		if err := json.Unmarshal([]byte(body(t, e.post("/api/plan", `{"model":"alpha"}`))), &p); err != nil {
			t.Fatal(err)
		}
		p["host"] = "server"
		p["host_memory"] = map[string]any{"family": "windowed", "cell_mib": 50, "checkpoints": 3, "checkpoints_mib": ck, "prompt_cache_mib": pc,
			"counted": true, "machine": "host-1", "checkpoints_from": "the cluster default", "prompt_cache_from": "the cluster default", "estimate": true}
		return p
	}
	small, big := plan(151, 0), plan(1608, 0)
	cases := []any{
		map[string]any{"name": "s5: 151 MiB is 151 MB", "route": "#/placement/alpha", "status": status, "fetch": map[string]any{"/api/plan": small},
			"expect": []string{"Conversation memory of the host process: up to 151 MB (restore points 151 MB, prompt cache 0 MB)"}, "absent": []string{"0.1 GB", "0.0 GB"}},
		map[string]any{"name": "s5: 1,608 MiB is 1.6 GB", "route": "#/placement/alpha", "status": status, "fetch": map[string]any{"/api/plan": big},
			"expect": []string{"up to 1.6 GB (restore points 1.6 GB"}},
	}
	uiExec(t, map[string]any{"modules": []string{"views/placement.js"}, "fetch": map[string]any{}, "cases": cases})
}
