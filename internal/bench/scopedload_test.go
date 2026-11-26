package bench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// scopedFake is a scheduler that answers a scoped load the OLD way (202
// naming the production id, as an image before slice A does) or the new
// way, and records every request.
type scopedFake struct {
	mu       sync.Mutex
	reqs     []string
	old      bool
	loaded   []string // instances in the status
	loadCode int
}

func (f *scopedFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /api/status":
		var ins []map[string]any
		for _, p := range f.loaded {
			ins = append(ins, map[string]any{"profile": p, "state": "ready"})
		}
		json.NewEncoder(w).Encode(map[string]any{"instances": ins, "queue": []any{}})
	case "POST /api/load":
		var m map[string]any
		json.Unmarshal(b, &m)
		if f.loadCode >= 300 {
			w.WriteHeader(f.loadCode)
			io.WriteString(w, `{"error":{"message":"m already loaded or queued","reason":"already loaded or queued"}}`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if f.old { // the older image ignores scope and the settings: a plain or pinned load of the production profile
			json.NewEncoder(w).Encode(map[string]any{"msg": "loading", "model": "m", "nodes": m["nodes"]})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"msg": "loading", "model": "m@" + m["scope"].(string), "production": "m", "scope": m["scope"], "nodes": m["nodes"]})
	case "POST /api/unload":
		io.WriteString(w, `{"msg":"ok","model":"m"}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *scopedFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func scopedReq() ScopedLoadRequest {
	ctx := 8192
	return ScopedLoadRequest{Model: "m-alias", Scope: "run-7", Nodes: []string{"c"}, Ctx: &ctx}
}

// The way-back hole: on an older scheduler a scoped load loads the
// production profile itself. The helper sees the answer does not name the
// scoped profile, unloads what that load started (force: it is still
// loading), fails with the sentence, and requests nothing else.
func TestLoadScoped_OldSchedulerIsUnloadedAndFails(t *testing.T) {
	f := &scopedFake{old: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	h := &HTTPAPI{Base: srv.URL}
	ans, err := h.LoadScoped(context.Background(), scopedReq())
	if !errors.Is(err, ErrNoScopedLoads) || !strings.Contains(err.Error(), "this scheduler does not know run-scoped loads; nothing is left loaded") {
		t.Fatalf("err %v", err)
	}
	if ans == nil || ans.Status != 202 || ans.Model != "m" {
		t.Fatalf("answer %+v", ans)
	}
	got := f.requests()
	want := []string{"GET /api/status", "POST /api/load", `POST /api/unload {"force":true,"model":"m"}`}
	if len(got) != len(want) {
		t.Fatalf("requests %q", got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("request %d: %q, want %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(got[1], `"scope":"run-7"`) || !strings.Contains(got[1], `"ctx":8192`) || !strings.Contains(got[1], `"nodes":["c"]`) {
		t.Fatalf("load body %q", got[1])
	}
}

// On an older scheduler where the production profile was already loaded,
// the plain load it did started nothing: nothing is unloaded (production
// traffic is not cut), and the load still fails with the sentence.
func TestLoadScoped_OldSchedulerProductionAlreadyLoadedIsLeft(t *testing.T) {
	f := &scopedFake{old: true, loaded: []string{"m"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := (&HTTPAPI{Base: srv.URL}).LoadScoped(context.Background(), scopedReq())
	if !errors.Is(err, ErrNoScopedLoads) {
		t.Fatalf("err %v", err)
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "POST /api/unload") {
			t.Fatalf("production was unloaded: %q", f.requests())
		}
	}
	if n := len(f.requests()); n != 2 {
		t.Fatalf("requests %q", f.requests())
	}
}

// The check passes against the new answer: no unload, the scoped name back.
func TestLoadScoped_NewSchedulerPasses(t *testing.T) {
	f := &scopedFake{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ans, err := (&HTTPAPI{Base: srv.URL}).LoadScoped(context.Background(), scopedReq())
	if err != nil || ans.Model != "m@run-7" || ans.Production != "m" || ans.Scope != "run-7" {
		t.Fatalf("%+v %v", ans, err)
	}
	if got := f.requests(); len(got) != 2 || !strings.HasPrefix(got[1], "POST /api/load") {
		t.Fatalf("requests %q", got)
	}
}

// A refusal is an answer, as Load gives one: no unload, no error.
func TestLoadScoped_RefusalIsAnAnswer(t *testing.T) {
	f := &scopedFake{loadCode: 409}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ans, err := (&HTTPAPI{Base: srv.URL}).LoadScoped(context.Background(), scopedReq())
	if err != nil || ans.Status != 409 || ans.Reason != "already loaded or queued" {
		t.Fatalf("%+v %v", ans, err)
	}
	if len(f.requests()) != 2 {
		t.Fatalf("requests %q", f.requests())
	}
}

// The rule a script uses (the jq test in the status file) is the same.
func TestIsScopedAnswer(t *testing.T) {
	cases := []struct {
		a    ScopedLoadAnswer
		want bool
	}{
		{ScopedLoadAnswer{Model: "m@s", Production: "m", Scope: "s"}, true},
		{ScopedLoadAnswer{Model: "m", Production: "", Scope: ""}, false}, // an older image
		{ScopedLoadAnswer{Model: "m@other", Production: "m", Scope: "other"}, false},
		{ScopedLoadAnswer{Model: "x@s", Production: "m", Scope: "s"}, false},
		{ScopedLoadAnswer{Model: "m@s", Production: "m", Scope: ""}, false},
	}
	for _, c := range cases {
		if got := IsScopedAnswer(c.a, "s"); got != c.want {
			t.Errorf("%+v: %v", c.a, got)
		}
	}
}
