package instance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/harness"
)

func TestMain(m *testing.M) {
	code := m.Run()
	harness.Cleanup()
	os.Exit(code)
}

func TestParseListDevices(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    ProbeResult
		wantErr ProbeFailure
	}{
		{"fakellama style", "Available devices:\n  RPC0: 192.0.2.2:50052 (15000 MiB, 14000 MiB free)\n", ProbeResult{15000, 14000}, 0},
		{"other devices ignored", "Available devices:\n  CUDA0: NVIDIA GeForce RTX 5060 (8151 MiB, 7000 MiB free)\n  RPC0: 192.0.2.2:50052 (15000 MiB, 14000 MiB free)\n", ProbeResult{15000, 14000}, 0},
		{"description with parentheses and colons", "Available devices:\n  RPC0: AMD Radeon (TM) Graphics: gfx1013 (rev 0) [host:port] (14948 MiB, 9021 MiB free)\n", ProbeResult{14948, 9021}, 0},
		{"CRLF", "Available devices:\r\n  RPC0: x (100 MiB, 50 MiB free)\r\n", ProbeResult{100, 50}, 0},
		{"no leading spaces", "RPC0: x (100 MiB, 50 MiB free)\n", ProbeResult{100, 50}, 0},
		{"free zero", "  RPC0: x (100 MiB, 0 MiB free)\n", ProbeResult{100, 0}, 0},
		{"none printed", "Available devices:\n  (none)\n", ProbeResult{}, ProbeNoRPCLine},
		{"only local device", "Available devices:\n  CUDA0: GPU (8151 MiB, 7000 MiB free)\n", ProbeResult{}, ProbeNoRPCLine},
		{"empty", "", ProbeResult{}, ProbeNoRPCLine},
		{"rpc-like name not a device line", "  RPCX is great (1 MiB, 1 MiB free)\n", ProbeResult{}, ProbeNoRPCLine},
		{"malformed rpc line", "  RPC0: x (lots of MiB)\n", ProbeResult{}, ProbeNoRPCLine},
		{"two rpc lines", "  RPC0: a:1 (100 MiB, 50 MiB free)\n  RPC1: a:1 (100 MiB, 50 MiB free)\n", ProbeResult{}, ProbeMultipleRPCLines},
		{"two rpc lines one malformed", "  RPC0: a:1 (100 MiB, 50 MiB free)\n  RPC1: broken\n", ProbeResult{}, ProbeMultipleRPCLines},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseListDevices(tc.out)
			if tc.wantErr == 0 {
				if err != nil || got != tc.want {
					t.Fatalf("got %+v err %v, want %+v", got, err, tc.want)
				}
				return
			}
			var pe *ProbeError
			if !errors.As(err, &pe) || pe.Kind != tc.wantErr {
				t.Fatalf("err = %v, want kind %v", err, tc.wantErr)
			}
		})
	}
}

func TestProbeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "LLAMA_ARG_MODEL=x", "llama_arg_lower=1", "CUDA_VISIBLE_DEVICES=0", "LLAMA_ARGX=keep", "HOME=/h"}
	got := probeEnv(base)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "LLAMA_ARG_MODEL") {
		t.Errorf("LLAMA_ARG_ not removed: %v", got)
	}
	if !strings.Contains(joined, "LLAMA_ARGX=keep") || !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/h") {
		t.Errorf("unrelated vars lost: %v", got)
	}
	n := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "CUDA_VISIBLE_DEVICES=") {
			n++
			if kv != "CUDA_VISIBLE_DEVICES=" {
				t.Errorf("CUDA_VISIBLE_DEVICES = %q, want empty", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("CUDA_VISIBLE_DEVICES appears %d times", n)
	}
}

func probeFake(t *testing.T, timeout time.Duration, env map[string]string) (ProbeResult, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	p := &ExecProber{ServerBin: harness.BuildFakeLlama(t), Timeout: timeout}
	return p.Probe(context.Background(), "192.0.2.2:50052")
}

func probeKind(t *testing.T, err error) ProbeFailure {
	t.Helper()
	var pe *ProbeError
	if !errors.As(err, &pe) {
		t.Fatalf("not a ProbeError: %v", err)
	}
	return pe.Kind
}

func TestExecProberSuccess(t *testing.T) {
	r, err := probeFake(t, 10*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r != (ProbeResult{TotalMiB: 15000, FreeMiB: 14000}) {
		t.Fatalf("got %+v", r)
	}
}

func TestExecProberFailures(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want ProbeFailure
	}{
		{"hang is a timeout", map[string]string{"FAKELLAMA_LIST_DEVICES_HANG": "1"}, ProbeTimeout},
		{"non-zero exit", map[string]string{"FAKELLAMA_LIST_DEVICES_EXIT": "3"}, ProbeExit},
		{"no rpc line", map[string]string{"FAKELLAMA_DEVICES": "CUDA0:8000:7000"}, ProbeNoRPCLine},
		{"two rpc lines", map[string]string{"FAKELLAMA_DEVICES": "RPC0:100:50,RPC1:100:50"}, ProbeMultipleRPCLines},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := probeFake(t, 700*time.Millisecond, tc.env)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := probeKind(t, err); got != tc.want {
				t.Fatalf("kind = %v want %v (%v)", got, tc.want, err)
			}
		})
	}
}

func TestExecProberExitCarriesStderr(t *testing.T) {
	_, err := probeFake(t, 5*time.Second, map[string]string{"FAKELLAMA_LIST_DEVICES_EXIT": "3"})
	if !strings.Contains(err.Error(), "simulated --list-devices failure") {
		t.Fatalf("error lacks stderr: %v", err)
	}
	var pe *ProbeError
	errors.As(err, &pe)
	if pe.ExitCode != 3 {
		t.Fatalf("exit code = %d", pe.ExitCode)
	}
}

func TestExecProberTimeoutIsBounded(t *testing.T) {
	start := time.Now()
	_, err := probeFake(t, 300*time.Millisecond, map[string]string{"FAKELLAMA_LIST_DEVICES_HANG": "1"})
	if probeKind(t, err) != ProbeTimeout {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("probe took %v", el)
	}
}

func TestExecProberMissingBinary(t *testing.T) {
	p := &ExecProber{ServerBin: "definitely-not-a-real-binary-xyz", Timeout: time.Second}
	_, err := p.Probe(context.Background(), "a:1")
	if probeKind(t, err) != ProbeStart {
		t.Fatalf("err = %v", err)
	}
}

func TestExecProberCallerCancel(t *testing.T) {
	t.Setenv("FAKELLAMA_LIST_DEVICES_HANG", "1")
	ctx, cancel := context.WithCancel(context.Background())
	p := &ExecProber{ServerBin: harness.BuildFakeLlama(t), Timeout: time.Minute}
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err := p.Probe(ctx, "a:1")
	if probeKind(t, err) != ProbeCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecProberScrubsEnvironment(t *testing.T) {
	logPath := t.TempDir() + "/start.jsonl"
	t.Setenv("FAKELLAMA_START_LOG", logPath)
	t.Setenv("LLAMA_ARG_MODEL", "/should/not/be/seen")
	t.Setenv("LLAMA_ARG_N_GPU_LAYERS", "9")
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	if _, err := probeFake(t, 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Mode string
		Argv []string
		Env  map[string]string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Mode != "list-devices" {
		t.Fatalf("mode %q", rec.Mode)
	}
	if strings.Join(rec.Argv, " ") != "--rpc 192.0.2.2:50052 --list-devices" {
		t.Fatalf("argv = %q", rec.Argv)
	}
	for k := range rec.Env {
		if strings.HasPrefix(k, "LLAMA_ARG_") {
			t.Errorf("child saw %s", k)
		}
	}
	if v, ok := rec.Env["CUDA_VISIBLE_DEVICES"]; !ok || v != "" {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q (set=%v), want set and empty", v, ok)
	}
}

// The probe hides CUDA on purpose, so llama.cpp always prints a CUDA init
// failure on stderr. It must not lead the reason, and the stack trace is noise.
const realConnectRefusedStderr = "0.00.021.162 E ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected\n" +
	"/src/ggml/src/ggml-rpc/ggml-rpc.cpp:653: Failed to connect to 192.0.2.250:50052\n\n" +
	"llama-server-rpc(+0x1c95a7b)[0x5a9a3448ea7b]\nllama-server-rpc(+0x1c9606c)[0x5a9a3448f06c]\n" +
	"/lib/x86_64-linux-gnu/libc.so.6(+0x2a1ca)[0x73d8f7f9a1ca]\n" +
	"/lib/x86_64-linux-gnu/libc.so.6(__libc_start_main+0x8b)[0x73d8f7f9a28b]\nllama-server-rpc(+0x238205)[0x5a9a32a31205]"

func TestProbeErrorLeadsWithRealCauseNotCUDANoise(t *testing.T) {
	e := &ProbeError{Kind: ProbeExit, Addr: "192.0.2.250:50052", Detail: "signal: aborted (core dumped)", Stderr: realConnectRefusedStderr}
	got := e.Error()
	want := "pre-flight of 192.0.2.250:50052 failed (exit): ggml-rpc.cpp:653: Failed to connect to 192.0.2.250:50052 (signal: aborted (core dumped))"
	if !strings.HasPrefix(got, "pre-flight of 192.0.2.250:50052 failed (exit): ") || !strings.Contains(got, "Failed to connect to 192.0.2.250:50052") {
		t.Fatalf("reason = %q, want it to lead with the connect failure (e.g. %q)", got, want)
	}
	for _, noise := range []string{"ggml_cuda_init", "CUDA", "libc.so", "llama-server-rpc(+0x", "\n"} {
		if strings.Contains(got, noise) {
			t.Errorf("reason %q still contains %q", got, noise)
		}
	}
	if e.Stderr != realConnectRefusedStderr {
		t.Error("raw stderr must stay on the error for debug logging")
	}
}

func TestProbeErrorOtherKindsKeepTheirCauseFirst(t *testing.T) {
	cuda := "0.00.020.000 E ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected\n"
	tests := []struct {
		name string
		e    *ProbeError
		want string
	}{
		{"timeout", &ProbeError{Kind: ProbeTimeout, Addr: "a:1", Detail: "no answer within 10s", Stderr: cuda},
			"pre-flight of a:1 failed (timeout): no answer within 10s"},
		{"no rpc line", &ProbeError{Kind: ProbeNoRPCLine, Addr: "a:1", Detail: "no RPC device line in output", Stderr: cuda},
			"pre-flight of a:1 failed (no-rpc-line): no RPC device line in output"},
		{"exit with only noise", &ProbeError{Kind: ProbeExit, Addr: "a:1", Detail: "exit status 1", Stderr: cuda},
			"pre-flight of a:1 failed (exit): exit status 1"},
		{"timeout with a real message", &ProbeError{Kind: ProbeTimeout, Addr: "a:1", Detail: "no answer within 10s", Stderr: cuda + "rpc: waiting for server\n"},
			"pre-flight of a:1 failed (timeout): no answer within 10s; stderr: rpc: waiting for server"},
	}
	for _, tt := range tests {
		if got := tt.e.Error(); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}
