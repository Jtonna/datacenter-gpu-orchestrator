package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/harness"
)

const wait = 10 * time.Second

func TestMain(m *testing.M) {
	code := m.Run()
	harness.Cleanup()
	os.Exit(code)
}

// ---- helpers ----

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type proc struct {
	cmd    *exec.Cmd
	stdout syncBuf
	stderr syncBuf
	done   chan struct{}
}

func startProc(t *testing.T, env map[string]string, args ...string) *proc {
	t.Helper()
	p := &proc{done: make(chan struct{})}
	p.cmd = exec.Command(harness.BuildFakeLlama(t), args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FAKELLAMA_") && !strings.HasPrefix(kv, "LLAMA_ARG_") {
			p.cmd.Env = append(p.cmd.Env, kv)
		}
	}
	for k, v := range env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.cmd.Process.Kill(); <-p.done })
	return p
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *proc) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(wait):
		t.Fatalf("process did not exit within %v; stderr: %s", wait, p.stderr.String())
		return -1
	}
}

// waitFor polls cond until true or fails.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// listDevices runs the binary with --list-devices and returns stdout and exit code.
func listDevices(t *testing.T, env map[string]string, args ...string) (string, int) {
	t.Helper()
	p := startProc(t, env, append([]string{"--list-devices"}, args...)...)
	code := p.waitExit(t)
	return p.stdout.String(), code
}

// startServer starts a fake server on a free port and returns its base URL.
func startServer(t *testing.T, env map[string]string, extra ...string) (*proc, string) {
	t.Helper()
	port := strconv.Itoa(harness.FreePort(t))
	args := append([]string{"--host", "127.0.0.1", "--port", port, "--model", "/models/gemma.gguf"}, extra...)
	p := startProc(t, env, args...)
	return p, "http://127.0.0.1:" + port
}

// startHealthy starts a server and waits until /health is 200.
func startHealthy(t *testing.T, env map[string]string, extra ...string) (*proc, string) {
	t.Helper()
	p, base := startServer(t, env, extra...)
	harness.WaitForOK(t, base+"/health", wait)
	return p, base
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, decode(t, resp.Body)
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, decode(t, resp.Body)
}

func decode(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	return m
}

func readStartLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad start-log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// ---- --list-devices ----

var deviceLine = regexp.MustCompile(`^  (\S+): (.+) \((\d+) MiB, (\d+) MiB free\)$`)

func TestListDevicesDefaultHasOneRPCDevicePerEndpointPlusCUDA(t *testing.T) {
	out, code := listDevices(t, nil, "--rpc", "10.0.0.2:50052,10.0.0.3:50052")
	if code != 0 {
		t.Fatalf("exit code %d, output %q", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "Available devices:" {
		t.Fatalf("first line %q", lines[0])
	}
	var names []string
	for _, l := range lines[1:] {
		m := deviceLine.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("line does not match llama.cpp format: %q", l)
		}
		names = append(names, m[1])
	}
	if got := strings.Join(names, ","); got != "RPC0,RPC1,CUDA0" {
		t.Fatalf("devices = %s", got)
	}
}

func TestListDevicesWithoutRPCIsJustCUDA(t *testing.T) {
	out, _ := listDevices(t, nil)
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "  CUDA0: ") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestListDevicesCustom(t *testing.T) {
	out, code := listDevices(t, map[string]string{"FAKELLAMA_DEVICES": "RPC0:14948:12000,CUDA0:8151:100"})
	if code != 0 || !strings.HasPrefix(out, "Available devices:\n  RPC0: ") {
		t.Fatalf("code %d output %q", code, out)
	}
	if !strings.Contains(out, "(14948 MiB, 12000 MiB free)") || !strings.Contains(out, "(8151 MiB, 100 MiB free)") {
		t.Fatalf("sizes missing: %q", out)
	}
}

func TestListDevicesNone(t *testing.T) {
	out, _ := listDevices(t, map[string]string{"FAKELLAMA_DEVICES": "none"})
	if out != "Available devices:\n  (none)\n" {
		t.Fatalf("output %q", out)
	}
}

func TestListDevicesFailure(t *testing.T) {
	p := startProc(t, map[string]string{"FAKELLAMA_LIST_DEVICES_EXIT": "3"}, "--list-devices")
	if code := p.waitExit(t); code != 3 {
		t.Fatalf("exit code %d", code)
	}
	if p.stderr.String() == "" {
		t.Fatal("expected a message on stderr")
	}
}

func TestListDevicesHang(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.jsonl")
	p := startProc(t, map[string]string{"FAKELLAMA_LIST_DEVICES_HANG": "1", "FAKELLAMA_START_LOG": log}, "--list-devices")
	waitFor(t, "start log entry", func() bool { return len(readStartLog(t, log)) == 1 })
	time.Sleep(300 * time.Millisecond) // negative check: it must still be alive
	if p.exited() {
		t.Fatal("process exited; expected it to hang")
	}
	if p.stdout.String() != "" {
		t.Fatalf("hung process printed %q", p.stdout.String())
	}
}

// ---- command line and start log ----

func TestStartLogRecordsArgvAndEnv(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.jsonl")
	env := map[string]string{"FAKELLAMA_START_LOG": log, "LLAMA_ARG_HOST": "leak"}
	args := []string{"--host", "127.0.0.1", "--port", "0", "--model", "m.gguf",
		"--n-gpu-layers", "999", "--fit", "off", "--jinja", "--cache-ram=2048",
		"--rpc", "a:1", "--device", "RPC0", "-ctk", "q8_0", "--flash-attn", "on", "--list-devices"}
	p := startProc(t, env, args...)
	p.waitExit(t)
	entries := readStartLog(t, log)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	got := entries[0]["argv"].([]any)
	if len(got) != len(args) {
		t.Fatalf("argv = %v", got)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("argv[%d] = %v, want %s", i, got[i], args[i])
		}
	}
	e := entries[0]["env"].(map[string]any)
	if e["LLAMA_ARG_HOST"] != "leak" || e["FAKELLAMA_START_LOG"] != log {
		t.Fatalf("env = %v", e)
	}
}

func TestStartLogAppendsOneLinePerStart(t *testing.T) {
	log := filepath.Join(t.TempDir(), "starts.jsonl")
	env := map[string]string{"FAKELLAMA_START_LOG": log}
	listDevices(t, env)
	listDevices(t, env)
	if n := len(readStartLog(t, log)); n != 2 {
		t.Fatalf("got %d entries, want 2", n)
	}
}

func TestUnknownFlagsWithAndWithoutValuesAreTolerated(t *testing.T) {
	_, base := startHealthy(t, nil,
		"--tensor-split", "60,40", "--ctx-size", "8192", "--parallel", "2",
		"--rpc", "10.0.0.2:50052", "--device", "RPC0",
		"--some-future-flag", "--another", "value", "--neg", "-1", "-x", "--eq=1")
	if code, _ := getJSON(t, base+"/props"); code != 200 {
		t.Fatalf("props %d", code)
	}
}

// ---- health and lifecycle ----

func TestHealthLoadingThenOK(t *testing.T) {
	_, base := startServer(t, map[string]string{"FAKELLAMA_LOAD_DELAY": "1s"})
	harness.WaitForStatus(t, base+"/health", 503, wait)
	code, body := getJSON(t, base+"/health")
	if code == 503 { // on a very slow machine it may already be ready
		e := body["error"].(map[string]any)
		if e["message"] != "Loading model" || e["type"] != "unavailable_error" || e["code"] != float64(503) {
			t.Fatalf("loading body = %v", body)
		}
		if c, _ := getJSON(t, base+"/props"); c != 503 {
			t.Fatalf("/props while loading = %d, want 503", c)
		}
	}
	harness.WaitForOK(t, base+"/health", wait)
	_, body = getJSON(t, base+"/health")
	if body["status"] != "ok" {
		t.Fatalf("ready body = %v", body)
	}
}

func TestNeverHealthyStaysLoading(t *testing.T) {
	_, base := startServer(t, map[string]string{"FAKELLAMA_NEVER_HEALTHY": "1"})
	harness.WaitForStatus(t, base+"/health", 503, wait)
	time.Sleep(300 * time.Millisecond) // negative check
	if code, _ := getJSON(t, base+"/health"); code != 503 {
		t.Fatalf("health = %d, want 503", code)
	}
}

func TestExitAfterDelayWithCode(t *testing.T) {
	p, _ := startServer(t, map[string]string{"FAKELLAMA_EXIT_CODE": "7", "FAKELLAMA_EXIT_AFTER": "100ms"})
	if code := p.waitExit(t); code != 7 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(p.stderr.String(), "fakellama") {
		t.Fatalf("stderr = %q", p.stderr.String())
	}
}

func TestSigtermExitsCleanly(t *testing.T) {
	skipIfNoSigterm(t)
	p, _ := startHealthy(t, nil)
	p.cmd.Process.Signal(syscall.SIGTERM)
	if code := p.waitExit(t); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestSigtermIgnoredWhenAsked(t *testing.T) {
	skipIfNoSigterm(t)
	p, _ := startHealthy(t, map[string]string{"FAKELLAMA_IGNORE_SIGTERM": "1"})
	p.cmd.Process.Signal(syscall.SIGTERM)
	waitFor(t, "ignore message", func() bool { return strings.Contains(p.stderr.String(), "ignoring") })
	if p.exited() {
		t.Fatal("process exited despite ignoring SIGTERM")
	}
}

func skipIfNoSigterm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go cannot deliver SIGTERM to a child on Windows")
	}
}

// ---- endpoints ----

func TestChatCompletionNonStreaming(t *testing.T) {
	_, base := startHealthy(t, nil)
	code, body := postJSON(t, base+"/v1/chat/completions", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || body["object"] != "chat.completion" {
		t.Fatalf("%d %v", code, body)
	}
	msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] == "" {
		t.Fatalf("message = %v", msg)
	}
	if _, ok := body["usage"].(map[string]any)["completion_tokens"]; !ok {
		t.Fatalf("usage = %v", body["usage"])
	}
	if _, ok := body["timings"].(map[string]any); !ok {
		t.Fatalf("timings missing: %v", body)
	}
}

type sseResult struct {
	chunks []map[string]any
	done   bool
	err    error
}

// readSSE reads data lines from a streaming chat completion until [DONE],
// EOF, or ctx expiry.
func readSSE(ctx context.Context, url string) sseResult {
	var res sseResult
	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(`{"stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.err = err
		return res
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			res.done = true
			return res
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			res.err = err
			return res
		}
		res.chunks = append(res.chunks, m)
	}
	res.err = sc.Err()
	return res
}

func TestChatCompletionStreaming(t *testing.T) {
	_, base := startHealthy(t, map[string]string{"FAKELLAMA_STREAM_CHUNKS": "4", "FAKELLAMA_STREAM_DELAY": "60ms"})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	start := time.Now()
	res := readSSE(ctx, base+"/v1/chat/completions")
	elapsed := time.Since(start)
	if res.err != nil || !res.done {
		t.Fatalf("done=%v err=%v", res.done, res.err)
	}
	if len(res.chunks) != 5 { // 4 content chunks + final
		t.Fatalf("got %d chunks", len(res.chunks))
	}
	if elapsed < 3*60*time.Millisecond {
		t.Fatalf("finished in %v; delay between chunks not applied", elapsed)
	}
	for _, c := range res.chunks[:4] {
		if c["object"] != "chat.completion.chunk" || c["usage"] != nil {
			t.Fatalf("content chunk = %v", c)
		}
	}
	last := res.chunks[4]
	if last["usage"] == nil || last["timings"] == nil {
		t.Fatalf("final chunk lacks usage/timings: %v", last)
	}
	if fr := last["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "stop" {
		t.Fatalf("finish_reason = %v", fr)
	}
}

func TestStreamStallsAfterNChunks(t *testing.T) {
	_, base := startHealthy(t, map[string]string{"FAKELLAMA_STREAM_CHUNKS": "5", "FAKELLAMA_STALL_AFTER": "2"})
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	res := readSSE(ctx, base+"/v1/chat/completions")
	if res.done {
		t.Fatal("stream finished; expected a stall")
	}
	if res.err == nil {
		t.Fatal("expected the read to time out")
	}
	if len(res.chunks) != 2 {
		t.Fatalf("got %d chunks before the stall, want 2", len(res.chunks))
	}
}

func TestOtherEndpoints(t *testing.T) {
	_, base := startHealthy(t, nil, "--ctx-size", "8192", "--parallel", "2")

	t.Run("completions", func(t *testing.T) {
		code, body := postJSON(t, base+"/v1/completions", `{"prompt":"hi"}`)
		if code != 200 || body["object"] != "text_completion" || body["usage"] == nil {
			t.Fatalf("%d %v", code, body)
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		code, body := postJSON(t, base+"/v1/embeddings", `{"input":"hi"}`)
		if code != 200 || len(body["data"].([]any)) != 1 {
			t.Fatalf("%d %v", code, body)
		}
	})
	t.Run("props", func(t *testing.T) {
		code, body := getJSON(t, base+"/props")
		if code != 200 {
			t.Fatal(code)
		}
		if body["total_slots"] != float64(2) || body["model_path"] != "/models/gemma.gguf" || body["n_ctx"] != float64(4096) {
			t.Fatalf("props = %v", body)
		}
		dgs := body["default_generation_settings"].(map[string]any)
		if dgs["n_ctx"] != float64(4096) {
			t.Fatalf("default_generation_settings = %v", dgs)
		}
	})
	t.Run("slots", func(t *testing.T) {
		resp, err := http.Get(base + "/slots")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var slots []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
			t.Fatal(err)
		}
		if len(slots) != 2 {
			t.Fatalf("got %d slots", len(slots))
		}
		for i, s := range slots {
			if s["id"] != float64(i) || s["n_ctx"] != float64(4096) || s["is_processing"] != false {
				t.Fatalf("slot %d = %v", i, s)
			}
		}
	})
	t.Run("models", func(t *testing.T) {
		code, body := getJSON(t, base+"/v1/models")
		if code != 200 || body["object"] != "list" || len(body["data"].([]any)) != 1 {
			t.Fatalf("%d %v", code, body)
		}
	})
}

func TestFakeNvidiaSmiMode(t *testing.T) {
	p := startProc(t, nil, "--nvidia-smi", "8151", "500")
	<-p.done
	if got := strings.TrimSpace(p.stdout.String()); got != "8151, 500" {
		t.Fatalf("stdout = %q, want %q", got, "8151, 500")
	}
	p = startProc(t, nil, "--nvidia-smi")
	<-p.done
	if got := strings.TrimSpace(p.stdout.String()); got != "8151, 0" {
		t.Fatalf("defaults: stdout = %q", got)
	}
}

func TestListDevicesHangOnlyForNamedEndpoints(t *testing.T) {
	env := map[string]string{"FAKELLAMA_LIST_DEVICES_HANG_ADDR": "10.0.0.2:50052, 10.0.0.9:50052"}
	hung := startProc(t, env, "--rpc", "10.0.0.2:50052", "--list-devices")
	ok := startProc(t, env, "--rpc", "10.0.0.4:50052", "--list-devices")
	ok.waitExit(t)
	if !strings.Contains(ok.stdout.String(), "RPC0: 10.0.0.4:50052") {
		t.Fatalf("unlisted endpoint should answer, got %q", ok.stdout.String())
	}
	time.Sleep(300 * time.Millisecond)
	if hung.exited() {
		t.Fatal("listed endpoint should hang")
	}
}
