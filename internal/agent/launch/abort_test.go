package launch

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The fork's file-load timeout is a GGML_ABORT: its line carries a
// "file:line: " prefix before the token, and the process ends on SIGABRT
// (exit 134), or on SIGSEGV (139) as the contract used to say. The token is
// matched anywhere in the line and wins whatever the signal, on a host only.
func TestFileLoadTimeoutAbortLine(t *testing.T) {
	for _, line := range []string{
		"/src/llama.cpp/ggml/src/ggml-rpc/ggml-rpc.cpp:612: rpc_file_load_timeout: endpoint=192.0.2.4:50052 command=LOAD_FILE timeout_s=600",
		"rpc_file_load_timeout: endpoint=192.0.2.4:50052 command=LOAD_FILE timeout_s=600",
	} {
		for _, sig := range []string{"ABRT", "SEGV"} {
			s := &logScan{}
			var partial []byte
			s.feed(&partial, []byte("load_tensors: loading\n"+line+"\n"))
			never := func() bool { t.Fatal("oom asked although a token decides"); return false }
			if got, _ := decideExit(agentproto.RoleHost, "", "", s, false, true, sig, never); got != agentproto.ExitFileLoadTimeout {
				t.Errorf("%s, %q: %s", sig, line, got)
			}
			if got, _ := decideExit(agentproto.RoleWorker, "", "", s, false, true, sig, func() bool { return false }); got == agentproto.ExitFileLoadTimeout {
				t.Errorf("a worker took the host token: %q", line)
			}
		}
	}
}
