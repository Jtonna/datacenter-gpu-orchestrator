package api

import (
	"encoding/json"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
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
var nextRoutes = []string{"overview", "requests", "report", "zones", "nodes", "models", "queue", "activity", "placement", "benchmarks", "benchmarks/runs", "benchmarks/trends",
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
	const active, paused, excluded, named = "02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03", "02:00:00:00:00:04"
	var conf *config.Config
	fe, p, _ := feedEnv(t, func(c *config.Config) { c.ExcludeNodes = []string{excluded}; c.NoAutoNodes = []string{named}; conf = c })
	for _, id := range []string{active, paused, excluded, named} {
		register(t, fe, id)
		feedHeartbeat(t, p, id, "")
	}
	if _, err := p.SetPaused(paused, true); err != nil {
		t.Fatal(err)
	}
	st := statusWithAgentNodes(t, fe, func() *config.Config { return conf })
	if s, _ := json.Marshal(st); !strings.Contains(string(s), `"paused":true`) || !strings.Contains(string(s), `"excluded_by":"`+excluded+`"`) || !strings.Contains(string(s), `"auto":false`) {
		t.Fatalf("the status lacks the paused, the excluded or the named-only machine: %s", s)
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
		"zones": {"ALL 5 ZONES", "2 off", "Switched off", "console-1", "In use, darker is busier", "Page 1 of 1"},
	})
	cluster = append(cluster, map[string]any{"name": "agents: zones, one zone opened", "route": "#/zones?zone=" + active, "status": st, "absent": designSamples},
		// the reason a zone is off: the exclude_nodes entry that matches it
		map[string]any{"name": "agents: zones, only the off ones", "route": "#/zones?show=off", "status": st,
			"expect": []string{"Switched off"}, "titles": []string{"exclude_nodes: " + excluded}, "absent": []string{"host-1"}})
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": fe.front.URL, "cases": cluster})

	be := benchEnv(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": be.front.URL, "cases": every("profiles", "fetch", nil)})

	empty, _, _ := feedEnv(t)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": empty.front.URL,
		"cases": every("empty", "fetch", map[string][]string{"zones": {"Create new zone", "Page 1 of 1"}})})
}

// The Zones page's selection and its zone panel, on a real agent cluster:
// the panel of one zone shows its facts, its recent events (GET /api/history
// for that node) and the machine card with its two controls; ticking zones
// shows the selection bar; its "Stop accepting models" asks for a comment
// first and counts only the zones it would change (one is paused already;
// the scheduler's own machine has no agent and cannot be selected).
func TestUINextZonesSelectionAndPanel(t *testing.T) {
	const active, paused, excluded, named = "02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03", "02:00:00:00:00:04"
	var conf *config.Config
	fe, p, _ := feedEnv(t, func(c *config.Config) { c.ExcludeNodes = []string{excluded}; c.NoAutoNodes = []string{named}; conf = c })
	for _, id := range []string{active, paused, excluded, named} {
		register(t, fe, id)
		feedHeartbeat(t, p, id, "")
	}
	if _, err := p.SetPaused(paused, true); err != nil {
		t.Fatal(err)
	}
	st := statusWithAgentNodes(t, fe, func() *config.Config { return conf })
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": fe.front.URL, "cases": []any{
		// this scheduler keeps no history: the panel says so
		map[string]any{"name": "one zone's panel", "route": "#/zones?zone=" + active, "status": st, "absent": designSamples,
			"page": []string{"Platform", "linux-amd64-vulkan", "Agent release", "0.1.0", "Recent events", "The event history is off", "Pause", "Force unload"}},
		map[string]any{"name": "the paused zone's panel", "route": "#/zones?zone=" + paused, "status": st,
			"page": []string{"Switched off", "Unpause"}},
		map[string]any{"name": "the excluded zone's panel", "route": "#/zones?zone=" + excluded, "status": st,
			"page": []string{"Switched off"}, "titles": []string{"exclude_nodes: " + excluded}},
		map[string]any{"name": "a selection", "route": "#/zones", "status": st, "click": []string{"input[data-sel=all]"},
			"expect": []string{"4 selected", "Stop accepting models", "Accept models again", "Clear"}},
		map[string]any{"name": "the bulk pause asks first", "route": "#/zones", "status": st, "click": []string{"input[data-sel=all]", "button[data-bulk=pause]"},
			"page": []string{"Stop accepting models on 3 zones?", "Comment (required)"}},
	}})

	// A scheduler that keeps its history: the panel lists the events that name
	// the zone (and only those), newest first.
	hs := openStore(t)
	now := time.Now().UnixMilli()
	hs.Record(history.Event{Ts: now - 9000, Type: "loadFinished", Severity: "info", Profile: "alpha", Nodes: []string{"host-1"}, Summary: "alpha finished loading in 12.0 s.", Data: map[string]any{"outcome": "ready", "load_s": 12.0}})
	hs.Record(history.Event{Ts: now - 5000, Type: "nodeQuarantined", Severity: "warn", Nodes: []string{"host-1"}, Summary: "host-1 set aside for 60 s after a failed pre-load check.", Data: map[string]any{"for_s": 60.0}})
	hs.Record(history.Event{Ts: now - 4000, Type: "loadStarted", Severity: "info", Profile: "beta", Nodes: []string{"other-node"}, Summary: "Loading beta onto other-node."})
	he := histEnv(t, hs)
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": he.front.URL, "cases": []any{
		map[string]any{"name": "a zone's events", "route": "#/zones?zone=host-1", "status": "fetch",
			"page":      []string{"alpha finished loading in 12.0 s.", "host-1 set aside for 60 s after a failed pre-load check.", "All events on Activity"},
			"notOnPage": []string{"Loading beta onto other-node."}},
	}})

	// On an agent cluster a zone's id is its MAC and its events name its
	// hostname: the panel still lists them (B1).
	ah := openStore(t)
	var aconf *config.Config
	ae, ap, _ := feedEnvOpts(t, func(o *Options) { o.History = ah }, func(c *config.Config) { aconf = c })
	register(t, ae, active) // named console-1
	feedHeartbeat(t, ap, active, "")
	ah.Record(history.Event{Ts: now - 3000, Type: "loadFinished", Severity: "info", Profile: "alpha", Nodes: []string{"console-1"}, Summary: "alpha finished loading on console-1.", Data: map[string]any{"outcome": "ready", "load_s": 9.0}})
	ah.Record(history.Event{Ts: now - 2000, Type: "loadStarted", Severity: "info", Profile: "beta", Nodes: []string{"console-7"}, Summary: "Loading beta onto console-7."})
	ast := statusWithAgentNodes(t, ae, func() *config.Config { return aconf })
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": ae.front.URL, "cases": []any{
		map[string]any{"name": "an agent zone's events, asked by id", "route": "#/zones?zone=" + active, "status": ast,
			"page": []string{"alpha finished loading on console-1."}, "notOnPage": []string{"Loading beta onto console-7.", "No events"}},
	}})
}

// The Models list and its side panel, on a real scheduler's catalog: every
// model file is a row with its name; ?model=<key> opens its panel with the
// file, its hand-written configurations and the link to its full page; a key
// that is not in the catalog says so; and #/models/<key> is still the full
// model page. No Pin, Delete or Add control is drawn (no backend yet).
func TestUINextModelsListAndPanel(t *testing.T) {
	e := benchEnv(t)
	var cat struct {
		Models []struct {
			Key, Name, File, Kind string
			Profiles              []struct {
				ID        string `json:"id"`
				Generated bool   `json:"generated"`
			} `json:"profiles"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/models"))), &cat); err != nil {
		t.Fatal(err)
	}
	var key, name, file, profile string
	var names []string
	for _, m := range cat.Models {
		names = append(names, m.Name)
		if key == "" && m.Kind == "llama" {
			for _, p := range m.Profiles {
				if !p.Generated {
					key, name, file, profile = m.Key, m.Name, m.File, p.ID
					break
				}
			}
		}
	}
	if key == "" {
		t.Fatalf("no model file with a hand-written profile in the test catalog: %+v", cat.Models)
	}
	noControls := []string{"Pin", "Delete", "Add a model", "Remove now", "Unload"}
	uiExec(t, map[string]any{"shell": "next", "app": true, "base": e.front.URL, "cases": []any{
		map[string]any{"name": "the list", "route": "#/models", "status": "fetch", "expect": append([]string{"All models", "Configurations", "Last used"}, names...),
			"absent": append(append([]string{}, designSamples...), noControls...)},
		map[string]any{"name": "one model's panel", "route": "#/models?model=" + key, "status": "fetch",
			"page": []string{name, file, "Configurations", profile, "Edit under Profiles", "Open the model page"}, "notOnPage": noControls},
		map[string]any{"name": "a model that is not there", "route": "#/models?model=no-such-model.gguf", "status": "fetch",
			"page": []string{"This model is not in the catalog"}},
		map[string]any{"name": "the full model page", "route": "#/models/" + key, "status": "fetch", "expect": []string{name}},
	}})
}

// Each UI has a visible link to the other, so the owner switches without
// typing an address: "Classic UI" in the new shell's Settings menu (the
// gear, Q73), "New UI" in the classic UI's header and navigation pane. Both
// are drawn before any status arrives.
func TestUISwitchLinks(t *testing.T) {
	for _, c := range []struct{ shell, text, path string }{{"next", "Classic UI", "/ui/classic/"}, {"classic", "New UI", "/ui/"}} {
		cases := []any{}
		for _, r := range []string{"models", "zones", "about"} {
			if r == "zones" && c.shell == "classic" {
				r = "nodes"
			}
			if c.shell == "next" && r != "about" {
				r = map[string]string{"models": "profiles", "zones": "audit"}[r]
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
