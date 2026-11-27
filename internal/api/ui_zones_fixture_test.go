package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixtureServer starts scripts/ui-fixture-server.mjs with args and returns a
// GET that decodes its JSON answer (the test fails on anything but 200).
func fixtureServer(t *testing.T, args ...string) func(p string, v any) {
	t.Helper()
	get, _ := fixtureStart(t, args...)
	return get
}

// fixtureStart is fixtureServer that also gives the server's base URL.
func fixtureStart(t *testing.T, args ...string) (func(p string, v any), string) {
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
	}, base
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
	get = fixtureServer(t, "--zones", "live")
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
	if _, ok := set.Settings["ui_model_actions"]; ok {
		t.Errorf("the fixture still answers the removed setting ui_model_actions: %v", set.Settings)
	}
	get = fixtureServer(t, "--zones", "one-group")
	g = groupsDoc{}
	get("/api/groups", &g)
	if len(g.Groups) != 1 || g.Groups[0].Name != "pair" || g.Groups[0].Count != 2 || g.Ungrouped.Count != 6 {
		t.Errorf("one-group: %+v", g)
	}
}

// fixturePost starts the fixture server with args and returns a POST that
// gives the status and the decoded answer.
func fixturePost(t *testing.T, args ...string) func(p, body string) (int, map[string]any) {
	t.Helper()
	get, base := fixtureStart(t, args...)
	var warm any
	get("/api/status", &warm) // waits until it answers
	return func(p, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(base+p, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", p, err)
		}
		defer resp.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v
	}
}

// The fixture's model actions answer as the server does (the shapes of
// p4-zones-backend-STATUS, "For the hardware proof"): the plan for chosen
// zones (start; reject with the server's sentence), the load (202; 409
// chosen zone unusable; 409 already loaded or queued; 400 for a zone the
// view does not hold), the drain with and without an estimate and its
// next_load sentence, undrain; the status of a draining and of a pinned
// instance; each zone's attention_rank, chip and last_load_link.
func TestUIFixtureServerModelActions(t *testing.T) {
	errOf := func(v map[string]any) (reason, message string) {
		e, _ := v["error"].(map[string]any)
		reason, _ = e["reason"].(string)
		message, _ = e["message"].(string)
		return
	}
	const body = `{"model":"qwen3-4b","nodes":["ps5-04"]}`
	post := fixturePost(t, "--zones", "live", "--load", "start")
	if st, v := post("/api/plan", body); st != 200 || v["action"] != "start" || v["need_mib"] == nil {
		t.Errorf("plan start: %d %v", st, v)
	}
	if st, v := post("/api/load", body); st != 202 || v["msg"] != "loading" {
		t.Errorf("load: %d %v", st, v)
	}
	if st, v := post("/api/models/drain/gemma-4-27b%3A32k", ``); st != 200 || v["eta_s"] != 40.0 || v["next_load"] == nil {
		t.Errorf("drain: %d %v", st, v)
	}
	if st, v := post("/api/models/undrain/gemma-4-27b%3A32k", ``); st != 409 {
		t.Errorf("undrain with nothing draining: %d %v", st, v)
	} else if r, _ := errOf(v); r != "not_draining" {
		t.Errorf("undrain reason %q", r)
	}
	if st, _ := post("/api/agents", `{}`); st != 405 {
		t.Errorf("other writes: %d, want 405", st)
	}

	post = fixturePost(t, "--zones", "live", "--load", "reject")
	if st, v := post("/api/plan", body); st != 200 || v["action"] != "reject" || !strings.Contains(v["reason"].(string), "is paused") {
		t.Errorf("plan reject: %d %v", st, v)
	}
	if st, v := post("/api/load", body); st != 409 {
		t.Errorf("load refused: %d %v", st, v)
	} else if r, m := errOf(v); r != "chosen zone unusable" || !strings.Contains(m, "cannot be loaded on the chosen zones") {
		t.Errorf("load refused: %q %q", r, m)
	}
	post = fixturePost(t, "--zones", "live", "--load", "loaded")
	if st, v := post("/api/load", body); st != 409 {
		t.Errorf("load already loaded: %d %v", st, v)
	} else if r, _ := errOf(v); r != "already loaded or queued" {
		t.Errorf("load already loaded: %q", r)
	}
	post = fixturePost(t, "--zones", "live", "--load", "unknown")
	if st, v := post("/api/plan", `{"model":"qwen3-4b","nodes":["nope"]}`); st != 400 {
		t.Errorf("plan unknown zone: %d %v", st, v)
	} else if _, m := errOf(v); !strings.Contains(m, "is not a node in the fleet view") {
		t.Errorf("plan unknown zone: %q", m)
	}

	type inst struct {
		Profile  string   `json:"profile"`
		State    string   `json:"state"`
		Drain    bool     `json:"drain"`
		ETA      *float64 `json:"drain_eta_s"`
		Pinned   []string `json:"pinned_nodes"`
		InFlight int      `json:"in_flight"`
	}
	type status struct {
		Instances []inst `json:"instances"`
		Agents    []struct {
			Name     string         `json:"name"`
			Rank     *int           `json:"attention_rank"`
			Chip     string         `json:"chip"`
			LoadLink map[string]any `json:"last_load_link"`
		} `json:"agents"`
	}
	get := fixtureServer(t, "--zones", "live", "--drain", "eta", "--pinned")
	var st status
	get("/api/status", &st)
	if i := st.Instances[0]; i.State != "draining" || !i.Drain || i.ETA == nil || *i.ETA != 40 || len(i.Pinned) != 3 || i.InFlight != 2 {
		t.Errorf("draining, pinned instance: %+v", i)
	}
	at := map[string]int{}
	for k, a := range st.Agents {
		at[a.Name] = k
		if a.Rank == nil {
			t.Errorf("%s has no attention_rank", a.Name)
		}
	}
	if a := st.Agents[at["laptop-01"]]; a.Chip != "RTX 3070 Ti" {
		t.Errorf("laptop-01 chip %q", a.Chip)
	}
	if a := st.Agents[at["ps5-01"]]; a.Chip != "" || a.LoadLink == nil || a.LoadLink["mb_s"] != 104.6 {
		t.Errorf("ps5-01: chip %q, last_load_link %v", a.Chip, a.LoadLink)
	}
	if a := st.Agents[at["ps5-04"]]; a.LoadLink != nil {
		t.Errorf("ps5-04 has a last cold load: %v", a.LoadLink)
	}
	get = fixtureServer(t, "--zones", "live", "--drain", "noeta")
	st = status{}
	get("/api/status", &st)
	if i := st.Instances[0]; i.State != "draining" || i.ETA != nil || i.Pinned != nil {
		t.Errorf("draining without an estimate: %+v", i)
	}
	post = fixturePost(t, "--zones", "live", "--drain", "noeta")
	if st, v := post("/api/models/drain/gemma-4-27b%3A32k", ``); st != 200 || v["eta_s"] != nil {
		t.Errorf("drain without an estimate: %d %v", st, v)
	}
	if st, v := post("/api/models/undrain/gemma-4-27b%3A32k", ``); st != 200 || v["msg"] != "ok" {
		t.Errorf("undrain: %d %v", st, v)
	}
}

// The fixture's Create new zone answers (--pending) have the server's shapes:
// the fields of PendingZone, the state words of pendingStateText, the 400
// sentence NoModelSource, a 409 for a taken name, 404 for an id the
// scheduler forgot, the cancel answer, and the setting default_model_source
// on GET /api/zones/pending and GET /api/config/settings.
// --pending mixed: the list keeps a cancelled zone with its state beside a
// waiting one (as the server does until the expiry); each answers by id.
func TestUIFixtureServerPendingMixed(t *testing.T) {
	get, _ := fixtureStart(t, "--zones", "groups", "--pending", "mixed")
	var list struct {
		Pending []map[string]any `json:"pending"`
	}
	get("/api/zones/pending", &list)
	if len(list.Pending) != 2 || list.Pending[0]["state"] != PendingWaiting || list.Pending[1]["state"] != PendingCancelled ||
		list.Pending[1]["state_text"] != pendingStateText[PendingCancelled] || list.Pending[1]["id"] != "pz-0000000000000002" {
		t.Fatalf("list %+v", list.Pending)
	}
	for id, want := range map[string]string{"pz-0000000000000001": PendingWaiting, "pz-0000000000000002": PendingCancelled} {
		var z map[string]any
		get("/api/zones/pending/"+id, &z)
		if z["state"] != want || z["id"] != id {
			t.Errorf("%s: %v, want %s", id, z["state"], want)
		}
	}
}

func TestUIFixtureServerPending(t *testing.T) {
	keys := func(v map[string]any) string {
		var k []string
		for n := range v {
			k = append(k, n)
		}
		sort.Strings(k)
		return strings.Join(k, ",")
	}
	var want map[string]any
	b, _ := json.Marshal(PendingZone{Group: "g", JoinedNode: "n", JoinedAt: "t"})
	json.Unmarshal(b, &want)

	for _, st := range []string{PendingWaiting, PendingJoined, PendingExpired, PendingCancelled} {
		get, base := fixtureStart(t, "--zones", "groups", "--pending", st)
		var list struct {
			Pending []map[string]any `json:"pending"`
			Default string           `json:"default_model_source"`
		}
		get("/api/zones/pending", &list)
		if len(list.Pending) != 1 || list.Pending[0]["state"] != st || list.Pending[0]["state_text"] != pendingStateText[st] || list.Default != "share:192.0.2.10:/models" {
			t.Fatalf("%s: list %+v", st, list)
		}
		var z map[string]any
		get("/api/zones/pending/pz-0000000000000001", &z)
		if st == PendingJoined {
			if keys(z) != keys(want) {
				t.Errorf("fields: fixture %s, server %s", keys(z), keys(want))
			}
		}
		if c, _ := z["command"].(string); !strings.HasPrefix(c, "curl -sSL 'http://192.0.2.1:8080/install.sh?zone=pz-") || !strings.HasSuffix(c, "' | sudo sh") {
			t.Errorf("command %q", c)
		}
		var s struct {
			Settings map[string]any `json:"settings"`
		}
		get("/api/config/settings", &s)
		if s.Settings["default_model_source"] != "share:192.0.2.10:/models" {
			t.Errorf("setting %v", s.Settings["default_model_source"])
		}
		if st == PendingWaiting {
			req, _ := http.NewRequest(http.MethodDelete, base+"/api/zones/pending/pz-0000000000000001", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var c map[string]any
			json.NewDecoder(resp.Body).Decode(&c)
			resp.Body.Close()
			if resp.StatusCode != 200 || c["state"] != PendingCancelled {
				t.Errorf("cancel: %d %v", resp.StatusCode, c)
			}
			post := fixturePost(t, "--zones", "groups", "--pending", st)
			if code, v := post("/api/zones/pending", `{"name":"zone-7","ttl_min":30,"comment":"x"}`); code != 201 || v["name"] != "zone-7" || v["state"] != PendingWaiting {
				t.Errorf("create: %d %v", code, v)
			}
		}
	}
	msg := func(v map[string]any) string {
		e, _ := v["error"].(map[string]any)
		m, _ := e["message"].(string)
		return m
	}
	post := fixturePost(t, "--zones", "groups", "--pending", "nosource")
	if code, v := post("/api/zones/pending", `{"name":"zone-7"}`); code != 400 || msg(v) != NoModelSource {
		t.Errorf("no source: %d %v", code, v)
	}
	post = fixturePost(t, "--zones", "groups", "--pending", "taken")
	if code, v := post("/api/zones/pending", `{"name":"zone-7","model_source":"push"}`); code != 409 || !strings.HasPrefix(msg(v), "the name zone-7 is already taken by the zone ") {
		t.Errorf("taken: %d %v", code, v)
	}
	get, base := fixtureStart(t, "--zones", "groups", "--pending", "gone")
	var warm any
	get("/api/status", &warm)
	if resp, err := http.Get(base + "/api/zones/pending/pz-0000000000000001"); err != nil || resp.StatusCode != 404 {
		t.Errorf("gone: %v %v", resp, err)
	}
}

// The fixture's agents report the machine's own uptime (host_uptime_s) when
// reachable; --no-host-uptime: none does, as an older agent or a zone from
// the legacy fleet feed (the zone panel then leaves the Uptime tile out).
func TestUIFixtureServerHostUptime(t *testing.T) {
	for _, off := range []bool{false, true} {
		args := []string{"--zones", "groups"}
		if off {
			args = append(args, "--no-host-uptime")
		}
		get := fixtureServer(t, args...)
		var st struct {
			Agents []map[string]any `json:"agents"`
		}
		get("/api/status", &st)
		with := 0
		for _, a := range st.Agents {
			up, has := a["host_uptime_s"].(float64)
			if has {
				with++
				if a["state"] != "active" || up <= 0 {
					t.Errorf("off=%v: %v reports %v while %v", off, a["name"], up, a["state"])
				}
			}
		}
		if off && with != 0 || !off && with == 0 {
			t.Errorf("off=%v: %d agents report host_uptime_s", off, with)
		}
	}
}
