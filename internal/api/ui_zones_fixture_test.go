package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixtureServer starts scripts/ui-fixture-server.mjs with args and returns a
// GET that decodes its JSON answer (the test fails on anything but 200).
func fixtureServer(t *testing.T, args ...string) func(p string, v any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(here), "..", "..", "scripts", "ui-fixture-server.mjs")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(node, append([]string{script, "--port", strconv.Itoa(port), "--host", "127.0.0.1"}, args...)...)
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	return func(p string, v any) {
		t.Helper()
		var last error
		for i := 0; i < 50; i++ {
			resp, err := http.Get(base + p)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("GET %s: %d %s", p, resp.StatusCode, b)
				}
				if err := json.Unmarshal(b, v); err != nil {
					t.Fatalf("GET %s: %v", p, err)
				}
				return
			}
			last = err
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("the fixture server did not answer GET %s: %v\n%s", p, last, out)
	}
}

type fxCounts struct{ Total, Reachable, Down, Off, InUse, Free, Loading int }

func (c *fxCounts) UnmarshalJSON(b []byte) error {
	var m map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*c = fxCounts{m["total"], m["reachable"], m["down"], m["off"], m["in_use"], m["free"], m["loading"]}
	return nil
}

// The fixture's Zones states answer what v43 serves: zone_state and group on
// each zone, the one set of counts, the groups with their figures, the
// uptime; fleet draws the Zones-Fleet mockup's 232 zones and its counts.
func TestUIFixtureServerZones(t *testing.T) {
	type nodesDoc struct {
		Nodes []struct {
			Name      string `json:"name"`
			ZoneState string `json:"zone_state"`
			Group     string `json:"group"`
		} `json:"nodes"`
		Counts fxCounts `json:"counts"`
	}
	type group struct {
		Name     string   `json:"name"`
		Count    int      `json:"count"`
		Counts   fxCounts `json:"counts"`
		Accept   string   `json:"accepting"`
		Uptime7d struct {
			Uptime *float64 `json:"uptime"`
		} `json:"uptime_7d"`
	}
	type groupsDoc struct {
		Groups    []group `json:"groups"`
		Ungrouped group   `json:"ungrouped"`
	}
	type uptimeDoc struct {
		Window struct {
			Complete    bool   `json:"complete"`
			CoveredFrom string `json:"covered_from"`
		} `json:"window"`
		Zones []struct{ Name string } `json:"zones"`
	}

	get := fixtureServer(t, "--zones", "fleet")
	var n nodesDoc
	get("/api/nodes", &n)
	if want := (fxCounts{Total: 232, Reachable: 221, Down: 11, Off: 10, InUse: 154, Free: 49, Loading: 8}); n.Counts != want || len(n.Nodes) != 232 {
		t.Errorf("fleet counts %+v over %d zones, want %+v (the mockup's pills)", n.Counts, len(n.Nodes), want)
	}
	for _, z := range n.Nodes {
		if z.ZoneState == "" {
			t.Fatalf("zone %s has no zone_state", z.Name)
		}
	}
	var g groupsDoc
	get("/api/groups", &g)
	names := []string{}
	for _, x := range g.Groups {
		names = append(names, x.Name)
		if x.Name == "rack-a" && (x.Count != 48 || x.Counts.InUse != 30 || x.Counts.Free != 14 || x.Counts.Loading != 1 || x.Counts.Down != 3) {
			t.Errorf("rack-a: %+v, want 48 zones: 30 in use, 14 free, 1 loading, 3 down as drawn", x)
		}
		if x.Name == "gpu-row-1" && (x.Accept != "none" || x.Counts.Off != 10) {
			t.Errorf("the paused group: %+v", x)
		}
	}
	if strings.Join(names, ",") != "gpu-row-1,rack-a,rack-b,rack-c,rack-d" || g.Ungrouped.Count != 28 {
		t.Errorf("fleet groups %v, %d with no group", names, g.Ungrouped.Count)
	}
	var u uptimeDoc
	get("/api/nodes/uptime?days=7", &u)
	if !u.Window.Complete || len(u.Zones) != 232 {
		t.Errorf("fleet uptime: complete %v over %d zones", u.Window.Complete, len(u.Zones))
	}

	get = fixtureServer(t, "--zones", "groups", "--uptime-partial")
	n = nodesDoc{}
	get("/api/nodes", &n)
	states := map[string]string{}
	for _, z := range n.Nodes {
		states[z.Name] = z.ZoneState + "/" + z.Group
	}
	if states["ps5-06"] != "lost/home" || states["ps5-05"] != "set_aside/home" || states["laptop-02"] != "off/" || states["ps5-01"] != "in_use/rack-a" {
		t.Errorf("groups: %v", states)
	}
	u = uptimeDoc{}
	get("/api/nodes/uptime?days=7", &u)
	if u.Window.Complete || u.Window.CoveredFrom == "" {
		t.Errorf("--uptime-partial: %+v", u.Window)
	}

	// live: eight zones and no group, one removed machine counted nowhere
	get = fixtureServer(t, "--zones", "live", "--model-actions", "off")
	n = nodesDoc{}
	get("/api/nodes", &n)
	states = map[string]string{}
	for _, z := range n.Nodes {
		states[z.Name] = z.ZoneState + "/" + z.Group
	}
	if n.Counts.Total != 8 || n.Counts.Off != 1 || states["ps5-old"] != "removed/" || states["ps5-06"] == "lost/" || states["laptop-02"] != "off/" {
		t.Errorf("live: counts %+v, states %v", n.Counts, states)
	}
	g = groupsDoc{}
	get("/api/groups", &g)
	if len(g.Groups) != 0 || g.Ungrouped.Count != 8 {
		t.Errorf("live groups: %+v", g)
	}
	var set struct {
		Settings map[string]any `json:"settings"`
	}
	get("/api/config/settings", &set)
	if set.Settings["ui_model_actions"] != false {
		t.Errorf("--model-actions off: settings %v", set.Settings)
	}
	get = fixtureServer(t, "--zones", "one-group")
	g = groupsDoc{}
	get("/api/groups", &g)
	if len(g.Groups) != 1 || g.Groups[0].Name != "pair" || g.Groups[0].Count != 2 || g.Ungrouped.Count != 6 {
		t.Errorf("one-group: %+v", g)
	}
	set.Settings = nil
	get("/api/config/settings", &set)
	if set.Settings["ui_model_actions"] != true {
		t.Errorf("model actions default: settings %v", set.Settings)
	}
}
