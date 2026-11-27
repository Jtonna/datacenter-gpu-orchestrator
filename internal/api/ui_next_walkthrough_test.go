package api

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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

// W3.1: on the Benchmarks Overview the "Recent runs" card stands above
// "Latest result for each model" (the owner's walkthrough, build-92).
func TestUINextBenchWalkthroughOverviewOrder(t *testing.T) {
	f := benchBoards(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "W3.1: Recent runs above the latest results", "route": "#/benchmarks", "status": f["/api/status"],
			"order": []string{"Recent runs", "All 49", "Latest result for each model"},
			"has":   []string{"section.bp-recentcard", "section.bp-latest"}},
	}})
}

// cssRule is one rule of a stylesheet: its selector, the @-block it sits in
// ("" at the top), and its declarations as written.
type cssRule struct{ at, sel, body string }

// cssRules reads a stylesheet into its rules, comments removed, the rules
// inside @media (and other conditional blocks) listed with their block.
func cssRules(src string) []cssRule {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	var out []cssRule
	var walk func(s, at string)
	walk = func(s, at string) {
		for i := 0; ; {
			j := strings.IndexByte(s[i:], '{')
			if j < 0 {
				return
			}
			j += i
			depth, k := 1, j+1
			for depth > 0 && k < len(s) {
				switch s[k] {
				case '{':
					depth++
				case '}':
					depth--
				}
				k++
			}
			sel, body := strings.TrimSpace(s[i:j]), s[j+1:k-1]
			if strings.HasPrefix(sel, "@media") || strings.HasPrefix(sel, "@supports") || strings.HasPrefix(sel, "@container") {
				walk(body, sel)
			} else {
				out = append(out, cssRule{at, sel, body})
			}
			i = k
		}
	}
	walk(src, "")
	return out
}

// W3.2: the weights are one rule for the whole product, set in the tokens
// (the owner's walkthrough, build-92): body 500, labels and column headers
// 600, the emphasised name 600, mono numbers 400, buttons 600, headings 700,
// the page title 800. No rule writes a weight as a number; the heading and
// title weights only on headings, titles and the big figures of a tile.
func TestUINextWeightTokens(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	rules := cssRules(string(raw))
	want := map[string]string{"--w-body": "500", "--w-label": "600", "--w-strong": "600", "--w-num": "400", "--w-btn": "600", "--w-head": "700", "--w-title": "800"}
	decl := regexp.MustCompile(`(?:^|[;{\s])(--w-[a-z]+)\s*:\s*([^;]+)`)
	weight := regexp.MustCompile(`font-weight\s*:\s*([^;}]+)`)
	got := map[string]string{}
	bySel := map[string]string{}
	// the selectors that may carry the heading or the title weight: a heading
	// element, or one of these titles and tile figures
	headOK := map[string]bool{".bign": true, ".menu-head b": true, ".stat>b": true, ".prog-top .pct": true, ".empty b": true, ".side-title": true,
		".viz figcaption b": true, ".viz figcaption .fig": true, ".hp-title": true, ".fact b": true, ".plc-fig b": true, ".bp-sec": true, ".bp-h": true,
		".bp-kpi b,.bp-head b": true, ".ed-h": true, ".ed-sec>h3": true, ".setp-title b": true, ".snip-h": true, ".cmp-h b": true}
	titleOK := map[string]bool{".page-head h1": true, ".mhead h1": true}
	headingEl := regexp.MustCompile(`(^|[\s>+~,])h[1-4]([^\w-]|$)`)
	for _, r := range rules {
		if r.sel == ":root" {
			for _, m := range decl.FindAllStringSubmatch(r.body, -1) {
				got[m[1]] = strings.TrimSpace(m[2])
			}
			continue
		}
		if strings.HasPrefix(r.sel, "@font-face") {
			continue
		}
		for _, m := range weight.FindAllStringSubmatch(r.body, -1) {
			v := strings.TrimSpace(m[1])
			if v == "inherit" { // a bold word inside a heading keeps the heading's weight
				continue
			}
			tok := strings.TrimSuffix(strings.TrimPrefix(v, "var("), ")")
			if _, ok := want[tok]; !ok || !strings.HasPrefix(v, "var(") {
				t.Errorf("%s %s: font-weight %q, want one of the weight tokens", r.at, r.sel, v)
				continue
			}
			if r.at == "" {
				bySel[r.sel] = tok
			}
			if tok == "--w-title" && !titleOK[r.sel] {
				t.Errorf("%s: the title weight on what is not the page title", r.sel)
			}
			if tok == "--w-head" && !headOK[r.sel] && !headingEl.MatchString(r.sel) {
				t.Errorf("%s: the heading weight on what is not a heading, title or tile figure", r.sel)
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf(":root %s = %q, want %s", k, got[k], v)
		}
	}
	for sel, tok := range map[string]string{"body": "--w-body", "td": "--w-body", "th": "--w-label", ".mono": "--w-num", "b,strong": "--w-strong",
		".btn": "--w-btn", ".lbl": "--w-label", ".ch h2": "--w-head", ".page-head h1": "--w-title", ".input.mono,.textarea.mono": "--w-num"} {
		if bySel[sel] != tok {
			t.Errorf("%s: font-weight %q, want var(%s)", sel, bySel[sel], tok)
		}
	}
	// the views write no weight as a number in an inline style either
	num := regexp.MustCompile(`font-weight\s*:\s*\d`)
	for _, dir := range []string{filepath.Join("ui", "next"), filepath.Join("ui", "core"), filepath.Join("ui", "views")} {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
				return err
			}
			b, _ := os.ReadFile(p)
			for i, ln := range strings.Split(string(b), "\n") {
				if num.MatchString(ln) {
					t.Errorf("%s:%d: a font-weight written as a number: %s", p, i+1, strings.TrimSpace(ln))
				}
			}
			return nil
		})
	}
}

// W1: the model's side panel, toned down (the owner's walkthrough, build-92;
// board walkthrough-fixes frame 1). One scroll container (the panel body);
// "Open the model's page" first; the conversation memory in one line that
// names no profile; no line of profiles under "Runs on"; the six facts in
// the quiet grid; the per-profile restore points in a column of the panel's
// and the page's Configurations tables (a profile with its own
// --ctx-checkpoints: its number, "own" beside it); "Make standard profiles"
// on the model's page, not in the panel.
func TestUINextModelPanelToneDown(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	cluster := map[string]any{"family": "recurrent", "cell_mib": 50.25, "checkpoints": 32, "checkpoints_mib": 1608, "prompt_cache_mib": 0, "on_node": false, "counted": true,
		"per_slot": 32, "checkpoints_from": "the cluster default", "prompt_cache_from": "the cluster default"}
	own := map[string]any{"family": "recurrent", "cell_mib": 50.25, "checkpoints": 3, "checkpoints_mib": 151, "prompt_cache_mib": 0, "on_node": false, "counted": true,
		"per_slot": 3, "checkpoints_from": "from this profile's extra arguments", "prompt_cache_from": "from this profile's extra arguments"}
	mixed := withHostMemory(t, fetch["/api/models"], mdlLoaded, cluster).(map[string]any)
	var first string
	var ids []string
	for _, m := range jlist(mixed["models"]) {
		if mm := jmap(m); jstr(mm["key"]) == mdlLoaded {
			ps := jlist(mm["profiles"])
			jmap(ps[0])["host_memory"] = own
			first = jstr(jmap(ps[0])["id"])
			for _, p := range ps {
				ids = append(ids, jstr(jmap(p)["id"]))
			}
		}
	}
	f := map[string]any{}
	for k, v := range fetch {
		f[k] = v
	}
	f["/api/models"] = mixed
	// the conversation line and the needs line name no profile
	var noIDs []string
	for _, id := range ids {
		noIDs = append(noIDs, `Conversation memory:[^\n.]*`+regexp.QuoteMeta(id))
	}
	panel := "#/models?model=" + mdlLoaded
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{
		map[string]any{"name": "W1: one scroll container, the panel body", "route": panel, "status": st,
			"pageHas":  []string{"aside.side .side-body table.mdl-ptbl", "aside.side .mdl-ptwrap table.mdl-ptbl"},
			"pageNone": []string{"aside.side .side-body .scroll", "aside.side .mdl-pscroll"}},
		map[string]any{"name": "W1: the conversation line is one line and names no profile", "route": panel, "status": st,
			"page":        []string{"Conversation memory: about 50 MB per restore point on the hosting machine · default 32 (1,608 MB) · 1 profile sets its own · counted in what it needs."},
			"notOnPageRe": noIDs, "pageCount": map[string]any{"aside.side [data-convmem]": 1}},
		map[string]any{"name": "W1: no line of profiles under Runs on", "route": panel, "status": st,
			"pageHas": []string{"aside.side [data-needs]"}, "pageNone": []string{"aside.side .mdl-need-ends"}},
		map[string]any{"name": "W1: the facts in the quiet grid", "route": panel, "status": st,
			"pageHas": []string{"aside.side dl.factgrid dt", "aside.side dl.factgrid dd"}, "pageNone": []string{"aside.side dl.facts"}},
		map[string]any{"name": "W1: Open the model's page first, no Make standard profiles in the panel", "route": panel, "status": st,
			"pageHas":  []string{"aside.side .mdl-acts a.btn[data-act=page][href='#/models/" + mdlLoaded + "']"},
			"pageSeq":  map[string]any{"aside.side .mdl-acts [data-act]": []string{"Open the model's page", "Benchmarks", "Pinned", "Delete model"}},
			"pageNone": []string{"aside.side [data-act=standard]"}, "notOnPage": []string{"Open the model page"}},
		map[string]any{"name": "W1: the panel's Restore points column", "route": panel, "status": st,
			"pageHas": []string{"aside.side th.r[title^=Restore]", "aside.side tr[data-profile='" + first + "'] td[data-restore-points='3'][data-own]"}},
		map[string]any{"name": "W1: the page's Restore points column and Make standard profiles", "route": "#/models/" + mdlLoaded, "status": st,
			"has": []string{"[data-restore-points='3'][data-own]", "button[data-act=standard]"}},
	}})
}

// W2: the Test profiles page (the owner's walkthrough, build-92; board
// walkthrough-fixes frame 2). Promote in the row is a small plain button;
// it asks twice and writes nothing before the second press (Demote on
// Profiles the same); a row opens the profile's side panel, the same one
// Profiles opens, with its config and Promote, Edit, Duplicate, Compare
// with… and Delete, one primary button; "Compare with…" reads two profiles
// and two dry plans and writes nothing.
func TestUINextTestProfilePanel(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--catalog"}, "/api/profiles/test", "/api/profiles", "/api/models", "/api/status")
	st := fetch["/api/status"]
	const tp, prod = "qwen3-14b-128k", "gemma-4-27b"
	for _, x := range jlist(jmap(fetch["/api/profiles"])["profiles"]) { // as the server gives every row: its unload when idle
		if jstr(jmap(x)["id"]) == tp {
			jmap(x)["ttl_s"] = 600
		}
	}
	fetch["/api/config/profiles/"+tp] = map[string]any{"revision": 412, "id": tp, "profile": map[string]any{"kind": "llama", "model": "qwen3-14b-Q5_K_M.gguf", "ctx": 131072,
		"stage": "test", "extra_args": []string{"--ctx-checkpoints", "3", "--jinja", "--temp=0.7"}}}
	fetch["/api/config/profiles/"+prod] = map[string]any{"revision": 412, "id": prod, "profile": map[string]any{"kind": "llama", "model": "gemma-4-27b-it-Q4_K_M.gguf", "ctx": 131072,
		"display_name": "Gemma chat", "extra_args": []string{"--ctx-checkpoints=8", "--jinja", "--mlock"}}}
	fetch["POST /api/plan"] = map[string]any{"$match": []any{
		[]any{`"model":"` + tp + `"`, map[string]any{"action": "start", "need_mib": 9216, "need_basis": "estimated from the file size"}},
		[]any{`"model":"` + prod + `"`, map[string]any{"action": "serve", "need_mib": 18432, "need_basis": "measured at a load"}}}}
	page := "#/test-profiles?q=" + tp
	open := page + "&profile=" + tp
	writes := []string{`/promote`, `/demote`, `^PUT `, `^DELETE `}
	promote := []any{"tr[data-profile=" + tp + "] button[data-act=promote]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "it works"}, "div.dlg-foot button.pri"}
	compare := []any{"aside.side button[data-act=compare]", map[string]any{"type": "input[data-cmp-search]", "value": prod}, "div.dlg button[data-pick=" + prod + "]"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": fetch, "cases": []any{
		map[string]any{"name": "W2: Promote in the row is a small plain button", "route": page, "status": st,
			"has": []string{"tr[data-profile=" + tp + "] button.btn.sm.plain[data-act=promote]"}, "none": []string{"tr button.pri"}},
		map[string]any{"name": "W2: step 1 says what it does, takes the comment, Review", "route": page, "status": st,
			"click":   []any{"tr[data-profile=" + tp + "] button[data-act=promote]"},
			"pageHas": []string{"div.dlg [data-dlg=promote] [data-after]", "div.dlg-foot button.pri[disabled]", "textarea[aria-label=Comment]"},
			"pageSeq": map[string]any{"div.dlg-foot button": []string{"Cancel", "Review"}}, "notFetched": writes},
		map[string]any{"name": "W2: step 2 asks Promote <name>? and nothing is written yet", "route": page, "status": st,
			"click":   promote,
			"pageHas": []string{"div.dlg [data-dlg=promote-confirm]", "div.dlg [data-comment]"}, "page": []string{"Promote Qwen 128k trial?", "it works"},
			"pageSeq": map[string]any{"div.dlg-foot button": []string{"Back", "Promote"}}, "notFetched": writes},
		map[string]any{"name": "W2: Back returns to step 1, still nothing written", "route": page, "status": st,
			"click":   append(append([]any{}, promote...), "div.dlg-foot button"),
			"pageHas": []string{"div.dlg [data-dlg=promote]"}, "pageNone": []string{"div.dlg [data-dlg=promote-confirm]"}, "notFetched": writes},
		map[string]any{"name": "W2: the row's Promote opens no panel", "route": page, "status": st,
			"click": []any{"tr[data-profile=" + tp + "] button[data-act=promote]"}, "pageNone": []string{"aside.side"}},
		map[string]any{"name": "W2: a row click opens the profile's side panel", "route": page, "status": st,
			"click": []any{"tr[data-profile=" + tp + "]"}, "hash": open,
			"pageHas": []string{"aside.side [data-shown]", "aside.side [data-stage=test]", "aside.side [data-extra-args]", "aside.side [data-ttl]",
				"aside.side a[data-act=edit]", "aside.side a[data-act=copy]", "aside.side button[data-act=promote]", "aside.side button[data-act=compare]", "aside.side button[data-act=delete]"},
			"page":      []string{"Test profile", "Model file", "Context", "Extra arguments", "--ctx-checkpoints 3 --jinja --temp=0.7", "Unload when idle", "Stage", "Placement", "Where it would go now"},
			"pageCount": map[string]any{"aside.side .btn.pri": 1}, "notFetched": writes},
		map[string]any{"name": "W2: the panel's Promote asks twice too", "route": open, "status": st,
			"click":   []any{"aside.side button[data-act=promote]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "ok"}, "div.dlg-foot button.pri"},
			"pageHas": []string{"div.dlg [data-dlg=promote-confirm]"}, "notFetched": writes},
		map[string]any{"name": "W2: Compare with: any profile, the differences only", "route": open, "status": st,
			"click": compare,
			"pageHas": []string{"table.cmp-tbl[data-compare=" + prod + "] tr[data-need]",
				"table.cmp-tbl tr[data-key=model][data-diff=diff]", "table.cmp-tbl tr[data-key=stage][data-only=a]", "table.cmp-tbl tr[data-key=display_name][data-only=b]",
				"table.cmp-tbl tr[data-flag=--ctx-checkpoints][data-diff=diff]", "table.cmp-tbl tr[data-flag=--temp][data-only=a]", "table.cmp-tbl tr[data-flag=--mlock][data-only=b]"},
			"pageNone": []string{"table.cmp-tbl tr[data-key=kind]", "table.cmp-tbl tr[data-key=ctx]", "table.cmp-tbl tr[data-flag=--jinja]", "table.cmp-tbl tr[data-diff=same]"},
			"page":     []string{"Setting", "What it needs (the plan now)", "Show all settings", "6 settings differ"},
			"fetchedQuery": []string{`^GET /api/config/profiles/` + tp + `$`, `^GET /api/config/profiles/` + prod + `$`,
				`^POST /api/plan \{"model":"` + tp + `"\}$`, `^POST /api/plan \{"model":"` + prod + `"\}$`},
			"notFetched": writes},
		map[string]any{"name": "W2: Show all settings shows the same ones too", "route": open, "status": st,
			"click":      append(append([]any{}, compare...), "div.dlg button.switch"),
			"pageHas":    []string{"table.cmp-tbl tr[data-key=kind][data-diff=same]", "table.cmp-tbl tr[data-key=ctx][data-diff=same]", "table.cmp-tbl tr[data-flag=--jinja][data-diff=same]"},
			"notFetched": writes},
		map[string]any{"name": "W2: Compare picks a test profile too", "route": "#/profiles?profile=" + prod, "status": st,
			"click":   []any{"aside.side button[data-act=compare]", map[string]any{"type": "input[data-cmp-search]", "value": tp}},
			"pageHas": []string{"div.dlg button[data-pick=" + tp + "]"}},
		map[string]any{"name": "W2: Demote asks twice, nothing before the second press", "route": "#/profiles?profile=" + prod, "status": st,
			"click":   []any{"aside.side button[data-act=demote]", map[string]any{"type": "textarea[aria-label=Comment]", "value": "trial"}, "div.dlg-foot button.pri"},
			"pageHas": []string{"div.dlg [data-dlg=demote-confirm]"}, "page": []string{"Demote Gemma chat?"}, "notFetched": writes},
	}})
}

// W2: the one pure function behind "Compare with…" (core/profilediff.js):
// a same value, a different value, a flag on one side only, a flag with a
// different value ("--x=8" against "--x 3"), a setting absent on one side.
func TestUINextProfileDiff(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	mod := "file:///" + strings.TrimPrefix(filepath.ToSlash(filepath.Join(filepath.Dir(here), "ui", "next", "core", "profilediff.js")), "/")
	script := `import { profileDiff, splitFlags } from '` + mod + `';
const a = {model: 'm.gguf', ctx: 8192, parallel: 2, extra_args: ['--ctx-checkpoints', '3', '--jinja', '--temp=0.7', '--n-cpu-moe', '4', '--bias', '-1']};
const b = {ctx: 16384, model: 'm.gguf', aliases: ['x'], extra_args: ['--ctx-checkpoints=8', '--jinja', '--mlock', '--n-cpu-moe', '4']};
console.log(JSON.stringify({rows: profileDiff(a, b, ['model', 'ctx']), none: profileDiff({}, {}), flags: splitFlags(a.extra_args)}));`
	out, err := exec.Command(node, "--input-type=module", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	type row struct {
		Key  string `json:"key"`
		A    any    `json:"a"`
		B    any    `json:"b"`
		Same bool   `json:"same"`
		Only string `json:"only"`
	}
	var got struct {
		Rows  []row   `json:"rows"`
		None  []row   `json:"none"`
		Flags [][]any `json:"flags"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	by := map[string]row{}
	var keys []string
	for _, r := range got.Rows {
		by[r.Key] = r
		keys = append(keys, r.Key)
	}
	want := []struct {
		key  string
		a, b any
		same bool
		only string
	}{
		{"model", "m.gguf", "m.gguf", true, ""},               // the same value
		{"ctx", 8192.0, 16384.0, false, ""},                   // a different value
		{"parallel", 2.0, nil, false, "a"},                    // a setting absent on one side
		{"aliases", nil, []any{"x"}, false, "b"},              // and on the other
		{"extra_args --ctx-checkpoints", "3", "8", false, ""}, // a flag with a different value, either spelling
		{"extra_args --jinja", true, true, true, ""},          // a flag on both sides
		{"extra_args --temp", "0.7", nil, false, "a"},         // a flag on one side only
		{"extra_args --n-cpu-moe", "4", "4", true, ""},        // a flag with the same value
		{"extra_args --bias", "-1", nil, false, "a"},          // a negative number is a value, not a flag
		{"extra_args --mlock", nil, true, false, "b"},         // a flag on the other side only
	}
	for _, w := range want {
		r, ok := by[w.key]
		if !ok {
			t.Errorf("no row %q in %v", w.key, keys)
			continue
		}
		wa, _ := json.Marshal(w.a)
		wb, _ := json.Marshal(w.b)
		ga, _ := json.Marshal(r.A)
		gb, _ := json.Marshal(r.B)
		if string(wa) != string(ga) || string(wb) != string(gb) || r.Same != w.same || r.Only != w.only {
			t.Errorf("%s: got a=%s b=%s same=%v only=%q, want a=%s b=%s same=%v only=%q", w.key, ga, gb, r.Same, r.Only, wa, wb, w.same, w.only)
		}
	}
	if len(got.Rows) != len(want) {
		t.Errorf("rows %v, want %d", keys, len(want))
	}
	if _, ok := by["extra_args"]; ok {
		t.Error("extra_args is compared as one row, not flag by flag")
	}
	if len(keys) < 2 || keys[0] != "model" || keys[1] != "ctx" {
		t.Errorf("the given order first: %v", keys)
	}
	if len(got.None) != 0 {
		t.Errorf("two empty profiles: %v", got.None)
	}
	if len(got.Flags) != 5 {
		t.Errorf("splitFlags: %v", got.Flags)
	}
}
