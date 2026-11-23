package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

// startWithBuilds starts a scheduler with one stored need (11000 on build
// aaaaaaa1, release 0.1.3) whose builds are unreadable for the first
// failHost and failServed reads.
func startWithBuilds(t *testing.T, failHost, failServed int32) (*Scheduler, *lockedLog) {
	t.Helper()
	st := openNeedStore(t)
	measureF(t, st)
	var hostCalls, manifestCalls atomic.Int32
	clk := newFakeClock()
	view := newFakeView(clk)
	view.set(psNode("a", 2, 12000))
	cfg := testConfig()
	cfg.Defaults.ForcedFitCheck = config.ForcedFitReject
	cfg.Profiles["f"] = llamaP("f", 0, func(p *core.Profile) { p.ForceNodes = []string{"a"} })
	logs := &lockedLog{}
	s, err := New(cfg, Options{Clock: clk, View: view, Prober: newFakeProber(view), Launcher: newFakeLauncher(),
		WorkerProber: newFakeWorkerProber(), NeedStore: st, Release: "0.1.3",
		HostBuild: func(context.Context) (string, error) {
			if hostCalls.Add(1) <= failHost {
				return "", errors.New("--version: timed out")
			}
			return "aaaaaaa1", nil
		},
		ReleaseBuild: func(rel string) string {
			if manifestCalls.Add(1) <= failServed {
				return ""
			}
			return map[string]string{"0.1.3": "aaaaaaa1"}[rel]
		},
		Needs: NeedFunc(func(p *core.Profile) (NeedInfo, error) {
			return NeedInfo{Need: placer.Estimated(14000), FileBytes: 1 << 30}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.MultiWriter(logs), nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
		<-done
	})
	return s, logs
}

// Item 2 (0.1.9 readiness): release.json unreadable for a moment right
// after start made every stored need untrusted until the background read
// 5 s later; a plan in that moment used the estimate (14000, raised) instead
// of the measurement (11000), and a forced profile that fits by the
// measurement was refused. The first reads are now repeated before the
// scheduler takes its first event, for up to buildStartWait: the first
// listing and the first plan already see the needs trusted.
func TestBuildsReadableAMomentAfterStartAreReadBeforeTheFirstPlan(t *testing.T) {
	s, logs := startWithBuilds(t, 1, 3)
	ctx := ctxT(t)
	doc, err := s.Needs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.HostBuild != "aaaaaaa1" || doc.ServedBuild != "aaaaaaa1" || len(doc.Rows) != 1 || !doc.Rows[0].Trusted {
		t.Fatalf("first listing: %+v\n%s", doc, logs.String())
	}
	if r, err := s.Plan(ctx, "f"); err != nil || r.FitNeedMiB != 11000 || r.Action != PlanStart {
		t.Fatalf("first plan: %+v %v", r, err)
	}
}
