package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/access"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
)

func accessEnv(t *testing.T) (*env, *mcpserver.Server, *cfgstore.Store) {
	t.Helper()
	e, _, cs := cfgEnvWith(t, func(o *Options) { o.keyExpiryEvery = -1 }) // the tests run the expiry check themselves
	mcp := mcpserver.New(e.api, mcpserver.Options{Version: "test", Identify: e.api.IdentifyAgent, Scope: e.api.ScopeOf,
		Bridge: func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }})
	e.api.SetMCPTools(mcp.ToolInfo)
	return e, mcp, cs
}

var webUI = map[string]string{"X-Source": "ui", "X-Actor": "alex", "Content-Type": "application/json"}

// newKey creates a key from the web UI and returns its key and agent view.
func newKey(t *testing.T, e *env, body string) (string, map[string]any) {
	t.Helper()
	st, m := e.cdo("POST", "/api/agents", body, webUI)
	if st != 201 {
		t.Fatalf("create %s: %d %v", body, st, m)
	}
	return m["key"].(string), m["agent"].(map[string]any)
}

func bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

func errOf(m map[string]any) map[string]any { e, _ := m["error"].(map[string]any); return e }

// The key model behind artboards 1 to 4 of the MCP page: a new key starts
// with ptl_, is shown once with the settings to copy, expires in 30 days by
// default and may observe only; the list shows its hint, what it can do,
// its expiry, last use and status; the Manage panel changes its name,
// description, expiry and permissions; an expired key is refused and can be
// extended; revoked is final.
func TestAccessKeys_ModelAndViews(t *testing.T) {
	e, _, cs := accessEnv(t)
	now := e.api.opts.Now()

	key, ag := newKey(t, e, `{"name":"ops-bot","description":"watches the fleet at night"}`)
	id := ag["id"].(string)
	wantHint := "ptl_••••" + key[len(key)-4:]
	if !strings.HasPrefix(key, "ptl_") || len(key) != 36 || ag["key_hint"] != wantHint || ag["status"] != KeyActive || ag["can"] != "Observe" ||
		ag["expires_at"].(float64) != float64(now.Add(30*24*time.Hour).UnixMilli()) || ag["expires_in_days"].(float64) != 30 ||
		ag["legacy"] != false || ag["description"] != "watches the fleet at night" {
		t.Fatalf("created: %q %v", key, ag)
	}
	perms := ag["permissions"].(map[string]any)
	if perms["observe"] != "allow" || perms["zones"] != "off" || perms["models"] != "off" || perms["settings"] != "off" {
		t.Fatalf("default permissions: %v", perms)
	}
	// the settings to copy, with the full key only in the create answer
	st, m := e.cdo("POST", "/api/agents", `{"name":"snippet","permissions":{"zones":"allow"},"expires_at":0}`, webUI)
	snip, _ := json.Marshal(m["mcp_settings"])
	if st != 201 || !strings.Contains(string(snip), `"Authorization":"Bearer `+m["key"].(string)+`"`) || !strings.Contains(string(snip), `/mcp"`) ||
		!strings.Contains(string(snip), `"potluck"`) || m["agent"].(map[string]any)["can"] != "Observe · switch zones" || m["agent"].(map[string]any)["expires_at"] != nil {
		t.Fatalf("create with settings: %d %s %v", st, snip, m)
	}
	// no forwarding header changes the URL
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "evil.example") || !strings.Contains(rec.Body.String(), `"mcp_url":"http://example.com/mcp"`) {
		t.Fatalf("the MCP URL follows a forwarding header: %s", rec.Body.String())
	}

	// the list: never the key or its hash; the snippet there holds a placeholder
	_, lm := e.cdo("GET", "/api/agents", "", nil)
	raw, _ := json.Marshal(lm)
	stored, _ := cs.Agent(context.Background(), id)
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), stored.KeyHash) || !strings.Contains(string(raw), "paste your key") || lm["active"].(float64) != 2 {
		t.Fatalf("the list: %s", raw)
	}
	if ps := lm["permissions"].([]any); len(ps) != 4 || ps[0].(map[string]any)["label"] != "Observe" || len(ps[0].(map[string]any)["levels"].([]any)) != 2 {
		t.Fatalf("permissions: %v", ps)
	}

	// Manage: name, description, expiry, permissions
	exp := now.Add(90 * 24 * time.Hour).UnixMilli()
	st, m = e.cdo("PATCH", "/api/agents/"+id, `{"name":"ops-bot-2","description":"d2","expires_at":`+msStr(exp)+`,"permissions":{"models":"ask","settings":"allow"}}`, webUI)
	a := m["agent"].(map[string]any)
	if st != 200 || a["name"] != "ops-bot-2" || a["description"] != "d2" || a["expires_at"].(float64) != float64(exp) || a["can"] != "Observe · change settings" ||
		a["asks"].([]any)[0] != "models" || a["key_hint"] != wantHint {
		t.Fatalf("patch: %d %v", st, m)
	}
	for _, bad := range []string{`{"permissions":{"observe":"ask"}}`, `{"permissions":{"zones":"maybe"}}`, `{"permissions":{"admin":"allow"}}`,
		`{"expires_at":1000}`, `{"expires_at":"soon"}`} {
		if st, m := e.cdo("PATCH", "/api/agents/"+id, bad, webUI); st != 400 {
			t.Errorf("patch %s: %d %v", bad, st, m)
		}
	}
	// the audit entry of the change carries the hint, never the key
	for _, es := range auditOf(t, e, "section=agents&limit=50") {
		if r := string(rawJSON(es)); strings.Contains(r, key) || strings.Contains(r, stored.KeyHash) {
			t.Fatalf("an audit entry holds the key: %s", r)
		}
	}

	// expiring, expired, extended
	e.advance(84 * 24 * time.Hour)
	_, gm := e.cdo("GET", "/api/agents", "", nil)
	a = agentIn(gm, id)
	if a["status"] != KeyExpiring || a["expires_in_days"].(float64) != 6 || gm["active"].(float64) != 2 {
		t.Fatalf("6 days before expiry: %v", a)
	}
	if st, _ := e.cdo("GET", "/api/status", "", bearer(key)); st != 200 {
		t.Fatalf("an expiring key: %d", st)
	}
	e.advance(7 * 24 * time.Hour)
	st, m = e.cdo("GET", "/api/status", "", bearer(key))
	if st != 401 || !strings.Contains(errOf(m)["message"].(string), "expired") || errOf(m)["type"] != "invalid_agent_key" {
		t.Fatalf("an expired key: %d %v", st, m)
	}
	_, gm = e.cdo("GET", "/api/agents", "", nil)
	if a = agentIn(gm, id); a["status"] != KeyExpired || gm["active"].(float64) != 1 {
		t.Fatalf("expired in the list: %v", a)
	}
	if st, m := e.cdo("PATCH", "/api/agents/"+id, `{"expires_at":0}`, webUI); st != 200 || m["agent"].(map[string]any)["status"] != KeyActive {
		t.Fatalf("extend: %d %v", st, m)
	}
	if st, _ := e.cdo("GET", "/api/status", "", bearer(key)); st != 200 {
		t.Fatalf("an extended key: %d", st)
	}
	// rotate keeps permissions and expiry, new hint
	st, m = e.cdo("POST", "/api/agents/"+id+"/rotate", "", webUI)
	key2 := m["key"].(string)
	if a = m["agent"].(map[string]any); st != 200 || a["key_hint"] != "ptl_••••"+key2[len(key2)-4:] || a["can"] != "Observe · change settings" || a["expires_at"] != nil {
		t.Fatalf("rotate: %d %v", st, m)
	}
	// revoke is final
	e.cdo("DELETE", "/api/agents/"+id, "", webUI)
	if st, _ := e.cdo("PATCH", "/api/agents/"+id, `{"expires_at":0}`, webUI); st != 409 {
		t.Fatalf("patch of a revoked key: %d", st)
	}
	_, gm = e.cdo("GET", "/api/agents", "", nil)
	if a = agentIn(gm, id); a["status"] != KeyRevoked || a["revoked_at"] == nil {
		t.Fatalf("revoked in the list: %v", a)
	}
}

func agentIn(list map[string]any, id string) map[string]any {
	for _, x := range list["agents"].([]any) {
		if a := x.(map[string]any); a["id"] == id {
			return a
		}
	}
	return nil
}

func msStr(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A key made before permissions existed (lls_, no access row) keeps working
// exactly as it did: every permission allowed, no expiry; its hint is its
// stored start. Setting its permissions makes it a key like any other.
func TestAccessKeys_LegacyKeyKeepsFullAccess(t *testing.T) {
	e, mcp, cs := accessEnv(t)
	legacy := "lls_kBFyEkAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := cs.CreateAgent(cfgstore.Agent{ID: "ag_legacy", Name: "deploy-check", KeyHash: hashKey(legacy), KeyPrefix: legacy[:10], CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	_, lm := e.cdo("GET", "/api/agents", "", nil)
	a := lm["agents"].([]any)[0].(map[string]any)
	if a["legacy"] != true || a["key_hint"] != "lls_kBFyEk••••" || a["expires_at"] != nil || a["status"] != KeyActive || a["can"] != "Observe · switch zones · load and unload · change settings" {
		t.Fatalf("legacy view: %v", a)
	}
	for _, r := range [][2]string{{"POST", "/api/nodes/x/pause"}, {"PUT", "/api/config/tiers"}, {"POST", "/api/load"}} {
		if st, m := e.cdo(r[0], r[1], `{}`, map[string]string{"X-Agent-Key": legacy}); st == 403 || st == 401 {
			t.Errorf("legacy key on %v: %d %v", r, st, m)
		}
	}
	if code, out := mcpCall(mcp, "create_profile", `{"profile":{"id":"l1","model":"l1.gguf","ctx":2048,"needs_mib":100}}`, bearer(legacy)); code != 200 || strings.Contains(out, `"isError":true`) {
		t.Fatalf("legacy key over MCP: %d %s", code, out)
	}
	// a rename leaves it legacy; setting a permission makes it a ptl_-style key with access
	if _, m := e.cdo("PATCH", "/api/agents/ag_legacy", `{"name":"deploy"}`, webUI); m["agent"].(map[string]any)["legacy"] != true {
		t.Fatalf("rename: %v", m)
	}
	if _, m := e.cdo("PATCH", "/api/agents/ag_legacy", `{"permissions":{"settings":"off"}}`, webUI); m["agent"].(map[string]any)["legacy"] != false ||
		m["agent"].(map[string]any)["expires_at"] != nil {
		t.Fatalf("set permissions: %v", m)
	}
	if st, m := e.cdo("PUT", "/api/config/tiers", `{}`, map[string]string{"X-Agent-Key": legacy}); st != 403 || errOf(m)["scope"] != "settings" {
		t.Fatalf("after settings off: %d %v", st, m)
	}
}

// Enforcement: only requests that carry a key are checked, against the route
// table, on REST and over MCP; the sentence says what is missing.
func TestAccessKeys_EnforcementOnRestAndMCP(t *testing.T) {
	e, mcp, _ := accessEnv(t)
	observer, _ := newKey(t, e, `{"name":"watcher"}`)
	asker, _ := newKey(t, e, `{"name":"asker","permissions":{"zones":"ask","models":"ask","settings":"ask"}}`)
	blind, _ := newKey(t, e, `{"name":"blind","permissions":{"observe":"off","zones":"allow"}}`)

	type want struct {
		method, path string
		key          string
		status       int
		typ          string
	}
	for _, c := range []want{
		{"GET", "/api/status", observer, 200, ""},
		{"POST", "/api/plan", observer, 0, ""}, // observe: the plan changes nothing
		{"POST", "/api/nodes/x/pause", observer, 403, "permission_denied"},
		{"PUT", "/api/config/exclude-nodes", observer, 403, "permission_denied"},
		{"POST", "/api/load", observer, 403, "permission_denied"},
		{"POST", "/api/bench/suites/x/run", observer, 403, "permission_denied"},
		{"PUT", "/api/config/tiers", observer, 403, "permission_denied"},
		{"POST", "/api/config/import", observer, 403, "permission_denied"},
		{"GET", "/api/agents", observer, 403, "owner_only"},
		{"POST", "/api/agents", observer, 403, "owner_only"},
		{"POST", "/api/nodes/x/pause", asker, 403, "approval_required"},
		{"POST", "/api/unload", asker, 403, "approval_required"},
		{"GET", "/api/status", blind, 403, "permission_denied"},
		{"POST", "/api/nodes/x/pause", blind, 0, ""},
		{"GET", "/v1/models", blind, 200, ""},                 // the model API is not checked yet
		{"GET", "/health", blind, 200, ""},                    // open
		{"POST", "/api/pool/nodes/x/heartbeat", blind, 0, ""}, // the agents' protocol
		{"GET", "/api/no/such/route", observer, 404, ""},      // no such route: the catch-all only says so
		{"DELETE", "/api/no/such/route", observer, 404, ""},
	} {
		st, m := e.cdo(c.method, c.path, `{}`, bearer(c.key))
		switch {
		case c.status == 0 && (st == 401 || st == 403):
			t.Errorf("%s %s: refused %d %v", c.method, c.path, st, m)
		case c.status != 0 && st != c.status:
			t.Errorf("%s %s: %d, want %d (%v)", c.method, c.path, st, c.status, m)
		case c.typ != "" && (errOf(m)["type"] != c.typ || !strings.Contains(errOf(m)["message"].(string), "under MCP in the web UI") && c.typ != "owner_only"):
			t.Errorf("%s %s: %v", c.method, c.path, m)
		}
	}
	// without a key: the default user may do everything
	for _, r := range [][2]string{{"GET", "/api/agents"}, {"PUT", "/api/config/exclude-nodes"}} {
		if st, m := e.cdo(r[0], r[1], `[]`, webUI); st == 401 || st == 403 {
			t.Errorf("no key on %v: %d %v", r, st, m)
		}
	}
	// an unknown key is refused, not treated as keyless
	if st, _ := e.cdo("GET", "/api/status", "", bearer("ptl_notakeynotakeynotakeynotakeynota")); st != 401 {
		t.Fatalf("unknown ptl_ key: %d", st)
	}

	// MCP: tools/list hides what is Off; a call says why it is refused
	names := func(hdr map[string]string) map[string]bool {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mcp.ServeHTTP(rec, req)
		var out struct {
			Result struct{ Tools []struct{ Name string } }
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		got := map[string]bool{}
		for _, x := range out.Result.Tools {
			got[x.Name] = true
		}
		return got
	}
	all, obs, bl, ask := names(nil), names(bearer(observer)), names(bearer(blind)), names(bearer(asker))
	if !all["pause_node"] || !all["load_model"] || !all["get_status"] || !obs["get_status"] || !obs["plan_load"] || obs["pause_node"] || obs["load_model"] || obs["update_settings"] ||
		bl["get_status"] || !bl["pause_node"] || !ask["pause_node"] || len(all) <= len(obs) {
		t.Fatalf("tools/list: all %d, observer %v, blind %v", len(all), obs, bl)
	}
	if code, out := mcpCall(mcp, "pause_node", `{"id":"x","comment":"c"}`, bearer(observer)); code != 200 || !strings.Contains(out, `"isError":true`) ||
		!strings.Contains(out, "pause_node was refused") || !strings.Contains(out, "may not switch zones on or off") {
		t.Fatalf("pause_node with observe only: %s", out)
	}
	if code, out := mcpCall(mcp, "load_model", `{"model":"m"}`, bearer(asker)); code != 200 || !strings.Contains(out, "approvals are not available yet") {
		t.Fatalf("load_model with ask: %s", out)
	}
	if _, out := mcpCall(mcp, "get_status", `{}`, bearer(observer)); strings.Contains(out, `"isError":true`) {
		t.Fatalf("get_status with observe: %s", out)
	}
	// every tool has a permission scope (none is owner-only or unchecked)
	_, tm := e.cdo("GET", "/api/mcp/tools", "", nil)
	for _, x := range tm["tools"].([]any) {
		tool := x.(map[string]any)
		sc := access.Scope(tool["scope"].(string))
		if sc != access.Observe && sc != access.Zones && sc != access.Models && sc != access.Settings {
			t.Errorf("tool %v has scope %v", tool["name"], sc)
		}
		if tool["read_only"] == true && sc != access.Observe {
			t.Errorf("read-only tool %v needs %v", tool["name"], sc)
		}
	}
}

// The audit entry of a keyed change names the key's agent (never the
// secret); a change without a key is signed by the default user or the
// name the web UI sends.
func TestAccessKeys_AuditNamesTheKeyOrTheDefaultUser(t *testing.T) {
	e, _, _ := accessEnv(t)
	key, ag := newKey(t, e, `{"name":"tuner","permissions":{"settings":"allow"}}`)
	prof := func(n string) string { return `{"id":"` + n + `","model":"` + n + `.gguf","ctx":2048,"needs_mib":100}` }
	if st, m := e.cdo("POST", "/api/config/profiles", prof("k1"), bearer(key)); st != 201 {
		t.Fatalf("%d %v", st, m)
	}
	if st, m := e.cdo("POST", "/api/config/profiles", prof("d1"), map[string]string{"Content-Type": "application/json"}); st != 201 {
		t.Fatalf("%d %v", st, m)
	}
	by := map[string]map[string]any{}
	for _, a := range auditOf(t, e, "section=profiles") {
		by[a["target"].(string)] = a
		if strings.Contains(string(rawJSON(a)), key) {
			t.Fatal("an audit entry holds the key")
		}
	}
	if by["k1"]["actor"] != "agent:tuner" || by["k1"]["agent_id"] != ag["id"] {
		t.Errorf("keyed: %v", by["k1"])
	}
	if by["d1"]["actor"] != audit.DefaultActor || by["d1"]["agent_id"] != nil && by["d1"]["agent_id"] != "" {
		t.Errorf("default user: %v", by["d1"])
	}
}

// Expiry warnings: agentKeyExpiring once at 7 days, once at 1 day, then
// agentKeyExpired once; never again after a restart (kept with the key);
// a new expiry starts over. The events name the agent and hint, never the
// key. The health row needs attention while a key expires soon.
func TestAccessKeys_ExpiryWarnings(t *testing.T) {
	hist := history.Open(history.Options{Dir: t.TempDir()})
	t.Cleanup(hist.Close)
	e, _, cs := cfgEnvWith(t, func(o *Options) { o.History = hist; o.keyExpiryEvery = -1 })
	now := e.api.opts.Now()
	key, ag := newKey(t, e, `{"name":"short-lived","expires_at":`+msStr(now.Add(10*24*time.Hour).UnixMilli())+`}`)
	id := ag["id"].(string)
	newKey(t, e, `{"name":"forever","expires_at":0}`)
	events := func() []history.Event {
		return hist.Query(history.Query{Types: []string{history.TAgentKeyExpiring, history.TAgentKeyExpired}, Limit: 100}).Events
	}
	check := func() { e.api.checkKeyExpiry(context.Background(), e.api.opts.Now()) }

	check()
	if n := len(events()); n != 0 {
		t.Fatalf("10 days left: %d events", n)
	}
	e.advance(4 * 24 * time.Hour) // 6 days left
	check()
	check()
	ev := events()
	if len(ev) != 1 || ev[0].Type != history.TAgentKeyExpiring || ev[0].Severity != history.Warn || ev[0].Data["days_left"] != float64(6) && ev[0].Data["days_left"] != 6 ||
		!strings.Contains(ev[0].Summary, "short-lived expires in 6 days") || ev[0].Data["agent_id"] != id {
		t.Fatalf("at 6 days: %+v", ev)
	}
	if _, h := e.cdo("GET", "/api/health", "", nil); !strings.Contains(string(rawJSON(h)), "1 expires soon") {
		t.Errorf("health: %v", h)
	}
	// a restart: the mark is kept with the key
	stored, _ := cs.Agent(context.Background(), id)
	if stored.Access.Warned != "7d" {
		t.Fatalf("warned %q", stored.Access.Warned)
	}
	e.advance(5*24*time.Hour + time.Hour) // under 1 day left
	check()
	e.advance(time.Hour)
	check()
	if ev = events(); len(ev) != 2 || !strings.Contains(ev[0].Summary, "expires in 1 day") {
		t.Fatalf("at 1 day: %+v", ev)
	}
	e.advance(24 * time.Hour)
	check()
	check()
	if ev = events(); len(ev) != 3 || ev[0].Type != history.TAgentKeyExpired || !strings.Contains(ev[0].Summary, "expired on") {
		t.Fatalf("expired: %+v", ev)
	}
	for _, x := range ev {
		if strings.Contains(string(rawJSON(x)), key) {
			t.Fatal("an event holds the key")
		}
	}
	// extended: the warnings start over for the new expiry
	e.cdo("PATCH", "/api/agents/"+id, `{"expires_at":`+msStr(e.api.opts.Now().Add(12*time.Hour).UnixMilli())+`}`, webUI)
	check()
	if ev = events(); len(ev) != 4 || ev[0].Type != history.TAgentKeyExpiring || !strings.Contains(ev[0].Summary, "expires in 1 day") {
		t.Fatalf("after extending: %+v", ev)
	}
}

// Only a credential that looks like a Potluck key (ptl_ or lls_) in
// Authorization: Bearer is a key. Whatever else OpenAI-compatible clients
// send by habit (sk-..., none, an empty Bearer, Basic auth) is ignored: the
// request is keyless, the default user, on every route, exactly as before.
// A ptl_ or lls_ value that is unknown is refused with 401 everywhere,
// the model API included. X-Agent-Key is Potluck's own header: any value
// there is a key, as it always was (a wrong one: 401).
func TestAccessKeys_ForeignBearerTokensAreIgnored(t *testing.T) {
	e, mcp, _ := accessEnv(t)
	chat := []byte(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
	routes := []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/v1/chat/completions", chat},
		{"GET", "/v1/models", nil},
		{"POST", "/upstream/no-such-model/v1/chat/completions", chat},
		{"GET", "/api/status", nil},
	}
	keyless := map[string]int{}
	for _, r := range routes {
		st, _, _ := e.hdo(r.method, r.path, r.body, map[string]string{"Content-Type": "application/json"})
		keyless[r.path] = st
	}
	t.Logf("keyless answers: %v", keyless)
	foreign := map[string]map[string]string{
		"Bearer sk-anything":    {"Authorization": "Bearer sk-anything"},
		"Bearer sk-local":       {"Authorization": "Bearer sk-local"},
		"Bearer none":           {"Authorization": "Bearer none"},
		"Bearer dummy":          {"Authorization": "Bearer dummy"},
		"empty Bearer":          {"Authorization": "Bearer "},
		"bare Bearer":           {"Authorization": "Bearer"},
		"empty Authorization":   {"Authorization": ""},
		"Basic auth":            {"Authorization": "Basic dXNlcjpwYXNz"},
		"an OpenAI-style key":   {"Authorization": "Bearer sk-proj-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		"a prefix mid-token":    {"Authorization": "Bearer x-ptl_abc"},
		"Token scheme ptl_":     {"Authorization": "Token ptl_abcdefabcdefabcdefabcdefabcdefab"},
		"200 random characters": {"Authorization": "Bearer " + strings.Repeat("aZ9-_x.Q~", 23)[:200]},
		"lower-case bearer sk":  {"Authorization": "bearer sk-lower"},
		"spaces around sk":      {"Authorization": "   Bearer    sk-spaced   "},
		"ptl_ in the middle":    {"Authorization": "Bearer sk-ptl_lls_inside"},
	}
	for name, hdr := range foreign {
		for _, r := range routes {
			h := map[string]string{"Content-Type": "application/json"}
			for k, v := range hdr {
				h[k] = v
			}
			st, body, _ := e.hdo(r.method, r.path, r.body, h)
			if st != keyless[r.path] || st == 401 || st == 403 {
				t.Errorf("%s on %s %s: %d, keyless %d: %s", name, r.method, r.path, st, keyless[r.path], body)
			}
		}
		if code, out := mcpCall(mcp, "get_status", `{}`, hdr); code != 200 || strings.Contains(out, `"isError":true`) {
			t.Errorf("%s over MCP: %d %s", name, code, out)
		}
	}
	// the same on a scheduler that serves a model: the chat is answered (200),
	// not refused, with each foreign token
	served := newEnv(t, chatOK, func(o *Options) { o.keyExpiryEvery = -1 })
	alpha := []byte(`{"model":"alpha","messages":[{"role":"user","content":"hi"}]}`)
	for name, hdr := range foreign {
		for _, path := range []string{"/v1/chat/completions", "/upstream/alpha/v1/chat/completions"} {
			h := map[string]string{"Content-Type": "application/json"}
			for k, v := range hdr {
				h[k] = v
			}
			if st, body, _ := served.hdo("POST", path, alpha, h); st != 200 {
				t.Errorf("%s on %s with a served model: %d %s", name, path, st, body)
			}
		}
	}

	// a keyless change with a foreign token is the default user's
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"f1","model":"f1.gguf","ctx":2048,"needs_mib":100}`, map[string]string{"Authorization": "Bearer sk-local"}); st != 201 {
		t.Fatalf("change with a foreign token: %d %v", st, m)
	}
	if es := auditOf(t, e, "target=f1"); len(es) != 1 || es[0]["actor"] != audit.DefaultActor || strings.Contains(string(rawJSON(es[0])), "sk-local") {
		t.Errorf("audit of a change with a foreign token: %v", es)
	}

	// unmistakably a Potluck key, but unknown: 401 everywhere, one sentence
	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer ptl_unknownunknownunknownunknownunkn"},
		{"Authorization": "bearer lls_unknownunknownunknownunknownunkn"},
		{"X-Agent-Key": "sk-local"},                  // Potluck's own header: any value is a key, as before
		{"Authorization": "Bearer ptl_!!garbage##"},  // begins ptl_: always taken as a Potluck key
		{"Authorization": "  bearer   ptl_spaced  "}, // any case, any spaces
		{"Authorization": "Bearer ptl_ two words"},   // still begins ptl_
	} {
		for _, r := range routes {
			h := map[string]string{"Content-Type": "application/json"}
			for k, v := range hdr {
				h[k] = v
			}
			st, body, _ := e.hdo(r.method, r.path, r.body, h)
			if st != 401 || !strings.Contains(string(body), "unknown agent key") || !strings.Contains(string(body), "invalid_agent_key") {
				t.Errorf("%v on %s %s: %d %s", hdr, r.method, r.path, st, body)
			}
		}
		if code, out := mcpCall(mcp, "get_status", `{}`, hdr); code != 401 || !strings.Contains(out, "unknown agent key") {
			t.Errorf("%v over MCP: %d %s", hdr, code, out)
		}
	}
}

// Two Authorization headers: a Potluck key in either counts; two different
// Potluck keys are refused; two foreign values are keyless.
func TestAccessKeys_TwoAuthorizationHeaders(t *testing.T) {
	e, _, _ := accessEnv(t)
	key, _ := newKey(t, e, `{"name":"two-headers"}`)
	do := func(vals ...string) (int, string) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		for _, v := range vals {
			req.Header.Add("Authorization", v)
		}
		rec := httptest.NewRecorder()
		e.api.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		vals []string
		want int
		has  string
	}{
		{[]string{"Bearer sk-one", "Basic dXNlcjpwYXNz"}, 200, ""},
		{[]string{"Bearer sk-one", "Bearer " + key}, 200, ""},
		{[]string{"Bearer " + key, "Bearer " + key}, 200, ""},
		{[]string{"Bearer sk-one", "Bearer ptl_unknownunknownunknownunknownunkn"}, 401, "unknown agent key"},
		{[]string{"Bearer " + key, "Bearer ptl_unknownunknownunknownunknownunkn"}, 401, "two different agent keys"},
	} {
		st, body := do(c.vals...)
		if st != c.want || !strings.Contains(body, c.has) {
			t.Errorf("%q: %d %s", c.vals, st, body)
		}
	}
}
