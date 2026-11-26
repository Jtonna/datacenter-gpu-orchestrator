package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

const extBody = `{"id":"ctx-1","title":"Context scaling","note":"synthetic",
 "tables":[{"title":"Steps","columns":[{"key":"model"},{"key":"ctx","unit":"tokens"},{"key":"gen_tps","label":"Generation","unit":"tok/s","higher_is_better":true}],
  "rows":[{"model":"Model A","profile":"alpha","ctx":2048,"gen_tps":16.4},{"model":"Model A","ctx":4096,"gen_tps":15.9}]}],
 "attachments":[{"name":"prompt.md","text":"Recall five facts."}]}`

// extTables builds a body with n tables of rows rows and cols columns.
func extTables(n, rows, cols int) string {
	var ts []string
	for i := 0; i < n; i++ {
		var cs, rs []string
		for c := 0; c < cols; c++ {
			cs = append(cs, fmt.Sprintf(`{"key":"c%d"}`, c))
		}
		for r := 0; r < rows; r++ {
			rs = append(rs, fmt.Sprintf(`{"c0":%d}`, r))
		}
		ts = append(ts, fmt.Sprintf(`{"title":"t%d","columns":[%s],"rows":[%s]}`, i, strings.Join(cs, ","), strings.Join(rs, ",")))
	}
	return `{"id":"big","title":"Big","tables":[` + strings.Join(ts, ",") + `]}`
}

func TestExternalOff(t *testing.T) {
	e := newEnv(t, chatOK)
	for _, c := range [][2]string{{"GET", "/api/bench/external"}, {"POST", "/api/bench/external"}, {"GET", "/api/bench/external/x"}, {"PUT", "/api/bench/external/x"}, {"DELETE", "/api/bench/external/x"}} {
		if st, b, _ := e.do(c[0], c[1], ""); st != 503 || !strings.Contains(b, "data_dir") {
			t.Errorf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
}

func TestExternalCRUD(t *testing.T) {
	e := benchEnv(t)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	if st, b, _ := e.do("GET", "/api/bench/external", ""); st != 200 || strings.TrimSpace(b) != `{"external":[]}` {
		t.Fatalf("empty list: %d %s", st, b)
	}
	st, b, h := e.do("POST", "/api/bench/external", extBody)
	if st != 201 || h.Get("Location") != "/api/bench/external/ctx-1" {
		t.Fatalf("%d %s", st, b)
	}
	m := jsonOf(t, b)
	if m["source"] != "external" || m["created_at"] == nil || len(m["tables"].([]any)) != 1 {
		t.Fatalf("%v", m)
	}
	if st, b, _ := e.do("POST", "/api/bench/external", extBody); st != 409 || !strings.Contains(b, "PUT /api/bench/external/ctx-1") {
		t.Fatalf("duplicate: %d %s", st, b)
	}
	// the listing has no rows and no attachment texts
	st, b, _ = e.do("GET", "/api/bench/external", "")
	if st != 200 || strings.Contains(b, `"rows"`) || strings.Contains(b, "Recall five facts") {
		t.Fatalf("list: %d %s", st, b)
	}
	item := jsonOf(t, b)["external"].([]any)[0].(map[string]any)
	if item["id"] != "ctx-1" || item["tables"].([]any)[0].(map[string]any)["row_count"] != 2.0 || item["attachments"].([]any)[0].(map[string]any)["name"] != "prompt.md" {
		t.Fatalf("%v", item)
	}
	// get has everything
	st, b, _ = e.do("GET", "/api/bench/external/ctx-1", "")
	if st != 200 || !strings.Contains(b, "Recall five facts") || !strings.Contains(b, `"higher_is_better":true`) || !strings.Contains(b, `"profile":"alpha"`) {
		t.Fatalf("get: %d %s", st, b)
	}
	// replace: keeps created_at, the id may be left out of the body
	put := strings.Replace(strings.Replace(extBody, `"id":"ctx-1",`, ``, 1), `"Context scaling"`, `"Renamed"`, 1)
	if st, b, _ := e.do("PUT", "/api/bench/external/ctx-1", put); st != 200 || jsonOf(t, b)["title"] != "Renamed" || jsonOf(t, b)["created_at"] != m["created_at"] {
		t.Fatalf("put: %d %s", st, b)
	}
	if st, _, _ := e.do("PUT", "/api/bench/external/other", put); st != 404 {
		t.Fatalf("put missing: %d", st)
	}
	if st, _, _ := e.do("PUT", "/api/bench/external/ctx-1", strings.Replace(extBody, `"ctx-1"`, `"zzz"`, 1)); st != 400 {
		t.Fatalf("id mismatch: %d", st)
	}
	if st, _, _ := e.do("GET", "/api/bench/external/ghost", ""); st != 404 {
		t.Fatalf("get missing: %d", st)
	}
	if st, b, _ := e.do("DELETE", "/api/bench/external/ctx-1", ""); st != 200 || !strings.Contains(b, `"deleted":"ctx-1"`) {
		t.Fatalf("delete: %d %s", st, b)
	}
	if st, _, _ := e.do("DELETE", "/api/bench/external/ctx-1", ""); st != 404 {
		t.Fatalf("delete twice: %d", st)
	}
	var got []string
	for _, c := range rec.list() {
		if c.section != "bench" {
			t.Errorf("section %q", c.section)
		}
		got = append(got, c.action+" "+c.target)
	}
	if strings.Join(got, ",") != "create external:ctx-1,update external:ctx-1,delete external:ctx-1" {
		t.Fatalf("audit %v", got)
	}
	calls := rec.list()
	if calls[0].before != nil || calls[0].after.(map[string]any)["id"] != "ctx-1" || calls[1].before.(map[string]any)["title"] != "Context scaling" ||
		calls[1].after.(map[string]any)["title"] != "Renamed" || calls[2].after != nil {
		t.Fatalf("audit before and after: %+v", calls)
	}
	if a := fmt.Sprint(calls[0].after); strings.Contains(a, "rows:") || strings.Contains(a, "Recall five facts") {
		t.Fatalf("the audit entry carries the rows: %s", a)
	}
}

func TestExternalValidationOverHTTP(t *testing.T) {
	e := benchEnv(t)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":      {`{`, 400},
		"unknown field": {`{"id":"a","title":"A","tables":[],"nope":1}`, 400},
		"bad id":        {strings.Replace(extBody, `"ctx-1"`, `"A B"`, 1), 422},
		"no title":      {strings.Replace(extBody, `"Context scaling"`, `""`, 1), 422},
		"bool cell":     {strings.Replace(extBody, `"gen_tps":16.4`, `"gen_tps":true`, 1), 422},
		"unknown cell":  {strings.Replace(extBody, `"gen_tps":16.4`, `"speed":16.4`, 1), 422},
		"tables":        {extTables(bench.MaxExternalTables+1, 1, 1), 422},
		"rows":          {extTables(1, bench.MaxExternalRows+1, 1), 422},
		"columns":       {extTables(1, 1, bench.MaxExternalColumns+1), 422},
		"too large":     {`{"id":"a","title":"A","note":"` + strings.Repeat("x", bench.MaxExternalBytes) + `","tables":[]}`, 413},
	} {
		if st, b, _ := e.do("POST", "/api/bench/external", tc.body); st != tc.want {
			t.Errorf("%s: %d %s", name, st, b)
		}
	}
	for _, body := range []string{extTables(bench.MaxExternalTables, 1, 1), extTables(1, bench.MaxExternalRows, 1), extTables(1, 1, bench.MaxExternalColumns)} {
		e.do("DELETE", "/api/bench/external/big", "")
		if st, b, _ := e.do("POST", "/api/bench/external", body); st != 201 {
			t.Errorf("at the limit: %d %.200s", st, b)
		}
	}
}

// The read-only MCP tools reach the external sets, and a REST change is
// audited with the caller's source and actor.
func TestExternalThroughMCPAndAudit(t *testing.T) {
	j := newJoined(t)
	sid := j.session("claude-code")
	if st, m := j.cdo("POST", "/api/bench/external", extBody, map[string]string{"X-Source": "ui", "X-Actor": "alex"}); st != 201 {
		t.Fatalf("%d %v", st, m)
	}
	list := j.ok(sid, "list_external_results", `{}`)
	if l := list["external"].([]any); len(l) != 1 || l[0].(map[string]any)["id"] != "ctx-1" {
		t.Fatalf("%v", list)
	}
	if one := j.ok(sid, "get_external_results", `{"id":"ctx-1"}`); len(one["tables"].([]any)[0].(map[string]any)["rows"].([]any)) != 2 {
		t.Fatalf("%v", one)
	}
	j.fails(sid, "get_external_results", `{"id":"ghost"}`, "HTTP 404")
	es := j.waitAudit("external audit", "target=external:ctx-1", 1)
	if e0 := es[0]; e0["action"] != "create" || e0["section"] != "bench" || e0["source"] != "ui" || e0["actor"] != "alex" {
		t.Fatalf("%v", e0)
	}
}

// The external result sets keep their routes (above); their page went with
// the classic benchmark pages (dropped 25 Nov) and no board draws one.
func TestUIExternalPageIsGone(t *testing.T) {
	for _, name := range uiFiles(t) {
		if strings.HasSuffix(name, ".js") && strings.Contains(uiText(t, name), "/api/bench/external") {
			t.Errorf("%s still reads /api/bench/external", name)
		}
	}
}
