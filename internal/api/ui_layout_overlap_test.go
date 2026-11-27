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

// v88 was refused (26 Nov): on the Models list a program's Needs cell read
// "~5.3 GB, set by hand" with "Runs only on host-1, which is excluded" under
// it, wider than its column, painted over Profiles and Last used; a file with
// one written need ran into its neighbour ("set by hand1 profile"). The fake
// DOM of ui-exec has no layout, so this measures the pages in a real headless
// Chrome (scripts/ui-layout.mjs): in every table row, the drawn text of each
// cell against the drawn text of every other cell of the row. Two text
// rectangles of different cells that intersect, or that share a line with
// less than 4 px between them, fail (OVERLAP); so does a page wider than the
// window (SIDEWAYS), text past its row (PAST), and from 720 px on, text past
// the card the row is seen through (CUT). The fixture: --models written (three
// programs whose plans reject with the long exclusion reason and whose need is
// written, a file with one written need, the range rows) and --long-names
// (Requests rows with live's longest names). Skipped without Chrome (UI_BROWSER
// or the usual install paths) and with -short.
func TestUILayoutNoOverlap(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: no browser")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	get, base := fixtureStart(t, "--models", "written", "--long-names")
	var models any
	get("/api/models", &models) // the server is up
	mdl := []string{".mdl .mr:not(.mr-h)"}
	cases := []map[string]any{
		{"name": "models", "route": "#/models", "rows": mdl, "minRows": 9},
		{"name": "models-panel-program", "route": "#/models?model=command:image-gen-fast", "rows": mdl},
		{"name": "models-panel-file", "route": "#/models?model=tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf", "rows": mdl},
		{"name": "profiles", "route": "#/profiles", "minRows": 10},
		{"name": "profile-program", "route": "#/profiles?profile=image-gen-fast"},
		{"name": "requests", "route": "#/requests", "menu": "open", "widths": []int{1536, 1280}, "minRows": 10},
		{"name": "requests", "route": "#/requests", "menu": "closed", "minRows": 10},
	}
	for _, c := range cases {
		c["base"] = base
	}
	// build-92 (the lead's look at 390 px): the profile panel's facts are never
	// cut and every button of its foot is in the window; "Compare with…" stacks
	// its settings at 390, breaks no word and scrolls nothing sideways; a
	// button's label is the sans face everywhere, also in a figure cell. These
	// run on the profiles fixture (glm-4.5-air carries a long extra argument
	// list), each check throwing what it found.
	_, base2 := fixtureStart(t)
	panel := "#/profiles?profile=glm-4.5-air"
	cases = append(cases,
		map[string]any{"base": base2, "name": "profile-panel-narrow", "route": panel, "widths": []int{390, 1280}, "js": layoutJSPanel},
		map[string]any{"base": base2, "name": "compare-narrow", "route": panel, "widths": []int{390, 1280}, "js": layoutJSCompare},
		map[string]any{"base": base2, "name": "test-profiles-button-faces", "route": "#/test-profiles", "widths": []int{1536}, "js": layoutJSFaces},
	)
	b, _ := json.Marshal(cases)
	list := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(list, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, here, _, _ := runtime.Caller(0)
	cmd := exec.Command(node, filepath.Join(filepath.Dir(here), "..", "..", "scripts", "ui-layout.mjs"), list)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 3 {
		t.Skip("no Chrome: " + strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("layout: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// the shared part of the build-92 checks: wait for a selector, the text
// rectangles of an element, the button faces
const layoutJSHead = `
const sleep = ms => new Promise(r => setTimeout(r, ms));
const until = async sel => { for (let i = 0; i < 60 && !document.querySelector(sel); i++) await sleep(100); return document.querySelector(sel); };
const errs = [];
const W = innerWidth;
const rects = el => { const out = []; const w = document.createTreeWalker(el, NodeFilter.SHOW_TEXT); let n; while ((n = w.nextNode())) { if (!n.textContent.trim()) continue; const r = document.createRange(); r.selectNodeContents(n); out.push(...[...r.getClientRects()].filter(x => x.width > 0)); } return out; };
const clipped = el => { const cs = getComputedStyle(el); return cs.overflowX !== 'visible' && el.scrollWidth > el.clientWidth + 1; };
const faces = root => root.querySelectorAll('button, .btn').forEach(b => { const r = b.getBoundingClientRect(); if (r.width && !/Manrope/.test(getComputedStyle(b).fontFamily)) errs.push('a button not in the sans face: ' + (b.textContent.trim() || b.getAttribute('aria-label')).slice(0, 30)); });
`

// the profile panel: no fact cut, every fact's text inside the panel, every
// button of the foot inside the window
const layoutJSPanel = layoutJSHead + `
await until('aside.side [data-extra-args]');
const side = document.querySelector('aside.side');
if (!side) throw new Error('no panel');
const box = side.getBoundingClientRect();
const dds = side.querySelectorAll('dl.prf-facts dd');
if (dds.length < 5) errs.push('only ' + dds.length + ' facts');
dds.forEach(dd => { [dd, ...dd.querySelectorAll('*')].forEach(el => { if (clipped(el)) errs.push('a fact is cut: ' + el.textContent.trim().slice(0, 40)); });
  rects(dd).forEach(r => { if (r.right > box.right + 1 || r.left < box.left - 1) errs.push('a fact past the panel: ' + dd.textContent.trim().slice(0, 40)); }); });
const acts = side.querySelectorAll('.sf button, .sf a.btn');
if (acts.length < 5) errs.push('only ' + acts.length + ' actions in the foot');
acts.forEach(b => { const r = b.getBoundingClientRect(); if (r.left < -1 || r.right > W + 1 || r.width === 0) errs.push('an action out of the window: ' + b.textContent.trim()); });
faces(side);
if (errs.length) throw new Error(errs.join(' | '));
`

// "Compare with…": any profile picked, every setting shown; the dialog inside
// the window, nothing in it scrolls sideways, no word of a label is broken,
// no "null" or "undefined", the buttons in the sans face
const layoutJSCompare = layoutJSHead + `
await until('aside.side [data-extra-args]');
document.querySelector('aside.side button[data-act=compare]').click();
(await until('div.dlg button[data-pick]')).click();
(await until('div.dlg table.cmp-tbl'));
const sw = document.querySelector('div.dlg button.switch'); if (sw) sw.click();
await sleep(300);
const dlg = document.querySelector('div.dlg');
const box = dlg.getBoundingClientRect();
if (box.left < -1 || box.right > W + 1) errs.push('the dialog is wider than the window');
if (/null|undefined/.test(dlg.textContent)) errs.push('the dialog prints null or undefined');
[dlg, ...dlg.querySelectorAll('*')].forEach(el => { if (clipped(el)) errs.push('scrolls or cuts sideways: ' + el.className + ' ' + el.textContent.trim().slice(0, 30)); });
dlg.querySelectorAll('table.cmp-tbl td').forEach(td => rects(td).forEach(r => { if (r.right > box.right + 1) errs.push('past the dialog: ' + td.textContent.trim().slice(0, 30)); }));
dlg.querySelectorAll('table.cmp-tbl td:first-child').forEach(td => { const w = document.createTreeWalker(td, NodeFilter.SHOW_TEXT); let n;
  while ((n = w.nextNode())) { const re = /\S+/g; let m; while ((m = re.exec(n.textContent))) { const r = document.createRange(); r.setStart(n, m.index); r.setEnd(n, m.index + m[0].length);
    const lines = new Set([...r.getClientRects()].filter(x => x.width > 0).map(x => Math.round(x.top))); if (lines.size > 1) errs.push('a word broken in a label: ' + m[0]); } } });
const rows = dlg.querySelectorAll('table.cmp-tbl tbody tr').length;
if (rows < 5) errs.push('only ' + rows + ' rows');
faces(dlg);
if (errs.length) throw new Error(errs.join(' | '));
`

// the Test profiles list: every button's label in the sans face (Promote sits in a figure cell)
const layoutJSFaces = layoutJSHead + `
await until('tr[data-profile] button[data-act=promote]');
faces(document);
if (errs.length) throw new Error(errs.join(' | '));
`
