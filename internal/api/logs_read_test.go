package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/selflog"
)

// The read side on the real API: the store's pages and the query pass
// through; a write is refused, also when it hides behind "..", which the
// scheduler's router turns into the path it names.
func TestLogPagesThroughTheAPI(t *testing.T) {
	e, _, store := logsEnv(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/logs/select/vmui/", 200},
		{"POST", "/logs/select/logsql/query", 200},
		{"GET", "/api/logs/query?query=error&limit=5", 200},
		{"POST", "/logs/insert/jsonline", 403},
		{"GET", "/logs/delete/run_task", 403},
		{"GET", "/logs/internal/force_merge", 403},
		{"GET", "/logs/select/../internal/force_merge", 403},
	} {
		if st, b, _ := e.ldo(c.method, c.path, "query=*", "Content-Type", "application/x-www-form-urlencoded"); st != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, st, b, c.want)
		}
	}
	var paths []string
	for _, s := range store.stored() {
		paths = append(paths, s.path)
	}
	if strings.Join(paths, " ") != "/select/vmui/ /select/logsql/query /select/logsql/query" {
		t.Errorf("the store saw %q", paths)
	}
}

// Potluck's own lines go through a ring to a store that hangs; the ring is
// full; Potluck logs as fast as it can. A chat request still takes its
// normal time: logging never waits for the store.
func TestOwnLoggingDoesNotSlowRequests(t *testing.T) {
	var hung atomic.Int32
	release := make(chan struct{})
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer store.Close()
	defer close(release)
	relay, err := logrelay.New(logrelay.Options{StoreURL: store.URL, Config: func() config.Logs { return config.DefaultLogs() }})
	if err != nil {
		t.Fatal(err)
	}
	self := selflog.New(selflog.Options{On: relay.StoreOn, InsertURL: relay.InsertURL, Release: "0.1.3",
		BufferBytes: 64 << 10, HTTP: &http.Client{Timeout: 30 * time.Second}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { self.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	log := slog.New(self.Handler(slog.NewTextHandler(io.Discard, nil)))

	e := newEnv(t, chatOK, func(o *Options) { o.Logger = log })
	timeIt := func() time.Duration {
		t0 := time.Now()
		resp := e.post("/v1/chat/completions", chat("alpha"))
		if b := body(t, resp); resp.StatusCode != 200 {
			t.Errorf("chat: %d %s", resp.StatusCode, b)
		}
		return time.Since(t0)
	}
	timeIt() // loads the model once
	normal := timeIt()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var logged atomic.Int64
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				log.Info("flood line from Potluck's own log", "n", logged.Add(1), "pad", strings.Repeat("p", 200))
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for self.Ring().DroppedTotal() == 0 || hung.Load() == 0 {
		if time.Now().After(deadline) {
			close(stop)
			wg.Wait()
			t.Fatalf("the ring never filled (dropped %d) or the store got nothing (%d)", self.Ring().DroppedTotal(), hung.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	var worst time.Duration
	for range 20 {
		worst = max(worst, timeIt())
	}
	close(stop)
	wg.Wait()
	t.Logf("chat: %v before, worst %v of 20 while %d lines were logged into a full ring (%d dropped) in front of a hung store",
		normal, worst, logged.Load(), self.Ring().DroppedTotal())
	if worst > 2*time.Second {
		t.Errorf("a chat request took %v with the store hung and the ring full", worst)
	}
	if self.Ring().Bytes() > 64<<10 {
		t.Errorf("the ring holds %d bytes, more than its bound", self.Ring().Bytes())
	}
}
