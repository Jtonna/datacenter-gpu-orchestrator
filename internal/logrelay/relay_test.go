package logrelay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

const (
	node1 = "02:00:00:00:00:01"
	node2 = "02:00:00:00:00:02"
)

// stored is one request the fake store got.
type stored struct {
	path, enc, ctype string
	extra            map[string]string
	ignore           []string
	stream           string
	body             []byte
}

type rig struct {
	t     *testing.T
	relay *Relay
	front *httptest.Server
	store *httptest.Server

	mu     sync.Mutex
	cfg    config.Logs
	nodes  map[string]Node
	launch map[string][2]string // launch id -> node id, profile
	got    []stored
	offset time.Duration
	// hold, when set, is closed to let the store answer; status is its answer.
	hold   chan struct{}
	status int
	// in and peak count the requests in flight at the store.
	in, peak atomic.Int32
}

func newRig(t *testing.T, mod ...func(*Options)) *rig {
	t.Helper()
	g := &rig{t: t, cfg: config.DefaultLogs(), status: http.StatusOK,
		nodes: map[string]Node{
			node1: {StartID: "s1", Name: "console-1", Platform: "linux-amd64-vulkan", Release: "0.1.0"},
			node2: {StartID: "s2", Name: "console-2", Platform: "linux-amd64-vulkan", Release: "0.1.0"}},
		launch: map[string][2]string{"ln-1-7-12": {node1, "gemma-4-12b"}}}
	g.store = httptest.NewServer(http.HandlerFunc(g.serveStore))
	t.Cleanup(g.store.Close)
	o := Options{StoreURL: g.store.URL,
		Config: func() config.Logs { g.mu.Lock(); defer g.mu.Unlock(); return g.cfg },
		Node:   func(id string) (Node, bool) { g.mu.Lock(); defer g.mu.Unlock(); n, ok := g.nodes[id]; return n, ok },
		Launch: func(lid, node string) (string, bool) {
			g.mu.Lock()
			defer g.mu.Unlock()
			l, ok := g.launch[lid]
			return l[1], ok && l[0] == node
		},
		Now: func() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return time.Now().Add(g.offset) }}
	for _, m := range mod {
		m(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	g.relay = r
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/pool/nodes/{id}/logs", func(w http.ResponseWriter, q *http.Request) { r.ServeNodeObject(w, q, q.PathValue("id")) })
	mux.HandleFunc("POST /api/pool/nodes/{id}/logs", func(w http.ResponseWriter, q *http.Request) { r.ServeNode(w, q, q.PathValue("id")) })
	mux.HandleFunc("POST /api/logs", r.ServeClient)
	mux.HandleFunc("GET /api/logs/config", r.ServeClientObject)
	g.front = httptest.NewServer(mux)
	t.Cleanup(func() {
		g.release()
		g.front.CloseClientConnections()
		g.front.Close()
	})
	return g
}

func (g *rig) serveStore(w http.ResponseWriter, r *http.Request) {
	g.peak.Store(max(g.peak.Load(), g.in.Add(1)))
	defer g.in.Add(-1)
	b, _ := io.ReadAll(r.Body)
	extra, _, ignore, err := storeParse(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.got = append(g.got, stored{path: r.URL.Path, enc: r.Header.Get("Content-Encoding"), ctype: r.Header.Get("Content-Type"),
		extra: extra, ignore: ignore, stream: r.FormValue("_stream_fields"), body: b})
	hold, status := g.hold, g.status
	g.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(status)
}

func (g *rig) set(f func())            { g.mu.Lock(); f(); g.mu.Unlock() }
func (g *rig) advance(d time.Duration) { g.set(func() { g.offset += d }) }
func (g *rig) block()                  { g.set(func() { g.hold = make(chan struct{}) }) }
func (g *rig) release() {
	g.set(func() {
		if g.hold != nil {
			close(g.hold)
			g.hold = nil
		}
	})
}

func (g *rig) stored() []stored {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]stored(nil), g.got...)
}

type answer struct {
	status     int
	reason     string
	retryAfter string
	body       string
}

// send posts body to path with the headers given as name, value pairs.
func (g *rig) send(method, path string, body io.Reader, hdr ...string) answer {
	g.t.Helper()
	req, err := http.NewRequest(method, g.front.URL+path, body)
	if err != nil {
		g.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	json.Unmarshal(b, &e)
	return answer{status: resp.StatusCode, reason: e.Error.Reason, retryAfter: resp.Header.Get("Retry-After"), body: string(b)}
}

func nodePath(id, query string) string { return "/api/pool/nodes/" + id + "/logs?" + query }

func (g *rig) push(id, start, query, body string) answer {
	g.t.Helper()
	return g.send("POST", nodePath(id, query), strings.NewReader(body), agentproto.HeaderAgentStartID, start)
}

const lines = `{"_msg":"registered","level":"info","unit":"potluck-agent.service","stream":"stderr"}` + "\n" +
	`{"_msg":"heartbeat sent","level":"debug","unit":"potluck-agent.service","stream":"stderr"}` + "\n"

func eventually(t *testing.T, d time.Duration, ok func() bool, what string) {
	t.Helper()
	agenttest.Eventually(t, d, what, ok)
}

// ---- the logs object ----

func TestObjectAndRevision(t *testing.T) {
	g := newRig(t)
	o := g.relay.Object(node1)
	b, _ := json.Marshal(o)
	want := fmt.Sprintf(`{"revision":%d,"url":"/api/pool/nodes/02:00:00:00:00:01/logs","format":"jsonline","auth":null,`+
		`"limits":{"max_batch_bytes":1048576,"max_line_bytes":8192,"max_bytes_per_s":262144,"every_s":2},"deny_units":[]}`, o.Revision)
	if string(b) != want || o.Revision <= 0 {
		t.Fatalf("object\n got %s\nwant %s", b, want)
	}
	if again := g.relay.Object(node2); again.Revision != o.Revision || again.URL != "/api/pool/nodes/02:00:00:00:00:02/logs" {
		t.Fatalf("second node: %+v", again)
	}
	c := g.relay.ClientObject()
	if c == nil || c.URL != "/api/logs" || c.Revision != o.Revision || c.Limits.MaxBytesPerS != 65536 || c.Auth != nil {
		t.Fatalf("client object %+v", c)
	}

	// Every logging setting changes the revision.
	seen := map[int64]string{o.Revision: "defaults"}
	for name, change := range map[string]func(){
		"logs_retention_days": func() { g.cfg.RetentionDays = 30 },
		"logs_max_disk_gib":   func() { g.cfg.MaxDiskGiB = 40 },
		"logs_client_intake":  func() { g.cfg.ClientIntake = false },
		"logs_push_url":       func() { g.cfg.PushURL = "https://logs.example:8443/insert" },
	} {
		g.set(change)
		rev := g.relay.Revision()
		if prev, dup := seen[rev]; dup || rev <= 0 {
			t.Fatalf("%s: revision %d is the one of %s", name, rev, prev)
		}
		seen[rev] = name
	}
	// With logs_push_url the senders are told that address.
	if o := g.relay.Object(node1); o.URL != "https://logs.example:8443/insert" {
		t.Fatalf("pushed object %+v", o)
	}
	if g.relay.ClientObject() != nil {
		t.Fatal("client object with the client intake off")
	}
	// Back to the defaults: the first revision again (it is not a counter).
	g.set(func() { g.cfg = config.DefaultLogs() })
	if g.relay.Revision() != o.Revision {
		t.Fatalf("revision %d after the settings went back, was %d", g.relay.Revision(), o.Revision)
	}
	g.set(func() { g.cfg.Enabled = false })
	if g.relay.Object(node1) != nil || g.relay.ClientObject() != nil || g.relay.Revision() != 0 || g.relay.On() {
		t.Fatal("logs_enabled off still hands out an object")
	}
}

// No store address: logging is off, unless logs_push_url names another address.
func TestNoStoreMeansOff(t *testing.T) {
	g := newRig(t, func(o *Options) { o.StoreURL = "" })
	if g.relay.On() || g.relay.Object(node1) != nil || g.relay.ClientObject() != nil {
		t.Fatal("logging is on without a store address")
	}
	for _, a := range []answer{
		g.send("GET", nodePath(node1, ""), nil, agentproto.HeaderAgentStartID, "s1"),
		g.push(node1, "s1", "source=agent", lines),
		g.send("POST", "/api/logs", strings.NewReader(lines)),
		g.send("GET", "/api/logs/config", nil),
	} {
		if a.status != http.StatusNotFound {
			t.Fatalf("answer %d %s", a.status, a.body)
		}
	}
	g.set(func() { g.cfg.PushURL = "http://gate.example:9428/insert/jsonline" })
	if o := g.relay.Object(node1); o == nil || o.URL != "http://gate.example:9428/insert/jsonline" {
		t.Fatalf("object with logs_push_url and no store: %+v", o)
	}
	if a := g.send("GET", nodePath(node1, ""), nil, agentproto.HeaderAgentStartID, "s1"); a.status != 200 {
		t.Fatalf("GET object: %d %s", a.status, a.body)
	}
	// Potluck has nothing to forward to: its own pass-through stays 404.
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNotFound {
		t.Fatalf("push without a store: %d", a.status)
	}
	if _, err := New(Options{StoreURL: "potluck-logs:9428", Config: config.DefaultLogs}); err == nil {
		t.Fatal("a store address without a scheme was accepted")
	}
}

func TestObjectRoutes(t *testing.T) {
	g := newRig(t)
	a := g.send("GET", nodePath(node1, ""), nil, agentproto.HeaderAgentStartID, "s1")
	var o agentproto.Logs
	if err := json.Unmarshal([]byte(a.body), &o); a.status != 200 || err != nil || o.Revision != g.relay.Revision() || o.Format != "jsonline" {
		t.Fatalf("%d %s", a.status, a.body)
	}
	if a := g.send("GET", nodePath(node1, ""), nil, agentproto.HeaderAgentStartID, "old"); a.status != http.StatusGone {
		t.Fatalf("stale start id: %d", a.status)
	}
	if a := g.send("GET", nodePath("02:00:00:00:00:99", ""), nil, agentproto.HeaderAgentStartID, "s1"); a.status != http.StatusGone {
		t.Fatalf("unknown node: %d", a.status)
	}
	if a := g.send("GET", "/api/logs/config", nil); a.status != 200 || !strings.Contains(a.body, `"url":"/api/logs"`) {
		t.Fatalf("client config: %d %s", a.status, a.body)
	}
	g.set(func() { g.cfg.ClientIntake = false })
	if a := g.send("GET", "/api/logs/config", nil); a.status != http.StatusNotFound {
		t.Fatalf("client config with the intake off: %d", a.status)
	}
	g.set(func() { g.cfg.Enabled = false })
	if a := g.send("GET", nodePath(node1, ""), nil, agentproto.HeaderAgentStartID, "s1"); a.status != http.StatusNotFound {
		t.Fatalf("object with logging off: %d", a.status)
	}
}

// ---- the pass-through ----

func TestNodePassThroughAddsTrustedFieldsAndKeepsTheBody(t *testing.T) {
	g := newRig(t)
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent || a.body != "" {
		t.Fatalf("%d %s", a.status, a.body)
	}
	got := g.stored()
	if len(got) != 1 {
		t.Fatalf("store got %d requests", len(got))
	}
	s := got[0]
	want := map[string]string{"node_id": node1, "node_name": "console-1", "platform": "linux-amd64-vulkan", "release": "0.1.0", "source": "agent"}
	if s.path != "/insert/jsonline" || fmt.Sprint(s.extra) != fmt.Sprint(want) || s.stream != "source,node_id,unit" ||
		strings.Join(s.ignore, ",") != "launch_id,profile,actor,client_addr" || string(s.body) != lines || s.enc != "" {
		t.Fatalf("store got %+v body %q", s, s.body)
	}

	// A gzip body goes through byte for byte, with its Content-Encoding.
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write([]byte(lines))
	zw.Close()
	raw := append([]byte(nil), zb.Bytes()...)
	if a := g.send("POST", nodePath(node1, "source=journal"), &zb, agentproto.HeaderAgentStartID, "s1", "Content-Encoding", "gzip"); a.status != http.StatusNoContent {
		t.Fatalf("gzip: %d %s", a.status, a.body)
	}
	s = g.stored()[1]
	if s.enc != "gzip" || !bytes.Equal(s.body, raw) || s.extra["source"] != "journal" {
		t.Fatalf("gzip at the store: enc %q, %d bytes (sent %d), %v", s.enc, len(s.body), len(raw), s.extra)
	}

	// A launch: the profile is looked up from the launch id.
	if a := g.push(node1, "s1", "source=launch&launch_id=ln-1-7-12", lines); a.status != http.StatusNoContent {
		t.Fatalf("launch: %d %s", a.status, a.body)
	}
	s = g.stored()[2]
	if s.extra["source"] != "launch" || s.extra["launch_id"] != "ln-1-7-12" || s.extra["profile"] != "gemma-4-12b" || strings.Join(s.ignore, ",") != "actor,client_addr" {
		t.Fatalf("launch at the store: %v ignore %v", s.extra, s.ignore)
	}
	// A launch Potluck does not hold, or holds for another node: no profile,
	// and a line cannot set one.
	for _, q := range []string{"source=launch&launch_id=ln-9-9-9", "source=launch&launch_id=ln-1-7-12"} {
		id, start := node1, "s1"
		if strings.HasSuffix(q, "ln-1-7-12") {
			id, start = node2, "s2"
		}
		if a := g.push(id, start, q, lines); a.status != http.StatusNoContent {
			t.Fatalf("%s: %d %s", q, a.status, a.body)
		}
		got := g.stored()
		s := got[len(got)-1]
		if _, has := s.extra["profile"]; has || s.extra["node_id"] != id || strings.Join(s.ignore, ",") != "profile,actor,client_addr" {
			t.Fatalf("%s at the store: %v ignore %v", q, s.extra, s.ignore)
		}
	}
	// An empty body is accepted and not forwarded.
	n := len(g.stored())
	if a := g.push(node1, "s1", "source=agent", ""); a.status != http.StatusNoContent || len(g.stored()) != n {
		t.Fatalf("empty body: %d, store got %d more", a.status, len(g.stored())-n)
	}
	c, _ := g.relay.NodeCounters(node1)
	if c.AcceptedRequests != 4 || c.AcceptedBytes != int64(3*len(lines)+len(raw)) || len(c.Refused) != 0 {
		t.Fatalf("counters %+v", c)
	}
}

// A node name is whatever the agent registered with: it must not be able to
// write another field.
func TestHostileNodeNameCannotSetAnotherField(t *testing.T) {
	g := newRig(t)
	g.set(func() {
		g.nodes[node1] = Node{StartID: "s1", Name: "x,node_id=" + node2 + ",source=scheduler", Platform: "p", Release: "0.1.0"}
	})
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("%d %s", a.status, a.body)
	}
	s := g.stored()[0]
	if s.extra["node_id"] != node1 || s.extra["source"] != "agent" || s.extra["node_name"] != "x%2Cnode_id="+node2+"%2Csource=scheduler" || len(s.extra) != 5 {
		t.Fatalf("%v", s.extra)
	}
}

func TestNodeRefusals(t *testing.T) {
	g := newRig(t)
	for _, c := range []struct {
		name, id, start, query string
		hdr                    []string
		status                 int
		reason                 string
	}{
		{"unknown node", "02:00:00:00:00:99", "s1", "source=agent", nil, 410, ReasonUnknownSender},
		{"stale start id", node1, "s0", "source=agent", nil, 410, ReasonUnknownSender},
		{"no start id", node1, "", "source=agent", nil, 410, ReasonUnknownSender},
		{"no source", node1, "s1", "", nil, 400, ReasonBadRequest},
		{"source client", node1, "s1", "source=client", nil, 400, ReasonBadRequest},
		{"source scheduler", node1, "s1", "source=scheduler", nil, 400, ReasonBadRequest},
		{"launch without id", node1, "s1", "source=launch", nil, 400, ReasonBadRequest},
		{"launch id with a comma", node1, "s1", "source=launch&launch_id=a%2Cb", nil, 400, ReasonBadRequest},
		{"zstd body", node1, "s1", "source=agent", []string{"Content-Encoding", "zstd"}, 415, ReasonBadEncoding},
	} {
		hdr := append([]string{agentproto.HeaderAgentStartID, c.start}, c.hdr...)
		a := g.send("POST", nodePath(c.id, c.query), strings.NewReader(lines), hdr...)
		if a.status != c.status || a.reason != c.reason {
			t.Errorf("%s: %d %q, want %d %q (%s)", c.name, a.status, a.reason, c.status, c.reason, a.body)
		}
	}
	if n := len(g.stored()); n != 0 {
		t.Fatalf("the store got %d refused requests", n)
	}
	c, _ := g.relay.NodeCounters(node1)
	if c.Refused[ReasonBadRequest] != 5 || c.Refused[ReasonBadEncoding] != 1 || c.AcceptedRequests != 0 {
		t.Fatalf("counters %+v", c)
	}
	if st := g.relay.Status(); st.UnknownSender != 3 {
		t.Fatalf("status %+v", st)
	}
	// An id nobody registered leaves no state behind.
	if _, ok := g.relay.NodeCounters("02:00:00:00:00:99"); ok {
		t.Fatal("state was kept for an unknown node")
	}
}

// chunked hides the length of a body, so it is sent without Content-Length.
type chunked struct{ io.Reader }

func TestBodyLimit(t *testing.T) {
	g := newRig(t)
	big := strings.Repeat("x", MaxBodyBytes+1)
	// Declared length: refused before anything is read or forwarded.
	if a := g.push(node1, "s1", "source=agent", big); a.status != http.StatusRequestEntityTooLarge || a.reason != ReasonTooLarge {
		t.Fatalf("declared: %d %s", a.status, a.body)
	}
	if len(g.stored()) != 0 {
		t.Fatal("an oversize body with a declared length reached the store")
	}
	// No declared length: counted while it streams.
	a := g.send("POST", nodePath(node1, "source=agent"), chunked{strings.NewReader(big)}, agentproto.HeaderAgentStartID, "s1")
	if a.status != http.StatusRequestEntityTooLarge || a.reason != ReasonTooLarge {
		t.Fatalf("chunked: %d %s", a.status, a.body)
	}
	// Exactly the limit is accepted, declared or not.
	exact := big[:MaxBodyBytes]
	if a := g.push(node2, "s2", "source=agent", exact); a.status != http.StatusNoContent {
		t.Fatalf("exact, declared: %d %s", a.status, a.body)
	}
	g.advance(time.Minute)
	if a := g.send("POST", nodePath(node2, "source=agent"), chunked{strings.NewReader(exact)}, agentproto.HeaderAgentStartID, "s2"); a.status != http.StatusNoContent {
		t.Fatalf("exact, chunked: %d %s", a.status, a.body)
	}
	c, _ := g.relay.NodeCounters(node1)
	if c.Refused[ReasonTooLarge] != 2 || c.AcceptedRequests != 0 {
		t.Fatalf("counters %+v", c)
	}
	if st := g.relay.Status(); st.InFlight != 0 {
		t.Fatalf("in flight after the refusals: %d", st.InFlight)
	}
}

// 256 KiB/s with a burst of 2 MiB per node.
func TestNodeRateLimit(t *testing.T) {
	g := newRig(t)
	mib := strings.Repeat("y", 1<<20)
	for i := range 2 {
		if a := g.push(node1, "s1", "source=agent", mib); a.status != http.StatusNoContent {
			t.Fatalf("batch %d inside the burst: %d %s", i, a.status, a.body)
		}
	}
	a := g.push(node1, "s1", "source=agent", mib)
	if a.status != http.StatusTooManyRequests || a.reason != ReasonRateLimited || a.retryAfter != "4" {
		t.Fatalf("third batch: %d %q Retry-After %q", a.status, a.reason, a.retryAfter)
	}
	// Another node has its own budget.
	if a := g.push(node2, "s2", "source=agent", mib); a.status != http.StatusNoContent {
		t.Fatalf("other node: %d %s", a.status, a.body)
	}
	g.advance(2 * time.Second)
	if a := g.push(node1, "s1", "source=agent", mib); a.status != http.StatusTooManyRequests || a.retryAfter != "2" {
		t.Fatalf("after 2 s: %d Retry-After %q", a.status, a.retryAfter)
	}
	// A small batch fits what has come back.
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("small batch: %d %s", a.status, a.body)
	}
	g.advance(2*time.Second + 100*time.Millisecond)
	if a := g.push(node1, "s1", "source=agent", mib); a.status != http.StatusNoContent {
		t.Fatalf("after 4 s: %d %s", a.status, a.body)
	}
	// A body without a declared length is let in while the node has any
	// budget left and is charged after it streamed, so the node can go one
	// batch into debt and then waits that much longer.
	g.advance(time.Minute)
	for i := range 3 {
		if a := g.send("POST", nodePath(node1, "source=agent"), chunked{strings.NewReader(mib)}, agentproto.HeaderAgentStartID, "s1"); a.status != http.StatusNoContent {
			t.Fatalf("chunked batch %d: %d %s", i, a.status, a.body)
		}
	}
	if a := g.send("POST", nodePath(node1, "source=agent"), chunked{strings.NewReader(mib)}, agentproto.HeaderAgentStartID, "s1"); a.status != http.StatusTooManyRequests {
		t.Fatalf("fourth chunked batch: %d", a.status)
	}
	g.advance(3 * time.Second)
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusTooManyRequests {
		t.Fatalf("while still in debt: %d", a.status)
	}
	g.advance(2 * time.Second)
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("after the debt was paid: %d", a.status)
	}
	c, _ := g.relay.NodeCounters(node1)
	if c.Refused[ReasonRateLimited] != 4 || c.AcceptedRequests != 8 {
		t.Fatalf("counters %+v", c)
	}
}

// One request per node at a time, and at most MaxInFlight in the process:
// the next is refused at once.
func TestOneRequestPerNodeAndTheCap(t *testing.T) {
	g := newRig(t)
	g.set(func() {
		for i := 3; i <= 12; i++ {
			g.nodes[fmt.Sprintf("02:00:00:00:00:%02d", i)] = Node{StartID: "s", Name: fmt.Sprintf("console-%d", i)}
		}
	})
	g.block()
	var wg sync.WaitGroup
	results := make(chan answer, 16)
	start := func(id, sid string) {
		wg.Add(1)
		go func() { defer wg.Done(); results <- g.push(id, sid, "source=agent", lines) }()
	}
	start(node1, "s1")
	eventually(t, 5*time.Second, func() bool { return g.in.Load() == 1 }, "the first request at the store")
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusTooManyRequests || a.reason != ReasonNodeBusy || a.retryAfter == "" {
		t.Fatalf("second request of the node: %d %q", a.status, a.reason)
	}
	for i := 3; i <= 9; i++ {
		start(fmt.Sprintf("02:00:00:00:00:%02d", i), "s")
	}
	eventually(t, 5*time.Second, func() bool { return g.in.Load() == MaxInFlight }, "eight requests at the store")
	began := time.Now()
	a := g.push("02:00:00:00:00:10", "s", "source=agent", lines)
	if a.status != http.StatusTooManyRequests || a.reason != ReasonTooManyRequests {
		t.Fatalf("ninth request: %d %q", a.status, a.reason)
	}
	if d := time.Since(began); d > time.Second {
		t.Fatalf("the ninth request waited %v", d)
	}
	// A client is inside the same cap.
	if a := g.send("POST", "/api/logs", strings.NewReader(lines)); a.status != http.StatusTooManyRequests || a.reason != ReasonTooManyRequests {
		t.Fatalf("client at the cap: %d %q", a.status, a.reason)
	}
	if st := g.relay.Status(); st.InFlight != MaxInFlight || st.PeakInFlight != MaxInFlight {
		t.Fatalf("status %+v", st)
	}
	g.release()
	wg.Wait()
	close(results)
	for a := range results {
		if a.status != http.StatusNoContent {
			t.Fatalf("a held request ended %d %s", a.status, a.body)
		}
	}
	if g.peak.Load() != MaxInFlight {
		t.Fatalf("the store saw %d requests at once", g.peak.Load())
	}
	if st := g.relay.Status(); st.InFlight != 0 || st.Clients.Refused[ReasonTooManyRequests] != 1 {
		t.Fatalf("status after: %+v", st)
	}
	if a := g.push("02:00:00:00:00:10", "s", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("after the cap cleared: %d", a.status)
	}
}

func TestStoreFailures(t *testing.T) {
	g := newRig(t, func(o *Options) { o.ForwardTimeout = 300 * time.Millisecond })
	g.set(func() { g.status = http.StatusInternalServerError })
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusServiceUnavailable || a.reason != ReasonStoreUnavailable || a.retryAfter == "" {
		t.Fatalf("store 500: %d %q", a.status, a.reason)
	}
	g.set(func() { g.status = http.StatusBadRequest })
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusBadRequest || a.reason != ReasonStoreRejected {
		t.Fatalf("store 400: %d %q", a.status, a.reason)
	}
	// The store's own 204 is mapped like its 200.
	g.set(func() { g.status = http.StatusNoContent })
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("store 204: %d", a.status)
	}
	// A store that does not answer: 503 after the limit for the whole forward.
	g.set(func() { g.status = http.StatusOK })
	g.block()
	began := time.Now()
	a := g.push(node1, "s1", "source=agent", lines)
	if d := time.Since(began); a.status != http.StatusServiceUnavailable || a.reason != ReasonStoreUnavailable || d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("silent store: %d %q after %v", a.status, a.reason, d)
	}
	g.release()
	// A store that is gone.
	g.store.Close()
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", a.status)
	}
	c, _ := g.relay.NodeCounters(node1)
	if c.Refused[ReasonStoreUnavailable] != 3 || c.Refused[ReasonStoreRejected] != 1 || c.AcceptedRequests != 1 {
		t.Fatalf("counters %+v", c)
	}
	if st := g.relay.Status(); st.InFlight != 0 {
		t.Fatalf("in flight %d", st.InFlight)
	}
}

// A sender that stalls in the middle of its body gives its place back after
// the limit for the whole forward.
func TestStalledSenderIsCutOff(t *testing.T) {
	g := newRig(t, func(o *Options) { o.ForwardTimeout = 2 * time.Second })
	conn, err := net.Dial("tcp", g.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: potluck\r\n%s: s1\r\nContent-Length: 1000\r\n\r\n{\"_msg\":", nodePath(node1, "source=agent"), agentproto.HeaderAgentStartID)
	began := time.Now()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to the stalled sender: %v", err)
	}
	resp.Body.Close()
	if d := time.Since(began); resp.StatusCode != http.StatusBadRequest || d > 5*time.Second {
		t.Fatalf("stalled sender: %d after %v", resp.StatusCode, d)
	}
	eventually(t, 3*time.Second, func() bool { return g.relay.Status().InFlight == 0 }, "the place to be given back")
	c, _ := g.relay.NodeCounters(node1)
	if c.Refused[ReasonBodyRead] != 1 {
		t.Fatalf("counters %+v", c)
	}
	// The node can send again.
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("after the stall: %d %s", a.status, a.body)
	}
}

// ---- clients ----

func TestClientIntake(t *testing.T) {
	g := newRig(t)
	body := `{"_msg":"hello","node_id":"` + node1 + `","source":"agent"}` + "\n"
	if a := g.send("POST", "/api/logs?source=agent&node_id="+node1, strings.NewReader(body), "X-Actor", "build-bot, source=agent"); a.status != http.StatusNoContent {
		t.Fatalf("%d %s", a.status, a.body)
	}
	s := g.stored()[0]
	// What the line claims is not Potluck's business (the body is not read),
	// but the store is told: source is client, and no node field may be set.
	if len(s.extra) != 3 || s.extra["source"] != "client" || s.extra["client_addr"] != "127.0.0.1" || s.extra["actor"] != "build-bot%2C source=agent" ||
		strings.Join(s.ignore, ",") != "node_id,node_name,platform,release,launch_id,profile" || string(s.body) != body {
		t.Fatalf("store got %v ignore %v", s.extra, s.ignore)
	}
	// Without X-Actor the field is not set and cannot be set by a line.
	g.send("POST", "/api/logs", strings.NewReader(body))
	if s := g.stored()[1]; len(s.extra) != 2 || strings.Join(s.ignore, ",") != "node_id,node_name,platform,release,launch_id,profile,actor" {
		t.Fatalf("store got %v ignore %v", s.extra, s.ignore)
	}
	if st := g.relay.Status(); st.Clients.AcceptedRequests != 2 || st.Clients.AcceptedBytes != int64(2*len(body)) || !st.ClientIntake {
		t.Fatalf("status %+v", st)
	}
	g.set(func() { g.cfg.ClientIntake = false })
	if a := g.send("POST", "/api/logs", strings.NewReader(body)); a.status != http.StatusNotFound {
		t.Fatalf("intake off: %d", a.status)
	}
	// The node route is not touched by that setting.
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNoContent {
		t.Fatalf("node push with the client intake off: %d", a.status)
	}
	g.set(func() { g.cfg.Enabled = false })
	if a := g.push(node1, "s1", "source=agent", lines); a.status != http.StatusNotFound {
		t.Fatalf("node push with logging off: %d", a.status)
	}
}

// 64 KiB/s per client address (burst 1 MiB), 1 MiB/s for all clients
// together (burst 2 MiB), and at most MaxClientInFlight client requests at
// the same time.
func TestClientLimits(t *testing.T) {
	g := newRig(t)
	mib := strings.Repeat("z", 1<<20)
	if a := g.send("POST", "/api/logs", strings.NewReader(mib)); a.status != http.StatusNoContent {
		t.Fatalf("first: %d %s", a.status, a.body)
	}
	a := g.send("POST", "/api/logs", strings.NewReader(mib))
	if a.status != http.StatusTooManyRequests || a.reason != ReasonRateLimited || a.retryAfter != "16" {
		t.Fatalf("second: %d %q Retry-After %q", a.status, a.reason, a.retryAfter)
	}
	// All clients together: other addresses share the 2 MiB.
	r := g.relay
	if reason, _ := r.admit("192.0.2.50", true, 1<<20); reason != "" {
		t.Fatalf("second address: %s", reason)
	}
	r.release("192.0.2.50", true, 1<<20, 1<<20, "")
	if reason, retry := r.admit("192.0.2.51", true, 1<<20); reason != ReasonRateLimited || retry != 1 {
		t.Fatalf("third address, all clients together: %q retry %d", reason, retry)
	}
	g.advance(time.Second)
	if reason, _ := r.admit("192.0.2.51", true, 1<<20); reason != "" {
		t.Fatalf("third address after 1 s: %s", reason)
	}
	r.release("192.0.2.51", true, 1<<20, 1<<20, "")

	// Clients take at most MaxClientInFlight places.
	g.advance(time.Hour)
	for i := range MaxClientInFlight {
		if reason, _ := r.admit(fmt.Sprintf("192.0.2.%d", 60+i), true, 10); reason != "" {
			t.Fatalf("client %d: %s", i, reason)
		}
	}
	if reason, _ := r.admit("192.0.2.70", true, 10); reason != ReasonTooManyRequests {
		t.Fatalf("one client too many: %q", reason)
	}
	if reason, _ := r.admit(node1, false, 10); reason != "" {
		t.Fatalf("a node while the clients are at their cap: %s", reason)
	}
	r.release(node1, false, 10, 10, "")
	for i := range MaxClientInFlight {
		r.release(fmt.Sprintf("192.0.2.%d", 60+i), true, 10, 10, "")
	}

	// The per-address state is bounded: idle addresses are forgotten.
	for i := range maxClientAddrs + 50 {
		key := fmt.Sprintf("198.51.100.%d", i)
		if reason, _ := r.admit(key, true, 1); reason != "" {
			t.Fatalf("address %d: %s", i, reason)
		}
		r.release(key, true, 1, 1, "")
		g.advance(time.Second)
	}
	r.mu.Lock()
	n := len(r.clients)
	r.mu.Unlock()
	if n > maxClientAddrs {
		t.Fatalf("%d client addresses kept", n)
	}
}

// The deadline for the sender's body must not outlive the request: a
// connection that is kept stays good for the next push, after an accepted
// batch and after a refusal with a small body. A refusal with a large body
// closes the connection instead of reading the body away.
func TestConnectionsAreKeptOrClosedAsDocumented(t *testing.T) {
	g := newRig(t, func(o *Options) { o.ForwardTimeout = 200 * time.Millisecond })
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	push := func(id, start, body string) (status int, reused bool) {
		t.Helper()
		req, _ := http.NewRequest("POST", g.front.URL+nodePath(id, "source=agent"), strings.NewReader(body))
		req.Header.Set(agentproto.HeaderAgentStartID, start)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, reused
	}
	if st, _ := push(node1, "s1", lines); st != http.StatusNoContent {
		t.Fatalf("first push: %d", st)
	}
	time.Sleep(400 * time.Millisecond) // longer than the body deadline of the first push
	if st, reused := push(node1, "s1", lines); st != http.StatusNoContent || !reused {
		t.Fatalf("second push on the kept connection: %d, reused %v", st, reused)
	}
	// A refusal with a small body keeps the connection.
	if st, reused := push(node1, "stale", lines); st != http.StatusGone || !reused {
		t.Fatalf("refused push: %d, reused %v", st, reused)
	}
	if st, reused := push(node1, "s1", lines); st != http.StatusNoContent || !reused {
		t.Fatalf("push after a small refusal: %d, reused %v", st, reused)
	}
	// A refusal with a large body closes it.
	if st, _ := push(node1, "stale", strings.Repeat("x", keepAliveBodyBytes+1)); st != http.StatusGone {
		t.Fatalf("large refused push: %d", st)
	}
	if st, reused := push(node1, "s1", lines); st != http.StatusNoContent || reused {
		t.Fatalf("push after a large refusal: %d, reused %v", st, reused)
	}
	// The store is down: a small batch was not read, the connection is kept.
	g.store.Close()
	if st, _ := push(node1, "s1", lines); st != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", st)
	}
	if st, reused := push(node1, "s1", lines); st != http.StatusServiceUnavailable || !reused {
		t.Fatalf("second push with the store down: %d, reused %v", st, reused)
	}
}
