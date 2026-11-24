package api

import (
	"net/http"
	"strings"
	"testing"
)

// Drain a busy model: it stays up while its run finishes, the status says
// drain, then it unloads by itself; undrain on an idle model is 409.
func TestDrain_BusyModelUnloadsAfterItsRun(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(e.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(chat("alpha")))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	if b := status(t, e.post("/api/models/drain/alpha", ""), 200); !strings.Contains(b, `"msg":"draining"`) {
		t.Fatal(b)
	}
	if b := body(t, e.get("/api/status")); !strings.Contains(b, `"drain":true`) || !strings.Contains(b, `"state":"draining"`) {
		t.Fatalf("status %s", b)
	}
	close(release)
	<-done
	eventually(t, "alpha gone", func() bool { return runningOnly(t, e) == `{"running":[]}` })
	status(t, e.post("/api/models/undrain/alpha", ""), http.StatusConflict)
	if b := status(t, e.post("/api/models/drain/alpha", ""), 200); !strings.Contains(b, `"not loaded"`) {
		t.Fatal(b)
	}
}
