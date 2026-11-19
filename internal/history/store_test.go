package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type capHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *capHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Level.String()+" "+r.Message)
	h.mu.Unlock()
	return nil
}
func (h *capHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capHandler) WithGroup(string) slog.Handler      { return h }
func (h *capHandler) warns() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.msgs {
		if strings.HasPrefix(m, "WARN") {
			out = append(out, m)
		}
	}
	return out
}

func logger() (*slog.Logger, *capHandler) {
	h := &capHandler{}
	return slog.New(h), h
}

// ev builds a test event at a time.
func ev(ts int64, typ, sev, summary string) Event {
	return Event{Ts: ts, Type: typ, Severity: sev, Summary: summary}
}

func openT(t *testing.T, dir string, mod ...func(*Options)) (*Store, *capHandler) {
	t.Helper()
	lg, h := logger()
	o := Options{Dir: dir, Logger: lg}
	for _, m := range mod {
		m(&o)
	}
	s := Open(o)
	t.Cleanup(s.Close)
	return s, h
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestOff_NothingWrittenAndTheAnswerSaysOff(t *testing.T) {
	s, _ := openT(t, "")
	s.Record(ev(1, "x", Info, "nothing"))
	if s.On() {
		t.Fatal("on")
	}
	r := s.Query(Query{})
	if r.History != "off" || len(r.Events) != 0 || r.Events == nil {
		t.Fatalf("%+v", r)
	}
	if sum := s.Summarize(0, 0); sum.History != "off" {
		t.Fatalf("%+v", sum)
	}
	var b bytes.Buffer
	if err := s.Export(&b, 0, 0); err != nil || b.Len() != 0 {
		t.Fatalf("export %q %v", b.String(), err)
	}
}

func TestBadDirectoryWarnsOnceAndHistoryStaysOff(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, h := openT(t, filepath.Join(file, "sub")) // a directory below a file
	if s.On() {
		t.Fatal("history on with a bad directory")
	}
	for i := 0; i < 10; i++ {
		s.Record(ev(int64(i), "x", Info, "x")) // never panics, never blocks
	}
	if w := h.warns(); len(w) != 1 {
		t.Fatalf("warnings %v, want exactly one", w)
	}
	if r := s.Query(Query{}); r.History != "off" {
		t.Fatalf("%+v", r)
	}
}

func TestRecordAssignsSeqAndWritesJSONLines(t *testing.T) {
	dir := t.TempDir()
	s, _ := openT(t, dir)
	s.Record(Event{Ts: 1000, Type: TLoadStarted, Severity: Info, Profile: "p", Nodes: []string{"a", "b"}, SessionID: "ld-1", Summary: "s",
		Data: map[string]any{"k": 1}})
	s.Record(ev(2000, "y", Warn, "w"))
	s.Close()
	fs := names(t, dir)
	if len(fs) != 1 || !fileName.MatchString(fs[0]) {
		t.Fatalf("files %v", fs)
	}
	b, _ := os.ReadFile(filepath.Join(dir, fs[0]))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.Seq != 1 || e.Profile != "p" || e.SessionID != "ld-1" || len(e.Nodes) != 2 {
		t.Fatalf("%+v %v", e, err)
	}
	var raw map[string]any
	json.Unmarshal([]byte(lines[1]), &raw)
	if raw["seq"].(float64) != 2 || raw["nodes"] == nil || raw["data"] == nil {
		t.Fatalf("second line %v: nodes and data must be present", raw)
	}
	t.Logf("line: %s", lines[0])
}

// Rotation by size, names that sort by time, and retention of the newest files.
func TestRotationBySizeAndRetention(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 11, 19, 3, 15, 0, 0, time.UTC)
	s, _ := openT(t, dir, func(o *Options) {
		o.MaxFileBytes = 600
		o.KeepFiles = 3
		o.Now = func() time.Time { return clk } // the same second every time: names must still be unique and increasing
	})
	for i := 0; i < 40; i++ {
		s.Record(Event{Ts: int64(1000 + i), Type: "x", Severity: Info, Summary: fmt.Sprintf("event number %02d", i)})
	}
	s.Close()
	fs := names(t, dir)
	if len(fs) != 3 {
		t.Fatalf("%d files kept, want 3: %v", len(fs), fs)
	}
	for i, f := range fs {
		if !fileName.MatchString(f) {
			t.Fatalf("name %q", f)
		}
		if i > 0 && fs[i-1] >= f {
			t.Fatalf("names do not sort by time: %v", fs)
		}
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Size() > 600 {
			t.Fatalf("%s is %d bytes, over the 600 limit", f, st.Size())
		}
	}
	if fs[0] != "events-20261119T031500Z.jsonl" && !strings.HasPrefix(fs[0], "events-202611") {
		t.Fatalf("first name %q", fs[0])
	}
	// The newest events are in the newest files, none lost between them.
	var last Event
	scanFile(filepath.Join(dir, fs[len(fs)-1]), func(e *Event) bool { last = *e; return true })
	if last.Summary != "event number 39" {
		t.Fatalf("last event %+v", last)
	}
	t.Logf("kept files: %v", fs)
}

// A restart loads the newest events back into memory (up to history_memory_events)
// and keeps appending to the newest file.
func TestReloadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UnixMilli() // event times are wall-clock times; the file names are too
	s, _ := openT(t, dir, func(o *Options) { o.MaxFileBytes = 700; o.KeepFiles = 50 })
	for i := 0; i < 30; i++ {
		s.Record(ev(base+int64(i), "x", Info, fmt.Sprintf("n%02d", i)))
	}
	s.Close()
	before := len(names(t, dir))
	if before < 3 {
		t.Fatalf("want several files for this test, got %d", before)
	}

	s2, _ := openT(t, dir, func(o *Options) { o.MaxFileBytes = 700; o.KeepFiles = 50; o.MemoryEvents = 10 })
	r := s2.Query(Query{Limit: 10})
	if len(r.Events) != 10 || r.Events[0].Summary != "n29" || r.Events[9].Summary != "n20" {
		t.Fatalf("reloaded %d events, first %q last %q", len(r.Events), r.Events[0].Summary, r.Events[len(r.Events)-1].Summary)
	}
	// Older than the memory window: read from the files, newest first, with no gap or duplicate.
	r = s2.Query(Query{Limit: 100, Until: base + 29})
	if len(r.Events) != 30 {
		t.Fatalf("%d events over the files, want 30", len(r.Events))
	}
	for i, e := range r.Events {
		if want := fmt.Sprintf("n%02d", 29-i); e.Summary != want {
			t.Fatalf("event %d is %q, want %q", i, e.Summary, want)
		}
	}
	s2.Record(ev(base+100, "x", Info, "after restart"))
	s2.Close()
	if got := len(names(t, dir)); got != before && got != before+1 {
		t.Fatalf("%d files after the restart, had %d", got, before)
	}
	s3, _ := openT(t, dir, func(o *Options) { o.MaxFileBytes = 700; o.KeepFiles = 50 })
	r = s3.Query(Query{Limit: 1})
	if r.Events[0].Summary != "after restart" || r.TotalMatching != 31 || !r.Truncated {
		t.Fatalf("%+v", r)
	}
}

func TestReloadAppendsToTheNewestFileWhenItHasRoomAndFixesATornLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events-20261119T031500Z.jsonl")
	line, _ := json.Marshal(Event{Ts: 1, Seq: 1, Type: "x", Severity: Info, Summary: "old", Nodes: []string{}, Data: map[string]any{}})
	os.WriteFile(p, append(append(line, '\n'), []byte(`{"ts":2,"typ`)...), 0o644) // a torn last line
	s, _ := openT(t, dir)
	s.Record(ev(3, "x", Info, "new"))
	s.Close()
	if fs := names(t, dir); len(fs) != 1 {
		t.Fatalf("files %v, want the newest reused", fs)
	}
	s2, _ := openT(t, dir)
	r := s2.Query(Query{Asc: true})
	if len(r.Events) != 2 || r.Events[0].Summary != "old" || r.Events[1].Summary != "new" {
		t.Fatalf("%+v", r.Events)
	}
}

func TestRetentionIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep me"), 0o644)
	for i := 1; i <= 5; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("events-2026111%dT000000Z.jsonl", i)), nil, 0o644)
	}
	s, _ := openT(t, dir, func(o *Options) { o.KeepFiles = 2 })
	s.Close()
	got := names(t, dir)
	if !contains(got, "notes.txt") || len(got) != 3 {
		t.Fatalf("%v", got)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// A full writer queue drops events from the file but never blocks Record; the
// memory window keeps them.
func TestRecordNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	dir := t.TempDir()
	s, _ := openT(t, dir, func(o *Options) { o.QueueSize = 1 })
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20000; i++ {
			s.Record(ev(int64(i), "x", Info, "x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Record blocked")
	}
	if r := s.Query(Query{Limit: 1}); r.TotalMatching != 5000 {
		t.Fatalf("memory holds %d, want the window of 5000", r.TotalMatching)
	}
}

// A write failure warns once and never affects Record.
func TestWriteFailureWarnsOnceAndKeepsRecording(t *testing.T) {
	dir := t.TempDir()
	s, h := openT(t, dir)
	s.Record(ev(1, "x", Info, "first"))
	waitFor(t, func() bool { return fileSize(t, dir) > 0 })
	s.f.Close() // the writer's file goes bad underneath it (writer is idle: only touched here)
	for i := 0; i < 5; i++ {
		s.Record(ev(int64(10+i), "x", Info, "after"))
	}
	waitFor(t, func() bool { return len(h.warns()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if w := h.warns(); len(w) != 1 {
		t.Fatalf("warnings %v, want one", w)
	}
	if r := s.Query(Query{}); len(r.Events) != 6 {
		t.Fatalf("%d events in memory, want 6", len(r.Events))
	}
}

func fileSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, f := range names(t, dir) {
		st, _ := os.Stat(filepath.Join(dir, f))
		n += st.Size()
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestQueryFilters(t *testing.T) {
	s, _ := openT(t, t.TempDir())
	s.Record(Event{Ts: 100, Type: "loadStarted", Severity: Info, Profile: "gemma", Nodes: []string{"ps5-1", "ps5-2"}, SessionID: "s1", Summary: "Loading gemma"})
	s.Record(Event{Ts: 200, Type: "loadFinished", Severity: Info, Profile: "gemma", Nodes: []string{"ps5-1"}, SessionID: "s1", Summary: "Gemma is ready"})
	s.Record(Event{Ts: 300, Type: "nodeLost", Severity: Error, Profile: "devstral", Nodes: []string{"ps5-2"}, SessionID: "s2", Summary: "Node ps5-2 was lost"})
	s.Record(Event{Ts: 400, Type: "requestRefused", Severity: Warn, Profile: "devstral", Summary: "Request refused: backoff"})
	cases := []struct {
		name string
		q    Query
		want []int64 // newest first
	}{
		{"all", Query{}, []int64{400, 300, 200, 100}},
		{"asc", Query{Asc: true}, []int64{100, 200, 300, 400}},
		{"since", Query{Since: 200}, []int64{400, 300, 200}},
		{"until", Query{Until: 200}, []int64{200, 100}},
		{"type list", Query{Types: []string{"nodeLost", "loadStarted"}}, []int64{300, 100}},
		{"min warn", Query{MinSeverity: Warn}, []int64{400, 300}},
		{"min error", Query{MinSeverity: Error}, []int64{300}},
		{"node", Query{Node: "ps5-2"}, []int64{300, 100}},
		{"profile", Query{Profile: "devstral"}, []int64{400, 300}},
		{"session", Query{SessionID: "s1"}, []int64{200, 100}},
		{"text", Query{Text: "READY"}, []int64{200}},
		{"limit", Query{Limit: 2}, []int64{400, 300}},
		{"combined", Query{Profile: "gemma", MinSeverity: Info, Node: "ps5-2"}, []int64{100}},
	}
	for _, c := range cases {
		r := s.Query(c.q)
		var got []int64
		for _, e := range r.Events {
			got = append(got, e.Ts)
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if r.History != "on" || r.OldestTs != 100 {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
	if r := s.Query(Query{Limit: 2}); !r.Truncated || r.TotalMatching != 4 {
		t.Errorf("limit: %+v", r)
	}
	if r := s.Query(Query{Limit: 99999}); len(r.Events) != 4 || (Query{Limit: 99999}).limit() != MaxLimit {
		t.Errorf("max limit: %+v", r)
	}
}

// With a small window, a query for old events reads the files and stops at the limit.
func TestQueryBeyondTheWindowReadsFilesAndStopsAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	s, _ := openT(t, dir, func(o *Options) { o.MaxFileBytes = 1000; o.KeepFiles = 100 })
	for i := 0; i < 100; i++ {
		sev := Info
		if i%10 == 0 {
			sev = Error
		}
		s.Record(ev(int64(i+1), "x", sev, fmt.Sprintf("n%03d", i)))
	}
	s.Close()
	s2, _ := openT(t, dir, func(o *Options) { o.MaxFileBytes = 1000; o.KeepFiles = 100; o.MemoryEvents = 20 })
	// Errors only, far beyond the 20-event window: ten of them over the whole history.
	r := s2.Query(Query{MinSeverity: Error, Limit: 4})
	if len(r.Events) != 4 || r.Events[0].Summary != "n090" || r.Events[3].Summary != "n060" || !r.Truncated {
		t.Fatalf("%+v", r)
	}
	r = s2.Query(Query{MinSeverity: Error, Limit: 100, Asc: true})
	if len(r.Events) != 10 || r.Events[0].Summary != "n000" || r.Events[9].Summary != "n090" {
		t.Fatalf("asc %+v", r)
	}
	r = s2.Query(Query{MinSeverity: Error, Limit: 100})
	if len(r.Events) != 10 || r.TotalMatching != 10 || r.Truncated {
		t.Fatalf("all %d total %d truncated %v", len(r.Events), r.TotalMatching, r.Truncated)
	}
	// A query inside the window does not need the files at all.
	r = s2.Query(Query{Since: 90})
	if len(r.Events) != 11 {
		t.Fatalf("%d", len(r.Events))
	}
	if r.OldestTs != 1 {
		t.Fatalf("oldest_ts %d, want the oldest in the files (1)", r.OldestTs)
	}
}

func TestExportIsTheMatchingLinesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UnixMilli()
	s, _ := openT(t, dir, func(o *Options) { o.MemoryEvents = 5; o.MaxFileBytes = 800 })
	for i := 0; i < 20; i++ {
		s.Record(ev(base+int64(i), "x", Info, fmt.Sprintf("n%02d", i)))
	}
	s.Flush()
	var b bytes.Buffer
	if err := s.Export(&b, base+5, base+12); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 8 {
		t.Fatalf("%d lines: %q", len(lines), lines)
	}
	for i, l := range lines {
		var e Event
		if err := json.Unmarshal([]byte(l), &e); err != nil || e.Ts != base+int64(5+i) {
			t.Fatalf("line %d %q: %v", i, l, err)
		}
	}
}

func TestSummary(t *testing.T) {
	s, _ := openT(t, t.TempDir())
	load := func(ts int64, prof, outcome string, secs, mbs float64) {
		sev := Info
		if outcome == "failed" {
			sev = Error
		}
		s.Record(Event{Ts: ts, Type: TLoadFinished, Severity: sev, Profile: prof, Summary: "x",
			Data: map[string]any{"outcome": outcome, "load_s": secs, "avg_mb_s": mbs}})
	}
	load(10, "gemma", "ready", 60, 100)
	load(20, "gemma", "ready", 80, 80)
	load(30, "gemma", "ready", 70, 90)
	load(40, "gemma", "failed", 12, 0)
	load(50, "devstral", "cancelled", 5, 0)
	s.Record(Event{Ts: 60, Type: TWorkerHealth, Severity: Error, Nodes: []string{"ps5-1"}, Summary: "hung", Data: map[string]any{"state": "hung"}})
	s.Record(Event{Ts: 61, Type: TWorkerHealth, Severity: Info, Nodes: []string{"ps5-1"}, Summary: "ok", Data: map[string]any{"state": "ok"}})
	s.Record(Event{Ts: 70, Type: TNodeLost, Severity: Error, Nodes: []string{"ps5-1"}, Summary: "lost"})
	s.Record(Event{Ts: 80, Type: TLoadLowMemory, Severity: Error, Nodes: []string{"ps5-2"}, Summary: "low"})
	sum := s.Summarize(0, 0)
	if sum.Loads.Ready != 3 || sum.Loads.Failed != 1 || sum.Loads.Cancelled != 1 || sum.Events != 9 {
		t.Fatalf("%+v", sum)
	}
	if sum.ByType[TLoadFinished] != 5 || sum.BySeverity[Error] != 4 {
		t.Fatalf("%+v %+v", sum.ByType, sum.BySeverity)
	}
	var g ProfileSummary
	for _, p := range sum.Profiles {
		if p.Profile == "gemma" {
			g = p
		}
	}
	if g.Loads != 4 || g.Ready != 3 || g.MinS != 60 || g.MedianS != 70 || g.MaxS != 80 || g.AvgMBs != 90 || g.Failed != 1 {
		t.Fatalf("%+v", g)
	}
	want := map[string][3]int{"ps5-1": {1, 1, 0}, "ps5-2": {0, 0, 1}}
	if len(sum.Nodes) != 2 {
		t.Fatalf("%+v", sum.Nodes)
	}
	for _, n := range sum.Nodes {
		w := want[n.Node]
		if n.Hung != w[0] || n.Lost != w[1] || n.LowMemory != w[2] {
			t.Fatalf("%+v", n)
		}
	}
	if len(sum.Incidents) != 4 || sum.Incidents[0].Ts != 80 || sum.Incidents[3].Ts != 40 {
		t.Fatalf("incidents %+v", sum.Incidents)
	}
	if got := s.Summarize(45, 65); got.Events != 3 || got.Loads.Ready != 0 {
		t.Fatalf("%+v", got)
	}
}
