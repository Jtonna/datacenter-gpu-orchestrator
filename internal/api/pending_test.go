package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	agentdeploy "github.com/Jtonna/datacenter-gpu-orchestrator/deploy/agent"
)

// A zone made before its machine: the served script fills in Potluck's
// address, the name and the model source; the first agent with that name
// joins it and lands in its group.
func TestPendingZone_CreateServeJoin(t *testing.T) {
	e, _, au := groupsEnv(t)
	status(t, e.post("/api/groups", `{"name":"rack-a"}`), http.StatusCreated)
	status(t, e.post("/api/zones/pending", `{"name":"zone-9"}`), http.StatusBadRequest) // no model source known yet
	status(t, e.post("/api/zones/pending", `{"name":"zone 9","model_source":"push"}`), http.StatusBadRequest)
	status(t, e.post("/api/zones/pending", `{"name":"zone-9","group":"nope","model_source":"push"}`), http.StatusNotFound)
	status(t, e.post("/api/zones/pending", `{"name":"zone-9","model_source":"share:bad'quote:/x"}`), http.StatusBadRequest)
	var z PendingZone
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-9","group":"rack-a","model_source":"share:192.0.2.1:/srv/models","comment":"new box"}`), http.StatusCreated)), &z)
	host := strings.TrimPrefix(e.front.URL, "http://")
	if z.State != PendingWaiting || z.Group != "rack-a" || z.Mode != "bare" || !strings.HasPrefix(z.ID, "pz-") ||
		z.Command != "curl -sSL 'http://"+host+"/install.sh?zone="+z.ID+"' | sudo sh" || z.DownloadName != "potluck-zone-zone-9.sh" {
		t.Fatalf("%+v", z)
	}
	status(t, e.post("/api/zones/pending", `{"name":"ZONE-9","model_source":"push"}`), http.StatusConflict)

	resp := e.get(z.DownloadPath)
	script := status(t, resp, 200)
	if resp.Header.Get("Content-Disposition") != `attachment; filename="potluck-zone-zone-9.sh"` ||
		!strings.Contains(script, "potluck='"+host+"'\n") ||
		!strings.Contains(script, "sh \"$setup\" --bare --potluck \"$potluck\" --model-source 'share:192.0.2.1:/srv/models' --name 'zone-9' \"$@\"") ||
		!strings.Contains(script, "\"http://$potluck/install/potluck-agent-setup\"") {
		t.Fatalf("script:\n%s", script)
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -n: %v %s", err, out)
		}
	}
	if b := status(t, e.get("/install/potluck-agent-setup"), 200); !bytes.Equal([]byte(b), agentdeploy.Setup) {
		t.Fatal("setup script differs")
	}
	status(t, e.get("/install.sh?zone=pz-nope"), http.StatusNotFound)

	registerNamed(t, e, "02:00:00:00:00:09", "zone-9")
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending/"+z.ID), 200)), &z)
	if z.State != PendingJoined || z.JoinedNode != "02:00:00:00:00:09" || nodeGroup(t, e, "02:00:00:00:00:09") != "rack-a" {
		t.Fatalf("after the join %+v", z)
	}
	status(t, e.get(z.ScriptPath), http.StatusGone)
	status(t, e.send("DELETE", "/api/zones/pending/"+z.ID, ""), http.StatusConflict)
	au.mu.Lock()
	var acts []string
	for _, en := range au.es {
		if en.Section == SectionGroups {
			acts = append(acts, en.Action+" "+en.Target)
		}
	}
	au.mu.Unlock()
	if want := "create group:rack-a,create_pending pending:" + z.ID + ",add_members group:rack-a"; strings.Join(acts, ",") != want {
		t.Fatalf("audit %v", acts)
	}
	// The groups document carries the pending zones.
	if !strings.Contains(body(t, e.get("/api/groups")), `"pending_zones":[{"id":"`+z.ID) {
		t.Fatal("no pending_zones on /api/groups")
	}
}

// A waiting pending zone expires after its time, or is cancelled.
func TestPendingZone_ExpireAndCancel(t *testing.T) {
	e, _, _ := groupsEnv(t)
	var a, b PendingZone
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-a","model_source":"push","ttl_min":5}`), 201)), &a)
	json.Unmarshal([]byte(status(t, e.post("/api/zones/pending", `{"name":"zone-b","model_source":"dir:/srv/models","mode":"docker"}`), 201)), &b)
	status(t, e.post("/api/zones/pending", `{"name":"zone-c","model_source":"push","ttl_min":2}`), http.StatusBadRequest)
	status(t, e.send("DELETE", "/api/zones/pending/"+b.ID, ""), 200)
	e.advance(6 * time.Minute)
	var l struct {
		Pending []PendingZone `json:"pending"`
	}
	json.Unmarshal([]byte(status(t, e.get("/api/zones/pending"), 200)), &l)
	states := map[string]string{}
	for _, z := range l.Pending {
		states[z.Name] = z.State
	}
	if states["zone-a"] != PendingExpired || states["zone-b"] != PendingCancelled {
		t.Fatalf("%v", states)
	}
	// An expired name may be made again.
	status(t, e.post("/api/zones/pending", `{"name":"zone-a","model_source":"push"}`), 201)
}
