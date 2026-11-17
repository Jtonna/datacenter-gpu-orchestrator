package e2e

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	psA = "aa:bb:cc:00:00:01"
	psB = "aa:bb:cc:00:00:02"
)

// profilesBasic: a small model that fits a PS5 (with an old alias and a
// colon in its name, as the hub sees it), two models that each need most of
// a PS5, and one pinned to the local GPU.
const profilesBasic = `  "tiny:2k":
    model: tiny.gguf
    ctx: 2048
    needs_mib: 1000
    allow_tiers: [1]
    aliases: [tiny-old]
  big-a:
    model: big-a.gguf
    ctx: 4096
    needs_mib: 12000
    allow_tiers: [1]
  big-b:
    model: big-b.gguf
    ctx: 4096
    needs_mib: 12000
    allow_tiers: [1]
  local-only:
    model: local.gguf
    ctx: 4096
    needs_mib: 1000
    allow_tiers: [3]
`

func withPS5(t *testing.T, env map[string]string) *cluster {
	t.Helper()
	o := options{profiles: profilesBasic, env: env}
	o.members = append(o.members, ps5(psA, "ps5-062e1d84", "10.0.0.2"))
	return startCluster(t, o)
}

// 1. A chat completion triggers pre-flight and the load, and streams through.
func TestChatTriggersPreflightLoadAndStreams(t *testing.T) {
	c := withPS5(t, map[string]string{"FAKELLAMA_STREAM_CHUNKS": "5"})
	status, body := c.chat("tiny:2k", true)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	for i := 0; i < 5; i++ {
		if !strings.Contains(body, "tok"+string(rune('0'+i))) {
			t.Fatalf("chunk %d missing from stream:\n%s", i, body)
		}
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream not terminated:\n%s", body)
	}
	// Pre-flight ran against the PS5 before the child was started.
	starts := c.starts()
	if len(starts) != 2 || starts[0].Mode != "list-devices" || starts[1].Mode != "serve" {
		t.Fatalf("starts = %+v, want one list-devices probe then one serve", starts)
	}
	if !strings.Contains(strings.Join(starts[0].Argv, " "), "--rpc 10.0.0.2:50052") {
		t.Fatalf("probe argv = %v", starts[0].Argv)
	}
	if got := strings.Join(starts[1].Argv, " "); !strings.Contains(got, "--rpc 10.0.0.2:50052") || !strings.Contains(got, "--device RPC0") || !strings.Contains(got, "--fit off") {
		t.Fatalf("serve argv = %s", got)
	}
}

// 2. A second model queues behind the first, evicts it when idle, and loads.
func TestSecondModelQueuesEvictsFirstAndLoads(t *testing.T) {
	c := withPS5(t, map[string]string{"FAKELLAMA_STREAM_CHUNKS": "5", "FAKELLAMA_STREAM_DELAY": "300ms"})
	aDone := make(chan int, 1)
	go func() { s, _ := c.chat("big-a", true); aDone <- s }()
	c.waitFor("big-a in flight", 20*time.Second, func() bool {
		return inFlight(c, "big-a") == 1
	})
	bDone := make(chan int, 1)
	go func() { s, _ := c.chat("big-b", false); bDone <- s }()
	c.waitFor("big-b queued", 10*time.Second, func() bool {
		q, _ := c.jsonGet("/api/status")["queue"].([]any)
		for _, e := range q {
			if e.(map[string]any)["profile"] == "big-b" {
				return true
			}
		}
		return false
	})
	select {
	case s := <-bDone:
		t.Fatalf("big-b answered %d while big-a held the only usable node", s)
	default:
	}
	if s := <-aDone; s != 200 {
		t.Fatalf("big-a: %d (a running generation must never be cut off)", s)
	}
	select {
	case s := <-bDone:
		if s != 200 {
			t.Fatalf("big-b: %d", s)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("big-b never loaded")
	}
	if got := c.runningModels(); len(got) != 1 || got[0] != "big-b:ready" {
		t.Fatalf("running = %v, want only big-b (big-a evicted)", got)
	}
	if n := c.countStarts("serve"); n != 2 {
		t.Fatalf("serve starts = %d, want 2", n)
	}
}

func inFlight(c *cluster, profile string) int {
	insts, _ := c.jsonGet("/api/status")["instances"].([]any)
	for _, i := range insts {
		i := i.(map[string]any)
		if i["profile"] == profile {
			n, _ := i["in_flight"].(float64)
			return int(n)
		}
	}
	return 0
}

// 3. /running has the A.1 shape and the hub's regexes work on cmd.
func TestRunningShapeAndHubRegexes(t *testing.T) {
	c := withPS5(t, nil)
	if status, _ := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("chat: %d", status)
	}
	var raw struct {
		Running []map[string]any `json:"running"`
	}
	_, b := c.get("/running")
	if err := json.Unmarshal([]byte(b), &raw); err != nil || len(raw.Running) != 1 {
		t.Fatalf("running = %s (%v)", b, err)
	}
	r := raw.Running[0]
	if r["model"] != "tiny:2k" || r["name"] != "tiny:2k" || r["state"] != "ready" {
		t.Fatalf("entry = %v", r)
	}
	cmd := r["cmd"].(string)
	rpc := regexp.MustCompile(`--rpc[= ]+(\S+)`).FindStringSubmatch(cmd)
	ctx := regexp.MustCompile(`(?:--ctx-size|-c)[= ]+(\d+)`).FindStringSubmatch(cmd)
	if rpc == nil || rpc[1] != "10.0.0.2:50052" || ctx == nil || ctx[1] != "2048" {
		t.Fatalf("hub regexes on %q: rpc=%v ctx=%v", cmd, rpc, ctx)
	}
}

// 4. /upstream/{model}/props passes through for a ready model; an unloaded
// model answers 503 at once and nothing is started.
func TestUpstreamPropsReadyAndUnloaded(t *testing.T) {
	c := withPS5(t, nil)
	c.chat("tiny:2k", false)
	hubPath := "/upstream/" + strings.ReplaceAll(url.QueryEscape("tiny:2k"), "+", "%20") + "/props" // python quote(safe='')
	if !strings.Contains(hubPath, "%3A") {
		t.Fatalf("test path %q should encode the colon", hubPath)
	}
	status, body := c.get(hubPath)
	if status != 200 || !strings.Contains(body, `"n_ctx"`) {
		t.Fatalf("props of ready model: %d %s", status, body)
	}
	if status, body = c.get("/upstream/tiny:2k/slots"); status != 200 || !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("slots: %d %s", status, body)
	}
	serves := c.countStarts("serve")
	start := time.Now()
	status, body = c.get("/upstream/big-a/props")
	if status != 503 {
		t.Fatalf("props of unloaded model: %d %s", status, body)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("/upstream waited")
	}
	time.Sleep(300 * time.Millisecond) // a load, had one been started, would show by now
	if c.countStarts("serve") != serves || len(c.running()) != 1 {
		t.Fatalf("/upstream started something: running %v", c.runningModels())
	}
}

// 5. /api/events: initial events, then an inflight upsert/remove pair, parsed as llama.ts does.
func TestEventsInitialAndInflightPair(t *testing.T) {
	c := withPS5(t, map[string]string{"FAKELLAMA_STREAM_CHUNKS": "4", "FAKELLAMA_STREAM_DELAY": "250ms"})
	ev := c.events()
	first := ev.next(5 * time.Second)
	if first.Type != "modelStatus" {
		t.Fatalf("first message = %+v", first)
	}
	models := first.Data.([]any)
	if len(models) != 4 {
		t.Fatalf("modelStatus lists %d profiles, want all 4: %v", len(models), models)
	}
	for _, m := range models {
		m := m.(map[string]any)
		if m["state"] != "stopped" {
			t.Fatalf("model = %v", m)
		}
	}
	snap := ev.next(5 * time.Second)
	if snap.Type != "inflight" || snap.Data.(map[string]any)["operation"] != "snapshot" {
		t.Fatalf("second message = %+v", snap)
	}

	done := make(chan struct{})
	go func() { c.chat("tiny:2k", true); close(done) }()
	var upsertID string
	var sawReady bool
	for upsertID == "" {
		m := ev.next(30 * time.Second)
		switch m.Type {
		case "modelStatus":
			for _, e := range m.Data.([]any) {
				e := e.(map[string]any)
				if e["id"] == "tiny:2k" && e["state"] == "ready" {
					sawReady = true
					if _, err := time.Parse(time.RFC3339, e["readySince"].(string)); err != nil {
						t.Fatalf("readySince: %v", err)
					}
				}
			}
		case "inflight":
			d := m.Data.(map[string]any)
			if d["operation"] != "upsert" {
				t.Fatalf("inflight = %v", d)
			}
			r := d["request"].(map[string]any)
			id, isStr := r["id"].(string)
			if !isStr || id == "" || r["model"] != "tiny:2k" {
				t.Fatalf("request = %v", r)
			}
			if _, err := time.Parse(time.RFC3339, r["timestamp"].(string)); err != nil {
				t.Fatalf("timestamp: %v", err)
			}
			upsertID = id
		}
	}
	if !sawReady {
		t.Fatal("no modelStatus ready before the request was admitted")
	}
	var removed, activity bool
	for !(removed && activity) {
		m := ev.next(30 * time.Second)
		if m.Type == "inflight" {
			if d := m.Data.(map[string]any); d["operation"] == "remove" {
				if d["id"] != upsertID {
					t.Fatalf("remove id %v != upsert id %s", d["id"], upsertID)
				}
				removed = true
			}
		}
		if m.Type == "activity" {
			activity = true
		}
	}
	<-done
}

// 6. /api/metrics/activity after a request.
func TestActivityAfterRequest(t *testing.T) {
	c := withPS5(t, map[string]string{"FAKELLAMA_STREAM_CHUNKS": "5"})
	c.chat("tiny:2k", false)
	c.chat("tiny:2k", true)
	data := c.jsonGet("/api/metrics/activity")["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %v", data)
	}
	for i, r := range data {
		r := r.(map[string]any)
		if r["model"] != "tiny:2k" || r["req_path"] != "/v1/chat/completions" || r["resp_status_code"].(float64) != 200 {
			t.Fatalf("record %d = %v", i, r)
		}
		tk := r["tokens"].(map[string]any)
		if tk["input_tokens"].(float64) != 10 || tk["output_tokens"].(float64) != 5 || tk["tokens_per_second"].(float64) != 100 {
			t.Fatalf("record %d tokens = %v (fakellama reports 10 in, 5 out, 100 tok/s)", i, tk)
		}
	}
	if data[0].(map[string]any)["id"].(float64) >= data[1].(map[string]any)["id"].(float64) {
		t.Fatal("ids must increase")
	}
}

// 7. Aliases resolve (and are logged); unknown models are 404.
func TestAliasAndUnknownModel(t *testing.T) {
	c := withPS5(t, nil)
	if status, body := c.chat("tiny-old", false); status != 200 {
		t.Fatalf("alias: %d %s", status, body)
	}
	if got := c.running(); len(got) != 1 || got[0].Model != "tiny:2k" {
		t.Fatalf("running after an alias request = %v (must show the profile, not the alias)", got)
	}
	if !strings.Contains(c.stderr.String(), "tiny-old") || !strings.Contains(c.stderr.String(), "127.0.0.1") {
		t.Fatalf("alias use not logged with the caller:\n%s", c.stderr.String())
	}
	status, body := c.chat("no-such-model", false)
	if status != 404 {
		t.Fatalf("unknown: %d %s", status, body)
	}
	var e struct {
		Error struct{ Message, Type, Code string }
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Message == "" || e.Error.Type != "invalid_request_error" {
		t.Fatalf("not an OpenAI error: %s", body)
	}
	status, body = c.get("/v1/models")
	if status != 200 || strings.Contains(body, "tiny-old") || !strings.Contains(body, `"tiny:2k"`) {
		t.Fatalf("/v1/models: %d %s", status, body)
	}
}

// 8. POST /api/plan changes nothing.
func TestPlanChangesNothing(t *testing.T) {
	c := withPS5(t, nil)
	status, body := c.post("/api/plan", `{"model":"big-a"}`)
	if status != 200 {
		t.Fatalf("plan: %d %s", status, body)
	}
	var p map[string]any
	json.Unmarshal([]byte(body), &p)
	if p["action"] != "start" || p["model"] != "big-a" {
		t.Fatalf("plan = %s", body)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(c.starts()); n != 0 {
		t.Fatalf("plan started processes: %+v", c.starts())
	}
	if got := c.running(); len(got) != 0 {
		t.Fatalf("plan changed state: %v", got)
	}
	st := c.jsonGet("/api/status")
	if len(st["instances"].([]any)) != 0 || len(st["queue"].([]any)) != 0 {
		t.Fatalf("status after plan: %v", st)
	}
}

// 9. A pre-flight hang quarantines that node and the request lands on another.
func TestPreflightFailureQuarantinesAndFallsOver(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 10 s: the pre-flight timeout")
	}
	// psA is smaller, so the placer tries it first ("smallest budget that fits").
	small := ps5(psA, "ps5-aaaa", "10.0.0.2")
	small.VRAMTotal, small.VRAMUsed = ptr(12.0), ptr(0.0)
	good := ps5(psB, "ps5-bbbb", "10.0.0.4")
	c := startCluster(t, options{
		members:  []ps5Member{small, good},
		profiles: profilesBasic,
		env:      map[string]string{"FAKELLAMA_LIST_DEVICES_HANG_ADDR": "10.0.0.2:50052"},
	})
	status, body := c.chat("tiny:2k", false)
	if status != 200 {
		t.Fatalf("request: %d %s", status, body)
	}
	st := c.jsonGet("/api/status")
	q, _ := st["quarantines"].([]any)
	if len(q) != 1 || q[0].(map[string]any)["node"] != psA {
		t.Fatalf("quarantines = %v, want %s", q, psA)
	}
	run := c.running()
	if len(run) != 1 || !strings.Contains(run[0].Cmd, "--rpc 10.0.0.4:50052") || strings.Contains(run[0].Cmd, "10.0.0.2") {
		t.Fatalf("running = %+v, want the model on 10.0.0.4", run)
	}
	var probes []string
	for _, s := range c.starts() {
		if s.Mode == "list-devices" {
			probes = append(probes, strings.Join(s.Argv, " "))
		}
	}
	if len(probes) < 2 {
		t.Fatalf("probes = %v, want the failing one then the good one", probes)
	}
}

// 10. A clean exit on SIGTERM / interrupt, with the children gone.
func TestCleanShutdownOnSignal(t *testing.T) {
	c := withPS5(t, nil)
	if status, body := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	ports := c.childPorts()
	if len(ports) != 1 {
		t.Fatalf("child ports = %v", ports)
	}
	if err := interrupt(c.cmd); err != nil {
		t.Skipf("cannot deliver an interrupt to the subprocess on this platform: %v", err)
	}
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		t.Fatal("llm-scheduler did not exit after the signal")
	}
	if c.exitErr != nil {
		t.Fatalf("exit: %v (want code 0)\n%s", c.exitErr, c.stderr.String())
	}
	if !c.cmd.ProcessState.Success() {
		t.Fatalf("exit status %v", c.cmd.ProcessState)
	}
	waitGone(t, "http://127.0.0.1:"+ports[0]+"/health")
	waitGone(t, c.url+"/health")
}

func waitGone(t *testing.T, u string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := (&httpClientNoKeepAlive).Get(u)
		if err != nil {
			return
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatalf("%s still answers", u)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 11. With the fleet console unreachable, a clear error is logged and the
// local node still serves.
func TestFleetUnreachableStillServesLocalNode(t *testing.T) {
	c := startCluster(t, options{profiles: profilesBasic, fleetURL: "http://127.0.0.1:1"})
	c.waitFor("the fleet error in the log", 10*time.Second, func() bool {
		return strings.Contains(c.stderr.String(), "cannot reach the fleet console")
	})
	if status, body := c.chat("local-only", false); status != 200 {
		t.Fatalf("local model with the fleet down: %d %s", status, body)
	}
}

// Defect 1: llama.cpp prints the buffer-size lines only from -lv 4 (fakellama
// copies that), so per-device memory in /api/status proves the scheduler
// passes the flag; /running shows it in the documented position; and the
// child's chatty output is not echoed to the scheduler's own stderr.
func TestChildVerbosityFeedsMemoryAndStderrStaysQuiet(t *testing.T) {
	c := withPS5(t, map[string]string{"FAKELLAMA_LOAD_LOG": "auto"})
	if status, body := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("chat: %d %s", status, body)
	}
	run := c.running()
	if len(run) != 1 || !strings.Contains(run[0].Cmd, "--cache-ram 2048 -lv 4 --rpc 10.0.0.2:50052 --device RPC0") {
		t.Fatalf("cmd = %v", run)
	}
	insts, _ := c.jsonGet("/api/status")["instances"].([]any)
	if len(insts) != 1 {
		t.Fatalf("instances = %v", insts)
	}
	in := insts[0].(map[string]any)
	if total, _ := in["device_total_mib"].(float64); total != 1300 {
		t.Fatalf("device_total_mib = %v, want 1300 (1000 model + 200 KV + 100 compute): %v", in["device_total_mib"], in)
	}
	for _, noise := range []string{"buffer size", "load_tensors", "llama_kv_cache"} {
		if strings.Contains(c.stderr.String(), noise) {
			t.Fatalf("child output leaked into the scheduler log (%q):\n%s", noise, c.stderr.String())
		}
	}
}

// Defect 2: a child placed only on remote workers must not initialise CUDA on
// the host GPU; one placed on the local node keeps the inherited variable.
func TestRemoteOnlyChildHidesHostGPULocalChildInherits(t *testing.T) {
	c := withPS5(t, nil)
	if status, body := c.chat("tiny:2k", false); status != 200 {
		t.Fatalf("remote chat: %d %s", status, body)
	}
	if status, body := c.chat("local-only", false); status != 200 {
		t.Fatalf("local chat: %d %s", status, body)
	}
	var remoteEnv, localEnv map[string]string
	for _, s := range c.starts() {
		if s.Mode != "serve" {
			continue
		}
		cmd := strings.Join(s.Argv, " ")
		switch {
		case strings.Contains(cmd, "--rpc 10.0.0.2:50052"):
			remoteEnv = s.Env
		case strings.Contains(cmd, "--device CUDA0") && !strings.Contains(cmd, "--rpc"):
			localEnv = s.Env
		}
	}
	if remoteEnv == nil || localEnv == nil {
		t.Fatalf("start log lacks a remote or local serve entry: %+v", c.starts())
	}
	if v, ok := remoteEnv["CUDA_VISIBLE_DEVICES"]; !ok || v != "" {
		t.Fatalf("remote-only child: CUDA_VISIBLE_DEVICES = %q (set=%v), want set and empty; env %v", v, ok, remoteEnv)
	}
	inherited, wasSet := os.LookupEnv("CUDA_VISIBLE_DEVICES")
	if v, ok := localEnv["CUDA_VISIBLE_DEVICES"]; ok != wasSet || v != inherited {
		t.Fatalf("local child: CUDA_VISIBLE_DEVICES = %q (set=%v), want inherited %q (set=%v)", v, ok, inherited, wasSet)
	}
}
