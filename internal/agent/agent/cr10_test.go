package agent

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/agenttest"
)

// CR-10, the agent's log shipper, against the fake Potluck.

// potluckNow is Potluck's clock in these tests.
var potluckNow = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

type logPush struct {
	query string
	lines []map[string]any
}

// logSink serves the log routes of the fake Potluck.
type logSink struct {
	mu     sync.Mutex
	obj    *agentproto.Logs
	pushes []logPush
	gets   int
	hang   chan struct{}
}

func (s *logSink) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	hang, obj := s.hang, s.obj
	if r.Method == http.MethodGet {
		s.gets++
	}
	s.mu.Unlock()
	if r.Method == http.MethodGet {
		if obj == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(obj)
		return
	}
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		body = z
	}
	var lines []map[string]any
	sc := bufio.NewScanner(body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		lines = append(lines, m)
	}
	s.mu.Lock()
	s.pushes = append(s.pushes, logPush{query: r.URL.RawQuery, lines: lines})
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *logSink) all() []logPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logPush(nil), s.pushes...)
}

func (s *logSink) find(query, sub string) map[string]any {
	for _, p := range s.all() {
		if p.query != query {
			continue
		}
		for _, l := range p.lines {
			if m, _ := l["_msg"].(string); strings.Contains(m, sub) {
				return l
			}
		}
	}
	return nil
}

func logsObj(rev int64) *agentproto.Logs {
	return &agentproto.Logs{Revision: rev, URL: "/api/pool/nodes/02:00:00:00:00:ab/logs", Format: agentproto.LogsFormatJSONLine,
		Limits:    agentproto.LogsLimits{MaxBatchBytes: 64 << 10, MaxLineBytes: 2048, MaxBytesPerS: 1 << 20, EveryS: 0.02},
		DenyUnits: []string{}}
}

// journalFlood produces journal entries as fast as they are read.
type journalFlood struct {
	n   atomic.Int64
	pos int
}

var floodLine = []byte(`{"MESSAGE":"` + strings.Repeat("j", 200) + `","_SYSTEMD_UNIT":"flood.service","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"1000000"}` + "\n")

func (f *journalFlood) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], floodLine[f.pos:])
		n, f.pos = n+c, f.pos+c
		if f.pos == len(floodLine) {
			f.pos = 0
			f.n.Add(1)
		}
	}
	return n, nil
}
func (f *journalFlood) Close() error { return nil }

// newLogsTenv is the usual test agent with a Potluck that hands out a logs
// object (offer false: one that does not, as release 0.1.0 and a Potluck
// with logging off). The journal is a fake follower, started through
// --journal-dir (a Docker install has no system log without it).
func newLogsTenv(t *testing.T, offer bool, mod func(*Options, *Deps)) (*tenv, *logSink, *atomic.Int32) {
	t.Helper()
	sink := &logSink{obj: logsObj(1)}
	var journalStarts atomic.Int32
	e := newTenvWith(t, func(o *Options, d *Deps) {
		o.JournalDir = "/host/journal"
		d.Journal = func(ctx context.Context, dir string) (io.ReadCloser, error) {
			journalStarts.Add(1)
			return io.NopCloser(strings.NewReader(`{"MESSAGE":"from the journal","_SYSTEMD_UNIT":"sshd.service","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"1000000"}` + "\n")), nil
		}
		d.Monotonic = func() (time.Duration, error) { return time.Second, nil }
		d.BootID = func() string { return "b1" }
		d.LaunchLogEvery = 10 * time.Millisecond
		d.LogsHTTP = &http.Client{Timeout: 3 * time.Second}
		if mod != nil {
			mod(o, d)
		}
	})
	h := http.HandlerFunc(sink.serve)
	e.pl.logs.Store(&h)
	if offer {
		prev := e.pl.answer
		e.pl.set(func() {
			e.pl.answer = func(req agentproto.RegisterRequest) agentproto.RegisterAnswer {
				a := prev(req)
				sink.mu.Lock()
				a.Logs = sink.obj
				sink.mu.Unlock()
				a.ServerTimeUnixMs = potluckNow.UnixMilli()
				return a
			}
			e.pl.hbAnswer.LogsRevision = 1
			e.pl.hbAnswer.ServerTimeUnixMs = potluckNow.UnixMilli()
		})
	}
	t.Cleanup(func() {
		sink.mu.Lock()
		if sink.hang != nil {
			select {
			case <-sink.hang:
			default:
				close(sink.hang)
			}
		}
		sink.mu.Unlock()
	})
	return e, sink, &journalStarts
}

// The agent ships its own lines, launch output and the journal, each as its
// own source, with Potluck's time; the heartbeat carries the figures.
func TestAgentShipsItsLines(t *testing.T) {
	e, sink, _ := newLogsTenv(t, true, nil)
	e.start()
	e.waitReady()
	agenttest.Eventually(t, 10*time.Second, "registered line shipped", func() bool {
		return sink.find("source=agent", "registered") != nil
	})
	l := sink.find("source=agent", "registered")
	// The fake clock does not move: every line is at Potluck's time of the
	// answer.
	if l["_time"] != "2026-11-21T10:00:00Z" || l["level"] != "info" || l["node_id"] != nil {
		t.Fatalf("%v", l)
	}
	agenttest.Eventually(t, 10*time.Second, "journal line", func() bool {
		return sink.find("source=journal", "from the journal") != nil
	})
	// Launch output: the launch's own log file, as the launch writes it.
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	f, err := os.OpenFile(filepath.Join(e.opt.DataDir, "launch", "ln-1.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("worker says hello\n")
	f.Close()
	agenttest.Eventually(t, 10*time.Second, "launch line", func() bool {
		return sink.find("launch_id=ln-1&source=launch", "worker says hello") != nil
	})
	agenttest.Eventually(t, 10*time.Second, "figures in the heartbeat", func() bool {
		hb := e.pl.lastBeat()
		return hb.Logs != nil && hb.Logs.LinesSent >= 3
	})
}

// A new logs_revision makes the agent read the object and use it without a
// restart; a heartbeat answer without logs_revision stops shipping.
func TestAgentFollowsTheLogsObject(t *testing.T) {
	e, sink, _ := newLogsTenv(t, true, nil)
	e.start()
	e.waitReady()
	agenttest.Eventually(t, 10*time.Second, "shipping", e.a.Ship.On)
	obj := logsObj(2)
	obj.Limits.MaxLineBytes = 300
	sink.mu.Lock()
	sink.obj = obj
	sink.mu.Unlock()
	e.pl.set(func() { e.pl.hbAnswer.LogsRevision = 2 })
	agenttest.Eventually(t, 10*time.Second, "new limits", func() bool {
		l, _, ok := e.a.Ship.Limits()
		return ok && l.MaxLineBytes == 300
	})
	e.a.log.Info("long " + strings.Repeat("z", 1000))
	agenttest.Eventually(t, 10*time.Second, "cut line", func() bool {
		l := sink.find("source=agent", "long zzz")
		return l != nil && l["truncated"] == true
	})
	// Logging switched off in Potluck.
	e.pl.set(func() { e.pl.hbAnswer.LogsRevision = 0 })
	agenttest.Eventually(t, 10*time.Second, "stopped", func() bool { return !e.a.Ship.On() && e.pl.lastBeat().Logs == nil })
	// A push that was on its way when logging went off may still land; what
	// counts is that nothing logged after it is kept or sent.
	e.a.log.Info("not shipped")
	if e.a.Ship.Ring().Len() != 0 {
		t.Fatal("kept a line with logging off")
	}
	time.Sleep(200 * time.Millisecond)
	if sink.find("source=agent", "not shipped") != nil {
		t.Fatal("shipped with logging off")
	}
}

// Compatibility: against a Potluck that hands out no logs object (release
// 0.1.0, or logging off), an agent of this branch ships nothing, starts no
// journal reader, keeps no lines and sends the heartbeat of before.
func TestAgentWithoutLogsObjectShipsNothing(t *testing.T) {
	e, sink, journalStarts := newLogsTenv(t, false, nil)
	e.start()
	e.waitReady()
	n := e.pl.nBeats()
	agenttest.Eventually(t, 5*time.Second, "more heartbeats", func() bool { return e.pl.nBeats() > n+5 })
	if w := e.do("POST", "/launch", workerReq("ln-1"), fromPotluck); w.Code != 202 {
		t.Fatal(w.Code)
	}
	time.Sleep(200 * time.Millisecond)
	sink.mu.Lock()
	gets := sink.gets
	sink.mu.Unlock()
	if len(sink.all()) != 0 || gets != 0 || journalStarts.Load() != 0 || e.a.Ship.Ring().Len() != 0 || e.a.Ship.On() {
		t.Fatalf("pushes %d gets %d journal %d ring %d", len(sink.all()), gets, journalStarts.Load(), e.a.Ship.Ring().Len())
	}
	e.pl.mu.Lock()
	defer e.pl.mu.Unlock()
	for _, hb := range e.pl.beats {
		if hb.Logs != nil {
			t.Fatalf("heartbeat with logs figures: %+v", hb.Logs)
		}
	}
}

// --logs off: no shipper at all, even when Potluck offers a logs object.
func TestLogsOff(t *testing.T) {
	e, sink, journalStarts := newLogsTenv(t, true, func(o *Options, d *Deps) { o.LogsOff = true })
	if e.a.Ship != nil {
		t.Fatal("a shipper with --logs off")
	}
	e.start()
	e.waitReady()
	n := e.pl.nBeats()
	agenttest.Eventually(t, 5*time.Second, "more heartbeats", func() bool { return e.pl.nBeats() > n+5 })
	if len(sink.all()) != 0 || journalStarts.Load() != 0 || e.pl.lastBeat().Logs != nil {
		t.Fatal("shipping with --logs off")
	}
}

// beatsDuring returns the heartbeats e's agent sends while d passes, and
// the longest gap between two of them.
func beatsDuring(e *tenv, d time.Duration) (n int, worst time.Duration) {
	e.pl.mu.Lock()
	from := len(e.pl.beatAt)
	e.pl.mu.Unlock()
	time.Sleep(d)
	e.pl.mu.Lock()
	at := append([]time.Time(nil), e.pl.beatAt[from:]...)
	e.pl.mu.Unlock()
	for i := 1; i < len(at); i++ {
		worst = max(worst, at[i].Sub(at[i-1]))
	}
	return len(at), worst
}

// The heartbeat and a launch start keep their time while the log sender
// hangs (Potluck never answers a push), the ring is full and the journal
// reader gets lines as fast as it can read them.
//
// The heartbeat is compared with a baseline taken just before on the same
// machine with logging off, so a slow or loaded test machine slows both and
// only what the logging adds is held to the bound.
func TestLoggingNeverHoldsUpHeartbeatOrLaunch(t *testing.T) {
	base, _, _ := newLogsTenv(t, true, func(o *Options, d *Deps) { o.LogsOff = true })
	base.start()
	base.waitReady()
	baseN, baseWorst := beatsDuring(base, 1800*time.Millisecond)
	base.stop()

	flood := &journalFlood{}
	e, sink, _ := newLogsTenv(t, true, func(o *Options, d *Deps) {
		o.LogBufferBytes = 256 << 10
		d.Journal = func(ctx context.Context, dir string) (io.ReadCloser, error) { return flood, nil }
	})
	sink.mu.Lock()
	sink.hang = make(chan struct{})
	sink.mu.Unlock()
	e.start()
	e.waitReady()
	agenttest.Eventually(t, 10*time.Second, "ring full and dropping", func() bool {
		return e.a.Ship.On() && e.a.Ship.Ring().DroppedTotal() > 10_000
	})
	// The agent logs a lot of its own as well.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.a.log.Info("busy agent line", "n", 1)
			}
		}
	}()
	var w *httptest.ResponseRecorder
	var took time.Duration
	launched := make(chan struct{})
	go func() {
		defer close(launched)
		time.Sleep(1500 * time.Millisecond)
		start := time.Now()
		w = e.do("POST", "/launch", workerReq("ln-1"), fromPotluck)
		took = time.Since(start)
	}()
	n, worst := beatsDuring(e, 1800*time.Millisecond)
	<-launched
	close(stop)
	wg.Wait()
	if w.Code != 202 || took > 2*time.Second {
		t.Fatalf("launch start: %d after %v: %s", w.Code, took, w.Body.String())
	}
	// Heartbeats every 20 ms. The count keeps the bound it had (20 in
	// 1.8 s); what the logging may add to the longest gap is 500 ms over the
	// baseline.
	if n < 20 || worst > baseWorst+500*time.Millisecond {
		t.Fatalf("%d heartbeats in 1.8 s, longest gap %v; with logging off %d, longest gap %v", n, worst, baseN, baseWorst)
	}
	if b := e.a.Ship.Ring().Bytes(); b > 256<<10 {
		t.Fatalf("ring %d bytes", b)
	}
	if len(sink.all()) != 0 || flood.n.Load() < 10_000 {
		t.Fatalf("pushes %d, journal lines read %d", len(sink.all()), flood.n.Load())
	}
	t.Logf("%d heartbeats, longest gap %v (logging off: %d, %v); launch start %v; journal lines read %d; dropped %d",
		n, worst, baseN, baseWorst, took, flood.n.Load(), e.a.Ship.Ring().DroppedTotal())
}
