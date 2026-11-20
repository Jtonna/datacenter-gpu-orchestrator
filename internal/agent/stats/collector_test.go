package stats

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

func newTestCollector(clock *fakeClock, srcs ...SourceConfig) *Collector {
	return New(Config{
		Sources:    srcs,
		Now:        clock.Now,
		Primary:    func() string { return "enx6c1ff7c349ea" },
		HTTPClient: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Timeout:    4 * time.Second,
	})
}

func TestNextIntervalRule(t *testing.T) {
	min, max := 2*time.Second, 10*time.Second
	for _, c := range []struct{ d, want time.Duration }{
		{34 * time.Millisecond, 2 * time.Second},
		{950 * time.Millisecond, 2850 * time.Millisecond},
		{4 * time.Second, 10 * time.Second},
		{0, 2 * time.Second},
	} {
		if got := nextInterval(c.d, min, max); got != c.want {
			t.Fatalf("nextInterval(%v) = %v, want %v", c.d, got, c.want)
		}
	}
}

func TestAdaptiveIntervalThroughScrapes(t *testing.T) {
	clock := &fakeClock{}
	ex := newFakeExporter(t, clock, readFile(t, "host_node.prom"))
	c := newTestCollector(clock, SourceConfig{Name: agentproto.SourceNode, URL: ex.URL()})
	c.cfg.Timeout = 100 * time.Millisecond // real timeout, kept short for the test
	ctx := context.Background()

	if got := c.Interval("node"); got != 2*time.Second {
		t.Fatalf("initial interval = %v", got)
	}
	ex.set(func(f *fakeExporter) { f.cost = 34 * time.Millisecond })
	if err := c.ScrapeOnce(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	if got := c.Interval("node"); got != 2*time.Second {
		t.Fatalf("34 ms => %v", got)
	}
	ex.set(func(f *fakeExporter) { f.cost = 950 * time.Millisecond })
	if err := c.ScrapeOnce(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	if got := c.Interval("node"); got != 2850*time.Millisecond {
		t.Fatalf("0.95 s => %v", got)
	}
	// Timeout: the exporter hangs; 4 s pass on the agent's clock.
	ex.set(func(f *fakeExporter) { f.cost = 4 * time.Second; f.hang = true })
	if err := c.ScrapeOnce(ctx, "node"); err == nil {
		t.Fatal("hanging scrape succeeded")
	}
	if got := c.Interval("node"); got != 10*time.Second {
		t.Fatalf("timeout => %v", got)
	}
	// The scrape_ms stays that of the last success.
	if s := c.Snapshot().Sources["node"]; *s.ScrapeMS != 950 {
		t.Fatalf("scrape_ms = %d", *s.ScrapeMS)
	}
	// HTTP error with elapsed time also counts.
	ex.set(func(f *fakeExporter) { f.cost = 1 * time.Second; f.hang = false; f.status = 500 })
	if err := c.ScrapeOnce(ctx, "node"); err == nil {
		t.Fatal("500 succeeded")
	}
	if got := c.Interval("node"); got != 3*time.Second {
		t.Fatalf("failed 1 s => %v", got)
	}
	// First success restores the normal rule.
	ex.set(func(f *fakeExporter) { f.cost = 34 * time.Millisecond; f.status = 200 })
	if err := c.ScrapeOnce(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	if got := c.Interval("node"); got != 2*time.Second {
		t.Fatalf("restored => %v", got)
	}
}

func TestNoOverlap(t *testing.T) {
	clock := &fakeClock{}
	ex := newFakeExporter(t, clock, readFile(t, "host_node.prom"))
	gate := make(chan struct{})
	ex.set(func(f *fakeExporter) { f.gate = gate })
	c := newTestCollector(clock, SourceConfig{Name: "node", URL: ex.URL()})

	errc := make(chan error, 1)
	go func() { errc <- c.ScrapeOnce(context.Background(), "node") }()
	waitFor(t, "first scrape in flight", func() bool { h, _ := ex.stats(); return h == 1 })

	if err := c.ScrapeOnce(context.Background(), "node"); !errors.Is(err, ErrScrapeInProgress) {
		t.Fatalf("second scrape err = %v", err)
	}
	close(gate)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h, maxIn := ex.stats(); h != 1 || maxIn != 1 {
		t.Fatalf("hits=%d maxInflight=%d", h, maxIn)
	}
}

func TestRunLoopNoOverlapAndStops(t *testing.T) {
	clock := &fakeClock{}
	node := newFakeExporter(t, clock, readFile(t, "host_node.prom"))
	nv := newFakeExporter(t, clock, readFile(t, "host_nvidia.prom"))
	c := newTestCollector(clock, SourceConfig{Name: "node", URL: node.URL()}, SourceConfig{Name: "nvidia", URL: nv.URL()})
	c.cfg.MinInterval, c.cfg.MaxInterval = 3*time.Millisecond, 20*time.Millisecond

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor(t, "several scrapes of each source", func() bool {
		a, _ := node.stats()
		b, _ := nv.stats()
		return a >= 5 && b >= 5
	})
	s := c.Snapshot()
	if s.Sources["node"].Status != "ok" || s.Sources["nvidia"].Status != "ok" || len(s.GPUs) != 2 {
		t.Fatalf("snapshot while running: %+v", s.Sources)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, m := node.stats(); m != 1 {
		t.Fatalf("node max in flight = %d", m)
	}
	waitFor(t, "goroutines released", func() bool { return runtime.NumGoroutine() <= before })
}

func TestSetSourcesWhileRunning(t *testing.T) {
	clock := &fakeClock{}
	node := newFakeExporter(t, clock, readFile(t, "host_node.prom"))
	nv := newFakeExporter(t, clock, readFile(t, "host_nvidia.prom"))
	c := newTestCollector(clock, SourceConfig{Name: "node", URL: node.URL()})
	c.cfg.MinInterval, c.cfg.MaxInterval = 3*time.Millisecond, 20*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "node scraped", func() bool { return c.Snapshot().Sources["node"].Status == "ok" })
	if _, ok := c.Snapshot().Sources["nvidia"]; ok {
		t.Fatal("unconfigured nvidia in sources")
	}

	c.SetSources([]SourceConfig{{Name: "node", URL: node.URL()}, {Name: "nvidia", URL: nv.URL()}})
	waitFor(t, "nvidia scraped", func() bool { return c.Snapshot().Sources["nvidia"].Status == "ok" })

	c.SetSources([]SourceConfig{{Name: "nvidia", URL: nv.URL()}})
	s := c.Snapshot()
	if _, ok := s.Sources["node"]; ok || s.CPU != nil || len(s.GPUs) != 1 || s.GPUs[0].Vendor != "nvidia" {
		t.Fatalf("after removing node: %+v %+v", s.Sources, s.GPUs)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestStaleAbsentAndThreshold(t *testing.T) {
	clock := &fakeClock{}
	node := newFakeExporter(t, clock, readFile(t, "host_node.prom"))
	nv := newFakeExporter(t, clock, "")
	nv.set(func(f *fakeExporter) { f.status = 500 })
	c := newTestCollector(clock, SourceConfig{Name: "node", URL: node.URL()}, SourceConfig{Name: "nvidia", URL: nv.URL()})
	ctx := context.Background()

	// Before any scrape both are absent, with only a status.
	s := c.Snapshot()
	for _, n := range []string{"node", "nvidia"} {
		if src := s.Sources[n]; src.Status != "absent" || src.AgeMS != nil || src.ScrapeMS != nil {
			t.Fatalf("%s = %+v", n, src)
		}
	}
	if s.CPU != nil || len(s.GPUs) != 0 {
		t.Fatal("data while absent")
	}

	node.set(func(f *fakeExporter) { f.cost = 34 * time.Millisecond })
	if err := c.ScrapeOnce(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	if err := c.ScrapeOnce(ctx, "nvidia"); err == nil {
		t.Fatal("nvidia 500 succeeded")
	}
	s = c.Snapshot()
	n := s.Sources["node"]
	if n.Status != "ok" || *n.AgeMS != 34 || *n.ScrapeMS != 34 {
		t.Fatalf("node = %s age=%d scrape=%d", n.Status, *n.AgeMS, *n.ScrapeMS)
	}
	if s.Sources["nvidia"].Status != "absent" {
		t.Fatalf("nvidia = %+v", s.Sources["nvidia"])
	}
	for _, g := range s.GPUs {
		if g.Vendor == "nvidia" {
			t.Fatal("nvidia gpu while absent")
		}
	}
	if len(s.GPUs) != 1 {
		t.Fatalf("gpus = %+v", s.GPUs)
	}

	// age exactly 10000 ms is ok; above it is stale (interval 2 s => limit 10 s).
	clock.Advance(10000*time.Millisecond - 34*time.Millisecond)
	if st := c.Snapshot().Sources["node"]; st.Status != "ok" || *st.AgeMS != 10000 {
		t.Fatalf("at limit: %s %d", st.Status, *st.AgeMS)
	}
	clock.Advance(time.Millisecond)
	s = c.Snapshot()
	if st := s.Sources["node"]; st.Status != "stale" || *st.AgeMS != 10001 {
		t.Fatalf("over limit: %s %d", st.Status, *st.AgeMS)
	}
	// Stale data is still sent.
	if s.Memory == nil || s.CPU == nil || len(s.GPUs) != 1 || len(s.Temps) == 0 {
		t.Fatal("stale source dropped its data")
	}

	// After a timeout-length failure the interval is 10 s, so the limit
	// becomes 30 s: an age of 20 s is ok again. The data is kept.
	node.set(func(f *fakeExporter) { f.cost = 4 * time.Second; f.status = 500 })
	if err := c.ScrapeOnce(ctx, "node"); err == nil {
		t.Fatal("500 succeeded")
	}
	clock.Advance(20*time.Second - 10001*time.Millisecond - 4*time.Second)
	s = c.Snapshot()
	if st := s.Sources["node"]; st.Status != "ok" || *st.AgeMS != 20000 || *st.ScrapeMS != 34 {
		t.Fatalf("with 10 s interval: %s age=%d scrape=%d", st.Status, *st.AgeMS, *st.ScrapeMS)
	}
	if s.Memory == nil || s.Memory.TotalBytes != 32099495936 {
		t.Fatal("last good values lost after failure")
	}
	clock.Advance(10*time.Second + time.Millisecond)
	if st := c.Snapshot().Sources["node"]; st.Status != "stale" {
		t.Fatalf("30001 ms: %s", st.Status)
	}

	raw, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	top := assertNoNull(t, raw)
	nvsrc := top["sources"].(map[string]any)["nvidia"].(map[string]any)
	if len(nvsrc) != 1 {
		t.Fatalf("absent nvidia = %v", nvsrc)
	}
}

func TestRatesThroughCollector(t *testing.T) {
	clock := &fakeClock{}
	ex := newFakeExporter(t, clock, cpuNetText(100, 1e6))
	c := newTestCollector(clock, SourceConfig{Name: "node", URL: ex.URL()})
	c.cfg.Primary = func() string { return "eth0" }
	ctx := context.Background()
	if err := c.ScrapeOnce(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	s := c.Snapshot()
	wantNil(t, "first rx", s.Net[0].RxMbps)
	clock.Advance(2 * time.Second)
	ex.set(func(f *fakeExporter) { f.body = cpuNetText(150, 1e6+250000) })
	if err := c.ScrapeOnce(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	s = c.Snapshot()
	wantF(t, "rx", s.Net[0].RxMbps, 1)
	wantF(t, "cpu", s.CPU.UtilPercent, 50)

	// A failed scrape keeps the last rates.
	ex.set(func(f *fakeExporter) { f.status = 500 })
	_ = c.ScrapeOnce(ctx, "node")
	s = c.Snapshot()
	wantF(t, "rx kept", s.Net[0].RxMbps, 1)
}

func cpuNetText(v, rx float64) string {
	b, _ := json.Marshal(v) // plain number formatting
	r, _ := json.Marshal(rx)
	return "node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} " + string(b) + "\n" +
		"node_cpu_seconds_total{cpu=\"0\",mode=\"user\"} " + string(b) + "\n" +
		"node_network_up{device=\"eth0\"} 1\n" +
		"node_network_receive_bytes_total{device=\"eth0\"} " + string(r) + "\n"
}

func TestUnknownSourceAndConfig(t *testing.T) {
	clock := &fakeClock{}
	c := newTestCollector(clock, SourceConfig{Name: "node", URL: "http://127.0.0.1:1/metrics"})
	if err := c.ScrapeOnce(context.Background(), "nvidia"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("err = %v", err)
	}
	s := c.Snapshot()
	if len(s.Sources) != 1 || s.StatsVersion != agentproto.StatsVersion {
		t.Fatalf("sources = %+v", s.Sources)
	}
}
