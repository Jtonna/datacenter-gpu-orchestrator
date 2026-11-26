package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// The agent's child is started without any LLAMA_ARG_* variable, as the
// scheduler's own children are (instance scrubEnv): from the agent's
// environment (any case) or from the launch's own Env. They would silently
// override the command line (--ctx-checkpoints, --cache-ram). Others pass.
func TestChildRunnerScrubsLlamaArgs(t *testing.T) {
	t.Setenv("LLAMA_ARG_CTX_CHECKPOINTS", "7")
	t.Setenv("llama_arg_cache_ram", "9")
	dir := t.TempDir()
	prog := agenttest.Install(t, dir, "rpc-server")
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	logPath := filepath.Join(dir, "out.log")
	p, err := NewChild().Start(context.Background(), Spec{Unit: "potluck-launch-s", Path: prog,
		Args: []string{"--host", "127.0.0.1", "--port", fmt.Sprint(port)}, LogPath: logPath,
		Env: []string{"FAKEPROG_PRINT_ENV=LLAMA_ARG_CTX_CHECKPOINTS,llama_arg_cache_ram,LLAMA_ARG_N_PARALLEL,LLAMA_RPC_FILE_LOAD",
			"LLAMA_ARG_N_PARALLEL=4", "LLAMA_RPC_FILE_LOAD=required"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.Signal(osx.SigKill); <-p.Done() }()
	agenttest.Eventually(t, 10*time.Second, "start line in log", func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), "Starting RPC server")
	})
	b, _ := os.ReadFile(logPath)
	for _, want := range []string{"env LLAMA_ARG_CTX_CHECKPOINTS=\n", "env llama_arg_cache_ram=\n", "env LLAMA_ARG_N_PARALLEL=\n", "env LLAMA_RPC_FILE_LOAD=required\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log lacks %q:\n%s", want, b)
		}
	}
}

// The systemd runner passes no LLAMA_ARG_* of the launch's Env (systemd-run
// does not hand the agent's own environment to the unit).
func TestSystemdStartArgsScrubLlamaArgs(t *testing.T) {
	got := strings.Join((&Systemd{}).StartArgs(Spec{Unit: "u", Path: "/x/llama-server",
		Env: []string{"LLAMA_ARG_CTX_CHECKPOINTS=7", "Llama_Arg_Cache_Ram=1", "CUDA_DEVICE_ORDER=PCI_BUS_ID"}}), " ")
	if strings.Contains(strings.ToUpper(got), "LLAMA_ARG_") || !strings.Contains(got, "--setenv=CUDA_DEVICE_ORDER=PCI_BUS_ID") {
		t.Fatal(got)
	}
}
