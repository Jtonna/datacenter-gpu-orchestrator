package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

type memAudit struct {
	mu sync.Mutex
	es []audit.Entry
}

func (m *memAudit) Record(_ context.Context, e audit.Entry) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.es = append(m.es, e)
	return int64(len(m.es)), nil
}
func (m *memAudit) Query(context.Context, cfgstore.Filter) ([]audit.Entry, int, error) {
	return nil, 0, nil
}
func (m *memAudit) GetEntry(context.Context, int64) (*audit.Entry, error) { return nil, nil }

func agentsEnv(t *testing.T, mods ...func(*Options)) (*env, *pool.Pool, *memAudit) {
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT-BINARY")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("RPC-SERVER")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d}
	p, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: rel}, PotluckRelease: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	au := &memAudit{}
	e := newEnv(t, chatOK, func(o *Options) {
		o.Pool, o.Audit = p, au
		for _, m := range mods {
			m(o)
		}
	})
	p.SetForceUnload(e.sched.ForceUnloadNode)
	return e, p, au
}

func register(t *testing.T, e *env, id string) agentproto.RegisterAnswer {
	req := agentproto.RegisterRequest{ID: id, Name: "console-1", AgentRelease: "0.1.0", AgentStartID: "s1", OS: "linux", Arch: "amd64",
		Install: "bare", Addr: "192.0.2.101", Backends: []string{"vulkan"}}
	req.FillEmpty()
	b, _ := json.Marshal(req)
	resp := e.post(agentproto.PathRegister, string(b))
	if resp.StatusCode != 200 {
		t.Fatalf("register: %d %s", resp.StatusCode, body(t, resp))
	}
	var a agentproto.RegisterAnswer
	json.Unmarshal([]byte(body(t, resp)), &a)
	return a
}

func TestAgentEndpointsOffInFleetMembership(t *testing.T) {
	e := newEnv(t, chatOK)
	for _, c := range []struct{ method, path string }{
		{"POST", agentproto.PathRegister}, {"POST", "/api/pool/nodes/02:00:00:00:00:01/heartbeat"}, {"GET", agentproto.PathRelease},
		{"POST", "/api/nodes/n1/pause"}, {"POST", "/api/nodes/n1/force-unload"}, {"DELETE", "/api/nodes/n1"},
	} {
		req, _ := http.NewRequest(c.method, e.front.URL+c.path, strings.NewReader(`{"comment":"x"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if b := body(t, resp); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(b, "membership is fleet") {
			t.Errorf("%s %s: %d %s", c.method, c.path, resp.StatusCode, b)
		}
	}
	// The AI-agent identity routes on the same prefix are untouched.
	if resp := e.get("/api/agents"); resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/agents: %d", resp.StatusCode)
	}
}

func TestAgentRegisterHeartbeatAndPrograms(t *testing.T) {
	e, _, _ := agentsEnv(t)
	a := register(t, e, "02:00:00:00:00:01")
	if !a.Accepted || a.Platform != "linux-amd64-vulkan" || len(a.Programs) != 2 {
		t.Fatalf("%+v", a)
	}
	var rpc agentproto.ProgramEntry
	for _, p := range a.Programs {
		if p.Name == "rpc-server" {
			rpc = p
		}
	}
	resp := e.get(rpc.Path)
	if b := body(t, resp); resp.StatusCode != 200 || b != "RPC-SERVER" || resp.Header.Get("X-Checksum-Sha256") != rpc.SHA256 {
		t.Fatalf("download: %d %q", resp.StatusCode, b)
	}
	for _, bad := range []string{"/api/pool/programs/0.1.0/linux-amd64-vulkan/SHA256SUMS", "/api/pool/programs/0.2.0/linux-amd64-vulkan/rpc-server",
		"/api/pool/programs/0.1.0/linux-amd64-vulkan/..%2frelease.json"} {
		if resp := e.get(bad); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
	resp = e.get(agentproto.PathRelease)
	var ri agentproto.ReleaseInfo
	json.Unmarshal([]byte(body(t, resp)), &ri)
	if ri.Release != "0.1.0" || ri.Agents["linux-amd64"].Path != "/api/pool/programs/0.1.0/agent/potluck-agent-linux-amd64" {
		t.Fatalf("%+v", ri)
	}
	resp = e.post("/api/pool/nodes/02:00:00:00:00:01/heartbeat", `{"agent_start_id":"s1","devices":[],"stats":{"stats_version":7}}`)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, `"release":"0.1.0"`) {
		t.Fatalf("heartbeat %d %s", resp.StatusCode, b)
	}
	if resp := e.post("/api/pool/nodes/02:00:00:00:00:09/heartbeat", `{}`); resp.StatusCode != http.StatusGone {
		t.Fatalf("unknown heartbeat %d", resp.StatusCode)
	}
	st := body(t, e.get("/api/status"))
	if !strings.Contains(st, `"agents":[`) || !strings.Contains(st, "stats version not supported") {
		t.Fatalf("status %s", st)
	}
	if resp := e.post("/api/pool/nodes/02:00:00:00:00:01/deregister", ``); resp.StatusCode != 200 {
		t.Fatalf("deregister %d", resp.StatusCode)
	}
	if st := body(t, e.get("/api/status")); !strings.Contains(st, `"state":"left"`) {
		t.Fatalf("status %s", st)
	}
}

func TestNodeActionsNeedACommentAndAreAudited(t *testing.T) {
	e, p, au := agentsEnv(t)
	register(t, e, "02:00:00:00:00:01")
	if resp := e.post("/api/nodes/02:00:00:00:00:01/pause", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no comment: %d", resp.StatusCode)
	}
	if resp := e.post("/api/nodes/02:00:00:00:00:01/pause", `{"comment":"fan is loud"}`); resp.StatusCode != 200 {
		t.Fatalf("pause: %d %s", resp.StatusCode, body(t, resp))
	}
	if f := p.Facts(); !f[0].Paused {
		t.Fatalf("%+v", f)
	}
	req, _ := http.NewRequest("POST", e.front.URL+"/api/nodes/02:00:00:00:00:01/unpause", nil)
	req.Header.Set("X-Comment", "quiet again")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("unpause: %v %v", resp, err)
	}
	if resp := e.post("/api/nodes/02:00:00:00:00:01/force-unload", `{"comment":"stuck"}`); resp.StatusCode != 200 {
		t.Fatalf("force: %d %s", resp.StatusCode, body(t, resp))
	}
	if resp := e.post("/api/nodes/02:00:00:00:00:99/pause", `{"comment":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown node: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("DELETE", e.front.URL+"/api/nodes/02:00:00:00:00:01", strings.NewReader(`{"comment":"sold"}`))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("remove: %v %v", resp, err)
	}
	if a := register(t, e, "02:00:00:00:00:01"); a.Accepted || a.Reason != agentproto.ReasonRemoved {
		t.Fatalf("%+v", a)
	}
	if resp := e.post("/api/nodes/02:00:00:00:00:01/readmit", `{"comment":"back"}`); resp.StatusCode != 200 {
		t.Fatalf("readmit: %d", resp.StatusCode)
	}
	if a := register(t, e, "02:00:00:00:00:01"); !a.Accepted {
		t.Fatalf("%+v", a)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var got []string
	for _, x := range au.es {
		if x.Section != SectionNodes || x.Target != "node:02:00:00:00:00:01" || x.Comment == "" {
			t.Fatalf("entry %+v", x)
		}
		got = append(got, x.Action+"="+x.Comment)
	}
	want := "pause=fan is loud,unpause=quiet again,force_unload=stuck,delete=sold,readmit=back"
	if strings.Join(got, ",") != want {
		t.Fatalf("audit %v", got)
	}
}

// The release answer is read on consoles by a shell script with sed: each
// platform's flat object on one line, an absolute path (never a URL), a
// lower-case 64-hex sha256 and no escapes in any string.
func TestAgentReleaseAnswerIsSedReadable(t *testing.T) {
	e, _, _ := agentsEnv(t)
	resp := e.get(agentproto.PathRelease)
	raw := body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if strings.Contains(raw, `\`) {
		t.Fatalf("escape in %q", raw)
	}
	entry := regexp.MustCompile(`"linux-amd64":\{"path":"(/[^"]*)","sha256":"([0-9a-f]{64})"\}`)
	found := false
	for _, line := range strings.Split(raw, "\n") {
		if m := entry.FindStringSubmatch(line); m != nil {
			found = true
			if strings.Contains(m[1], "://") {
				t.Fatalf("path is a URL: %s", m[1])
			}
		}
	}
	if !found {
		t.Fatalf("no one-line flat linux-amd64 entry in %q", raw)
	}
}

// CR-9 review, B7: stop_reason and stop_message from the agent's heartbeat
// are in the node's entry of agents in GET /api/status.
func TestStatusShowsTheAgentsStopReason(t *testing.T) {
	e, _, _ := agentsEnv(t)
	register(t, e, "02:00:00:00:00:01")
	hb := `{"agent_start_id":"s1","role":"worker","launch_id":null,"devices":[],` +
		`"stop_reason":"watchdog","stop_message":"MemAvailable 912 MiB fell below the watchdog floor of 1000 MiB"}`
	if resp := e.post("/api/pool/nodes/02:00:00:00:00:01/heartbeat", hb); resp.StatusCode != 200 {
		t.Fatalf("heartbeat %d", resp.StatusCode)
	}
	var st struct {
		Agents []struct {
			ID          string `json:"id"`
			StopReason  string `json:"stop_reason"`
			StopMessage string `json:"stop_message"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body(t, e.get("/api/status"))), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0].StopReason != "watchdog" || !strings.Contains(st.Agents[0].StopMessage, "912 MiB") {
		t.Fatalf("agents %+v", st.Agents)
	}
	// Without the fields (a running launch, or a release 0.1.0 agent) they are absent.
	e.post("/api/pool/nodes/02:00:00:00:00:01/heartbeat", `{"agent_start_id":"s1","devices":[]}`)
	if b := body(t, e.get("/api/status")); strings.Contains(b, `"stop_reason"`) {
		t.Fatalf("stop_reason still shown: %s", b)
	}
}
