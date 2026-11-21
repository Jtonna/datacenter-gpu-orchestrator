package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/release"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

// logStore is a fake log store: it keeps the query and the body of what it
// got and counts the requests in flight.
type logStore struct {
	srv *httptest.Server

	mu     sync.Mutex
	got    []storedLog
	delay  time.Duration
	silent bool // never answer; wait for the caller to give up

	in, peak, total atomic.Int32
}

type storedLog struct {
	path  string
	query map[string]string
	body  string
}

func newLogStore(t *testing.T) *logStore {
	s := &logStore{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.peak.Store(max(s.peak.Load(), s.in.Add(1)))
		defer s.in.Add(-1)
		s.total.Add(1)
		b, _ := io.ReadAll(r.Body)
		q := map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		s.mu.Lock()
		if len(s.got) < 64 {
			s.got = append(s.got, storedLog{r.URL.Path, q, string(b)})
		}
		delay, silent := s.delay, s.silent
		s.mu.Unlock()
		if silent {
			<-r.Context().Done()
			return
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *logStore) stored() []storedLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storedLog(nil), s.got...)
}

// logsEnv is agentsEnv with central logging on: a relay in front of a fake
// store, wired as the scheduler binary wires it (the pool hands out the
// relay's logs object; the relay reads the pool's snapshot and the
// scheduler's configuration snapshot).
func logsEnv(t *testing.T, mod ...func(*logrelay.Options)) (*env, *pool.Pool, *logStore) {
	t.Helper()
	root := progtree.Write(t, t.TempDir(), progtree.Tree{Release: "0.1.0", Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("AGENT-BINARY")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("RPC-SERVER")},
		}})
	rel, err := release.Load(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	store := newLogStore(t)
	var (
		e *env
		p atomic.Pointer[pool.Pool]
	)
	node, launch := LogLookups(p.Load)
	lo := logrelay.Options{StoreURL: store.srv.URL, Node: node, Launch: launch,
		Config: func() config.Logs { return e.sched.Config().Logs }}
	for _, m := range mod {
		m(&lo)
	}
	relay, err := logrelay.New(lo)
	if err != nil {
		t.Fatal(err)
	}
	d := config.DefaultDefaults()
	d.Membership = config.MembershipAgents
	cfg := &config.Config{Defaults: d}
	pl, err := pool.New(pool.Options{Config: func() *config.Config { return cfg }, Programs: pool.Programs{Release: rel}, PotluckRelease: "0.1.0",
		Logs: relay.Object, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The pool's call-back check must not leave the test process.
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no network in tests") }})
	if err != nil {
		t.Fatal(err)
	}
	p.Store(pl)
	e = newEnv(t, chatOK, func(o *Options) { o.Pool, o.Audit, o.Logs = pl, &memAudit{}, relay })
	pl.SetForceUnload(e.sched.ForceUnloadNode)
	return e, pl, store
}

func (e *env) ldo(method, path, body string, hdr ...string) (int, string, http.Header) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.front.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, body2(e.t, resp), resp.Header
}

const logLines = `{"_msg":"registered","level":"info","unit":"potluck-agent.service"}` + "\n"

// Without a log store address nothing changes: no logs object, no
// logs_revision, no logs in status, and the new routes answer 404.
func TestLogsOffChangesNothing(t *testing.T) {
	e, _, _ := agentsEnv(t)
	const id = "02:00:00:00:00:01"
	resp := e.post(agentproto.PathRegister, func() string {
		req := agentproto.RegisterRequest{ID: id, Name: "console-1", AgentRelease: "0.1.0", AgentStartID: "s1", OS: "linux", Arch: "amd64",
			Install: "bare", Addr: "192.0.2.101", Backends: []string{"vulkan"}}
		req.FillEmpty()
		b, _ := json.Marshal(req)
		return string(b)
	}())
	if b := body(t, resp); resp.StatusCode != 200 || strings.Contains(b, "logs") {
		t.Fatalf("registration answer: %d %s", resp.StatusCode, b)
	}
	resp = e.post("/api/pool/nodes/"+id+"/heartbeat", `{"agent_start_id":"s1","devices":[]}`)
	if b := body(t, resp); strings.TrimSpace(b) != `{"release":"0.1.0","paused":false}` {
		t.Fatalf("heartbeat answer: %s", b)
	}
	if st := body(t, e.get("/api/status")); strings.Contains(st, `"logs"`) {
		t.Fatalf("status names logs: %s", st)
	}
	for _, c := range [][2]string{{"GET", "/api/pool/nodes/" + id + "/logs"}, {"POST", "/api/pool/nodes/" + id + "/logs?source=agent"},
		{"POST", "/api/logs"}, {"GET", "/api/logs/config"}, {"GET", "/api/logs/query?query=*"}, {"GET", "/logs/select/vmui/"}, {"POST", "/logs/select/logsql/query"}} {
		if st, b, _ := e.ldo(c[0], c[1], logLines, agentproto.HeaderAgentStartID, "s1"); st != http.StatusNotFound {
			t.Errorf("%s %s: %d %s", c[0], c[1], st, b)
		}
	}
}

// The same with a relay that has no store address (the scheduler binary
// always builds one).
func TestLogsRelayWithoutAStoreIsOff(t *testing.T) {
	e, _, _ := logsEnv(t, func(o *logrelay.Options) { o.StoreURL = "" })
	const id = "02:00:00:00:00:01"
	if a := register(t, e, id); !a.Accepted || a.Logs != nil {
		t.Fatalf("registration answer: %+v", a)
	}
	resp := e.post("/api/pool/nodes/"+id+"/heartbeat", `{"agent_start_id":"s1","devices":[]}`)
	if b := body(t, resp); strings.TrimSpace(b) != `{"release":"0.1.0","paused":false}` {
		t.Fatalf("heartbeat answer: %s", b)
	}
	if st := body(t, e.get("/api/status")); strings.Contains(st, `"logs"`) {
		t.Fatalf("status names logs: %s", st)
	}
	if st, _, _ := e.ldo("POST", "/api/pool/nodes/"+id+"/logs?source=agent", logLines, agentproto.HeaderAgentStartID, "s1"); st != http.StatusNotFound {
		t.Fatalf("push: %d", st)
	}
}

// The whole path on the real API: the registration hands out the object, the
// heartbeat its revision, the route identifies the sender like the heartbeat
// and the store gets the trusted fields from the pool.
func TestLogsThroughTheAPI(t *testing.T) {
	e, _, store := logsEnv(t)
	const id = "02:00:00:00:00:01"
	a := register(t, e, id)
	if a.Logs == nil || a.Logs.URL != "/api/pool/nodes/"+id+"/logs" || a.Logs.Format != "jsonline" || a.Logs.Revision <= 0 ||
		a.Logs.Limits.MaxBatchBytes != 1<<20 || a.Logs.Limits.MaxLineBytes != 8192 || a.Logs.Limits.MaxBytesPerS != 262144 || a.Logs.Limits.EveryS != 2 {
		t.Fatalf("logs object in the registration answer: %+v", a.Logs)
	}
	hb := `{"agent_start_id":"s1","devices":[],"logs":{"lines_sent":40,"lines_dropped":1,"last_error":"503","last_error_age_s":9}}`
	resp := e.post("/api/pool/nodes/"+id+"/heartbeat", hb)
	var ha agentproto.HeartbeatAnswer
	if b := body(t, resp); json.Unmarshal([]byte(b), &ha) != nil || ha.LogsRevision != a.Logs.Revision {
		t.Fatalf("heartbeat answer: %s", b)
	}
	st, b, _ := e.ldo("GET", a.Logs.URL, "", agentproto.HeaderAgentStartID, "s1")
	var again agentproto.Logs
	if json.Unmarshal([]byte(b), &again); st != 200 || again.Revision != a.Logs.Revision || again.URL != a.Logs.URL {
		t.Fatalf("GET object: %d %s", st, b)
	}

	if st, b, _ := e.ldo("POST", a.Logs.URL+"?source=agent", logLines, agentproto.HeaderAgentStartID, "s1"); st != http.StatusNoContent || b != "" {
		t.Fatalf("push: %d %s", st, b)
	}
	got := store.stored()
	if len(got) != 1 || got[0].path != "/insert/jsonline" || got[0].body != logLines ||
		got[0].query["extra_fields"] != "node_id="+id+",node_name=console-1,platform=linux-amd64-vulkan,release=0.1.0,source=agent" ||
		got[0].query["_stream_fields"] != "source,node_id,unit" || got[0].query["ignore_fields"] != "launch_id,profile,actor,client_addr" {
		t.Fatalf("store got %+v", got)
	}
	// Identified like the heartbeat.
	for name, c := range map[string][2]string{"stale start id": {id, "s0"}, "unknown node": {"02:00:00:00:00:09", "s1"}} {
		if st, b, _ := e.ldo("POST", "/api/pool/nodes/"+c[0]+"/logs?source=agent", logLines, agentproto.HeaderAgentStartID, c[1]); st != http.StatusGone {
			t.Errorf("%s: %d %s", name, st, b)
		}
	}
	if st, _, _ := e.ldo("POST", a.Logs.URL+"?source=nonsense", logLines, agentproto.HeaderAgentStartID, "s1"); st != http.StatusBadRequest {
		t.Fatalf("bad source: %d", st)
	}

	// A client.
	if st, b, _ := e.ldo("POST", "/api/logs", logLines, "X-Actor", "build-bot"); st != http.StatusNoContent {
		t.Fatalf("client push: %d %s", st, b)
	}
	if g := store.stored()[1]; g.query["extra_fields"] != "source=client,client_addr=127.0.0.1,actor=build-bot" ||
		g.query["ignore_fields"] != "node_id,node_name,platform,release,launch_id,profile" {
		t.Fatalf("store got %+v", g)
	}
	if st, b, _ := e.ldo("GET", "/api/logs/config", ""); st != 200 || !strings.Contains(b, `"url":"/api/logs"`) {
		t.Fatalf("client config: %d %s", st, b)
	}

	// Status: per node what Potluck counted and what the agent reports.
	var status struct {
		Agents []struct {
			ID   string         `json:"id"`
			Logs *pool.NodeLogs `json:"logs"`
		} `json:"agents"`
		Logs *logrelay.Status `json:"logs"`
	}
	raw := body(t, e.get("/api/status"))
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 1 || status.Agents[0].Logs == nil {
		t.Fatalf("status %s", raw)
	}
	nl := status.Agents[0].Logs
	if nl.AcceptedBytes != int64(len(logLines)) || nl.AcceptedRequests != 1 || nl.Refused[logrelay.ReasonBadRequest] != 1 ||
		nl.Agent == nil || nl.Agent.LinesSent != 40 || nl.Agent.LinesDropped != 1 || nl.Agent.LastError != "503" {
		t.Fatalf("node logs in status: %+v agent %+v", nl, nl.Agent)
	}
	if l := status.Logs; l == nil || !l.Enabled || !l.Store || l.Revision != a.Logs.Revision || l.MaxInFlight != 8 || l.InFlight != 0 ||
		l.Clients.AcceptedRequests != 1 || l.UnknownSender != 2 {
		t.Fatalf("logs in status: %+v", l)
	}

	// A node that left is no longer a sender.
	if resp := e.post("/api/pool/nodes/"+id+"/deregister", ``); resp.StatusCode != 200 {
		t.Fatalf("deregister %d", resp.StatusCode)
	}
	if st, _, _ := e.ldo("POST", a.Logs.URL+"?source=agent", logLines, agentproto.HeaderAgentStartID, "s1"); st != http.StatusGone {
		t.Fatalf("push after leaving: %d", st)
	}
}

// The logging settings are live: a change shows in the next answer, with a
// new revision, without a restart.
func TestLogsSettingsAreLive(t *testing.T) {
	store := newLogStore(t)
	var e *env
	relay, err := logrelay.New(logrelay.Options{StoreURL: store.srv.URL, Config: func() config.Logs { return e.sched.Config().Logs }})
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ = cfgEnvWith(t, func(o *Options) { o.Logs = relay })
	rev0 := relay.Revision()
	if st, b, _ := e.ldo("GET", "/api/logs/config", ""); st != 200 || !strings.Contains(b, fmt.Sprintf(`"revision":%d`, rev0)) {
		t.Fatalf("client config: %d %s", st, b)
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"logs_retention_days":30}`, nil); st != 200 {
		t.Fatalf("patch: %d %v", st, m)
	}
	rev1 := relay.Revision()
	if rev1 == rev0 || rev1 == 0 {
		t.Fatalf("revision %d after a change, was %d", rev1, rev0)
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"logs_client_intake":false}`, nil); st != 200 {
		t.Fatalf("patch: %d %v", st, m)
	}
	if st, _, _ := e.ldo("GET", "/api/logs/config", ""); st != http.StatusNotFound {
		t.Fatalf("client config with the intake off: %d", st)
	}
	if st, _, _ := e.ldo("POST", "/api/logs", logLines); st != http.StatusNotFound {
		t.Fatalf("client push with the intake off: %d", st)
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"logs_client_intake":null,"logs_enabled":false}`, nil); st != 200 {
		t.Fatalf("patch: %d %v", st, m)
	}
	if st, _, _ := e.ldo("POST", "/api/logs", logLines); st != http.StatusNotFound || relay.Revision() != 0 {
		t.Fatalf("client push with logging off: %d, revision %d", st, relay.Revision())
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"logs_enabled":null,"logs_retention_days":null}`, nil); st != 200 {
		t.Fatalf("patch: %d %v", st, m)
	}
	if st, _, _ := e.ldo("POST", "/api/logs", logLines); st != http.StatusNoContent || relay.Revision() != rev0 {
		t.Fatalf("client push with the defaults back: %d, revision %d (first %d)", st, relay.Revision(), rev0)
	}
	if st, m := e.cdo("PATCH", "/api/config/settings", `{"logs_push_url":"not a url"}`, nil); st == 200 {
		t.Fatalf("a bad logs_push_url was accepted: %v", m)
	}
	// The schema lists the five settings, so the settings form shows them.
	st, b, _ := e.ldo("GET", "/api/config/schema", "")
	for _, k := range []string{"logs_enabled", "logs_retention_days", "logs_max_disk_gib", "logs_push_url", "logs_client_intake"} {
		if st != 200 || !strings.Contains(b, `"`+k+`"`) {
			t.Errorf("schema lacks %s (%d)", k, st)
		}
	}
}

// The flood test (design 4.4): many senders hammer the log routes, against a
// slow store and against one that never answers, while heartbeats and a chat
// request must still complete in their normal time, and never more than 8
// forwards are in flight.
func TestLogFloodDoesNotSlowHeartbeatsOrRequests(t *testing.T) {
	for _, c := range []struct {
		name   string
		delay  time.Duration
		silent bool
	}{
		{"slow store", 60 * time.Millisecond, false},
		{"silent store", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, store := logsEnv(t, func(o *logrelay.Options) { o.ForwardTimeout = 400 * time.Millisecond })
			store.mu.Lock()
			store.delay, store.silent = c.delay, c.silent
			store.mu.Unlock()
			const nodes = 24
			ids := make([]string, nodes)
			for i := range ids {
				ids[i] = fmt.Sprintf("02:00:00:00:01:%02d", i)
				if a := register(t, e, ids[i]); !a.Accepted || a.Logs == nil {
					t.Fatalf("register %s: %+v", ids[i], a)
				}
			}
			// The normal time of a heartbeat and of a chat request, before the flood.
			timeIt := func(f func()) time.Duration { t0 := time.Now(); f(); return time.Since(t0) }
			heartbeat := func(id string) {
				resp := e.post("/api/pool/nodes/"+id+"/heartbeat", `{"agent_start_id":"s1","devices":[]}`)
				if b := body(t, resp); resp.StatusCode != 200 {
					t.Errorf("heartbeat of %s: %d %s", id, resp.StatusCode, b)
				}
			}
			chatReq := func() {
				resp := e.post("/v1/chat/completions", chat("alpha"))
				if b := body(t, resp); resp.StatusCode != 200 {
					t.Errorf("chat: %d %s", resp.StatusCode, b)
				}
			}
			heartbeat(ids[0])
			chatReq() // loads the model once, so the flood measures a warm request

			stop := make(chan struct{})
			var wg sync.WaitGroup
			var sent, accepted, refused, unavailable, other atomic.Int64
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}, Timeout: 20 * time.Second}
			defer client.CloseIdleConnections()
			batch := strings.Repeat(logLines, 40) // about 2.7 KiB
			flood := func(path string, hdr ...string) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					req, _ := http.NewRequest("POST", e.front.URL+path, strings.NewReader(batch))
					for i := 0; i+1 < len(hdr); i += 2 {
						req.Header.Set(hdr[i], hdr[i+1])
					}
					resp, err := client.Do(req)
					sent.Add(1)
					if err != nil {
						other.Add(1) // a refusal can close the connection under the sender's write
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					switch resp.StatusCode {
					case http.StatusNoContent:
						accepted.Add(1)
					case http.StatusTooManyRequests:
						refused.Add(1)
					case http.StatusServiceUnavailable:
						unavailable.Add(1)
					default:
						other.Add(1)
						t.Errorf("flood: %s answered %d", path, resp.StatusCode)
					}
				}
			}
			for _, id := range ids {
				for range 3 { // three senders per node: two of them always meet "one request per node"
					wg.Add(1)
					go flood("/api/pool/nodes/"+id+"/logs?source=journal", agentproto.HeaderAgentStartID, "s1")
				}
			}
			for range 8 {
				wg.Add(1)
				go flood("/api/logs")
			}

			// While the flood runs: every node's heartbeat, several rounds, and
			// chat requests. Each must stay far below the lost-node time (10 s)
			// and the heartbeat period (2 s).
			var worstHB, worstChat time.Duration
			deadline := time.Now().Add(2 * time.Second)
			rounds := 0
			for time.Now().Before(deadline) || rounds < 3 {
				for _, id := range ids {
					worstHB = max(worstHB, timeIt(func() { heartbeat(id) }))
				}
				worstChat = max(worstChat, timeIt(chatReq))
				rounds++
			}
			st, raw, _ := e.ldo("GET", "/api/status", "")
			close(stop)
			wg.Wait()

			t.Logf("%s: %d log requests sent (%d accepted, %d refused 429, %d answered 503, %d connection errors); %d heartbeat rounds, worst heartbeat %v, worst chat %v; store saw %d requests, at most %d at once",
				c.name, sent.Load(), accepted.Load(), refused.Load(), unavailable.Load(), other.Load(), rounds, worstHB, worstChat, store.total.Load(), store.peak.Load())
			if worstHB > time.Second {
				t.Errorf("a heartbeat took %v during the flood", worstHB)
			}
			if worstChat > 2*time.Second {
				t.Errorf("a chat request took %v during the flood", worstChat)
			}
			if st != 200 {
				t.Fatalf("status during the flood: %d", st)
			}
			var status struct {
				Logs logrelay.Status `json:"logs"`
			}
			if err := json.Unmarshal([]byte(raw), &status); err != nil {
				t.Fatal(err)
			}
			// The cap, as Potluck counted it and as the store saw it. A store
			// that never answers still holds a request for a moment after
			// Potluck gave it up, so only Potluck's own count is exact there.
			if p := status.Logs.PeakInFlight; p > logrelay.MaxInFlight || p < 2 {
				t.Errorf("Potluck had %d log requests in flight at once, the cap is %d", p, logrelay.MaxInFlight)
			}
			if p := store.peak.Load(); !c.silent && p > logrelay.MaxInFlight {
				t.Errorf("the store saw %d requests at once, the cap is %d", p, logrelay.MaxInFlight)
			}
			if refused.Load() == 0 {
				t.Error("the flood met no 429: it did not reach the limits")
			}
			if c.silent {
				if unavailable.Load() == 0 || accepted.Load() != 0 {
					t.Errorf("silent store: %d answered 503, %d accepted", unavailable.Load(), accepted.Load())
				}
			} else if accepted.Load() == 0 {
				t.Error("slow store: nothing was accepted")
			}
			// Every place was given back.
			deadline = time.Now().Add(5 * time.Second)
			for e.api.opts.Logs.Status().InFlight != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := e.api.opts.Logs.Status().InFlight; n != 0 {
				t.Errorf("%d log requests still in flight after the flood", n)
			}
		})
	}
}
