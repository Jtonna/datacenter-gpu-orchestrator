package api

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
)

// A pending zone never takes a name or id Potluck already knows: an active
// zone, a removed one, one known from an earlier run (lost until it checks
// in again), or another waiting pending zone. Case does not matter.
func TestPendingZone_NameTaken(t *testing.T) {
	e, p, _ := groupsEnv(t)
	registerNamed(t, e, "02:00:00:00:00:01", "zone-a")
	registerNamed(t, e, "02:00:00:00:00:02", "zone-b")
	req, _ := http.NewRequest("DELETE", e.front.URL+"/api/nodes/02:00:00:00:00:02", strings.NewReader(`{"comment":"sold"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	status(t, resp, http.StatusOK)
	registerNamed(t, e, "zone-id7", "zone-x") // a zone whose id is a name
	p.SetKnown([]pool.KnownNode{{ID: "02:00:00:00:00:07", Name: "zone-old", LastSeen: time.Now().Add(-time.Hour)}})

	for _, name := range []string{"zone-a", "ZONE-A", "zone-b", "Zone-Old", "ZONE-ID7"} {
		got := status(t, e.post("/api/zones/pending", `{"name":"`+name+`","model_source":"push"}`), http.StatusConflict)
		if !strings.Contains(got, "already") {
			t.Fatalf("%s: %s", name, got)
		}
	}
	status(t, e.post("/api/zones/pending", `{"name":"zone-c","model_source":"push"}`), http.StatusCreated)
	status(t, e.post("/api/zones/pending", `{"name":"Zone-C","model_source":"push"}`), http.StatusConflict)
	var list struct {
		Pending []PendingZone `json:"pending"`
	}
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending"), 200)), &list)
	if len(list.Pending) != 1 || list.Pending[0].Name != "zone-c" {
		t.Fatalf("%+v", list)
	}
}

// Only a machine new to Potluck joins a pending zone: a zone Potluck
// already knows that re-registers under the pending name gets nothing from
// it (no group), and the pending zone keeps waiting.
func TestPendingZone_KnownMachineDoesNotJoin(t *testing.T) {
	e, _, _ := groupsEnv(t)
	status(t, e.post("/api/groups", `{"name":"rack-a"}`), http.StatusCreated)
	registerNamed(t, e, "02:00:00:00:00:01", "zone-a")
	var z PendingZone
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-new","group":"rack-a","model_source":"push"}`), http.StatusCreated)), &z)
	registerNamed(t, e, "02:00:00:00:00:01", "zone-new") // the known machine renamed itself
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending/"+z.ID), 200)), &z)
	if z.State != PendingWaiting || nodeGroup(t, e, "02:00:00:00:00:01") != "" {
		t.Fatalf("a known machine joined: %+v group %q", z, nodeGroup(t, e, "02:00:00:00:00:01"))
	}
	registerNamed(t, e, "02:00:00:00:00:04", "zone-new")
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending/"+z.ID), 200)), &z)
	if z.State != PendingJoined || z.JoinedNode != "02:00:00:00:00:04" || nodeGroup(t, e, "02:00:00:00:00:04") != "rack-a" {
		t.Fatalf("the new machine did not join: %+v", z)
	}
}

// The script route answers a zone it does not know (a restart forgets
// pending zones) or one no longer waiting with a short script that says so
// and exits 1, so the person running the command reads the sentence; the
// status stays 404 or 410.
func TestPendingZone_ScriptGoneSays(t *testing.T) {
	e, _, _ := groupsEnv(t)
	var z PendingZone
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-new","model_source":"push"}`), http.StatusCreated)), &z)
	if !strings.HasPrefix(z.Command, "curl -sSL 'http://") {
		t.Fatalf("command %q", z.Command)
	}
	status(t, e.send("DELETE", "/api/zones/pending/"+z.ID, `{"comment":"x"}`), 200)
	for _, c := range []struct {
		path, want string
		code       int
	}{
		{"/install.sh?zone=pz-0000000000000000", PendingNotFound, http.StatusNotFound},
		{"/install.sh", PendingNotFound, http.StatusNotFound},
		{z.ScriptPath, "this zone was cancelled: create it again in Potluck", http.StatusGone},
	} {
		resp := e.get(c.path)
		got := status(t, resp, c.code)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/x-shellscript") || !strings.Contains(got, c.want) {
			t.Fatalf("%s: %s %q", c.path, resp.Header.Get("Content-Type"), got)
		}
		if sh, err := exec.LookPath("sh"); err == nil {
			cmd := exec.Command(sh)
			cmd.Stdin = strings.NewReader(got)
			out, err := cmd.CombinedOutput()
			if err == nil || strings.TrimSpace(string(out)) != "potluck: "+c.want {
				t.Fatalf("%s: sh %v %q", c.path, err, out)
			}
		}
	}
	if PendingNotFound != "this zone was not found: create it again in Potluck" {
		t.Fatal(PendingNotFound)
	}
}
