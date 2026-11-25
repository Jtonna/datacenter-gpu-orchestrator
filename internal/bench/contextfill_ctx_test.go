package bench

import (
	"strings"
	"testing"
)

// The fill reads the server's own context per slot before its first turn
// and refuses, before any request, when it is below the planned one (the
// server would answer 400 "exceeds the available context size").
func TestContextFill_RefusesWhenTheServerRunsASmallerContext(t *testing.T) {
	api := newFake().prof("small", fakeProf{chatFn: recallingFill})
	n := 2048
	api.ctxServed = &n
	api.ctxNote = "runs at 2,048, profile says 4,096"
	tm := fillMgr(t, api)
	tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[50]},{"type":"unload"}]}`)
	tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
	r := tm.finished(tm.run("s").ID)
	if r.Steps[0].Outcome != OutcomeFailed || !strings.Contains(r.Steps[0].Reason, "runs at 2,048 tokens per slot") || !strings.Contains(r.Steps[0].Reason, "4,096") {
		t.Fatalf("%s %q", r.Steps[0].Outcome, r.Steps[0].Reason)
	}
	if c := len(api.chats["small"]); c != 0 {
		t.Fatalf("%d chats sent before the refusal", c)
	}
	f := fillOf(t, r, 0)
	if f.CtxServed == nil || *f.CtxServed != 2048 || len(f.Marks) != 0 {
		t.Fatalf("fill result: served %v marks %d", f.CtxServed, len(f.Marks))
	}
	if md := fillMDString(f); !strings.Contains(md, "Model server context: 2,048 tokens per slot (runs at 2,048, profile says 4,096)") {
		t.Fatalf("report:\n%s", md)
	}
}

// The same figure, or none at all (an older scheduler, or "context not
// confirmed"), lets the fill run; the result names what it found.
func TestContextFill_RunsWhenTheServerContextMatchesOrIsUnknown(t *testing.T) {
	for _, c := range []struct {
		name   string
		served *int
		note   string
	}{{"same", ptrInt(4096), ""}, {"unknown", nil, "context not confirmed: /props answered 404"}, {"older", nil, ""}} {
		t.Run(c.name, func(t *testing.T) {
			api := newFake().prof("small", fakeProf{chatFn: recallingFill})
			api.ctxServed, api.ctxNote = c.served, c.note
			tm := fillMgr(t, api)
			tm.bench(`{"id":"fill","profile":"small","phases":[{"type":"load"},{"type":"context_fill","marks_pct":[50]},{"type":"unload"}]}`)
			tm.suite(`{"id":"s","benchmarks":["fill"],"cooldown_s":0,"only_when_idle":false}`)
			r := tm.finished(tm.run("s").ID)
			if r.Steps[0].Outcome != OutcomeReady {
				t.Fatalf("%s %q", r.Steps[0].Outcome, r.Steps[0].Reason)
			}
			f := fillOf(t, r, 0)
			if c.note != "" && f.CtxNote != c.note {
				t.Fatalf("ctx note %q", f.CtxNote)
			}
		})
	}
}

func ptrInt(v int) *int { return &v }
