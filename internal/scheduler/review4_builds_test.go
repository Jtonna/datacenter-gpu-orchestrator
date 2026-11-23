package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
)

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// FS2 (fourth review): when the host build or the served release's build
// cannot be read at start (a slow share right after boot, release.json
// briefly unreadable), the two reads are retried in the background with a
// bounded backoff until both are known; trust is then decided, old rows
// stamped, and one line logged. GET /api/needs shows both builds and, per
// row, why it is not trusted.
func TestReview4_FS2_BuildsAreReadAgainUntilKnown(t *testing.T) {
	// the reads at start (buildstart_test.go) are left out: this is the
	// background path, for builds that stay unreadable longer
	old := buildStartWait
	buildStartWait = 0
	t.Cleanup(func() { buildStartWait = old })
	st := openNeedStore(t)
	measureF(t, st) // 11000 on build aaaaaaa1, release 0.1.3
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
		HostBuild: func(context.Context) (string, error) { // the first two reads time out
			if hostCalls.Add(1) <= 2 {
				return "", errors.New("--version: timed out")
			}
			return "aaaaaaa1", nil
		},
		ReleaseBuild: func(rel string) string { // release.json unreadable for the first three reads
			if manifestCalls.Add(1) <= 3 {
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
	ctx := ctxT(t)
	doc, err := s.Needs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.HostBuild != "" || len(doc.Rows) != 1 || doc.Rows[0].Trusted || doc.Rows[0].UntrustedWhy == "" {
		t.Fatalf("at start: %+v", doc)
	}
	if !strings.Contains(logs.String(), "stored memory needs only raise the estimates") {
		t.Fatalf("no start warning: %s", logs.String())
	}
	// while untrusted the stored 11000 only raises the estimate: the plan
	// meets the estimate (14700 with its margin), and this forced profile
	// is refused; the reads at start keep this out of the first moment
	if r, _ := s.Plan(ctx, "f"); r.FitNeedMiB != 14700 || r.Action != PlanReject {
		t.Fatalf("plan while untrusted: %+v", r)
	}
	// the background reads, with a growing wait: no busy loop
	deadline := time.Now().Add(5 * time.Second)
	for {
		clk.Advance(time.Minute)
		doc, err = s.Needs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Rows) == 1 && doc.Rows[0].Trusted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("trust never decided: %+v; host reads %d, manifest reads %d", doc, hostCalls.Load(), manifestCalls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if doc.HostBuild != "aaaaaaa1" || doc.ServedBuild != "aaaaaaa1" || doc.Rows[0].UntrustedWhy != "" {
		t.Fatalf("after: %+v", doc)
	}
	if n := hostCalls.Load(); n > 10 {
		t.Fatalf("%d host build reads: a busy loop", n)
	}
	if !strings.Contains(logs.String(), "llama builds known: stored memory needs of this build are trusted") {
		t.Fatalf("no line when trust was decided: %s", logs.String())
	}
	if r, _ := s.Plan(ctx, "f"); r.FitNeedMiB != 11000 {
		t.Fatalf("the restored 11000 is not used after trust was decided: %+v", r)
	}
}

// FS2: the reason a row is not trusted, per row.
func TestReview4_FS2_WhyARowIsNotTrusted(t *testing.T) {
	cases := []struct {
		host, served, row string
		want              string
	}{
		{"", "aaaaaaa1", "aaaaaaa1", "host program's llama build is not known"},
		{"aaaaaaa1", "", "aaaaaaa1", "served release's llama build is not known"},
		{"aaaaaaa1", "bbbbbbb2", "aaaaaaa1", "differ"},
		{"aaaaaaa1", "aaaaaaa1", "ccccccc3", "measured on llama build ccccccc3"},
		{"aaaaaaa1", "aaaaaaa1", "", "measured before llama builds were recorded"},
	}
	for _, tc := range cases {
		h := oneAndThree(t)
		h.c.llamaBuild, h.c.servedBuild = tc.host, tc.served
		p := h.c.cfg.Profiles["one"]
		h.c.putLearned(&learnedNeed{TotalMiB: 7000, key: needKeyOf(p), profile: "one", devices: 1, restored: true, at: h.clk.Now(), build: tc.row, release: "0.1.2"})
		rows := h.c.learnedRows()
		if len(rows) != 1 || rows[0].Trusted || !strings.Contains(rows[0].UntrustedWhy, tc.want) {
			t.Errorf("%+v: %+v, want a reason with %q", tc, rows, tc.want)
		}
	}
}
