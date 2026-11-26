package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Run and Cancel from the Benchmarks pages (board bench-run/run-cancel,
// seven states), on the --bench runcancel fixture: the run dialog from a
// row's Run, Run group, Run again and "Run a benchmark"; its dry read, the
// server's refusal, the confirm, the start with the comment and the evict
// list, a changed evict list; Cancel in the banner, the row and the panel,
// with its required comment; the queue's places; a recipe's Run greyed; the
// "Not startable here" line.

// fixtureAsk asks the fixture server a POST (the run dialog's answers).
func fixtureAsk(t *testing.T, base, path, body string) any {
	t.Helper()
	resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %s", path, b)
	}
	if resp.StatusCode >= 300 {
		return map[string]any{"$status": resp.StatusCode, "$body": v}
	}
	return v
}

// benchRunCancel: the GETs of the runcancel (or runwait) fixture, the runs by
// their query, and the POSTs of the run dialog and of a cancel.
func benchRunCancel(t *testing.T, state string) map[string]any {
	t.Helper()
	get, base := fixtureStart(t, "--bench", state)
	f := map[string]any{}
	for _, p := range []string{"/api/status", "/api/models", "/api/profiles", "/api/bench/state", "/api/bench/groups", "/api/bench/benchmarks",
		"/api/bench/groups/s3a/expand", "/api/bench/groups/s1-ref/expand", "/api/bench/runs/52", "/api/bench/runs/51", "/api/bench/runs/47", "/api/bench/results/models"} {
		var v any
		get(p, &v)
		f[p] = v
	}
	var r12, r4, r1, rs3a any
	get("/api/bench/runs?limit=12&offset=0", &r12)
	get("/api/bench/runs?limit=4", &r4)
	get("/api/bench/runs?limit=1", &r1)
	get("/api/bench/runs?suite=s3a&limit=5", &rs3a)
	f["/api/bench/runs"] = map[string]any{"$match": []any{[]any{`suite=s3a`, rs3a}, []any{`limit=4\b`, r4}, []any{`limit=1\b`, r1}, []any{`.`, r12}}}
	f["/api/bench/runs/run-52"] = f["/api/bench/runs/52"]
	f["/api/bench/runs/run-51"] = f["/api/bench/runs/51"]
	f["/api/bench/runs/run-47"] = f["/api/bench/runs/47"]
	// the run routes: the dry read by its query and body, the start by its body
	dry := func(path, body string) any { return fixtureAsk(t, base, path+"?dry_run=1", body) }
	start := func(path, body string) any { return fixtureAsk(t, base, path, body) }
	g := "/api/bench/groups/s3a/run"
	f["POST "+g] = map[string]any{"$match": []any{
		[]any{`dry_run=1 .*"again_of"`, dry(g, `{"again_of":"run-52"}`)},
		[]any{`dry_run=1 .*"only_when_idle":false`, dry(g, `{"only_when_idle":false,"evict_others":false}`)},
		[]any{`dry_run=1 .*"evict_others":false`, dry(g, `{"evict_others":false}`)},
		[]any{`dry_run=1`, dry(g, `{}`)},
		[]any{`"evict":\["gemma-4-12b:64k"\]`, start(g, `{"comment":"x","evict":["gemma-4-12b:64k"]}`)},
		[]any{`.`, start(g, `{"comment":"x","evict":[]}`)},
	}}
	for _, id := range []string{"qwen4b-1con", "qwen4b-2con", "host-gpu-alone"} {
		p := "/api/bench/benchmarks/" + id + "/run"
		f["POST "+p] = map[string]any{"$match": []any{[]any{`dry_run=1`, dry(p, `{}`)}, []any{`.`, start(p, `{"comment":"x","evict":[]}`)}}}
	}
	p := "/api/bench/groups/s1-ref/run"
	f["POST "+p] = map[string]any{"$match": []any{[]any{`dry_run=1`, dry(p, `{}`)}, []any{`.`, start(p, `{"comment":"x"}`)}}}
	for _, id := range []string{"run-52", "run-53", "run-54"} {
		f["POST /api/bench/runs/"+id+"/cancel"] = start("/api/bench/runs/"+id+"/cancel", `{"comment":"x"}`)
	}
	return f
}

func TestUINextBenchRunAndCancel(t *testing.T) {
	f := benchRunCancel(t, "runcancel")
	st := f["/api/status"]
	comment := func(v string) map[string]any {
		return map[string]any{"type": "div.dlg textarea[aria-label=Comment]", "value": v}
	}
	runOf := func(sel string) []any { return []any{sel} }
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		// the list: Run on each plain benchmark and group, a recipe greyed, S1 not startable
		map[string]any{"name": "run: the list's Run controls", "route": "#/benchmarks/all", "status": st,
			"has": []string{"div.bp-row[data-bench=qwen4b-1con] button[data-act=run]", "a.bp-row[data-group=s3a] button[data-act=run]",
				"div.bp-row[data-bench=cold-load] .bp-notyet[data-notyet=recipe]", "a.bp-row[data-group=s1-ref] [data-not-startable]"},
			"none":   []string{"a.bp-row[data-group=s1-ref] button[data-act=run]", "[data-notyet=run]"},
			"expect": []string{"Not startable here: listed in bench_not_startable (Settings)", "2 more queued"},
			"titles": []string{"Not available yet: a recipe asks for its inputs"}},
		// state 1: a single benchmark, the fleet idle
		map[string]any{"name": "run: a benchmark's dialog (state 1)", "route": "#/benchmarks/all", "status": st,
			"click":        runOf("div.bp-row[data-bench=qwen4b-1con] button[data-act=run]"),
			"fetchedQuery": []string{`^POST /api/bench/benchmarks/qwen4b-1con/run\?dry_run=1 \{\}$`},
			"pageHas": []string{"div.dlg.wide[data-dlg=run][data-state=form]", "div.dlg tr[data-load='qwen3.5-4b:16k'][data-fits=fits] .b.b-use",
				"div.dlg [data-opt=wait] input[type=checkbox]:checked", "div.dlg button[data-go=review][disabled]"},
			"page": []string{"Run", "Qwen 4B load and requests, 1 console", "What will run", "1 benchmark · 4 steps", "Load · Warm-up · Requests · Unload", "At most", "12 min", "bench_max_run_minutes",
				"Loads this run makes", "Profile", "On", "Takes there", "Now", "qwen3.5-4b:16k", "Qwen 3.5 4B", "ps5-01", "3,400 MiB", "Fits now",
				"Wait until idle", "Nothing is loaded and nothing is queued: it starts at once.", "Unload other people's idle models", "Off: this run only uses memory that is free.",
				"Note", "(optional, kept with the run)", "Comment (required)", "Review and start"},
			"notFetched": []string{`^POST /api/bench/benchmarks/qwen4b-1con/run \{`}},
		// state 2: a group, something loaded, what it would unload
		map[string]any{"name": "run: a group's dialog (state 2)", "route": "#/benchmarks/all/s3a", "status": st,
			"click": runOf("aside.side button[data-act=run]"),
			"pageHas": []string{"div.dlg[data-dlg=run][data-state=form]", "div.dlg [data-would-unload='1']", "div.dlg tr[data-fits=waits] .b.b-load",
				"div.dlg button.rd-more[data-go=all-loads]"},
			"pageCount": map[string]any{"div.dlg tr[data-load]": 3},
			"page": []string{"Run group", "S3a console workers only, 1 to 5 consoles, Qwen 4B", "5 benchmarks", "one after the other, 10 s between them", "60 min", "It carries on after a failure.",
				"Loads this run makes · 5", "TEST - s3-qwen4b-2con", "ps5-01, ps5-02", "1,700 MiB each", "3 machines", "Waits: in use", "Show the other 2",
				"Loaded now: ", "gemma-4-12b:64k", "on ps5-01, last used 3 min ago", "This would unload now: ", "(idle for 3 min)", "A model that is answering a request is never cut off.",
				"Another run is going: this one waits behind 2 queued runs."}},
		map[string]any{"name": "run: Show the other loads", "route": "#/benchmarks/all/s3a", "status": st,
			"click": []any{"aside.side button[data-act=run]", "div.dlg button[data-go=all-loads]"}, "pageCount": map[string]any{"div.dlg tr[data-load]": 5}},
		// an option changed: read again with it, the unload list follows
		map[string]any{"name": "run: unticking Unload reads again", "route": "#/benchmarks/all/s3a", "status": st,
			"click":        []any{"aside.side button[data-act=run]", "div.dlg [data-opt=evict] input"},
			"fetchedQuery": []string{`^POST /api/bench/groups/s3a/run\?dry_run=1 \{"only_when_idle":true,"evict_others":false\}$`},
			"pageNone":     []string{"div.dlg [data-would-unload]"}, "page": []string{"Off: this run only uses memory that is free."}},
		// state 3: the server refuses: its own sentence, only Close
		map[string]any{"name": "run: refused on the scheduler's machine (state 3)", "route": "#/benchmarks/all", "status": st,
			"click":    runOf("div.bp-row[data-bench=host-gpu-alone] button[data-act=run]"),
			"pageHas":  []string{"div.dlg[data-dlg=run][data-state=refused] [data-refusal=scheduler_gpu]", "div.dlg button[data-close]"},
			"page":     []string{"Not startable here.", "Qwen 4B, host GPU alone would load test-s1-qwen4b-host on scheduler, the scheduler's own machine; benchmarks never load there, so nothing of the run was started."},
			"pageNone": []string{"div.dlg button[data-go=review]", "div.dlg textarea"}},
		// state 4: the confirm, and the start with the comment and the list it showed
		map[string]any{"name": "run: the confirm (state 4)", "route": "#/benchmarks/all/s3a", "status": st,
			"click":   []any{"aside.side button[data-act=run]", comment("Re-run of S3a after the rename"), "div.dlg button[data-go=review]"},
			"pageHas": []string{"div.dlg:not(.wide)[data-dlg=run][data-state=confirm]", "div.dlg [data-unloads='1']", "div.dlg button[data-go=back]", "div.dlg button.pri[data-go=start]"},
			"page": []string{"Start this run?", "This starts ", "5 loads", "ps5-01, ps5-02, ps5-03, ps5-04 and ps5-05", "one after the other", "60 minutes at most.",
				"It unloads ", "gemma-4-12b:64k", " first.", "If what is loaded changes before you press Start, the run is not started and this dialog shows the new list."},
			"notFetched": []string{`^POST /api/bench/groups/s3a/run \{`}},
		map[string]any{"name": "run: Back keeps what was typed", "route": "#/benchmarks/all/s3a", "status": st,
			"click":   []any{"aside.side button[data-act=run]", comment("keep me"), "div.dlg button[data-go=review]", "div.dlg button[data-go=back]"},
			"pageHas": []string{"div.dlg[data-state=form]", "div.dlg button[data-go=review]:not([disabled])"}},
		map[string]any{"name": "run: Start sends the comment and the unload list", "route": "#/benchmarks/all/s3a", "status": st,
			"click":        []any{"aside.side button[data-act=run]", comment("Re-run of S3a after the rename"), "div.dlg button[data-go=review]", "div.dlg button[data-go=start]"},
			"fetchedQuery": []string{`^POST /api/bench/groups/s3a/run \{"only_when_idle":true,"evict_others":true,"comment":"Re-run of S3a after the rename","evict":\["gemma-4-12b:64k"\]\}$`},
			"sentHeader":   []string{"^X-Comment: Re-run of S3a after the rename$"}, "hash": "#/benchmarks/runs/55", "pageNone": []string{"div.dlg"}},
		// the evict list changed at Start: refused, the dialog reads again and says so
		map[string]any{"name": "run: a changed unload list is refused and shown again", "route": "#/benchmarks/all/s3a", "status": st,
			"fetch": withAnswer(f, "POST /api/bench/groups/s3a/run", map[string]any{"$match": []any{
				[]any{`dry_run=1`, (f["POST /api/bench/groups/s3a/run"].(map[string]any)["$match"].([]any))[3].([]any)[1]},
				[]any{`.`, (f["POST /api/bench/groups/s3a/run"].(map[string]any)["$match"].([]any))[5].([]any)[1]}}}),
			"click":   []any{"aside.side button[data-act=run]", comment("x"), "div.dlg button[data-go=review]", "div.dlg button[data-go=start]"},
			"pageHas": []string{"div.dlg[data-state=form] [data-changed]", "div.dlg [data-would-unload]"},
			"page":    []string{"What is loaded changed. ", "What this run would unload changed since it was shown: nothing was started; look at the new list and start again"}},
		// "Run a benchmark": what to run first
		map[string]any{"name": "run: Run a benchmark asks what to run", "route": "#/benchmarks", "status": st,
			"click":    []any{"button[data-act=run-a-benchmark]"},
			"pageHas":  []string{"div.dlg[data-dlg=run][data-state=pick] select[data-pick] option[value='group:s3a']", "div.dlg select[data-pick] option[value='benchmark:qwen4b-1con']"},
			"pageNone": []string{"div.dlg select[data-pick] option[value='group:s1-ref']", "div.dlg select[data-pick] option[value='benchmark:cold-load']"},
			"page":     []string{"Run a benchmark", "What to run", "Continue"}},
		map[string]any{"name": "run: Run a benchmark, then the dialog of what was picked", "route": "#/benchmarks", "status": st,
			"click":        []any{"button[data-act=run-a-benchmark]", map[string]any{"type": "div.dlg select[data-pick]", "value": "benchmark:qwen4b-1con"}, "div.dlg button[data-go=pick]"},
			"fetchedQuery": []string{`^POST /api/bench/benchmarks/qwen4b-1con/run\?dry_run=1`}, "pageHas": []string{"div.dlg[data-state=form]"}},
		// Run again: the run's own target, with again_of; a changed definition is a warning
		map[string]any{"name": "run: Run again from the run's panel", "route": "#/benchmarks/runs/52", "status": st,
			"click":        []any{"aside.side .bp-acts button[data-act=run-again]"},
			"fetchedQuery": []string{`^POST /api/bench/groups/s3a/run\?dry_run=1 \{"again_of":"run-52"\}$`},
			"pageHas":      []string{"div.dlg[data-state=form] [data-warning=definition_changed]"},
			"page":         []string{"Changed since run run-52: Qwen 4B, 2 consoles. This run uses the definitions as they are now."}},
		map[string]any{"name": "run: Run again of a recipe run stays greyed", "route": "#/benchmarks/runs/47", "status": st,
			"pageHas": []string{"aside.side .bp-notyet[data-notyet=recipe]"}, "pageNone": []string{"aside.side button[data-act=run-again]"}},
		// state 5: cancel a running run
		map[string]any{"name": "cancel: the dialog says where the run is (state 5)", "route": "#/benchmarks/runs/52", "status": st,
			"click":   []any{"aside.side .bp-live button[data-act=cancel]"},
			"pageHas": []string{"div.dlg [data-dlg-cancel=running]", "div.dlg .dlg-foot button.dng.solid[disabled]"},
			"page": []string{"Cancel this run?", "S3a console workers only, 1 to 5 consoles, Qwen 4B", " is on step 2 of 5: loading TEST - s3-qwen4b-2con on ps5-01 and ps5-02.",
				"The step in progress is stopped.", "The model this run is loading or using is unloaded.", "The 3 steps not started are skipped.", "The results so far are kept.",
				"Comment (required)", "Keep it running", "Cancel the run"},
			"notFetched": []string{`/cancel`}},
		map[string]any{"name": "cancel: sends the comment", "route": "#/benchmarks/runs/52", "status": st,
			"click":        []any{"aside.side .bp-acts button[data-act=cancel]", comment("wrong zones"), "div.dlg .dlg-foot button.dng"},
			"fetchedQuery": []string{`^POST /api/bench/runs/run-52/cancel \{"comment":"wrong zones"\}$`}, "sentHeader": []string{"^X-Comment: wrong zones$"}},
		// state 6: the running run's panel
		map[string]any{"name": "panel: the run going (state 6)", "route": "#/benchmarks/runs/52", "status": st, "width": 1536,
			"pageHas": []string{"aside.side .bp-live[data-live=running] button[data-act=cancel]", "aside.side .runbar[role=progressbar] i", "aside.side .bp-acts button.dng[data-act=cancel]",
				"aside.side [data-started-by]", "aside.side [data-run-note]"},
			"page": []string{"Step 2 of 5", "loading TEST - s3-qwen4b-2con on ps5-01 and ps5-02", "so far", "by anonymous",
				`Note: "Re-run of S3a after the rename". Results appear here as each step ends.`}},
		// state 7: the queue in the banner and the runs list
		map[string]any{"name": "runs: queued runs say their place and have Cancel (state 7)", "route": "#/benchmarks/runs", "status": st,
			"has": []string{"tr[data-run='53'] .b.b-q[data-queued='1']", "tr[data-run='54'] .b.b-q[data-queued='2']", "tr[data-run='53'] button[data-act=cancel][data-cancel=run-53]",
				"tr[data-run='52'] [data-step='2']", "tr[data-run='51'] [data-outcome=cancelled]"},
			"expect": []string{"Queued · 1st", "Queued · 2nd", "step 2 of 5", `cancelled at step 1 of 1 by anonymous: "wrong zones"`}},
		map[string]any{"name": "runs: a queued run's Cancel", "route": "#/benchmarks/runs", "status": st,
			"click":   []any{"tr[data-run='53'] button[data-act=cancel]"},
			"pageHas": []string{"div.dlg [data-dlg-cancel=queued]"}, "page": []string{"Smoke test", " is queued (1st): it has not started.", "It is taken out of the queue; nothing of it ran."}},
		map[string]any{"name": "overview: the running line's queue and Cancel", "route": "#/benchmarks", "status": st,
			"has":    []string{"section.bp-runcard .b.b-q[data-queued='2']", "section.bp-runcard button[data-act=cancel][data-cancel=run-52]", "button[data-act=run-a-benchmark]"},
			"expect": []string{"2 more queued"}, "none": []string{"[data-notyet=run]"}},
	}})
}

// The run waiting until idle: the banner and the panel say why, with Cancel.
func TestUINextBenchRunWaiting(t *testing.T) {
	f := benchRunCancel(t, "runwait")
	st := f["/api/status"]
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "overview: waiting until idle", "route": "#/benchmarks", "status": st,
			"expect": []string{"Waiting until idle", "waiting until idle: gemma-4-12b:64k is ready (not loaded by this run)"},
			"has":    []string{"section.bp-runcard button[data-act=cancel]"}, "none": []string{"section.bp-runcard .bar"}},
		map[string]any{"name": "panel: waiting until idle", "route": "#/benchmarks/runs/52", "status": st,
			"pageHas": []string{"aside.side .bp-live[data-live=paused] button[data-act=cancel]"},
			"page":    []string{"Waiting until idle", ": gemma-4-12b:64k is ready (not loaded by this run)"}, "pageNone": []string{"aside.side .runbar"}},
		map[string]any{"name": "cancel: a waiting run", "route": "#/benchmarks/runs/52", "status": st,
			"click": []any{"aside.side .bp-live button[data-act=cancel]"},
			"page":  []string{" is waiting until idle: gemma-4-12b:64k is ready (not loaded by this run).", "Nothing of it was loaded.", "Its steps are skipped."}},
	}})
}

// withAnswer: the fetch answers with one replaced.
func withAnswer(f map[string]any, key string, v any) map[string]any {
	out := map[string]any{}
	for k, x := range f {
		out[k] = x
	}
	out[key] = v
	return out
}
