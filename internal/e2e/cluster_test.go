package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/fakefleet"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/harness"
)

var (
	schedBin string
	buildDir string
)

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestMain(m *testing.M) {
	var err error
	buildDir, err = os.MkdirTemp("", "llm-scheduler-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	name := "llm-scheduler"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	schedBin = filepath.Join(buildDir, name)
	cmd := exec.Command("go", "build", "-o", schedBin, "./cmd/llm-scheduler")
	cmd.Dir = moduleRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building llm-scheduler: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	harness.Cleanup()
	os.RemoveAll(buildDir)
	os.Exit(code)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// options describe one cluster under test.
type options struct {
	members  []fakefleet.Member
	profiles string            // YAML, indented two spaces, placed under "profiles:"
	env      map[string]string // extra environment for the scheduler (and so its children)
	fleetURL string            // override the fake fleet's URL (e.g. an unreachable one)
	exclude  string            // YAML list for exclude_nodes, e.g. `["ps5-test*"]`; empty for none
	defaults string            // extra lines under "defaults:", each indented two spaces and ending in a newline
	topLevel string            // extra top-level YAML (say a recovery: block), ending in a newline
	args     []string          // extra scheduler flags (say --programs-dir)
}

// cluster is a running llm-scheduler with its fakes.
type cluster struct {
	t        *testing.T
	fleet    *fakefleet.Fleet
	url      string
	cmd      *exec.Cmd
	stderr   *syncBuf
	startLog string
	done     chan struct{}
	exitErr  error
	client   *http.Client
}

var modelRe = regexp.MustCompile(`(?m)^\s+model:\s*(\S+)`)

// dirRe finds data_dir and history_dir lines of a test snippet.
var dirRe = regexp.MustCompile(`(?m)^(data_dir|history_dir):\s*"([^"]+)"`)

func startCluster(t *testing.T, o options) *cluster {
	t.Helper()
	fake := harness.BuildFakeLlama(t)
	dir := t.TempDir()
	models := filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range modelRe.FindAllStringSubmatch(o.profiles, -1) {
		if err := os.WriteFile(filepath.Join(models, m[1]), []byte("not a real model"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fl := fakefleet.New(t)
	fl.SetMembers(o.members...)
	fleetURL := fl.URL()
	if o.fleetURL != "" {
		fleetURL = o.fleetURL
	}
	port := harness.FreePort(t)
	slash := filepath.ToSlash
	excludeLine := ""
	if o.exclude != "" {
		excludeLine = "exclude_nodes: " + o.exclude + "\n"
	}
	// The cluster is written for the legacy fleet path unless a test says
	// membership agents (the built-in default is agents since build-92).
	defaults := o.defaults
	if !strings.Contains(defaults, "membership:") {
		defaults = "  membership: fleet\n" + defaults
	}
	cfg := fmt.Sprintf(`listen: "127.0.0.1:%d"
fleet: {url: "%s", local_node: host-1, stale_after: 60s}
models_dir: "%s"
local_probe_cmd: "%s --nvidia-smi 8151 0"
defaults:
  ttl: 0s
  min_hold: 1ms
  max_hold: 1s
  drain_after: 1s
  queue_timeout: 60s
  stall_timeout: 0s
  shutdown_grace: 10s
  server_bin: "%s"
%stiers:
  - {match: {hostname: "ps5-*"}, priority: 1, headroom_mib: 1024, shared_memory: true, link_mb_s: 1000, settle: 100ms}
  - {match: {local: true}, priority: 3, headroom_mib: 256, settle: 100ms}
%s%sprofiles:
%s
`, port, fleetURL, slash(models), slash(fake), slash(fake), defaults, excludeLine, o.topLevel, o.profiles)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// The YAML is imported once into the database (the only use of YAML left), then the
	// scheduler starts from the database with the bootstrap flags.
	dataDir := filepath.Join(dir, "data")
	historyDir := ""
	for _, m := range dirRe.FindAllStringSubmatch(o.topLevel, -1) {
		switch m[1] {
		case "data_dir":
			dataDir = filepath.FromSlash(m[2])
		case "history_dir":
			historyDir = filepath.FromSlash(m[2])
		}
	}
	imp := exec.Command(schedBin, "import-config", "--data-dir", dataDir, "--models-dir", models, cfgPath)
	if out, err := imp.CombinedOutput(); err != nil {
		t.Fatalf("import-config: %v\n%s", err, out)
	}

	c := &cluster{t: t, fleet: fl, url: fmt.Sprintf("http://127.0.0.1:%d", port), stderr: &syncBuf{},
		startLog: filepath.Join(dir, "starts.jsonl"), done: make(chan struct{}), client: &http.Client{Timeout: 60 * time.Second}}
	args := []string{"--listen", fmt.Sprintf("127.0.0.1:%d", port), "--data-dir", dataDir, "--models-dir", models}
	if historyDir != "" {
		args = append(args, "--history-dir", historyDir)
	}
	args = append(args, o.args...)
	c.cmd = exec.Command(schedBin, args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FAKELLAMA_") && !strings.HasPrefix(kv, "LLAMA_ARG_") {
			c.cmd.Env = append(c.cmd.Env, kv)
		}
	}
	c.cmd.Env = append(c.cmd.Env, "FAKELLAMA_START_LOG="+c.startLog)
	for k, v := range o.env {
		c.cmd.Env = append(c.cmd.Env, k+"="+v)
	}
	c.cmd.Stderr = c.stderr
	prepareCmd(c.cmd)
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.exitErr = c.cmd.Wait(); close(c.done) }()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			// Ask for a clean shutdown first: it stops the children, which run
			// in process groups of their own and would outlive a kill of the
			// scheduler and keep files in the test's temp dir busy.
			if runtime.GOOS != "windows" && interrupt(c.cmd) == nil {
				select {
				case <-c.done:
				case <-time.After(10 * time.Second):
				}
			}
			select {
			case <-c.done:
			default:
				killTree(c.cmd)
				<-c.done
			}
		}
		if t.Failed() {
			t.Logf("llm-scheduler stderr:\n%s", c.stderr.String())
		}
	})
	harness.WaitForOK(t, c.url+"/health", 15*time.Second)
	return c
}

func (c *cluster) do(method, path, body string) (int, string, http.Header) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.url+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// a save of a benchmark or a group needs a comment (the editors)
	if p := strings.SplitN(path, "?", 2)[0]; (method == "POST" || method == "PUT" || method == "DELETE") && req.Header.Get("X-Comment") == "" && !strings.Contains(body, `"comment"`) &&
		(strings.HasPrefix(p, "/api/bench/benchmarks") || strings.HasPrefix(p, "/api/bench/suites") || strings.HasPrefix(p, "/api/bench/groups")) {
		req.Header.Set("X-Comment", "e2e test")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (c *cluster) get(path string) (int, string) { s, b, _ := c.do("GET", path, ""); return s, b }
func (c *cluster) post(path, body string) (int, string) {
	s, b, _ := c.do("POST", path, body)
	return s, b
}

func (c *cluster) chat(model string, stream bool) (int, string) {
	return c.post("/v1/chat/completions", fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, model, stream))
}

func (c *cluster) jsonGet(path string) map[string]any {
	c.t.Helper()
	status, b := c.get(path)
	if status != 200 {
		c.t.Fatalf("GET %s: %d %s", path, status, b)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(b), &m); err != nil {
		c.t.Fatalf("GET %s: %v\n%s", path, err, b)
	}
	return m
}

type runningEntry struct{ Model, State, Name, Cmd string }

func (c *cluster) running() []runningEntry {
	c.t.Helper()
	var doc struct {
		Running []struct {
			Model, State, Name, Cmd string
		}
	}
	_, b := c.get("/running")
	if err := json.Unmarshal([]byte(b), &doc); err != nil {
		c.t.Fatalf("/running: %v\n%s", err, b)
	}
	var out []runningEntry
	for _, r := range doc.Running {
		out = append(out, runningEntry{r.Model, r.State, r.Name, r.Cmd})
	}
	return out
}

func (c *cluster) runningModels() []string {
	var out []string
	for _, r := range c.running() {
		out = append(out, r.Model+":"+r.State)
	}
	return out
}

func (c *cluster) waitFor(what string, timeout time.Duration, f func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for !f() {
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out after %v waiting for %s\nrunning: %v", timeout, what, c.runningModels())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type startEntry struct {
	Mode string            `json:"mode"`
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	Time time.Time         `json:"time"`
}

// starts returns the fakellama start log: one entry per list-devices probe
// or served child.
func (c *cluster) starts() []startEntry {
	f, err := os.Open(c.startLog)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []startEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e startEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (c *cluster) countStarts(mode string) int {
	n := 0
	for _, s := range c.starts() {
		if s.Mode == mode {
			n++
		}
	}
	return n
}

// childPorts reads the --port of every running child from /running.
func (c *cluster) childPorts() []string {
	var ports []string
	for _, r := range c.running() {
		if m := regexp.MustCompile(`--port (\d+)`).FindStringSubmatch(r.Cmd); m != nil {
			ports = append(ports, m[1])
		}
	}
	return ports
}

func ps5(id, host, ip string) fakefleet.Member {
	return fakefleet.PS5(id, host, ip, 15, 0, 16, 0)
}

// ---- SSE client that parses exactly as llama.ts does ----

type sseClient struct {
	t     *testing.T
	body  io.Closer
	lines chan string
}

func (c *cluster) events() *sseClient {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.url+"/api/events", nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.t.Fatalf("/api/events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sseClient{t: c.t, body: resp.Body, lines: make(chan string, 4096)}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
	}()
	c.t.Cleanup(func() { resp.Body.Close() })
	return s
}

// message is one onmessage delivery: {type, data} with data parsed from its string.
type message struct {
	Type string
	Data any
}

func (s *sseClient) next(timeout time.Duration) message {
	s.t.Helper()
	deadline := time.After(timeout)
	var data []string
	for {
		select {
		case l, ok := <-s.lines:
			if !ok {
				s.t.Fatal("event stream closed")
			}
			switch {
			case strings.HasPrefix(l, ":"):
			case strings.HasPrefix(l, "event:"):
				s.t.Fatalf("named event %q is invisible to EventSource.onmessage", l)
			case strings.HasPrefix(l, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
			case l == "" && len(data) > 0:
				var env struct {
					Type string `json:"type"`
					Data any    `json:"data"`
				}
				raw := strings.Join(data, "\n")
				data = nil
				if err := json.Unmarshal([]byte(raw), &env); err != nil {
					s.t.Fatalf("envelope: %v: %s", err, raw)
				}
				str, ok := env.Data.(string)
				if !ok {
					s.t.Fatalf("envelope data is not a JSON string: %s", raw)
				}
				var inner any
				if err := json.Unmarshal([]byte(str), &inner); err != nil {
					s.t.Fatalf("data string: %v: %s", err, str)
				}
				return message{env.Type, inner}
			}
		case <-deadline:
			s.t.Fatal("timed out waiting for an event")
		}
	}
}

type ps5Member = fakefleet.Member

func ptr[T any](v T) *T { return &v }

var httpClientNoKeepAlive = http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
