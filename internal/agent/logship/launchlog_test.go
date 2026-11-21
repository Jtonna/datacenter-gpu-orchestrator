package logship

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/launch"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/runner"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// The test binary doubles as a launch that writes as fast as it can.
func TestMain(m *testing.M) {
	if n := os.Getenv("LOGSHIP_FLOOD_LINES"); n != "" {
		count, _ := strconv.Atoi(n)
		w := bufio.NewWriterSize(os.Stdout, 64<<10)
		line := strings.Repeat("y", 120) + "\n"
		for i := 0; i < count; i++ {
			w.WriteString(strconv.Itoa(i))
			w.WriteString(line)
		}
		w.Flush()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type current struct {
	mu sync.Mutex
	id string
	lf *launch.LogFile
}

func (c *current) set(id string, lf *launch.LogFile) { c.mu.Lock(); c.id, c.lf = id, lf; c.mu.Unlock() }
func (c *current) get() (string, *launch.LogFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id, c.lf, c.lf != nil
}

func appendFile(t *testing.T, path, s string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}

// Launch output is read from the launch log by offset: whole lines, a long
// line cut and marked, a line without its newline kept until it is whole,
// one request per launch with its launch_id. A launch that exists when the
// tail starts (re-adopted) is followed from its end.
func TestLaunchTailLines(t *testing.T) {
	r := newRig(t, 0)
	r.register(testObj(1))
	dir := t.TempDir()
	old := &launch.LogFile{Path: filepath.Join(dir, "ln-0.log")}
	appendFile(t, old.Path, "shipped by the agent before its restart\n")
	cur := &current{}
	cur.set("ln-0", old)
	tail := &LaunchTail{Ship: r.ship, Current: cur.get, Every: 10 * time.Millisecond}
	// The starting point is taken before Run's goroutine exists, so the
	// line appended below is after it, whichever goroutine runs first.
	tail.Begin()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tail.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	appendFile(t, old.Path, "after the restart\n")
	eventually(t, 5*time.Second, "re-adopted launch", func() bool { return len(r.sink.msgs(nil)) == 1 })
	if m := r.sink.msgs(nil); msgOf(m[0]) != "after the restart" {
		t.Fatalf("%v", m)
	}

	lf := &launch.LogFile{Path: filepath.Join(dir, "ln-1.log")}
	cur.set("ln-1", lf)
	appendFile(t, lf.Path, "first\r\nsecond\n"+strings.Repeat("L", 3000)+"\nhalf ")
	eventually(t, 5*time.Second, "lines", func() bool { return len(r.sink.msgs(nil)) == 4 })
	appendFile(t, lf.Path, "line\n")
	eventually(t, 5*time.Second, "joined", func() bool { return len(r.sink.msgs(nil)) == 5 })
	var got []map[string]any
	for _, q := range r.sink.requests() {
		if q.query == "launch_id=ln-1&source=launch" {
			got = append(got, q.lines...)
		}
	}
	if len(got) != 4 || msgOf(got[0]) != "first" || msgOf(got[1]) != "second" || got[2]["truncated"] != true ||
		len(msgOf(got[2])) > 1024 || msgOf(got[3]) != "half line" {
		t.Fatalf("%v", got)
	}
}

// A launch that writes much faster than shipping is never held up: it
// writes into its log file, never into a pipe the agent must drain. Here a
// real child process (the Docker install's runner) writes 64 MB while
// nothing is ever sent (the sender does not run, as with a Potluck that
// never answers): it finishes as fast as it can write, the tail skips what
// it cannot ship and says so, and the ring stays bounded.
func TestFastLaunchIsNeverBlocked(t *testing.T) {
	// No sender: the test is the ring's only reader.
	ship := New(Config{Now: func() time.Duration { return 0 }, BufferBytes: 1 << 20})
	obj := testObj(1)
	obj.Limits.MaxBytesPerS, obj.Limits.MaxBatchBytes = 256<<10, 64<<10
	ship.Registered(testNode, obj, T0.UnixMilli(), 0, 0)
	dir := t.TempDir()
	lf := &launch.LogFile{Path: filepath.Join(dir, "ln-1.log"), Cap: 1 << 40}
	cur := &current{}
	tail := &LaunchTail{Ship: ship, Current: cur.get, Every: 20 * time.Millisecond}
	tail.Begin()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tail.Run(ctx); close(done) }()
	stopTail := func() { cancel(); <-done }
	defer stopTail()

	exe, _ := os.Executable()
	t.Setenv("LOGSHIP_FLOOD_LINES", "500000") // about 64 MB
	start := time.Now()
	p, err := runner.NewChild().Start(context.Background(), runner.Spec{Unit: agentproto.LaunchUnitPrefix + "ln-1", Path: exe, LogPath: lf.Path})
	if err != nil {
		t.Fatal(err)
	}
	cur.set("ln-1", lf)
	select {
	case <-p.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("the launch was held up")
	}
	took := time.Since(start)
	if e := p.Exit(); !e.Known || e.Code != 0 {
		t.Fatalf("exit %+v", e)
	}
	if lf.Size() < 60<<20 {
		t.Fatalf("the launch wrote only %d bytes", lf.Size())
	}
	// The tail counts a skip and adds its line in the same round: wait for
	// the count, stop the tail (its round is over then), read the ring.
	eventually(t, 10*time.Second, "output skipped", func() bool { return ship.Ring().DroppedTotal() > 0 })
	stopTail()
	if b := ship.Ring().Bytes(); b > 1<<20 {
		t.Fatalf("ring %d bytes", b)
	}
	var lines []Line
	for {
		_, l, _, ok := ship.Ring().Take(1<<30, estimate(1<<20))
		if !ok {
			break
		}
		lines = append(lines, l...)
	}
	skipped := false
	for _, l := range lines {
		skipped = skipped || strings.Contains(l.Msg, "bytes of this launch's output were skipped")
	}
	if !skipped {
		t.Fatalf("no skip said (%d lines kept)", len(lines))
	}
	t.Logf("64 MB written in %v; %d lines kept", took, len(lines))
}
