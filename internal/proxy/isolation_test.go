package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// A prefetch that fails ends that request's upload with a 500 and wakes its
// reader; nothing else stops.
func TestPrefetchPanicFailsTheUploadOnly(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { t.Errorf("escalated: %s", r) }})()
	sp := &spill{memMax: 1 << 20, max: 1 << 20}
	b := sp.replay(strings.NewReader("an upload body"), io.NopCloser(nil))
	h, _ := supervise.PanicAtForTest(supervise.PointName("proxy.prefetch", b), 1)
	defer supervise.SetHookForTest(h)()
	errs := make(chan error, 1)
	b.startPrefetch(func(err error) { errs <- err }, func() {})
	var e *Error
	select {
	case err := <-errs:
		if !errors.As(err, &e) || e.Status != http.StatusInternalServerError {
			t.Fatalf("onErr got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failure was not reported")
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(b)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.As(err, &e) || e.Status != http.StatusInternalServerError {
			t.Fatalf("the reader got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reader still waits")
	}
}
