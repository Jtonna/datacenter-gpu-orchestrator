// Package selflog ships Potluck's own lines and the output of the programs
// it starts to the log store (CR-10, design section 4.5): source scheduler
// for Potluck's log, source host for a child's output, with profile and
// instance_id.
//
// It is the agent's shipper (internal/agent/logship) with its producers
// pointed at the store's intake: one RAM ring (8 MiB by default, oldest lines
// dropped first, the next batch of that source saying how many), one
// background sender with its own HTTP client. Potluck is the trusted side, so
// the sender posts straight to the store with the trusted fields set for the
// whole request, the same fields and the same store arguments as the
// pass-through (logrelay.Relay.InsertURL).
//
// Never blocking: a producer (the slog handler, a child's output tap) puts a
// line into the ring and returns. The ring's lock is held for memory work
// only; only the sender's goroutine waits on the store. The printed log and
// the child log files are written first and exactly as before.
package selflog

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/logship"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Defaults.
const (
	DefaultBufferBytes    = logship.DefaultBufferBytes // the ring, 8 MiB
	DefaultChildBytesPerS = 256 << 10                  // one child's output
	// sendBytesPerS bounds the sender to the store. The store is on the same
	// host; the figure only keeps a backlog from arriving as one burst.
	sendBytesPerS = 4 << 20
	// checkEvery is how often the on/off state of logging is looked at.
	checkEvery = time.Second
)

// Options configure a Shipper.
type Options struct {
	// On reports whether Potluck's lines go to the store now (a store
	// address and logs_enabled); InsertURL builds the store's intake address
	// for a set of trusted fields. Both must not block (logrelay.Relay's
	// StoreOn and InsertURL).
	On        func() bool
	InsertURL func([]logrelay.Field) string
	// Node is the local node's id and name, read when a batch is sent.
	Node    func() (id, name string)
	Release string
	// Platform defaults to <GOOS>-<GOARCH>.
	Platform       string
	BufferBytes    int // the ring; DefaultBufferBytes when 0
	ChildBytesPerS int // one child's output; DefaultChildBytesPerS when 0
	HTTP           *http.Client
	// CheckEvery replaces checkEvery (tests).
	CheckEvery time.Duration
	// FirstPause and MaxPause: the sender's pause after a failed post.
	FirstPause, MaxPause time.Duration
}

// Shipper is Potluck's own log shipper.
type Shipper struct {
	o       Options
	ship    *logship.Shipper
	start   time.Time
	mu      sync.Mutex
	on      bool
	checked bool
}

// SchedulerKey is the ring key of Potluck's own lines.
var SchedulerKey = logship.Key{Source: agentproto.LogSourceScheduler}

// New builds the shipper. Until Run looks at On for the first time it keeps
// the lines it gets (the start of Potluck's log); if logging is off then,
// they are dropped.
func New(o Options) *Shipper {
	if o.Platform == "" {
		o.Platform = runtime.GOOS + "-" + runtime.GOARCH
	}
	if o.ChildBytesPerS <= 0 {
		o.ChildBytesPerS = DefaultChildBytesPerS
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if o.CheckEvery <= 0 {
		o.CheckEvery = checkEvery
	}
	if o.On == nil {
		o.On = func() bool { return false }
	}
	if o.Node == nil {
		o.Node = func() (string, string) { return "local", "local" }
	}
	s := &Shipper{o: o, start: time.Now()}
	s.ship = logship.New(logship.Config{
		Now:         func() time.Duration { return time.Since(s.start) }, // Potluck's monotonic clock
		HTTP:        o.HTTP,
		BufferBytes: o.BufferBytes,
		FirstPause:  o.FirstPause,
		MaxPause:    o.MaxPause,
		Target:      s.target,
		Who:         "potluck",
	})
	// Potluck's own clock: the wall clock at start, then the monotonic time
	// since. A line's _time is when Potluck produced it.
	s.ship.Anchor(s.start.UnixNano(), 0)
	return s
}

// object is the limits the sender keeps to. url is unused (Target decides).
func object() *agentproto.Logs {
	return &agentproto.Logs{Revision: 1, URL: "store", Format: agentproto.LogsFormatJSONLine,
		Limits: agentproto.LogsLimits{MaxBatchBytes: agentproto.LogsMaxBatchBytes, MaxLineBytes: agentproto.LogsMaxLineBytes,
			MaxBytesPerS: sendBytesPerS, EveryS: 1}, DenyUnits: []string{}}
}

// check follows the on/off state of logging: off drops what is kept.
func (s *Shipper) check() {
	on := s.o.On()
	var changed bool
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		changed = !s.checked || on != s.on
		s.on, s.checked = on, true
	}()
	if !changed {
		return
	}
	if on {
		s.ship.Use(object())
	} else {
		s.ship.Use(nil)
	}
}

// target is the store intake address of a batch of key k.
func (s *Shipper) target(k logship.Key) string {
	id, name := s.o.Node()
	f := []logrelay.Field{{Name: logrelay.FieldNodeID, Value: id}, {Name: logrelay.FieldNodeName, Value: name},
		{Name: logrelay.FieldPlatform, Value: s.o.Platform}, {Name: logrelay.FieldRelease, Value: s.o.Release},
		{Name: logrelay.FieldSource, Value: k.Source}}
	if k.Source == agentproto.LogSourceHost {
		f = append(f, logrelay.Field{Name: logrelay.FieldProfile, Value: k.Profile}, logrelay.Field{Name: logrelay.FieldInstanceID, Value: k.Instance})
	}
	return s.o.InsertURL(f)
}

// Run runs the sender and follows the on/off state until ctx ends.
//
// The on/off check restarts alone after a failure; the sender's loops are
// supervised inside logship.
func (s *Shipper) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go supervise.Once("selflog.ship", func() { defer wg.Done(); s.ship.Run(ctx) }, nil)
	supervise.Go(ctx, supervise.Spec{Name: "selflog.check", Policy: supervise.Restart, Done: wg.Done}, func(ctx context.Context) {
		s.check()
		t := time.NewTicker(s.o.CheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.check()
			}
		}
	})
	wg.Wait()
}

// SetLogger sets where the sender's own warnings go (rate-limited; they may
// go through the ring themselves).
func (s *Shipper) SetLogger(l *slog.Logger) { s.ship.SetLogger(l) }

// Handler wraps inner: every record is printed by inner as before and copied
// into the ring as a line of source scheduler.
func (s *Shipper) Handler(inner slog.Handler) slog.Handler {
	return logship.NewHandlerKey(inner, s.ship, SchedulerKey)
}

// Ring is the shipper's ring (tests and figures).
func (s *Shipper) Ring() *logship.Ring { return s.ship.Ring() }

// Figures are the sender's counts (nil while nothing is shipped).
func (s *Shipper) Figures() *agentproto.LogShipping { return s.ship.Figures() }

// ChildTap returns the output tap of one child (instance.ProcessLauncher.Tap).
func (s *Shipper) ChildTap(profile string, instanceID uint64) *ChildTap {
	return &ChildTap{s: s, rate: float64(s.o.ChildBytesPerS),
		key: logship.Key{Source: agentproto.LogSourceHost, Profile: profile, Instance: strconv.FormatUint(instanceID, 10)}}
}

// ChildTap copies one child's output lines into the ring, at most
// ChildBytesPerS (a burst of one second). What is over the rate is skipped,
// never kept: the next line that fits is preceded by one line that says how
// many bytes were skipped. It is called from the goroutines that copy the
// child's stdout and stderr; it never waits for anything but its own short
// mutex and the ring's.
type ChildTap struct {
	s    *Shipper
	key  logship.Key
	rate float64

	mu      sync.Mutex
	tokens  float64
	last    time.Time
	skipped int64
}

// take spends n bytes of the rate budget; ok is false when the line is
// skipped. sk is how many bytes were skipped before it.
func (c *ChildTap) take(n float64, now time.Time) (sk int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.IsZero() {
		c.tokens = c.rate
	} else if now.After(c.last) {
		c.tokens = min(c.rate, c.tokens+now.Sub(c.last).Seconds()*c.rate)
	}
	c.last = now
	if c.tokens < n {
		c.skipped += int64(n)
		return 0, false
	}
	c.tokens -= n
	sk = c.skipped
	c.skipped = 0
	return sk, true
}

// Line takes one line of the child's output (stream is stdout or stderr).
func (c *ChildTap) Line(stream, text string) {
	if !c.s.ship.On() {
		return
	}
	n := float64(len(text) + 1)
	now := time.Now()
	sk, ok := c.take(n, now)
	if !ok {
		return
	}
	if sk > 0 {
		c.notice(sk)
	}
	c.s.ship.Add(c.key, logship.Line{Mono: c.s.ship.Now(), Msg: text, Stream: stream})
}

// Close says how many bytes were skipped at the end, if any.
func (c *ChildTap) Close() {
	var sk int64
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		sk = c.skipped
		c.skipped = 0
	}()
	if sk > 0 && c.s.ship.On() {
		c.notice(sk)
	}
}

func (c *ChildTap) notice(sk int64) {
	c.s.ship.Add(c.key, logship.Line{Mono: c.s.ship.Now(), Level: "warn",
		Msg: fmt.Sprintf("potluck: %d bytes of this program's output were skipped (written faster than %d bytes/s); the child log file, when one is kept, has all of it", sk, int(c.rate))})
}
