package api

import (
	"regexp"
	"testing"
)

// The carried-over benchmark detail pages (views/benchmarks/*, routes
// #/benchmarks/suite/<id>, #/benchmarks/benchmark/<id>, #/benchmarks/runs/<id>)
// in the new shell, with long texts (fixture --bench long): on live v66 a
// suite's or benchmark's description, a run's Note and Scheduler version and
// a failure reason were drawn 16 px wide, one letter per line, over the
// labels beside them. The cause: the Zones checkbox rule ".cb{width:16px}"
// in next/app.css also matched every card body (div.cb).

const (
	benchSuite = "cold-warm-consoles"
	benchID    = "qwen4b-2con"
)

// The checkbox rule is scoped to inputs; the card body keeps no width of its own.
func TestUINextCardBodyIsNotACheckbox(t *testing.T) {
	css := uiText(t, uiNextDir+"app.css")
	if m := regexp.MustCompile(`(^|[\s},])\.cb\s*\{[^}]*\bwidth\s*:`).FindString(css); m != "" {
		t.Errorf("app.css gives every .cb (the card body too) a width: %q", m)
	}
	if !regexp.MustCompile(`input\.cb\s*\{[^}]*width:16px`).MatchString(css) {
		t.Error("app.css: the 16 px checkbox rule is not on input.cb")
	}
	if !regexp.MustCompile(`\.stats\s*>\s*\.stat\s*\{[^}]*flex:\s*0 0 auto`).MatchString(css) {
		t.Error("app.css: a stat cell may shrink below its content")
	}
	if !regexp.MustCompile(`\.tbl td\.wide\s*\{[^}]*min-width`).MatchString(css) {
		t.Error("app.css: no td.wide (a table column that does not wrap to a word a line)")
	}
}

func TestUINextBenchDetailLongTexts(t *testing.T) {
	failed := "run-20261124T071800-a0c008" // FAILED_AT 8 in bench.mjs
	paths := []string{"/api/status", "/api/models", "/api/profiles", "/api/bench/state", "/api/bench/suites", "/api/bench/suites/" + benchSuite,
		"/api/bench/benchmarks", "/api/bench/benchmarks/" + benchID, "/api/bench/benchmarks/" + benchID + "/results", "/api/bench/runs",
		"/api/bench/runs/" + failed, "/api/bench/aggregates", "/api/bench/profiles", "/api/bench/external"}
	f := fixtureAnswers(t, []string{"--bench", "long"}, paths...)
	st := f["/api/status"]
	desc := "Measures load times and answer speed on the consoles"
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "suite: the description is its own row above the stats", "route": "#/benchmarks/suite/" + benchSuite, "status": st,
			"expect": []string{desc, "Benchmarks", "Last run"},
			"has":    []string{"section.card div.cb p", "section.card div.stats"}, "none": []string{".stat p"}},
		map[string]any{"name": "suite: the duration chart spreads its runs", "route": "#/benchmarks/suite/" + benchSuite, "status": st,
			"evenX": []string{".chart .pt"}},
		map[string]any{"name": "suite: Against the run before stays on one line", "route": "#/benchmarks/suite/" + benchSuite, "status": st,
			"has": []string{"td.tight span.row"}},
		map[string]any{"name": "benchmark: description row; Suite and On are wide columns", "route": "#/benchmarks/benchmark/" + benchID, "status": st,
			"expect": []string{"Qwen3 4B on two consoles", "Results, run by run", "Host: console-01"},
			"has":    []string{"section.card div.cb p", "div.scroll table.tbl td.wide"}, "count": map[string]any{"thead th.wide": 2}, "evenX": []string{".viz-grid .chart .pt"}},
		map[string]any{"name": "runs list: Suite does not wrap to a word a line", "route": "#/benchmarks/runs", "status": st,
			"count": map[string]any{"thead th.mid": 1}},
		map[string]any{"name": "failed run: one Failed badge, the reason in its own row", "route": "#/benchmarks/runs/" + failed, "status": st,
			"expect": []string{"It failed:", "out of memory", "Note", "Scheduler version", "v0.1.8-rc.3"},
			"has":    []string{"section.flag-err div.cb div.banner.err", "dl.kv"},
			"count":  map[string]any{"section.flag-err div.ch span.b": 1}},
	}})
}
