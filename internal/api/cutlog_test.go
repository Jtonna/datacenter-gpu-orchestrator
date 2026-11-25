package api

import (
	"errors"
	"io"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// The request log's sentence for a request a forced unload cut is the reason
// alone: the transport's word ("EOF") is noise there, the reason is known.
// Other failures keep the transport error, which is all they have.
func TestResultEnd_CutRequestSentenceHasNoTransportTail(t *testing.T) {
	e := resultEnd(proxy.Result{Status: 503, Unloaded: true, Err: io.ErrUnexpectedEOF})
	if e.Reason != "unloaded" || e.Message != "it was unloaded while it ran (a forced unload)" {
		t.Fatalf("cut: %q %q", e.Reason, e.Message)
	}
	e = resultEnd(proxy.Result{Status: 503, Unloaded: true, Err: io.EOF})
	if e.Message != "it was unloaded while it ran (a forced unload)" {
		t.Fatalf("cut with EOF: %q", e.Message)
	}
	e = resultEnd(proxy.Result{Status: 502, Err: errors.New("dial tcp: connection refused")})
	if e.Reason != "upstream_error" || e.Message != "the model server could not be reached: dial tcp: connection refused" {
		t.Fatalf("upstream error: %q %q", e.Reason, e.Message)
	}
}
