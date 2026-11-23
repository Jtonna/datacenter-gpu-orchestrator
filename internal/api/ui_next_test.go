package api

import (
	"encoding/json"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// The new shell (UI rewrite, phase U1) lives in ui/next/ and is served at
// /ui/ over the classic files (uifiles.go). These tests cover it; the
// classic UI keeps its own tests (ui_test.go, uiexec_test.go and the rest),
// which read the classic files.

// exportsOf lists the names a module exports.
func exportsOf(src string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^export\s+(?:async\s+)?(?:function\*?|const|let|var|class)\s+([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	for _, m := range regexp.MustCompile(`(?m)^export\s*\{([^}]*)\}`).FindAllStringSubmatch(src, -1) {
		for _, part := range strings.Split(m[1], ",") {
			f := strings.Fields(part)
			if len(f) > 0 {
				out = append(out, f[len(f)-1])
			}
		}
	}
	sort.Strings(out)
	return out
}

// A module of ui/next/ that replaces a classic module of the same path
// exports every name the classic one does: the classic views run on the new
// shell's modules (they import ../core/ui.js and so on, which below /ui/ are
// the next/ files), and the benchmark views are mounted unchanged.
func TestUINextKeepsTheClassicExports(t *testing.T) {
	n := 0
	for _, name := range uiFiles(t) {
		rel, ok := strings.CutPrefix(name, uiNextDir)
		if !ok || path.Ext(rel) != ".js" || rel == "app.js" {
			continue
		}
		if _, err := fs.Stat(uiFS, rel); err != nil {
			continue // a module of its own
		}
		n++
		have := map[string]bool{}
		for _, e := range exportsOf(uiText(t, name)) {
			have[e] = true
		}
		for _, e := range exportsOf(uiText(t, rel)) {
			if !have[e] {
				t.Errorf("%s does not export %s, which the classic %s does: a view that imports it would break below /ui/", name, e, rel)
			}
		}
	}
	if n < 2 {
		t.Fatalf("only %d next/ modules replace a classic one; core/ui.js and core/shell.js should", n)
	}
}

// nextRoutes are the screens of the new shell: every page of its navigation.
var nextRoutes = []string{"zones", "nodes", "models", "queue", "activity", "placement", "benchmarks", "benchmarks/runs", "benchmarks/trends",
	"profiles", "tiers", "settings", "audit", "mcp", "about"}

// Sample strings of the design's artboards: none of them may ever be drawn
// (no sample data, plan section 5).
var designSamples = []string{"rack-a", "ra-36", "ptl_", "Home Slack", "gemma-4-27b:32k", "Oberon"}

// The new shell starts the way index.html starts it (app.js: the rail, the
// section menu, the router, the live channel) and every screen draws with no
// status yet and no answer from the server.
func TestUINextAllModulesLoad(t *testing.T) {
	app := uiText(t, uiNextDir+"app.js")
	for _, want := range []string{"import './views/zones.js';", "startShell(", "startRouter(", "startLive("} {
		if !strings.Contains(app, want) {
			t.Errorf("next/app.js lacks %s", want)
		}
	}
	cases := []any{}
	for _, r := range nextRoutes {
		cases = append(cases, map[string]any{"name": r + " with no status yet", "route": "#/" + r, "absent": designSamples})
	}
	uiExec(t, map[string]any{"shell": "next", "app": true, "cases": cases, "fetch": map[string]any{}})
}

// Every screen of the new shell draws from a real server's answers, three
// times: a cluster of agent machines (one active, one paused, one excluded,
// and the scheduler's own machine), a scheduler with profiles and the
// benchmark store, and an empty scheduler (no agents, no profiles, no runs).
func TestUINextScreensExecute(t *testing.T) {
	const active, paused, excluded = "02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03"
	var conf *config.Config
	fe, p, _ := feedEnv(t, func(c *config.Config) { c.ExcludeNodes = []string{excluded}; conf = c })
	for _, id := range []string{active, paused, excluded} {
		register(t, fe, id)
		feedHeartbeat(t, p, id, "")
	}
	if _, err := p.SetPaused(paused, true); err != nil {
		t.Fatal(err)
	}
	st := statusWithAgentNodes(t, fe, func() *config.Config { return conf })
	if s, _ := json.Marshal(st); !strings.Contains(string(s), `"paused":true`) || !strings.Contains(string(s), `"excluded_by":"`+excluded+`"`) {
		t.Fatalf("the status lacks the paused or the excluded machine: %s", s)
	}
	every := func(label string, status any, extra map[string][]string) []any {
		out := []any{}
		for _, r := range nextRoutes {
			c := map[string]any{"name": label + ": " + r, "route": "#/" + r, "status": status, "absent": designSamples}
			if x, ok := extra[r]; ok {
				c["expect"] = x
			}
			out = append(out, c)
		}
		return out
	}
	// the zones of the agent cluster, and the side panel of one of them
	cluster := every("agents", st, map[string][]string{
		"zones": {"Zones", "All zones", "1 off", "Off", "Not accepting", "console-1", "Colour key", "Showing 4 of 4"},
	})
	cluster = append(cluster, map[string]any{"name": "agents: zones, one zone opened", "route": "#/zones?zone=" + active, "status": st, "absent": designSamples},
		map[string]any{"name": "agents: zones, only the off ones", "route": "#/zones?show=off", "status": st, "expect": []string{"Showing 1 of 4", "Switched off in the configuration"}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": fe.front.URL, "cases": cluster})

	be := benchEnv(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": be.front.URL, "cases": every("profiles", "fetch", nil)})

	empty, _, _ := feedEnv(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": empty.front.URL,
		"cases": every("empty", "fetch", map[string][]string{"zones": {"Zones"}})})
}

// Each UI has a visible link to the other, so the owner switches without
// typing an address: "Classic UI" at the foot of the new shell's rail,
// "New UI" in the classic UI's header and navigation pane. Both are drawn
// before any status arrives.
func TestUISwitchLinks(t *testing.T) {
	for _, c := range []struct{ shell, text, path string }{{"next", "Classic UI", "/ui/classic/"}, {"classic", "New UI", "/ui/"}} {
		cases := []any{}
		for _, r := range []string{"models", "zones", "about"} {
			if r == "zones" && c.shell == "classic" {
				r = "nodes"
			}
			cases = append(cases, map[string]any{"name": c.shell + ": " + r, "route": "#/" + r,
				"links": []map[string]string{{"text": c.text, "path": c.path}}})
		}
		uiExec(t, map[string]any{"shell": c.shell, "app": true, "cases": cases, "fetch": map[string]any{}})
	}
}

// No UI file can send a machine a reboot or a restart: no route of that kind
// is named anywhere (plan D9, D10; a PS5 must never get a normal reboot).
func TestUINoRebootOrRestartControl(t *testing.T) {
	bad := regexp.MustCompile("['\"`][^'\"`\\n]*/(reboot|restart|soft-reboot|expect-restart)\\b")
	for _, name := range uiFiles(t) {
		if path.Ext(name) != ".js" && path.Ext(name) != ".html" {
			continue
		}
		if m := bad.FindString(uiText(t, name)); m != "" {
			t.Errorf("%s names %s: the UI must not be able to reboot or restart a machine", name, m)
		}
	}
}
