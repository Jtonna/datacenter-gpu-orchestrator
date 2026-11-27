package api

import (
	"strings"
	"testing"
)

// The editors (board bench-editors): every save of a benchmark or a group
// needs a comment and is one audited change carrying it; a refused save
// names the part it is about (problems: path and the server's sentence, the
// sentence verbatim as in error.message); a run says which of its definitions
// changed since it ran (definitions_changed), the past run itself unchanged.

func errMsg(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["message"].(string)
		return s
	}
	return ""
}

func TestBenchSavesNeedAComment(t *testing.T) {
	e := benchEnv(t)
	rec := &auditRec{}
	e.api.SetAuditHook(rec)
	bm := `{"id":"b1","name":"One","profile":"alpha","requests":[{"prompt":"hi","max_tokens":4}]}`
	grp := `{"id":"g1","name":"G","benchmarks":["b1"]}`
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/bench/benchmarks", bm},
		{"POST", "/api/bench/groups", grp},
		{"POST", "/api/bench/suites", strings.Replace(grp, `"g1"`, `"s1"`, 1)},
	} {
		st, m := e.jdo(c.method, c.path, c.body, nil)
		if st != 400 || errMsg(m) != commentRequired {
			t.Fatalf("%s %s without a comment: %d %v", c.method, c.path, st, m)
		}
		// a blank comment is no comment
		if st, m := e.jdo(c.method, c.path, c.body, map[string]string{"X-Comment": "  "}); st != 400 || errMsg(m) != commentRequired {
			t.Fatalf("%s %s with a blank comment: %d %v", c.method, c.path, st, m)
		}
		if st, m := e.jdo(c.method, c.path, c.body, map[string]string{"X-Comment": "new for the S3 programme"}); st != 201 {
			t.Fatalf("%s %s: %d %v", c.method, c.path, st, m)
		}
	}
	// nothing was written by the refused calls
	if n := len(rec.list()); n != 3 {
		t.Fatalf("audit entries %d: %+v", n, rec.list())
	}
	// the comment in the body: taken out before the definition is read, kept in the audit
	st, m := e.jdo("PUT", "/api/bench/benchmarks/b1", `{"comment":"longer answers","id":"b1","name":"One","profile":"alpha","requests":[{"prompt":"hi","max_tokens":8}]}`, nil)
	if st != 200 || m["comment"] != nil {
		t.Fatalf("PUT with a body comment: %d %v", st, m)
	}
	st, m = e.jdo("PUT", "/api/bench/groups/g1", `{"id":"g1","name":"G","benchmarks":["b1"],"cooldown_s":5,"comment":"shorter pause"}`, nil)
	if st != 200 {
		t.Fatalf("PUT group with a body comment: %d %v", st, m)
	}
	if st, m := e.jdo("PUT", "/api/bench/benchmarks/b1", bm, nil); st != 400 || errMsg(m) != commentRequired {
		t.Fatalf("PUT without a comment: %d %v", st, m)
	}
	if st, m := e.jdo("PUT", "/api/bench/benchmarks/b1", `{"comment":7,"id":"b1","profile":"alpha"}`, nil); st != 400 || !strings.Contains(errMsg(m), "comment") {
		t.Fatalf("a comment that is not text: %d %v", st, m)
	}
	calls := rec.list()
	want := []struct{ action, target, comment string }{
		{"create", "benchmark:b1", "new for the S3 programme"}, {"create", "suite:g1", "new for the S3 programme"}, {"create", "suite:s1", "new for the S3 programme"},
		{"update", "benchmark:b1", "longer answers"}, {"update", "suite:g1", "shorter pause"},
	}
	if len(calls) != len(want) {
		t.Fatalf("audit %+v", calls)
	}
	for i, w := range want {
		if calls[i].action != w.action || calls[i].target != w.target || calls[i].comment != w.comment {
			t.Errorf("audit %d: %+v, want %+v", i, calls[i], w)
		}
	}
	// the stored definition does not carry the comment
	_, got := e.jdo("GET", "/api/bench/benchmarks/b1", "", nil)
	if _, has := got["comment"]; has {
		t.Fatalf("stored: %v", got)
	}
}

// A refused save names the part each sentence is about, so the editor can
// mark it; the sentences are the server's, the same as in error.message.
func TestBenchRefusedSaveNamesTheParts(t *testing.T) {
	e := benchEnv(t)
	hdr := map[string]string{"X-Comment": "x"}
	st, m := e.jdo("POST", "/api/bench/benchmarks", `{"id":"b2","profile":"alpha","phases":[{"type":"load"},{"type":"requests","repeats":0},{"type":"warmup","count":-1}],"expect":{"outcome":"maybe"}}`, hdr)
	if st != 422 {
		t.Fatalf("%d %v", st, m)
	}
	ps, _ := m["problems"].([]any)
	if len(ps) < 3 {
		t.Fatalf("problems %v", m)
	}
	msg := errMsg(m)
	paths := map[string]bool{}
	for _, p := range ps {
		pm := p.(map[string]any)
		path, _ := pm["path"].(string)
		text, _ := pm["message"].(string)
		paths[path] = true
		if text == "" || !strings.Contains(msg, text) {
			t.Errorf("problem %v not verbatim in %q", pm, msg)
		}
	}
	for _, want := range []string{"phases[1]", "phases[2]", "expect.outcome"} {
		if !paths[want] {
			t.Errorf("no problem at %s: %v", want, ps)
		}
	}
	// a group: the entry is named
	st, m = e.jdo("POST", "/api/bench/groups", `{"id":"g2","benchmarks":["nope"]}`, hdr)
	if st != 422 {
		t.Fatalf("%d %v", st, m)
	}
	ps, _ = m["problems"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["path"] != "benchmarks[0]" {
		t.Fatalf("group problems %v (%s)", ps, errMsg(m))
	}
}

// A run keeps what it ran; a later save shows on the run as a definition
// changed since it ran.
func TestRunSaysWhichDefinitionsChangedSince(t *testing.T) {
	e, _ := startEnv(t)
	st, m := e.jdo("POST", "/api/bench/groups/g/run", `{"comment":"x","only_when_idle":false}`, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, m)
	}
	id := m["id"].(string)
	waitRun(t, e, id)
	_, run := e.jdo("GET", "/api/bench/runs/"+id, "", nil)
	if ch, ok := run["definitions_changed"].([]any); !ok || len(ch) != 0 {
		t.Fatalf("nothing changed yet: %v", run["definitions_changed"])
	}
	if st, b, _ := e.do("PUT", "/api/bench/benchmarks/b-alpha", `{"id":"b-alpha","name":"Alpha check","profile":"alpha","requests":[{"prompt":"hello","max_tokens":4}]}`); st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	_, run = e.jdo("GET", "/api/bench/runs/"+id, "", nil)
	ch, _ := run["definitions_changed"].([]any)
	if len(ch) != 1 {
		t.Fatalf("changed %v", run["definitions_changed"])
	}
	c := ch[0].(map[string]any)
	if c["id"] != "b-alpha" || c["name"] != "Alpha check" || c["change"] != "changed" {
		t.Fatalf("changed %v", c)
	}
	// the run's own record is what it ran: the step still names the old prompt's benchmark as it was
	if run["state"] != "completed" {
		t.Fatalf("run %v", run["state"])
	}
}

// A delete of a benchmark or a group needs a comment as a save does, and the
// refusal says how to give one, so an agent can correct itself from it alone.
func TestBenchDeletesNeedACommentAndTheRefusalSaysHow(t *testing.T) {
	if !strings.Contains(commentRequired, `"comment"`) || !strings.Contains(commentRequired, "X-Comment") {
		t.Fatalf("the refusal does not name both ways: %q", commentRequired)
	}
	e := benchEnv(t)
	hdr := map[string]string{"X-Comment": "setup"}
	for _, c := range []struct{ path, body string }{
		{"/api/bench/benchmarks", `{"id":"d1","profile":"alpha","requests":[{"prompt":"hi","max_tokens":4}]}`},
		{"/api/bench/groups", `{"id":"dg","benchmarks":["d1"]}`},
		{"/api/bench/suites", `{"id":"ds","benchmarks":["d1"]}`},
	} {
		if st, m := e.jdo("POST", c.path, c.body, hdr); st != 201 {
			t.Fatalf("POST %s: %d %v", c.path, st, m)
		}
	}
	// groups and suites first: the benchmark is in use until they are gone
	for _, p := range []string{"/api/bench/groups/dg", "/api/bench/suites/ds", "/api/bench/benchmarks/d1"} {
		st, m := e.jdo("DELETE", p, "", nil)
		if st != 400 || errMsg(m) != commentRequired {
			t.Fatalf("DELETE %s without a comment: %d %v", p, st, m)
		}
		if er, _ := m["error"].(map[string]any); er["type"] != "invalid_request_error" || er["code"] != 400.0 {
			t.Fatalf("DELETE %s refusal shape: %v", p, m)
		}
		if st, m := e.jdo("DELETE", p, "", map[string]string{"X-Comment": " "}); st != 400 {
			t.Fatalf("DELETE %s with a blank comment: %d %v", p, st, m)
		}
		if st, m := e.jdo("GET", p, "", nil); st != 200 {
			t.Fatalf("GET %s after the refused deletes: %d %v (it was deleted)", p, st, m)
		}
	}
	// the header, or the body's comment
	if st, m := e.jdo("DELETE", "/api/bench/groups/dg", "", map[string]string{"X-Comment": "no longer run"}); st != 200 {
		t.Fatalf("DELETE group with X-Comment: %d %v", st, m)
	}
	if st, m := e.jdo("DELETE", "/api/bench/suites/ds", `{"comment":"no longer run"}`, nil); st != 200 {
		t.Fatalf("DELETE suite with a body comment: %d %v", st, m)
	}
	if st, m := e.jdo("DELETE", "/api/bench/benchmarks/d1", `{"comment":"no longer run"}`, nil); st != 200 {
		t.Fatalf("DELETE benchmark with a body comment: %d %v", st, m)
	}
}
