// Package stats scrapes the local Prometheus exporters (node_exporter and
// nvidia_gpu_exporter) and turns their output into the normalised stats
// object of docs/stats-contract.md (agentproto.Stats).
//
// All ages, durations and rates use the monotonic clock given in Config.Now;
// the node's wall clock is never read for them. Real timers are used only to
// sleep between scrapes.
package stats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// SourceConfig names one exporter to scrape.
type SourceConfig struct {
	Name string // agentproto.SourceNode or agentproto.SourceNvidia
	URL  string // e.g. http://127.0.0.1:50070/metrics
}

// Config configures a Collector.
type Config struct {
	Sources []SourceConfig
	// Now is the agent's monotonic clock (CLOCK_BOOTTIME on the node).
	// Required.
	Now func() time.Duration
	// Primary returns the name of the interface that carries the agent's
	// route to Potluck; it may return "". Optional.
	Primary    func() string
	HTTPClient *http.Client // optional
	// Defaults 2 s, 10 s and 4 s (contract section 3).
	MinInterval, MaxInterval, Timeout time.Duration
	// StartGrace is how long after a source was added, or its exporter was
	// started (Restarted), a failed scrape is not warned about; default 5 s.
	StartGrace time.Duration
	Logger     *slog.Logger // optional
	// OnScrape is called after each successful scrape, outside the
	// collector's lock, with the source's name. Optional.
	OnScrape func(source string)
}

// Errors returned by ScrapeOnce besides scrape failures.
var (
	ErrScrapeInProgress = errors.New("stats: a scrape of this source is already running")
	ErrUnknownSource    = errors.New("stats: source not configured")
	errSourceReplaced   = errors.New("stats: source was replaced while scraping")
)

type sourceState struct {
	cfg    SourceConfig
	busy   bool
	cancel context.CancelFunc // loop cancel while Run is active

	interval time.Duration // current scrape interval

	everOK    bool
	latest    Metrics
	goodStart time.Duration // start of the scrape that produced latest
	scrapeDur time.Duration // duration of the most recent successful scrape
	failing   bool          // the last scrape failed
	warned    bool          // the failure was logged (log transitions only)
	since     time.Duration // when the source was added or its exporter started

	counters counterState // node only
}

// Collector runs one scrape loop per configured exporter and builds
// snapshots on demand.
type Collector struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger

	mu     sync.Mutex
	srcs   map[string]*sourceState
	runCtx context.Context // non-nil while Run is active
	wg     sync.WaitGroup
}

// New returns a Collector. cfg.Now is required.
func New(cfg Config) *Collector {
	if cfg.Now == nil {
		panic("stats: Config.Now is required")
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = 2 * time.Second
	}
	if cfg.MaxInterval <= 0 {
		cfg.MaxInterval = 10 * time.Second
	}
	if cfg.MaxInterval < cfg.MinInterval {
		cfg.MaxInterval = cfg.MinInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 4 * time.Second
	}
	if cfg.StartGrace <= 0 {
		cfg.StartGrace = 5 * time.Second
	}
	c := &Collector{cfg: cfg, client: cfg.HTTPClient, log: cfg.Logger, srcs: map[string]*sourceState{}}
	if c.client == nil {
		c.client = &http.Client{}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	c.SetSources(cfg.Sources)
	return c
}

// nextInterval is the self-adjusting interval of contract section 3:
// max(min, 3 x last scrape duration), capped at max.
func nextInterval(d, min, max time.Duration) time.Duration {
	n := 3 * d
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

// SetSources replaces the configured set. A source whose name and URL are
// unchanged keeps its state; a removed or changed source is dropped (its
// loop stopped, its data forgotten). Names other than node and nvidia, and
// repeated names, are ignored. Safe before or during Run.
func (c *Collector) SetSources(srcs []SourceConfig) {
	want := map[string]SourceConfig{}
	for _, s := range srcs {
		if s.Name != agentproto.SourceNode && s.Name != agentproto.SourceNvidia {
			c.log.Warn("stats: ignoring unknown source", "name", s.Name)
			continue
		}
		if _, dup := want[s.Name]; dup {
			continue
		}
		want[s.Name] = s
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, st := range c.srcs {
		if w, ok := want[name]; !ok || w.URL != st.cfg.URL {
			if st.cancel != nil {
				st.cancel()
				st.cancel = nil
			}
			delete(c.srcs, name)
		}
	}
	for name, w := range want {
		if _, ok := c.srcs[name]; ok {
			continue
		}
		st := &sourceState{cfg: w, interval: c.cfg.MinInterval, since: c.cfg.Now()}
		c.srcs[name] = st
		if c.runCtx != nil {
			c.startLoopLocked(st)
		}
	}
}

// Run runs one loop per configured source until ctx is done and returns
// after all loops have stopped. Sources added by SetSources while running get
// a loop too. Calling Run while another Run is active returns at once.
func (c *Collector) Run(ctx context.Context) {
	c.mu.Lock()
	if c.runCtx != nil {
		c.mu.Unlock()
		c.log.Warn("stats: Run called while already running")
		return
	}
	c.runCtx = ctx
	for _, st := range c.srcs {
		c.startLoopLocked(st)
	}
	c.mu.Unlock()

	<-ctx.Done()

	c.mu.Lock()
	c.runCtx = nil
	for _, st := range c.srcs {
		if st.cancel != nil {
			st.cancel()
			st.cancel = nil
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *Collector) startLoopLocked(st *sourceState) {
	ctx, cancel := context.WithCancel(c.runCtx)
	st.cancel = cancel
	c.wg.Add(1)
	go c.loop(ctx, st)
}

// loop scrapes one source on its self-adjusting schedule. Ticks are measured
// from the start of each scrape; a scrape that runs past its tick makes the
// loop skip the missed ticks, so two scrapes of one exporter never overlap.
func (c *Collector) loop(ctx context.Context, st *sourceState) {
	defer c.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		start := c.cfg.Now()
		err := c.scrape(ctx, st)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, ErrScrapeInProgress) {
			c.log.Debug("stats: scrape failed", "source", st.cfg.Name, "err", err)
		}
		c.mu.Lock()
		interval := st.interval
		c.mu.Unlock()
		elapsed := c.cfg.Now() - start
		wait := interval - elapsed
		if wait <= 0 {
			wait = interval - elapsed%interval // skip the overlapped tick(s)
		}
		timer.Reset(wait)
	}
}

// ScrapeOnce scrapes the named source now. It returns ErrScrapeInProgress if
// a scrape of that source is already running (the tick is skipped, nothing
// changes), ErrUnknownSource if the source is not configured, or the scrape
// error.
func (c *Collector) ScrapeOnce(ctx context.Context, name string) error {
	c.mu.Lock()
	st := c.srcs[name]
	c.mu.Unlock()
	if st == nil {
		return ErrUnknownSource
	}
	return c.scrape(ctx, st)
}

func (c *Collector) scrape(ctx context.Context, st *sourceState) error {
	c.mu.Lock()
	if st.busy {
		c.mu.Unlock()
		return ErrScrapeInProgress
	}
	st.busy = true
	cfg := st.cfg
	c.mu.Unlock()

	start := c.cfg.Now()
	m, err := c.fetch(ctx, cfg)
	dur := c.cfg.Now() - start
	if dur < 0 {
		dur = 0
	}

	scraped := false
	defer func() { // after the unlock below: defers run last first
		if scraped && c.cfg.OnScrape != nil {
			c.cfg.OnScrape(cfg.Name)
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	st.busy = false
	if c.srcs[cfg.Name] != st {
		return errSourceReplaced
	}
	if err != nil && ctx.Err() != nil {
		// Shutting down: not a measurement of the exporter.
		return err
	}
	// A failed scrape counts with its elapsed time (section 3).
	st.interval = nextInterval(dur, c.cfg.MinInterval, c.cfg.MaxInterval)
	if err != nil {
		// An exporter that was just started is not listening yet.
		if !st.warned && c.cfg.Now()-st.since >= c.cfg.StartGrace {
			st.warned = true
			c.log.Warn("stats: exporter scrape failing; keeping last good values", "source", cfg.Name, "err", err)
		}
		st.failing = true
		return err
	}
	if st.warned {
		c.log.Info("stats: exporter scrape recovered", "source", cfg.Name)
	}
	st.failing, st.warned = false, false
	st.everOK = true
	st.latest = m
	st.goodStart = start
	st.scrapeDur = dur
	if cfg.Name == agentproto.SourceNode {
		st.counters.update(m, start)
	}
	scraped = true
	return nil
}

func (c *Collector) fetch(ctx context.Context, cfg SourceConfig) (Metrics, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout) // connect plus body
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stats: %s: HTTP %d", cfg.Name, resp.StatusCode)
	}
	keep := keepNode
	if cfg.Name == agentproto.SourceNvidia {
		keep = keepNvidia
	}
	m, bad, err := Parse(resp.Body, keep)
	if err != nil {
		return nil, fmt.Errorf("stats: %s: reading body: %w", cfg.Name, err)
	}
	if bad > 0 {
		c.log.Debug("stats: skipped malformed exporter lines", "source", cfg.Name, "lines", bad)
	}
	return m, nil
}

// Restarted tells the collector that the source's exporter was just started:
// a failed scrape is not warned about during the next StartGrace.
func (c *Collector) Restarted(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.srcs[name]; st != nil {
		st.since = c.cfg.Now()
	}
}

// Failing reports whether the last scrape of a source failed (false for a
// source that is not configured or was not scraped yet).
func (c *Collector) Failing(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.srcs[name]
	return st != nil && st.failing
}

// Interval returns the current scrape interval of a source (0 if not
// configured).
func (c *Collector) Interval(name string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.srcs[name]; st != nil {
		return st.interval
	}
	return 0
}

func roundMS(d time.Duration) int64 {
	if d < 0 {
		d = 0
	}
	return int64(math.Round(float64(d) / float64(time.Millisecond)))
}

// Snapshot builds the stats object now. Safe for concurrent use.
func (c *Collector) Snapshot() agentproto.Stats {
	primary := ""
	if c.cfg.Primary != nil {
		primary = c.cfg.Primary()
	}
	in := Input{Sources: map[string]agentproto.Source{}, Primary: primary}

	c.mu.Lock()
	now := c.cfg.Now()
	for name, st := range c.srcs {
		if !st.everOK {
			in.Sources[name] = agentproto.Source{Status: agentproto.SourceAbsent}
			continue
		}
		age := roundMS(now - st.goodStart)
		scr := roundMS(st.scrapeDur)
		limit := 3 * roundMS(st.interval)
		if limit < 10000 {
			limit = 10000
		}
		status := agentproto.SourceOK
		if age > limit {
			status = agentproto.SourceStale
		}
		in.Sources[name] = agentproto.Source{Status: status, AgeMS: &age, ScrapeMS: &scr}
		switch name {
		case agentproto.SourceNode:
			in.Node = st.latest
			in.Rates = st.counters.rates
		case agentproto.SourceNvidia:
			in.Nvidia = st.latest
		}
	}
	c.mu.Unlock()
	return Normalize(in)
}
