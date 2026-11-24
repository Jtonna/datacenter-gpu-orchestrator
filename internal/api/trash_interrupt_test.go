package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
)

// Models slice 3, Ruling 1: a delete with interrupt: true force unloads the
// file's instances, cancels its queued requests, says what it interrupted,
// then deletes. The default (no interrupt) is the 409 in_use refusal; a
// benchmark run still refuses; an unload that does not complete within the
// bound moves nothing.

type answerOf struct {
	status int
	reason string
}

// chatAsync sends a chat request for model in the background.
func (te *trashEnv) chatAsync(model string) <-chan answerOf {
	ch := make(chan answerOf, 1)
	go func() {
		resp, err := http.Post(te.front.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			ch <- answerOf{status: -1, reason: err.Error()}
			return
		}
		b := body(te.t, resp)
		var m map[string]any
		json.Unmarshal([]byte(b), &m)
		r, _ := errOf(m)["reason"].(string)
		ch <- answerOf{status: resp.StatusCode, reason: r}
	}()
	return ch
}

// wait is the answer of a background request, or a failure after 10 s.
func (te *trashEnv) wait(ch <-chan answerOf) answerOf {
	te.t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		te.t.Fatal("the request got no answer in 10 s")
		return answerOf{}
	}
}

func (te *trashEnv) instState(profile string) string {
	st, _ := te.sched.Status(context.Background())
	for _, i := range st.Instances {
		if i.Profile == profile {
			return i.State
		}
	}
	return ""
}

func (te *trashEnv) queued(profile string) int {
	st, _ := te.sched.Status(context.Background())
	for _, q := range st.Queue {
		if q.Profile == profile {
			return q.Waiters
		}
	}
	return 0
}

func (te *trashEnv) noRecords() bool {
	l, _ := te.cs.TrashList(context.Background())
	return len(l) == 0
}

func interruptedOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	in, ok := m["interrupted"].(map[string]any)
	if !ok {
		t.Fatalf("no interrupted in %v", m)
	}
	return in
}

func TestModelTrashInterrupt_ReadyInstance(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	body(t, te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`))
	eventually(t, "a1 ready", func() bool { return te.instState("a1") == "ready" })
	// the default answer is unchanged: refused, with the instances
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "in_use" || errOf(m)["instances"] == nil || !strings.Contains(errOf(m)["message"].(string), "interrupt") {
		t.Fatalf("default: %d %v", st, m)
	}
	// a dry run with interrupt says what it would cut, and cuts nothing
	st, m = te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","interrupt":true,"dry_run":true}`, trashUI)
	if st != 200 || m["would_interrupt"] == nil || te.instState("a1") != "ready" {
		t.Fatalf("dry run: %d %v", st, m)
	}
	st, m = te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"owner said so","interrupt":true}`, trashUI)
	if st != 200 {
		t.Fatalf("interrupt: %d %v", st, m)
	}
	in := interruptedOf(t, m)
	insts := in["instances"].([]any)
	if len(insts) != 1 || insts[0].(map[string]any)["profile"] != "a1" || in["queued_cancelled"] != float64(0) {
		t.Fatalf("interrupted: %v", in)
	}
	if te.instState("a1") != "" || te.has("a.gguf") || !te.has(filepath.Join(".trash", m["trash_id"].(string), "a.gguf")) {
		t.Fatal("not unloaded and moved")
	}
	es := auditOf(t, te.env, "section=models")
	if len(es) != 1 || !strings.Contains(jsonText(es[0]), "interrupted") {
		t.Fatalf("audit: %v", es)
	}
}

func TestModelTrashInterrupt_LoadingInstance(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	hold := make(chan struct{})
	defer close(hold)
	te.launcher.setHold(hold)
	ans := te.chatAsync("a1")
	eventually(t, "a1 loading", func() bool { return te.instState("a1") == "starting" })
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","interrupt":true}`, trashUI)
	if st != 200 {
		t.Fatalf("interrupt: %d %v", st, m)
	}
	in := interruptedOf(t, m)
	if in["requests_cut"].(float64) < 1 || in["instances"].([]any)[0].(map[string]any)["state"] != "starting" {
		t.Fatalf("interrupted: %v", in)
	}
	if a := te.wait(ans); a.status != 503 || a.reason != "unloaded" {
		t.Fatalf("the waiting request got %+v", a)
	}
	if te.has("a.gguf") {
		t.Fatal("not moved")
	}
}

func TestModelTrashInterrupt_QueuedRequests(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	te.file("x.gguf", "x")
	te.file("y.gguf", "y")
	te.cdo("POST", "/api/config/profiles", `{"id":"x1","model":"x.gguf","ctx":2048,"needs_mib":5000}`, nil)
	te.cdo("POST", "/api/config/profiles", `{"id":"y1","model":"y.gguf","ctx":2048,"needs_mib":5000}`, nil)
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	te.launcher.setHold(hold)
	yAns := te.chatAsync("y1")
	eventually(t, "y1 loading", func() bool { return te.instState("y1") == "starting" })
	xAns := te.chatAsync("x1")
	eventually(t, "x1 queued", func() bool { return te.queued("x1") == 1 })
	st, m := te.cdo("POST", "/api/models/delete/x.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["queued"] == nil {
		t.Fatalf("default with a queued request: %d %v", st, m)
	}
	st, m = te.cdo("POST", "/api/models/delete/x.gguf", `{"comment":"x","interrupt":true}`, trashUI)
	if st != 200 || interruptedOf(t, m)["queued_cancelled"] != float64(1) {
		t.Fatalf("interrupt: %d %v", st, m)
	}
	if a := te.wait(xAns); a.status != 503 || a.reason != "unloaded" {
		t.Fatalf("the queued request got %+v", a)
	}
	if te.has("x.gguf") || te.instState("y1") != "starting" {
		t.Fatalf("x moved %v, y1 %s", !te.has("x.gguf"), te.instState("y1"))
	}
	release()
	if a := te.wait(yAns); a.status != 200 {
		t.Fatalf("y1 was disturbed: %+v", a)
	}
}

func TestModelTrashInterrupt_BenchmarkRunStillRefuses(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	if _, err := te.bs.DB().Exec(`INSERT INTO runs (id, suite_id, suite_name, state, queued_at, started_at, finished_at, version, suite) VALUES ('r1','s','S','running',1,1,0,'v','{"steps":[{"profile":"a1"}]}')`); err != nil {
		t.Fatal(err)
	}
	body(t, te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`))
	eventually(t, "a1 ready", func() bool { return te.instState("a1") == "ready" })
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","interrupt":true}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "in_use" || errOf(m)["runs"] == nil || !strings.Contains(errOf(m)["message"].(string), "does not stop benchmark runs") {
		t.Fatalf("run: %d %v", st, m)
	}
	if te.instState("a1") != "ready" || !te.has("a.gguf") || len(te.profileIDs()) != 3 {
		t.Fatal("a refused interrupt changed something")
	}
}

func TestModelTrashInterrupt_UnloadDoesNotComplete(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	body(t, te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`))
	eventually(t, "a1 ready", func() bool { return te.instState("a1") == "ready" })
	rev := te.svc.Revision()
	te.launcher.setNoExit(true)
	t.Cleanup(func() { // whatever happens, the stuck child may exit before the env closes
		te.launcher.setNoExit(false)
		te.sched.Unload(context.Background(), "a1", true)
	})
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","interrupt":true}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "interrupt_incomplete" || errOf(m)["still"] == nil || !strings.Contains(errOf(m)["message"].(string), "Nothing was moved") {
		t.Fatalf("stuck unload: %d %v", st, m)
	}
	if !te.has("a.gguf") || len(te.profileIDs()) != 3 || te.svc.Revision() != rev || !te.noRecords() {
		t.Fatal("something moved")
	}
	if ents, _ := os.ReadDir(filepath.Join(te.dir, ".trash")); len(ents) != 0 {
		t.Fatalf("trash folders left: %v", ents)
	}
}

// A load that starts between the interrupt and the revision: the revision
// is undone and nothing moves.
func TestModelTrashInterrupt_LoadedAgainMeanwhile(t *testing.T) {
	var hold chan struct{}
	te := newTrashEnvWith(t, func(te *trashEnv, step string) bool {
		if step == "delete:after-interrupt" {
			te.chatAsync("a2")
			eventually(t, "a2 loading", func() bool { return te.instState("a2") == "starting" })
		}
		return false
	})
	te.setup()
	body(t, te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`))
	eventually(t, "a1 ready", func() bool { return te.instState("a1") == "ready" })
	hold = make(chan struct{})
	defer close(hold)
	te.launcher.setHold(hold)
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","interrupt":true}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "loaded_again" || !strings.Contains(errOf(m)["message"].(string), "loaded again meanwhile") {
		t.Fatalf("loaded again: %d %v", st, m)
	}
	if !te.has("a.gguf") || strings.Join(te.profileIDs(), ",") != "a1,a2,b1" || !te.noRecords() {
		t.Fatalf("after: file %v profiles %v", te.has("a.gguf"), te.profileIDs())
	}
	var _ = cfgstore.TrashTrashed
}
