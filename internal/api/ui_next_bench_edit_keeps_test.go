package api

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// Opening and saving never loses a setting (the reviewer's condition on
// difference 10, 26 Nov): a stored benchmark and a stored group that carry
// every field the editors do not draw (a request's own sampling and messages;
// a recipe's inputs; a group's matrix entry, inputs entry, schedule, baseline
// and status with its reason) are opened in the editor on a real scheduler,
// renamed, saved with a comment; the stored JSON read back afterwards is the
// same field by field except the name (and the store's own updated_at). The
// editor says in one line by its save bar which settings it does not show.
func TestUINextBenchEditorKeepsWhatItDoesNotDraw(t *testing.T) {
	e := benchCfgEnv(t)
	save := func(method, path, body string, want int) {
		t.Helper()
		if st, b, _ := e.do(method, path, body); st != want {
			t.Fatalf("%s %s: %d %s", method, path, st, b)
		}
	}
	save("POST", "/api/bench/benchmarks", `{"id":"rich","name":"Rich","description":"every field","profile":"alpha","tags":["a","b"],"timeout_s":900,
		"expect":{"outcome":"ready","max_load_s":120},
		"phases":[{"type":"load","label":"cold load","cold":true},{"type":"warmup","count":1},
		{"type":"requests","label":"loaded","repeats":2,"stream":true,"max_tokens":64,
		 "requests":[{"name":"own","prompt":"hi","max_tokens":32,"temperature":0.7,"seed":42,"cache_prompt":false,"stream":false},
		             {"name":"chat","messages":[{"role":"system","content":"terse"},{"role":"user","content":"hello"}]}]},
		{"type":"unload"}],"comment":"setup"}`, 201)
	save("POST", "/api/bench/benchmarks", `{"id":"lo-recipe","name":"Load recipe","inputs":{"ctx":4096,"model":"m1.gguf"},"phases":[{"type":"load"},{"type":"unload"}],"comment":"setup"}`, 201)
	save("POST", "/api/bench/groups", `{"id":"wide","name":"Wide","description":"every field","cooldown_s":7,"idle_wait_s":300,"only_when_idle":false,"evict_others":true,"stop_on_failure":true,
		"schedule":{"every":"24h"},"baseline":"run-1",
		"benchmarks":["rich",{"benchmark":"lo-recipe","inputs":{"model":"m2.gguf"}},{"benchmark":"lo-recipe","matrix":{"model":["m2.gguf","m3.gguf"]}}],"comment":"setup"}`, 201)
	// a warm-up's request with its own stream: not a setting the page hides
	save("POST", "/api/bench/benchmarks", `{"id":"warm","name":"Warm","profile":"alpha","phases":[{"type":"load"},{"type":"warmup","count":1,"request":{"name":"first","prompt":"hi","max_tokens":16,"stream":true}},{"type":"unload"}],"comment":"setup"}`, 201)
	// a planned group cannot have a schedule (the server): its status is on a group of its own
	save("POST", "/api/bench/groups", `{"id":"later","name":"Later","status":"planned","reason":"the fifth console is not set up yet",
		"benchmarks":[{"benchmark":"lo-recipe","matrix":{"model":["m2.gguf","m3.gguf"]}}],"comment":"setup"}`, 201)

	stored := func(path string) map[string]any {
		t.Helper()
		st, b, _ := e.do("GET", path, "")
		if st != 200 {
			t.Fatalf("GET %s: %d %s", path, st, b)
		}
		return jsonOf(t, b)
	}
	before := map[string]map[string]any{"rich": stored("/api/bench/benchmarks/rich"), "lo-recipe": stored("/api/bench/benchmarks/lo-recipe"), "wide": stored("/api/bench/groups/wide"), "later": stored("/api/bench/groups/later")}

	typeIn := func(sel, v string) map[string]any { return map[string]any{"type": sel, "value": v} }
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": e.front.URL,
		"passWrites": []string{`^PUT /api/bench/benchmarks/(rich|lo-recipe) `, `^PUT /api/bench/groups/(wide|later) `},
		"cases": []any{
			map[string]any{"name": "a benchmark with a request's own sampling and messages", "route": "#/benchmarks/benchmark/rich/edit",
				"click": []any{typeIn("input[name=name]", "Rich, renamed"), typeIn(".ed-savebar input[aria-label=Comment]", "rename only"), ".ed-savebar button[data-act=save]"},
				"hash":  "#/benchmarks/all?saved=rich", "fetchedQuery": []string{`^PUT /api/bench/benchmarks/rich `}},
			map[string]any{"name": "the line by the save bar names them", "route": "#/benchmarks/benchmark/rich/edit",
				"has":    []string{".ed-savebar .ed-kept[data-kept]"},
				"expect": []string{"This one also has settings this page does not show (a request's own sampling, messages): they are kept as they are."}},
			map[string]any{"name": "a recipe", "route": "#/benchmarks/benchmark/lo-recipe/edit",
				"click": []any{typeIn("input[name=name]", "Load recipe, renamed"), typeIn(".ed-savebar input[aria-label=Comment]", "rename only"), ".ed-savebar button[data-act=save]"},
				"hash":  "#/benchmarks/all?saved=lo-recipe"},
			map[string]any{"name": "a group with a matrix entry, an inputs entry, a schedule and a baseline", "route": "#/benchmarks/group/wide/edit",
				"expect": []string{"This one also has settings this page does not show (matrix entries, entry inputs, schedule, baseline): they are kept as they are."}},
			map[string]any{"name": "the group renamed and saved", "route": "#/benchmarks/group/wide/edit",
				"click": []any{typeIn("input[name=name]", "Wide, renamed"), typeIn(".ed-savebar input[aria-label=Comment]", "rename only"), ".ed-savebar button[data-act=save]"},
				"hash":  "#/benchmarks/all/wide?saved=wide", "fetchedQuery": []string{`^PUT /api/bench/groups/wide `}},
			map[string]any{"name": "a planned group", "route": "#/benchmarks/group/later/edit",
				"expect": []string{"(matrix entries, status)"}},
			map[string]any{"name": "the planned group renamed and saved", "route": "#/benchmarks/group/later/edit",
				"click": []any{typeIn("input[name=name]", "Later, renamed"), typeIn(".ed-savebar input[aria-label=Comment]", "rename only"), ".ed-savebar button[data-act=save]"},
				"hash":  "#/benchmarks/all/later?saved=later"},
			map[string]any{"name": "a warm-up request's own stream is not counted", "route": "#/benchmarks/benchmark/warm/edit",
				"has": []string{"input[name=name]"}, "none": []string{".ed-kept"}},
			map[string]any{"name": "a benchmark with nothing undrawn says nothing", "route": "#/benchmarks/new/load-only",
				"none": []string{".ed-kept"}},
		}})

	renamed := map[string]string{"rich": "Rich, renamed", "lo-recipe": "Load recipe, renamed", "wide": "Wide, renamed", "later": "Later, renamed"}
	after := map[string]map[string]any{"rich": stored("/api/bench/benchmarks/rich"), "lo-recipe": stored("/api/bench/benchmarks/lo-recipe"), "wide": stored("/api/bench/groups/wide"), "later": stored("/api/bench/groups/later")}
	for id, b := range before {
		a := after[id]
		if a["name"] != renamed[id] {
			t.Errorf("%s: name %v, want %q (the save did not go through)", id, a["name"], renamed[id])
		}
		keys := map[string]bool{}
		for k := range b {
			keys[k] = true
		}
		for k := range a {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if k == "name" || k == "updated_at" {
				continue
			}
			if !reflect.DeepEqual(b[k], a[k]) {
				bj, _ := json.Marshal(b[k])
				aj, _ := json.Marshal(a[k])
				t.Errorf("%s.%s changed by opening and saving:\n before %s\n after  %s", id, k, bj, aj)
			}
		}
	}
}
