package cfgsvc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Applying a committed change that fails is not continued: the process ends
// through the fatal path (once), and the caller gets ErrApplyFailed to
// answer with first. The change stays committed; later changes still run
// their callbacks in order.
func TestApplyFailureTakesTheFatalPath(t *testing.T) {
	supervise.ResetForTest()
	defer supervise.ResetForTest()
	var fatals []string
	defer supervise.Configure(supervise.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fatal: func(r string) { fatals = append(fatals, r) }})()
	e := newEnv(t)
	applied := 0
	e.svc.o.Apply = func(c *config.Config) { applied++ }
	h, _ := supervise.PanicAtForTest("config.apply", 1)
	defer supervise.SetHookForTest(h)()
	_, err := e.svc.CreateProfile(context.Background(), ui("x"), "a", llama("a.gguf", 1024))
	if !errors.Is(err, ErrApplyFailed) {
		t.Fatalf("err %v, want ErrApplyFailed", err)
	}
	if len(fatals) != 1 || e.svc.Revision() != 1 || applied != 0 {
		t.Fatalf("fatals %v, revision %d, applied %d", fatals, e.svc.Revision(), applied)
	}
	if _, err := e.svc.CreateProfile(context.Background(), ui("x"), "b", llama("b.gguf", 1024)); err != nil || applied != 1 {
		t.Fatalf("the next change: %v, applied %d", err, applied)
	}
}
