package api

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The machine controls of the web UI (agents membership): the reason words in
// plain text and the two operator actions. The UI has no JavaScript test
// runner, so these tests read the embedded files and send the server the
// request the page builds.

// Every reason word a node may carry has a plain sentence in the UI.
func TestUIUnplacedReasonsAreMapped(t *testing.T) {
	text := uiText(t, "core/nodeactions.js")
	codes := []string{scheduler.UnplacedNoTierMatch, scheduler.UnplacedProgramsNotReady, scheduler.UnplacedModelSourceUnreachable,
		scheduler.UnplacedOrphanLaunch, scheduler.UnplacedSilent, "agent_unreachable", "host_machine", scheduler.UnplacedPaused,
		scheduler.UnplacedBadFloor}
	for _, c := range codes {
		m := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(c) + `: \{ short: '([^']+)',\s*text: '([^']+)' \},`).FindStringSubmatch(text)
		if m == nil {
			t.Errorf("core/nodeactions.js has no plain words for the reason %q", c)
			continue
		}
		for _, s := range m[1:] {
			if strings.Contains(s, "_") {
				t.Errorf("reason %q: %q repeats a raw word instead of saying it plainly", c, s)
			}
		}
		if !strings.HasSuffix(m[2], ".") {
			t.Errorf("reason %q: the sentence %q has no full stop", c, m[2])
		}
	}
	// an unknown word is shown as it is
	if !strings.Contains(text, "return { short: c, text: c, known: false, code: c };") {
		t.Error("core/nodeactions.js no longer shows an unknown reason word as it is")
	}
}

// The page has exactly two actions on a machine, pause / unpause and force
// unload; each goes through the REST helper with the comment, and nothing
// else in the UI writes to a node.
func TestUINodeControlsAreTheTwoActions(t *testing.T) {
	text := uiText(t, "core/nodeactions.js")
	for _, want := range []string{`verb: 'pause'`, `verb: 'unpause'`, `verb: 'force-unload'`,
		`'/api/nodes/' + encodeURIComponent(id) + '/' + a.verb`,
		`api.post(nodeActionPath(id, action), { comment: c }, { comment: c })`,
		`if (!c) return Promise.reject(`} {
		if !strings.Contains(text, want) {
			t.Errorf("core/nodeactions.js lacks %s", want)
		}
	}
	if n := len(regexp.MustCompile(`verb: '`).FindAllString(text, -1)); n != 3 {
		t.Errorf("core/nodeactions.js has %d node actions, want the 3 endpoints of the two controls", n)
	}
	for _, name := range uiFiles(t) {
		if path.Ext(name) != ".js" {
			continue
		}
		src := uiText(t, name)
		// the availability series is a read, not an action on a machine
		if name != "core/nodeactions.js" && strings.Contains(strings.ReplaceAll(src, "/api/nodes/availability", ""), "/api/nodes") {
			t.Errorf("%s names /api/nodes: node actions go through core/nodeactions.js", name)
		}
		if strings.Contains(src, "readmit") || regexp.MustCompile(`del\(\s*['"`+"`"+`]/api/nodes`).MatchString(src) {
			t.Errorf("%s removes or re-admits a node: the UI has only pause / unpause and force unload", name)
		}
	}
	view := uiText(t, "views/nodes.js")
	for _, want := range []string{
		`agent ? controls(n, agent, refresh) : null`, // no agent facts (fleet-feed mode, the local machine): no controls
		`sendNodeAction(api, id, action, c)`,
		`disabled: !!p || !holds`,                  // force unload needs a launch or an instance
		`Requests waiting on that model will fail`, // the confirm step says what breaks
		`A comment is required`,
		`'lg ' + (force ? 'dng solid' : 'pri')`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("views/nodes.js lacks %s", want)
		}
	}
	if !strings.Contains(text, `st.membership === 'agents' && Array.isArray(st.agents)`) {
		t.Error("core/nodeactions.js: agentsMode no longer needs membership agents and the agents list")
	}
}

// The server takes the request the page sends: the node id escaped in the
// path, X-Source and X-Actor from the REST helper, the comment in the body
// (kept exactly) and in X-Comment (ASCII only).
func TestUINodeRequestIsAcceptedAndAudited(t *testing.T) {
	e, p, au := agentsEnv(t)
	const id = "02:00:00:00:00:01"
	register(t, e, id)
	send := func(verb, comment string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", e.front.URL+"/api/nodes/"+url.PathEscape(id)+"/"+verb, strings.NewReader(`{"comment":"`+comment+`"}`))
		req.URL.RawPath = "/api/nodes/" + strings.ReplaceAll(id, ":", "%3A") + "/" + verb // as encodeURIComponent writes it
		for k, v := range map[string]string{"Accept": "application/json", "Content-Type": "application/json", "X-Source": "ui", "X-Actor": "jo",
			"X-Comment": regexp.MustCompile(`[^\x20-\x7e]`).ReplaceAllString(comment, "?")} {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := send("pause", "fan is loud – café"); resp.StatusCode != 200 {
		t.Fatalf("pause: %d %s", resp.StatusCode, body(t, resp))
	}
	if f := p.Facts(); !f[0].Paused || f[0].Placeable {
		t.Fatalf("after pause: %+v", f[0])
	}
	if resp := send("unpause", "quiet again"); resp.StatusCode != 200 {
		t.Fatalf("unpause: %d %s", resp.StatusCode, body(t, resp))
	}
	if resp := send("force-unload", "stuck"); resp.StatusCode != 200 {
		t.Fatalf("force unload: %d %s", resp.StatusCode, body(t, resp))
	}
	// the page shows the server's own words when it refuses
	resp := send("pause", "")
	if b := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "a comment is required") {
		t.Fatalf("no comment: %d %s", resp.StatusCode, b)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var got []string
	for _, x := range au.es {
		if x.Source != "ui" || x.Actor != "jo" || x.Target != "node:"+id {
			t.Errorf("audit entry %+v: want source ui, actor jo, target node:%s", x, id)
		}
		got = append(got, x.Action+"="+x.Comment)
	}
	if want := "pause=fan is loud – café,unpause=quiet again,force_unload=stuck"; strings.Join(got, ",") != want {
		t.Fatalf("audit %v, want %s", got, want)
	}
}

// CR-9 review, A3: the settings page says that agent machines are checked
// whatever forced_fit_check is.
func TestUIForcedFitCheckNamesTheAgentRefusal(t *testing.T) {
	text := uiText(t, "views/settings.js")
	if !strings.Contains(text, "Machines that join through agents are always checked") {
		t.Fatal("views/settings.js: the forced_fit_check note does not say that agent machines are always checked")
	}
}

// The page cache the agent reports (stats.memory.cached_bytes) is shown next to
// the memory figures, and nothing is shown when an older agent does not send it.
func TestUINodesShowTheFileCache(t *testing.T) {
	view := uiText(t, "views/nodes.js")
	for _, want := range []string{
		`agent.stats && agent.stats.memory`,
		`typeof m.cached_bytes === 'number'`,
		`cachedMiB != null ? ['File cache'`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("views/nodes.js lacks %s", want)
		}
	}
}

// A machine whose unpause has not reached its agent yet (unpause_pending) is
// shown as unpausing, not as paused and not with a raw word.
func TestUIUnpausePendingIsShown(t *testing.T) {
	text := uiText(t, "core/nodeactions.js")
	for _, want := range []string{"a.unpause_pending", "short: 'Unpausing'", "once its agent has been told"} {
		if !strings.Contains(text, want) {
			t.Errorf("core/nodeactions.js lacks %s", want)
		}
	}
}
