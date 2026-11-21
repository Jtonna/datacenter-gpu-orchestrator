package logship

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startOnce(text string) (JournalStart, *atomic.Int32) {
	var n atomic.Int32
	return func(ctx context.Context, dir string) (io.ReadCloser, error) {
		n.Add(1)
		return io.NopCloser(strings.NewReader(text)), nil
	}, &n
}

// Journal entries: their monotonic stamp through the anchor, another boot's
// entry by arrival, deny_units and the agent's own unit dropped, a binary
// message, the priority as a level, a line too long to read dropped.
func TestJournalEntries(t *testing.T) {
	r := newRig(t, 0)
	obj := testObj(1)
	obj.DenyUnits = []string{"noisy"}
	text := strings.Join([]string{
		`{"MESSAGE":"same boot","PRIORITY":"6","_SYSTEMD_UNIT":"sshd.service","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"99000000","__REALTIME_TIMESTAMP":"1000000"}`,
		`{"MESSAGE":"other boot","PRIORITY":"4","_SYSTEMD_UNIT":"sshd.service","_BOOT_ID":"b0","__MONOTONIC_TIMESTAMP":"5000000"}`,
		`{"MESSAGE":"denied","_SYSTEMD_UNIT":"noisy.service","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"99000000"}`,
		`{"MESSAGE":"own line","_SYSTEMD_UNIT":"potluck-agent.service","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"99000000"}`,
		`{"MESSAGE":[104,105],"PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"100000000"}`,
		`{"MESSAGE":"` + strings.Repeat("x", 5000) + `"}`,
		`{"MESSAGE":"last","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"100000000"}`,
	}, "\n") + "\n"
	start, starts := startOnce(text)
	j := &Journal{Ship: r.ship, Start: start, BootID: "b1", MaxRead: 4096, Retry: time.Hour, Check: 10 * time.Millisecond,
		Monotonic: func() (time.Duration, error) { return 100 * time.Second, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); j.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()
	time.Sleep(50 * time.Millisecond)
	if starts.Load() != 0 {
		t.Fatal("the journal was read before shipping was on")
	}
	r.register(obj) // T0 at agent clock 10 s
	eventually(t, 5*time.Second, "journal lines", func() bool { return len(r.sink.msgs(nil)) >= 5 })
	lines := r.sink.msgs(nil)
	if len(lines) != 5 {
		t.Fatalf("%d lines: %v", len(lines), lines)
	}
	byMsg := map[string]map[string]any{}
	for _, l := range lines {
		byMsg[msgOf(l)] = l
	}
	same := byMsg["same boot"]
	if same == nil || !lineTime(t, same).Equal(T0.Add(-time.Second)) || same["unit"] != "sshd.service" || same["level"] != "info" || same["time_source"] != nil {
		t.Fatalf("same boot: %v", same)
	}
	other := byMsg["other boot"]
	if other == nil || !lineTime(t, other).Equal(T0) || other["time_source"] != "arrival" || other["level"] != "warn" {
		t.Fatalf("other boot: %v", other)
	}
	hi := byMsg["hi"]
	if hi == nil || hi["unit"] != "kernel" || hi["level"] != "error" || !lineTime(t, hi).Equal(T0) {
		t.Fatalf("binary message: %v", hi)
	}
	if byMsg["last"] == nil || !hasMsg(lines, "1 lines of this source were dropped") {
		t.Fatalf("the too-long line is not counted: %v", lines)
	}
	if byMsg["denied"] != nil || byMsg["own line"] != nil {
		t.Fatal("denied or own unit shipped")
	}
	for _, q := range r.sink.requests() {
		if q.query != "source=journal" {
			t.Fatal(q.query)
		}
	}
}

// endless produces journal lines as fast as it is read, and counts them.
type endless struct {
	n    atomic.Int64
	line []byte
	pos  int
}

func (e *endless) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], e.line[e.pos:])
		n += c
		e.pos += c
		if e.pos == len(e.line) {
			e.pos = 0
			e.n.Add(1)
		}
	}
	return n, nil
}
func (e *endless) Close() error { return nil }

// A journal flood is cut to the byte rate by dropping, not by buffering: the
// reader keeps reading at full speed (journalctl is never held up), the ring
// gets about the rate, and the rest is counted as dropped.
func TestJournalFloodIsDroppedNotBuffered(t *testing.T) {
	r := newRig(t, 1<<20)
	r.sink.mu.Lock()
	r.sink.hang = make(chan struct{}) // nothing leaves the ring
	r.sink.mu.Unlock()
	obj := testObj(1)
	obj.Limits.MaxBytesPerS, obj.Limits.MaxBatchBytes = 64<<10, 16<<10
	r.register(obj)
	src := &endless{line: []byte(fmt.Sprintf(`{"MESSAGE":"%s","_SYSTEMD_UNIT":"flood.service","_BOOT_ID":"b1","__MONOTONIC_TIMESTAMP":"1"}`+"\n", strings.Repeat("f", 150)))}
	j := &Journal{Ship: r.ship, BootID: "b1", Monotonic: func() (time.Duration, error) { return time.Second, nil },
		Start: func(ctx context.Context, dir string) (io.ReadCloser, error) { return src, nil }, Retry: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()
	time.Sleep(time.Second)
	cancel()
	read := src.n.Load()
	<-done
	kept := uint64(r.ship.Ring().Len())
	dropped := r.ship.Ring().DroppedTotal()
	// One second at 64 KiB/s plus a 16 KiB burst is about 80 KiB of lines,
	// about 400 of these; everything else read was dropped.
	if read < 20_000 || kept > 700 || dropped < uint64(read)-1000 {
		t.Fatalf("read %d, kept %d, dropped %d", read, kept, dropped)
	}
	t.Logf("read %d lines in 1 s, kept %d, dropped %d", read, kept, dropped)
}

// No journalctl: said once, nothing shipped, the agent goes on.
func TestJournalMissing(t *testing.T) {
	r := newRig(t, 0)
	r.register(testObj(1))
	var n atomic.Int32
	j := &Journal{Ship: r.ship, Start: func(ctx context.Context, dir string) (io.ReadCloser, error) {
		n.Add(1)
		return nil, ErrNoJournal
	}}
	done := make(chan struct{})
	go func() { j.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not give up")
	}
	if n.Load() != 1 {
		t.Fatal(n.Load())
	}
}
