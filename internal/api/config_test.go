package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// cfgEnv is an API with the configuration service on a real database. The
// scheduler behind it starts from the test YAML; the first applied revision replaces it.
func cfgEnv(t *testing.T) (*env, *cfgsvc.Service, *cfgstore.Store) {
	t.Helper()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	var e *env
	svc := cfgsvc.New(context.Background(), cfgsvc.Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }},
		Apply:    func(c *config.Config) { e.sched.ApplyConfig(c) },
		OnChange: func(ev cfgsvc.ChangeEvent) { e.api.PublishConfig(ev) }})
	e = newEnv(t, okUpstream(), func(o *Options) { o.Config, o.Audit = svc, cs })
	return e, svc, cs
}

func okUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
}

func (e *env) cdo(method, path, body string, hdr map[string]string) (int, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.front.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b := body2(e.t, resp)
	var m map[string]any
	_ = json.Unmarshal([]byte(b), &m)
	return resp.StatusCode, m
}

func body2(t *testing.T, r *http.Response) string { return body(t, r) }

const newProfile = `{"id":"p1","model":"p1.gguf","ctx":2048,"needs_mib":100}`

func TestConfigEmptyThenCreateProfileWithAuditSources(t *testing.T) {
	e, _, _ := cfgEnv(t)
	st, m := e.cdo("GET", "/api/config", "", nil)
	if st != 200 || m["configured"] != false || m["revision"] != float64(0) {
		t.Fatalf("%d %v", st, m)
	}
	for _, k := range []string{"settings", "tiers", "profiles", "exclude_nodes", "node_overrides", "recovery", "fleet", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("GET /api/config lacks %s", k)
		}
	}
	st, m = e.cdo("POST", "/api/config/profiles", newProfile, map[string]string{"X-Source": "ui", "X-Actor": "alex", "X-Comment": "first"})
	if st != 201 || m["revision"] != float64(1) || len(m["audit_entry_ids"].([]any)) != 1 {
		t.Fatalf("%d %v", st, m)
	}
	// Source is decided by the server: a client cannot claim mcp or import.
	e.cdo("PUT", "/api/config/profiles/p1", `{"model":"p1.gguf","ctx":4096,"needs_mib":100}`, map[string]string{"X-Source": "mcp", "X-Actor": "agent-7"})
	e.cdo("PUT", "/api/config/profiles/p1", `{"model":"p1.gguf","ctx":8192,"needs_mib":100}`, map[string]string{"X-Source": "import"})
	st, m = e.cdo("GET", "/api/audit?target=p1&order=asc", "", nil)
	es := m["entries"].([]any)
	if st != 200 || m["total_matching"] != float64(3) || len(es) != 3 {
		t.Fatalf("%d %v", st, m)
	}
	want := [][3]string{{"ui", "alex", "create"}, {"rest", "agent-7", "update"}, {"rest", "anonymous", "update"}}
	for i, w := range want {
		got := es[i].(map[string]any)
		if got["source"] != w[0] || got["actor"] != w[1] || got["action"] != w[2] || got["section"] != "profiles" {
			t.Fatalf("entry %d: %v", i, got)
		}
	}
	if es[0].(map[string]any)["comment"] != "first" || es[1].(map[string]any)["remote_addr"] == "" || es[1].(map[string]any)["user_agent"] == nil {
		t.Fatalf("%v", es[:2])
	}
	// the change reached the scheduler as a snapshot
	eventually(t, "scheduler config revision 3", func() bool { return e.sched.Config().Revision == 3 && e.sched.Config().Profiles["p1"] != nil })
	st, m = e.cdo("GET", "/api/status", "", nil)
	if st != 200 || m["config_revision"] != float64(3) || m["configured"] != true {
		t.Fatalf("status lacks the revision: %v", m)
	}
	st, m = e.cdo("GET", "/api/audit/1", "", nil)
	if st != 200 || m["id"] != float64(1) || m["diff"] == nil {
		t.Fatalf("%d %v", st, m)
	}
	if st, _ = e.cdo("GET", "/api/audit/999", "", nil); st != 404 {
		t.Fatalf("%d", st)
	}
}

func TestConfigWriteErrorsDryRunAndIfMatch(t *testing.T) {
	e, _, cs := cfgEnv(t)
	st, m := e.cdo("POST", "/api/config/profiles", `{"id":"bad","model":"x.gguf","ctx":1,"kv_quant":"q8_0"}`, nil)
	msgs := m["error"].(map[string]any)["messages"].([]any)
	if st != 400 || len(msgs) == 0 || !strings.Contains(msgs[0].(string), "q8_0 requires flash_attn") {
		t.Fatalf("%d %v", st, m)
	}
	if st, _ = e.cdo("POST", "/api/config/profiles", `{"id":"p","model":"x.gguf","ctx":1,"bogus":1}`, nil); st != 400 {
		t.Fatalf("unknown field accepted: %d", st)
	}
	st, m = e.cdo("POST", "/api/config/profiles?dry_run=1", newProfile, nil)
	if st != 200 || m["dry_run"] != true || m["revision"] != float64(0) || len(m["diff"].([]any)) == 0 {
		t.Fatalf("%d %v", st, m)
	}
	if _, total, _ := cs.Query(context.Background(), cfgstore.Filter{}); total != 0 {
		t.Fatalf("a rejected or dry-run change was audited: %d", total)
	}
	e.cdo("POST", "/api/config/profiles", newProfile, nil)
	if st, _ = e.cdo("POST", "/api/config/profiles", newProfile, nil); st != 409 {
		t.Fatalf("duplicate: %d", st)
	}
	st, m = e.cdo("PATCH", "/api/config/settings", `{"ttl":"1m"}`, map[string]string{"If-Match": "0"})
	if st != 409 || m["error"].(map[string]any)["current_revision"] != float64(1) {
		t.Fatalf("%d %v", st, m)
	}
	st, m = e.cdo("PATCH", "/api/config/settings", `{"ttl":"1m","comment":"short ttl"}`, map[string]string{"If-Match": "1"})
	if st != 200 || m["revision"] != float64(2) {
		t.Fatalf("%d %v", st, m)
	}
	st, m = e.cdo("PATCH", "/api/config/settings", `{"ttl":null}`, nil)
	if st != 200 || m["revision"] != float64(3) {
		t.Fatalf("%d %v", st, m)
	}
	if st, _ = e.cdo("DELETE", "/api/config/profiles/nope", "", nil); st != 404 {
		t.Fatalf("%d", st)
	}
}

func TestConfigSchemaRevisionsRollbackExportImport(t *testing.T) {
	e, _, _ := cfgEnv(t)
	st, m := e.cdo("GET", "/api/config/schema", "", nil)
	fields := m["fields"].([]any)
	scopes := map[string]bool{}
	for _, f := range fields {
		fm := f.(map[string]any)
		for _, k := range []string{"key", "section", "type", "description", "restart_required"} {
			if _, ok := fm[k]; !ok {
				t.Fatalf("field lacks %s: %v", k, fm)
			}
		}
		scopes[fm["scope"].(string)] = true
	}
	if st != 200 || !scopes["settings"] || !scopes["profile"] || !scopes["tier"] || !scopes["recovery"] || !scopes["fleet"] {
		t.Fatalf("%d %v", st, scopes)
	}
	if st, mm := e.cdo("POST", "/api/config/profiles", newProfile, nil); st != 200 && st != 201 {
		t.Fatalf("step 1: %d %v", st, mm)
	}
	if st, mm := e.cdo("PUT", "/api/config/tiers", `[{"match":{"local":true},"priority":1,"headroom_mib":100}]`, nil); st != 200 && st != 201 {
		t.Fatalf("step 2: %d %v", st, mm)
	}
	if st, mm := e.cdo("PUT", "/api/config/exclude-nodes", `{"exclude_nodes":["ps5-x*"],"comment":"c"}`, nil); st != 200 && st != 201 {
		t.Fatalf("step 3: %d %v", st, mm)
	}
	if st, mm := e.cdo("PUT", "/api/config/node-overrides", `{"a":{"rpc":"192.0.2.3:50052"}}`, nil); st != 200 && st != 201 {
		t.Fatalf("step 4: %d %v", st, mm)
	}
	st, m = e.cdo("PUT", "/api/config/recovery", `{"enabled":true,"actions":["restart_rpc_server"]}`, nil) // 5
	if st != 200 || m["revision"] != float64(5) {
		t.Fatalf("%d %v", st, m)
	}
	st, m = e.cdo("PUT", "/api/config/fleet", `{"url":"http://f:8090","local_node":"host-1"}`, nil) // 6
	if st != 200 || len(m["restart_required"].([]any)) != 2 {
		t.Fatalf("restart_required: %d %v", st, m)
	}
	st, m = e.cdo("GET", "/api/config/revisions?limit=3", "", nil)
	if st != 200 || len(m["revisions"].([]any)) != 3 {
		t.Fatalf("%d %v", st, m)
	}
	st, m = e.cdo("GET", "/api/config/revisions/1", "", nil)
	cfg := m["config"].(map[string]any)
	if st != 200 || len(cfg["profiles"].(map[string]any)) != 1 || len(cfg["tiers"].([]any)) != 0 {
		t.Fatalf("%d %v", st, m)
	}
	st, exp := e.cdo("GET", "/api/config/export", "", nil)
	if st != 200 || exp["format"] != "llm-scheduler-config/1" {
		t.Fatalf("%d %v", st, exp)
	}
	st, m = e.cdo("POST", "/api/config/revisions/1/rollback", `{"comment":"back"}`, nil) // 7
	if st != 200 || m["revision"] != float64(7) || m["rolled_back_to"] != float64(1) {
		t.Fatalf("%d %v", st, m)
	}
	st, m = e.cdo("GET", "/api/audit?action=rollback", "", nil)
	if st != 200 || m["total_matching"].(float64) < 1 {
		t.Fatalf("%d %v", st, m)
	}
	// import refuses a non-empty database unless replace=1
	b, _ := json.Marshal(exp)
	if st, _ = e.cdo("POST", "/api/config/import", string(b), nil); st != 409 {
		t.Fatalf("import into non-empty: %d", st)
	}
	st, m = e.cdo("POST", "/api/config/import?replace=1", string(b), nil) // 8
	if st != 200 || m["revision"] != float64(8) {
		t.Fatalf("%d %v", st, m)
	}
	st, m = e.cdo("GET", "/api/config", "", nil)
	if len(m["tiers"].([]any)) != 1 || m["fleet"].(map[string]any)["url"] != "http://f:8090" {
		t.Fatalf("import did not restore: %v", m)
	}
}

func TestRuntimeActionsAreAudited(t *testing.T) {
	e, _, _ := cfgEnv(t)
	resp := e.post("/api/load", `{"model":"alpha"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, body(t, resp))
	}
	eventually(t, "load audited", func() bool {
		_, m := e.cdo("GET", "/api/audit?section=runtime&action=load", "", nil)
		return m["total_matching"] == float64(1)
	})
	_, m := e.cdo("GET", "/api/audit?section=runtime", "", map[string]string{})
	es := m["entries"].([]any)
	if es[0].(map[string]any)["target"] != "alpha" || es[0].(map[string]any)["revision"] != float64(0) {
		t.Fatalf("%v", es[0])
	}
	if st, mm := e.cdo("GET", "/api/config", "", nil); st != 200 || mm["revision"] != float64(0) {
		t.Fatalf("a runtime action created a revision: %v", mm)
	}
}

func TestWSHelloCarriesRevisionAndConfigTopicAnnouncesChanges(t *testing.T) {
	e, _, _ := cfgEnv(t)
	c := dialWS(t, e, nil)
	hello := c.until(t, "hello", 3*time.Second)
	var h map[string]any
	_ = json.Unmarshal(hello.Data, &h)
	if rev, ok := h["config_revision"]; !ok || rev != float64(0) || !strings.Contains(string(hello.Data), `"config"`) {
		t.Fatalf("hello: %s", hello.Data)
	}
	e.cdo("POST", "/api/config/profiles", newProfile, map[string]string{"X-Source": "ui", "X-Actor": "alex"})
	m := c.until(t, "config", 3*time.Second)
	var d map[string]any
	_ = json.Unmarshal(m.Data, &d)
	if d["revision"] != float64(1) || d["section"] != "profiles" || d["target"] != "p1" || d["source"] != "ui" || d["actor"] != "alex" || len(d["audit_entry_ids"].([]any)) != 1 {
		t.Fatalf("%s", m.Data)
	}
}
