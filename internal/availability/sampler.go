package availability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Defaults of the sampler.
const (
	DefaultEvery      = 10 * time.Second
	DefaultFlushEvery = time.Minute
	// DefaultMaxGap is the most time one sample may account for. A longer
	// gap between two samples (a status read that failed, a stopped
	// process) leaves the rest unknown rather than guessed.
	DefaultMaxGap = 3 * DefaultEvery
	// sourceTimeout bounds one read of the source.
	sourceTimeout = 5 * time.Second
)

// Source returns the current sample of every machine. It must read only
// state held in memory.
type Source func(ctx context.Context) ([]Sample, error)

// Options configure a Sampler. Zero values take the defaults.
type Options struct {
	Every      time.Duration
	FlushEvery time.Duration
	MaxGap     time.Duration
	Now        func() time.Time
	Logger     *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Every <= 0 {
		o.Every = DefaultEvery
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = DefaultFlushEvery
	}
	if o.MaxGap <= 0 {
		o.MaxGap = 3 * o.Every
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

type hourKey struct {
	node string
	hour int64 // unix ms of the UTC hour start
}

// Sampler samples every machine and accumulates milliseconds per state per
// hour. Tick and Flush are called by its own task (Start); tests call them
// directly with a fixed clock.
type Sampler struct {
	store *usage.Store
	src   Source
	o     Options

	mu        sync.Mutex
	prev      []Sample
	prevAt    time.Time // when prev was taken
	counted   time.Time // time is counted up to here
	pending   map[hourKey]*usage.NodeHour
	lastFlush time.Time
	flushHour int64
	reads     uint64 // source reads (tests: one per tick, whatever the machine count)
}

// New makes a sampler; Start runs it.
func New(store *usage.Store, src Source, o Options) *Sampler {
	return &Sampler{store: store, src: src, o: o.withDefaults(), pending: map[hourKey]*usage.NodeHour{}}
}

// Start runs the sampler as a supervised task until ctx ends; the last
// partial minute is written then. done, when set, is called after it.
func (s *Sampler) Start(ctx context.Context, done func()) {
	supervise.Go(ctx, supervise.Spec{Name: "availability.sampler", Policy: supervise.Restart,
		Rebuild: func(any) { s.reset() }, Done: done}, func(ctx context.Context) {
		t := time.NewTicker(s.o.Every)
		defer t.Stop()
		s.Tick(ctx)
		for {
			select {
			case <-ctx.Done():
				s.Tick(context.Background()) // count up to the stop
				s.Flush()
				return
			case <-t.C:
			}
			s.Tick(ctx)
		}
	})
}

// reset forgets everything not yet written (after a panic): that time
// becomes unknown.
func (s *Sampler) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prev, s.prevAt, s.counted = nil, time.Time{}, time.Time{}
	s.pending = map[hourKey]*usage.NodeHour{}
}

// Tick takes one sample: the time since the previous sample is added to
// each machine's previous state, split at hour boundaries, and written when
// a minute passed or an hour ended. A sample accounts for at most MaxGap
// after it was taken: when reads fail (or ticks stop) the rest of the gap
// stays unknown.
func (s *Sampler) Tick(ctx context.Context) {
	now := s.o.Now()
	rctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	cur, err := s.src(rctx)
	cancel()
	if s.take(now, cur, err) {
		s.Flush()
	}
}

// take counts the time up to now and keeps cur as the previous sample; it
// reports whether a flush is due.
func (s *Sampler) take(now time.Time, cur []Sample, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.lastFlush.IsZero() {
		s.lastFlush, s.flushHour = now, hourOf(now)
	}
	if !s.prevAt.IsZero() {
		// The previous sample's state holds from what is already counted up
		// to now, but for at most MaxGap after that sample was taken.
		end := now
		if lim := s.prevAt.Add(s.o.MaxGap); end.After(lim) {
			end = lim
		}
		if end.After(s.counted) {
			for _, p := range s.prev {
				s.creditLocked(p, s.counted, end)
			}
			s.counted = end
		}
	}
	if err != nil {
		s.o.Logger.Debug("availability: sample skipped", "err", err)
	} else if s.prevAt.IsZero() || now.After(s.prevAt) {
		s.prev, s.prevAt, s.counted = cur, now, now
	}
	return now.Sub(s.lastFlush) >= s.o.FlushEvery || hourOf(now) != s.flushHour
}

// creditLocked adds [from, to) of p's state to the pending hours.
func (s *Sampler) creditLocked(p Sample, from, to time.Time) {
	for from.Before(to) {
		hEnd := from.UTC().Truncate(time.Hour).Add(time.Hour)
		seg := to
		if hEnd.Before(seg) {
			seg = hEnd
		}
		ms := seg.Sub(from).Milliseconds()
		k := hourKey{p.Node, hourOf(from)}
		h := s.pending[k]
		if h == nil {
			h = &usage.NodeHour{Node: p.Node, Hour: time.UnixMilli(k.hour).UTC()}
			s.pending[k] = h
		}
		h.Name = p.Name
		switch p.State {
		case Free:
			h.FreeMs += ms
		case InUse:
			h.InUseMs += ms
			h.BusyMs += int64(float64(ms) * p.Busy)
		case Loading:
			h.LoadingMs += ms
		case Lost:
			h.LostMs += ms
		case SetAside:
			h.SetAsideMs += ms
		case Off:
			h.OffMs += ms
		}
		if p.Reachable {
			h.ReachableMs += ms
		}
		from = seg
	}
}

// Flush writes what was counted since the last flush: one queued operation
// with one row per machine and hour. It never blocks.
func (s *Sampler) Flush() {
	if rows := s.takePending(); len(rows) > 0 {
		s.store.AddNodeHours(rows) // false (store off, dropped): that time reads unknown
	}
}

func (s *Sampler) takePending() []usage.NodeHour {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]usage.NodeHour, 0, len(s.pending))
	for _, h := range s.pending {
		rows = append(rows, *h)
	}
	s.pending = map[hourKey]*usage.NodeHour{}
	now := s.o.Now()
	s.lastFlush, s.flushHour = now, hourOf(now)
	return rows
}

// Pending is what was counted and not yet written, for the reader to add
// to the stored rows (the current minute).
func (s *Sampler) Pending() []usage.NodeHour {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]usage.NodeHour, 0, len(s.pending))
	for _, h := range s.pending {
		out = append(out, *h)
	}
	return out
}

// Known is the last sample of each machine (machines the reader lists even
// when they have no stored hour yet).
func (s *Sampler) Known() []Sample {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.prev...)
}

// Reads is how many times the source was read.
func (s *Sampler) Reads() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func hourOf(t time.Time) int64 { return t.UTC().Truncate(time.Hour).UnixMilli() }
