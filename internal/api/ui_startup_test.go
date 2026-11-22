package api

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

// The start-up figures in the web UI: the words, the sentence about what is
// not included, and kinds never put together. The UI has no JavaScript test
// runner, so these tests read the embedded files.

func TestUIStartupWording(t *testing.T) {
	core := uiText(t, "core/startup.js")
	m := regexp.MustCompile(`export const NOT_INCLUDED = '([^']+)';`).FindStringSubmatch(core)
	if m == nil || m[1] != StartupNotIncluded {
		t.Fatalf("core/startup.js NOT_INCLUDED is not the server's sentence %q: %v", StartupNotIncluded, m)
	}
	for _, s := range []string{"cannot be seen from inside Linux", "is not included", "power-on"} {
		if !strings.Contains(StartupNotIncluded, s) {
			t.Errorf("the not-included sentence lacks %q", s)
		}
	}
	for _, want := range []string{
		"`Ready for model loads ${secs(last.headline_s)} after boot start`",
		`full: 'full start',`, `soft: 'soft restart',`, `agent_restart: 'agent restart',`,
		`'Not ready for model loads yet'`, `'No start-up figure'`,
		`registered: 'the agent has registered',`, `programs_ready: 'its programs are ready',`, `share_mounted: 'the model share is reachable',`,
		`'The machine kept running, so there is no start-up figure.'`,
		`Requested with expect-restart`,
		// a partial figure is never shown: the figure only when the server has one
		`if (last.headline_s != null) {`,
		// where the figure counts from (the record's origin)
		`const from = originText(last.origin);`,
		`userspace: 'counted from the userspace start; the shutdown before it is not included',`,
		`boot_origin: 'Start began',`,
	} {
		if !strings.Contains(core, want) {
			t.Errorf("core/startup.js lacks %s", want)
		}
	}
	nodes := uiText(t, "views/nodes.js")
	for _, want := range []string{
		`agent && agent.last_startup ? startupPart(n, agent.last_startup, refresh) : null`,
		"h('b', t.head), t.kind ? ` (${t.kind})` : '', ', ', ago(last.at)",
		`more(open, toggle, 'Marks', 'Hide marks')`,
		`(rec.marks_in_order || []).map(`,
		`open ? h('span.mut', NOT_INCLUDED) : null`,
		`lastRecord(n.id, last.kind)`,
	} {
		if !strings.Contains(nodes, want) {
			t.Errorf("views/nodes.js lacks %s", want)
		}
	}
	bench := uiText(t, "views/benchmarks/startups.js")
	for _, want := range []string{
		`h('h2', 'Start-up')`, `NOT_INCLUDED`, `key: g => g.node_id + ':' + g.kind`,
		`{ label: 'Median'`, `{ label: 'Worst'`, `{ label: 'Last'`, `{ label: 'Starts'`,
		`expect-restart`, `g.mixed_origins ? h('span.sub', 'counted from different origins')`,
	} {
		if !strings.Contains(bench, want) {
			t.Errorf("views/benchmarks/startups.js lacks %s", want)
		}
	}
	if !strings.Contains(uiText(t, "views/benchmarks/overview.js"), "startupsCard(),") {
		t.Error("the Benchmarks overview has no Start-up section")
	}
}

// Kinds are never put into one figure: the page computes no figure of its
// own (the server's aggregates are per node and kind), and a node's marks
// are looked up by node and kind.
func TestUIStartupKindsNeverMixed(t *testing.T) {
	core := uiText(t, "core/startup.js")
	if !strings.Contains(core, `x.node_id === nodeId && x.kind === kind`) {
		t.Error("core/startup.js looks a record up without its kind")
	}
	for _, name := range []string{"core/startup.js", "views/benchmarks/startups.js", "views/nodes.js"} {
		src := uiText(t, name)
		if regexp.MustCompile(`headline_s\s*[+\-*/]|\.reduce\([^)]*(last_s|median_s|worst_s|headline_s)`).MatchString(src) {
			t.Errorf("%s computes a start-up figure itself", name)
		}
	}
	// Only core/startup.js reads the records, only GET /api/startups, and no
	// file starts an expect-restart: that is an operator route, not a page action.
	for _, name := range uiFiles(t) {
		if path.Ext(name) != ".js" {
			continue
		}
		src := uiText(t, name)
		if name != "core/startup.js" && strings.Contains(src, "'/api/startups") {
			t.Errorf("%s reads /api/startups: it goes through core/startup.js", name)
		}
		if strings.Contains(src, "/expect-restart") {
			t.Errorf("%s calls expect-restart", name)
		}
	}
}
