package logship

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The request shape: lines of one source per request, the launch id for
// launch output, the start id header, the store's field names, and no
// field that says who sent the line.
func TestRequestShape(t *testing.T) {
	r := newRig(t, 0)
	r.register(testObj(1))
	r.agentLine("agent line")
	r.ship.Add(Key{Source: agentproto.LogSourceLaunch, LaunchID: "ln-1"}, Line{Mono: r.clock.Now(), Msg: "launch line"})
	r.ship.Add(JournalKey, Line{Mono: r.clock.Now(), Msg: "kernel line", Unit: "kernel", Level: "info"})
	eventually(t, 5*time.Second, "three requests", func() bool { return len(r.sink.requests()) == 3 })
	seen := map[string]bool{}
	for _, q := range r.sink.requests() {
		if q.header.Get(agentproto.HeaderAgentStartID) != "s1" || len(q.lines) != 1 {
			t.Fatalf("%+v", q)
		}
		seen[q.query] = true
		for k := range q.lines[0] {
			switch k {
			case "_msg", "_time", "level", "unit", "stream", "truncated", "time_source":
			default:
				t.Fatalf("the agent set field %q", k)
			}
		}
	}
	for _, want := range []string{"source=agent", "launch_id=ln-1&source=launch", "source=journal"} {
		if !seen[want] {
			t.Fatalf("no request %q in %v", want, seen)
		}
	}
	// A large batch is gzipped.
	for i := 0; i < 200; i++ {
		r.agentLine(fmt.Sprintf("a longer line number %d with some words in it", i))
	}
	// The sender may take the 200 lines in one batch or, if a round falls
	// while they are added, in two; one of them is large enough for gzip.
	eventually(t, 5*time.Second, "all lines, gzipped", func() bool {
		gz := false
		for _, q := range r.sink.requests() {
			gz = gz || (q.header.Get("Content-Encoding") == "gzip" && len(q.lines) >= 100)
		}
		return gz && len(r.sink.msgs(nil)) == 203
	})
	// The sink records a request before the sender has read its answer and
	// counted it: wait for the figures.
	waitFigures(t, r, "203 sent, no error", func(f *agentproto.LogShipping) bool {
		return f.LinesSent == 203 && f.LinesDropped == 0 && f.LastError == ""
	})
}

// waitFigures waits until the shipper's figures satisfy ok. The sender counts
// a batch only after it has read Potluck's answer, which is after the fake
// Potluck recorded the request.
func waitFigures(t *testing.T, r *rig, what string, ok func(*agentproto.LogShipping) bool) {
	t.Helper()
	var last *agentproto.LogShipping
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if last = r.ship.Figures(); last != nil && ok(last) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("figures: want %s, have %+v", what, last)
}

// Lines carry Potluck's time from the anchor, never the node's wall clock:
// a record whose own time (the node's clock) is two months off changes
// nothing.
func TestTimeFromAnchorNotNodeClock(t *testing.T) {
	r := newRig(t, 0)
	r.register(testObj(1)) // T0 at agent clock 10 s
	h := NewHandler(slog.NewTextHandler(io.Discard, nil), r.ship)
	r.clock.Advance(1500 * time.Millisecond)
	wrong := time.Now().Add(-61 * 24 * time.Hour)
	rec := slog.NewRecord(wrong, slog.LevelWarn, "wall clock is off", 0)
	rec.AddAttrs(slog.String("why", "console clock"), slog.Int("n", 3))
	h.WithAttrs([]slog.Attr{slog.String("component", "test")}).Handle(context.Background(), rec)
	eventually(t, 5*time.Second, "line sent", func() bool { return len(r.sink.msgs(nil)) == 1 })
	l := r.sink.msgs(nil)[0]
	if got := lineTime(t, l); !got.Equal(T0.Add(1500 * time.Millisecond)) {
		t.Fatalf("_time %v, want %v", got, T0.Add(1500*time.Millisecond))
	}
	if msgOf(l) != `wall clock is off component=test why="console clock" n=3` || l["level"] != "warn" {
		t.Fatalf("%v", l)
	}
}

// Lines produced before the first anchor keep their monotonic stamp and are
// given Potluck's time when the anchor is known, in the order produced.
func TestLinesBeforeTheAnchorAreStampedLater(t *testing.T) {
	r := newRig(t, 0)
	for i := 0; i < 3; i++ {
		r.agentLine(fmt.Sprintf("early %d", i))
		r.clock.Advance(time.Second)
	}
	time.Sleep(50 * time.Millisecond)
	if len(r.sink.requests()) != 0 {
		t.Fatal("sent before the registration answer")
	}
	r.register(testObj(1)) // T0 at agent clock 13 s
	r.agentLine("after")
	eventually(t, 5*time.Second, "lines sent", func() bool { return len(r.sink.msgs(nil)) == 4 })
	lines := r.sink.msgs(nil)
	for i, want := range []time.Duration{-3 * time.Second, -2 * time.Second, -time.Second, 0} {
		if got := lineTime(t, lines[i]); !got.Equal(T0.Add(want)) {
			t.Fatalf("line %d (%s): %v, want %v", i, msgOf(lines[i]), got, T0.Add(want))
		}
	}
}

// The anchor is set again by every heartbeat answer. When the machine was
// paused and the monotonic clock did not count the pause, the next answer
// puts the times right.
func TestReanchorAfterAPause(t *testing.T) {
	r := newRig(t, 0)
	r.register(testObj(1)) // T0 at 10 s
	r.clock.Advance(2 * time.Second)
	r.agentLine("before the pause")
	// One hour passes; the agent's clock does not move.
	now := r.clock.Now()
	r.agentLine("after the pause, before the answer")
	r.ship.HeartbeatAnswered(1, T0.Add(time.Hour+2*time.Second).UnixMilli(), now, now)
	r.agentLine("after the answer")
	r.clock.Advance(time.Second)
	r.agentLine("a second later")
	eventually(t, 5*time.Second, "lines sent", func() bool { return len(r.sink.msgs(nil)) == 4 })
	lines := r.sink.msgs(nil)
	want := []time.Time{T0.Add(2 * time.Second), T0.Add(2 * time.Second), T0.Add(time.Hour + 2*time.Second), T0.Add(time.Hour + 3*time.Second)}
	for i := range want {
		if got := lineTime(t, lines[i]); !got.Equal(want[i]) {
			t.Fatalf("line %d (%s): %v, want %v", i, msgOf(lines[i]), got, want[i])
		}
	}
	// The answer's time is taken at the middle of the round trip.
	r.ship.HeartbeatAnswered(1, T0.Add(2*time.Hour).UnixMilli(), now, now+400*time.Millisecond)
	if a := r.ship.anchor.Load(); a.mono != now+200*time.Millisecond {
		t.Fatalf("anchor at %v", a.mono)
	}
}

// 413: the batch is split and sent again in halves; nothing is lost.
func TestBatchSplitOn413(t *testing.T) {
	r := newRig(t, 0)
	r.sink.setAnswer(func(n int, lines []map[string]any) (int, http.Header) {
		if len(lines) > 2 {
			return http.StatusRequestEntityTooLarge, nil
		}
		return http.StatusNoContent, nil
	})
	for i := 0; i < 10; i++ {
		r.agentLine(fmt.Sprintf("line %d", i))
	}
	r.register(testObj(1))
	var got []string
	eventually(t, 5*time.Second, "all lines accepted", func() bool {
		got = got[:0]
		for _, q := range r.sink.requests() {
			if len(q.lines) <= 2 {
				for _, l := range q.lines {
					got = append(got, msgOf(l))
				}
			}
		}
		return len(got) == 10
	})
	for i, m := range got {
		if m != fmt.Sprintf("line %d", i) {
			t.Fatalf("order: %v", got)
		}
	}
	waitFigures(t, r, "10 sent, last error 413", func(f *agentproto.LogShipping) bool {
		return f.LinesSent == 10 && f.LinesDropped == 0 && strings.HasPrefix(f.LastError, "413")
	})
}

// 429 with Retry-After: the sender waits that long and sends the same batch.
func TestPauseOn429WithRetryAfter(t *testing.T) {
	r := newRig(t, 0)
	r.sink.setAnswer(func(n int, lines []map[string]any) (int, http.Header) {
		if n == 0 {
			return http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}}
		}
		return http.StatusNoContent, nil
	})
	r.register(testObj(1))
	r.agentLine("one")
	eventually(t, 5*time.Second, "second request", func() bool { return len(r.sink.requests()) >= 2 })
	q := r.sink.requests()
	if gap := q[1].at.Sub(q[0].at); gap < 950*time.Millisecond {
		t.Fatalf("sent again after %v, Retry-After was 1 s", gap)
	}
	if msgOf(q[1].lines[0]) != "one" || len(q) != 2 {
		t.Fatalf("%+v", q)
	}
	waitFigures(t, r, "1 sent, last error 429", func(f *agentproto.LogShipping) bool {
		return f.LastError == "429" && f.LinesSent == 1
	})
	// Without Retry-After (503) the pause grows from FirstPause.
	r.sink.setAnswer(func(n int, lines []map[string]any) (int, http.Header) {
		if n < 5 {
			return http.StatusServiceUnavailable, nil
		}
		return http.StatusNoContent, nil
	})
	r.agentLine("two")
	eventually(t, 5*time.Second, "accepted", func() bool { return len(r.sink.requests()) == 6 })
	q = r.sink.requests()
	// The pause starts after the answer was read, which is after the fake
	// Potluck recorded the request, and a timer never fires early: each gap
	// is at least its pause (50 ms, then 100 ms, then 200 ms). Only lower
	// bounds, so a slow machine cannot fail it.
	for i, want := range []time.Duration{50, 100, 200} {
		if gap := q[3+i].at.Sub(q[2+i].at); gap < want*time.Millisecond {
			t.Fatalf("pause %d: %v, want at least %v ms", i+1, gap, want)
		}
	}
}

// 410: nothing is sent until the agent registers again.
func TestStopOn410UntilRegistered(t *testing.T) {
	r := newRig(t, 0)
	r.sink.setAnswer(func(n int, lines []map[string]any) (int, http.Header) {
		if n == 0 {
			return http.StatusGone, nil
		}
		return http.StatusNoContent, nil
	})
	r.register(testObj(1))
	r.agentLine("one")
	eventually(t, 5*time.Second, "first request", func() bool { return len(r.sink.requests()) == 1 })
	// The registration below must come after the sender took the 410, or
	// the 410 would stop it again after the registration cleared it.
	eventually(t, 5*time.Second, "sender stopped", r.ship.isGone)
	r.agentLine("two")
	time.Sleep(300 * time.Millisecond)
	if n := len(r.sink.requests()); n != 1 {
		t.Fatalf("%d requests after 410", n)
	}
	r.register(testObj(1))
	eventually(t, 5*time.Second, "sent after the registration", func() bool { return len(r.sink.msgs(nil)) == 3 })
	if m := r.sink.msgs(nil); msgOf(m[1]) != "one" || msgOf(m[2]) != "two" {
		t.Fatalf("%v", m)
	}
	// A 410 from the log route alone, with no new registration after it:
	// sending starts again after GoneWait (60 s of the agent's clock).
	r.sink.setAnswer(func(n int, lines []map[string]any) (int, http.Header) {
		if n == 3 {
			return http.StatusGone, nil
		}
		return http.StatusNoContent, nil
	})
	r.agentLine("three")
	eventually(t, 5*time.Second, "410 again", func() bool { return len(r.sink.requests()) == 4 })
	// The sink saw the request before the sender read the answer: wait until
	// the sender has stopped, so the clock moves after the 410 was taken.
	eventually(t, 5*time.Second, "sender stopped", r.ship.isGone)
	time.Sleep(200 * time.Millisecond)
	if n := len(r.sink.requests()); n != 4 {
		t.Fatalf("%d requests", n)
	}
	r.clock.Advance(61 * time.Second)
	eventually(t, 5*time.Second, "sent after the wait", func() bool { return len(r.sink.requests()) == 5 })
}

// 400: the batch is dropped and counted, not sent for ever; the next batch
// says how many lines were lost.
func TestRejectedBatchIsDroppedAndCounted(t *testing.T) {
	r := newRig(t, 0)
	r.sink.setAnswer(func(n int, lines []map[string]any) (int, http.Header) {
		if n == 0 {
			return http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}}
		}
		return http.StatusNoContent, nil
	})
	// The three lines are in the ring before shipping goes on, so the first
	// batch carries all three, whenever the sender wakes.
	for i := 0; i < 3; i++ {
		r.agentLine("bad")
	}
	r.register(testObj(1))
	eventually(t, 5*time.Second, "first request", func() bool { return len(r.sink.requests()) == 1 })
	if q := r.sink.requests(); len(q[0].lines) != 3 {
		t.Fatalf("first request: %v", q[0].lines)
	}
	r.agentLine("good")
	// The count goes first, alone or with the next line.
	eventually(t, 5*time.Second, "the rest", func() bool { return len(r.sink.msgs(nil)) == 5 })
	m := r.sink.msgs(nil)[3:]
	if !strings.Contains(msgOf(m[0]), "3 lines of this source were dropped") || msgOf(m[1]) != "good" {
		t.Fatalf("%v", m)
	}
	waitFigures(t, r, "3 dropped, 2 sent, last error 400", func(f *agentproto.LogShipping) bool {
		return f.LinesDropped == 3 && f.LinesSent == 2 && strings.HasPrefix(f.LastError, "400") && f.LastErrorAgeS != nil
	})
}

// The byte rate is kept by the sender itself.
func TestByteRateKept(t *testing.T) {
	r := newRig(t, 0)
	obj := testObj(1)
	obj.Limits.MaxBytesPerS, obj.Limits.MaxBatchBytes = 20_000, 4_000
	// Random text, so gzip saves little and the wire bytes are near the
	// line bytes. The lines are in the ring before shipping goes on, and the
	// time is taken from then, so the sender cannot have started earlier.
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		r.agentLine(fmt.Sprintf("%016x%016x%016x%016x", rnd.Uint64(), rnd.Uint64(), rnd.Uint64(), rnd.Uint64()))
	}
	start := time.Now()
	r.register(obj)
	eventually(t, 10*time.Second, "all sent", func() bool { return len(r.sink.msgs(nil)) == 400 })
	took := time.Since(start)
	var wire int64
	for _, q := range r.sink.requests() {
		wire += q.wire
	}
	// One batch may go at once (the burst); the rest at the rate.
	if min := time.Duration(float64(wire-4_000) / 20_000 * float64(time.Second) * 0.9); took < min {
		t.Fatalf("%d bytes on the wire in %v; at 20000 B/s that takes at least %v", wire, took, min)
	}
	t.Logf("%d bytes on the wire in %v", wire, took)
}

// A new logs_revision makes the shipper read the object and use its limits;
// a heartbeat answer without logs_revision stops shipping.
func TestObjectChangeAndStop(t *testing.T) {
	r := newRig(t, 0)
	r.register(testObj(1))
	r.agentLine(strings.Repeat("a", 900))
	eventually(t, 5*time.Second, "sent whole", func() bool { return len(r.sink.msgs(nil)) == 1 })
	if l := r.sink.msgs(nil)[0]; l["truncated"] != nil {
		t.Fatalf("%v", l)
	}
	obj := testObj(2)
	obj.Limits.MaxLineBytes = 300
	obj.DenyUnits = []string{"noisy"}
	r.sink.mu.Lock()
	r.sink.obj = obj
	r.sink.mu.Unlock()
	r.ship.HeartbeatAnswered(1, T0.UnixMilli(), r.clock.Now(), r.clock.Now())
	time.Sleep(50 * time.Millisecond)
	if r.sink.nGets() != 0 {
		t.Fatal("read the object although the revision is unchanged")
	}
	r.ship.HeartbeatAnswered(2, T0.UnixMilli(), r.clock.Now(), r.clock.Now())
	eventually(t, 5*time.Second, "new limits", func() bool {
		l, deny, ok := r.ship.Limits()
		return ok && l.MaxLineBytes == 300 && len(deny) == 1
	})
	r.agentLine(strings.Repeat("b", 900))
	eventually(t, 5*time.Second, "sent cut", func() bool { return len(r.sink.msgs(nil)) == 2 })
	if l := r.sink.msgs(nil)[1]; l["truncated"] != true || len(msgOf(l)) > 300 {
		t.Fatalf("%v", l)
	}
	// Logging off.
	r.ship.HeartbeatAnswered(0, T0.UnixMilli(), r.clock.Now(), r.clock.Now())
	if r.ship.On() || r.ship.Figures() != nil {
		t.Fatal("still on")
	}
	r.agentLine("not shipped")
	time.Sleep(100 * time.Millisecond)
	if len(r.sink.msgs(nil)) != 2 || r.ship.Ring().Len() != 0 {
		t.Fatal("shipped or kept a line with logging off")
	}
	// On again: the revision is back, the object is read.
	r.ship.HeartbeatAnswered(2, T0.UnixMilli(), r.clock.Now(), r.clock.Now())
	eventually(t, 5*time.Second, "on again", r.ship.On)
}

// No logs object: nothing is kept, sent or reported.
func TestNoObjectShipsNothing(t *testing.T) {
	r := newRig(t, 0)
	r.agentLine("before the answer")
	r.register(nil)
	r.agentLine("after")
	r.ship.HeartbeatAnswered(0, 0, 0, 0)
	time.Sleep(100 * time.Millisecond)
	if len(r.sink.requests()) != 0 || r.ship.Ring().Len() != 0 || r.ship.Figures() != nil || r.ship.On() || r.sink.nGets() != 0 {
		t.Fatal("shipping without a logs object")
	}
}

// A hung Potluck never holds up a producer or the heartbeat's calls: adding
// lines, the answers and the figures all return at once while the sender
// waits on a request that is never answered, and the ring stays bounded.
func TestHungPotluckNeverBlocksProducers(t *testing.T) {
	r := newRig(t, 256<<10)
	r.sink.mu.Lock()
	r.sink.hang = make(chan struct{})
	r.sink.mu.Unlock()
	r.register(testObj(1))
	r.agentLine("first")
	eventually(t, 5*time.Second, "a request hangs", func() bool {
		f := r.ship.Figures()
		return f != nil && r.ship.Ring().Len() == 0
	})
	var worst time.Duration
	for i := 0; i < 200_000; i++ {
		start := time.Now()
		r.agentLine(fmt.Sprintf("line %d %s", i, strings.Repeat("x", i%200)))
		if i%1000 == 0 {
			r.ship.HeartbeatAnswered(1, T0.UnixMilli(), r.clock.Now(), r.clock.Now())
			r.ship.Figures()
		}
		worst = max(worst, time.Since(start))
	}
	if worst > 50*time.Millisecond {
		t.Fatalf("a producer waited %v", worst)
	}
	if b := r.ship.Ring().Bytes(); b > 256<<10 {
		t.Fatalf("ring %d bytes", b)
	}
	if f := r.ship.Figures(); f.LinesDropped == 0 {
		t.Fatalf("%+v", f)
	}
	t.Logf("slowest add %v", worst)
}
