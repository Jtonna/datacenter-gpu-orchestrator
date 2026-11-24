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

// Load on chosen nodes: plan first, then load; the instance names them;
// a second pinned load while it is loaded is 409; unknown nodes are 400.
func TestLoadOnChosenNodes(t *testing.T) {
	e := newEnv(t, chatOK)
	status(t, e.post("/api/plan", `{"model":"alpha","nodes":["nope"]}`), http.StatusBadRequest)
	status(t, e.post("/api/load", `{"model":"alpha","nodes":["host-1","host-1"]}`), http.StatusBadRequest)
	if b := status(t, e.post("/api/plan", `{"model":"alpha","nodes":["host-1"]}`), 200); !strings.Contains(b, `"action":"start"`) {
		t.Fatalf("plan %s", b)
	}
	if b := status(t, e.post("/api/load", `{"model":"alpha","nodes":["host-1"]}`), http.StatusAccepted); !strings.Contains(b, `"nodes":["host-1"]`) {
		t.Fatalf("load %s", b)
	}
	eventually(t, "pinned_nodes on the status", func() bool {
		return strings.Contains(body(t, e.get("/api/status")), `"pinned_nodes":["host-1"]`)
	})
	status(t, e.post("/api/load", `{"model":"alpha","nodes":["host-1"]}`), http.StatusConflict)
	if b := status(t, e.post("/api/plan", `{"model":"alpha","nodes":["host-1"]}`), 200); !strings.Contains(b, `"action":"reject"`) {
		t.Fatalf("plan while loaded %s", b)
	}
}
