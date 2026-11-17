package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/testutil/harness"
)

func waitStderr(t *testing.T, p *proc, want string) string {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !strings.Contains(p.stderr.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("stderr never contained %q:\n%s", want, p.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p.stderr.String()
}

func TestLoadLogAutoUsesDeviceNamesAndRPCBufferNames(t *testing.T) {
	port := fmt.Sprint(harness.FreePort(t))
	p := startProc(t, map[string]string{"FAKELLAMA_LOAD_LOG": "auto"},
		"--port", port, "-lv", "4", "--rpc", "10.0.0.2:50052,10.0.0.3:50052", "--device", "RPC0,RPC1,CUDA0")
	out := waitStderr(t, p, "listening on")
	for _, want := range []string{
		"RPC0[10.0.0.2:50052] model buffer size =  1000.00 MiB",
		"RPC0[10.0.0.3:50052] KV buffer size =   200.00 MiB",
		"CUDA0 compute buffer size =   100.00 MiB",
		"CPU_Mapped model buffer size =   360.00 MiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
}

func TestLoadLogScripted(t *testing.T) {
	port := fmt.Sprint(harness.FreePort(t))
	p := startProc(t, map[string]string{"FAKELLAMA_LOAD_LOG": "CUDA0:4000.5:10:20.25"}, "--port", port, "--verbosity=4", "--device", "CUDA0")
	out := waitStderr(t, p, "listening on")
	if !strings.Contains(out, "CUDA0 model buffer size =  4000.50 MiB") || !strings.Contains(out, "CUDA0 compute buffer size =    20.25 MiB") {
		t.Fatalf("stderr:\n%s", out)
	}
}

func TestNoLoadLogByDefault(t *testing.T) {
	port := fmt.Sprint(harness.FreePort(t))
	p := startProc(t, nil, "--port", port)
	out := waitStderr(t, p, "listening on")
	if strings.Contains(out, "buffer size") {
		t.Fatalf("unexpected load log:\n%s", out)
	}
}

// Like llama.cpp at 330f0189, the buffer-size lines are INFO messages shown
// only at verbosity 4 or more (default 3): without -lv 4 nothing is printed.
func TestLoadLogHiddenBelowVerbosity4(t *testing.T) {
	for _, args := range [][]string{nil, {"-lv", "3"}, {"--log-verbosity", "0"}} {
		port := fmt.Sprint(harness.FreePort(t))
		p := startProc(t, map[string]string{"FAKELLAMA_LOAD_LOG": "auto"}, append([]string{"--port", port}, args...)...)
		out := waitStderr(t, p, "listening on")
		if strings.Contains(out, "buffer size") {
			t.Fatalf("args %v: load log printed below verbosity 4:\n%s", args, out)
		}
	}
}
