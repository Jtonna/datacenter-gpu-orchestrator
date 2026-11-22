package startup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

func TestParseShow(t *testing.T) {
	f := ParseShow([]byte("UserspaceTimestampMonotonic=4512345\nSoftRebootsCount=2\n"))
	if f.UserspaceStartS == nil || *f.UserspaceStartS != 4.512 || f.SoftReboots == nil || *f.SoftReboots != 2 {
		t.Fatalf("%+v", f)
	}
	// systemd older than 254: no soft-restart count. An unset timestamp is 0.
	f = ParseShow([]byte("UserspaceTimestampMonotonic=0\nSoftRebootsCount=\n"))
	if f.UserspaceStartS != nil || f.SoftReboots != nil {
		t.Fatalf("unset fields guessed: %+v", f)
	}
	f = ParseShow([]byte("garbage\nUserspaceTimestampMonotonic=abc\n"))
	if f.UserspaceStartS != nil || f.SoftReboots != nil {
		t.Fatalf("%+v", f)
	}
}

func TestExecSystemd(t *testing.T) {
	var got []string
	s := ExecSystemd{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return []byte("UserspaceTimestampMonotonic=3000000\n"), nil
	}}
	f, err := s.Show(context.Background())
	if err != nil || *f.UserspaceStartS != 3 || f.SoftReboots != nil {
		t.Fatalf("%+v %v", f, err)
	}
	if strings.Join(got, " ") != "systemctl show --property=UserspaceTimestampMonotonic --property=SoftRebootsCount" {
		t.Fatalf("ran %v", got)
	}
	// No systemd (a container, a machine without it): an error, nothing guessed.
	s.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not found") }
	if f, err := s.Show(context.Background()); err == nil || f.UserspaceStartS != nil {
		t.Fatalf("%+v %v", f, err)
	}
	s.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("Other=1\n"), nil }
	if _, err := s.Show(context.Background()); err == nil {
		t.Fatal("an answer without either property is not an error")
	}
}

func TestParseMarks(t *testing.T) {
	m := ParseMarks([]byte("boot_progress_start 2.10\nnetwork_ok 11.5\n# comment\n\nbad line here\nnegative -1\nnan NaN\n" +
		"payload_ok 30.25\nsome_future_mark 40\nnetwork_ok 12.0\nboot_ready 4"))
	want := map[string]float64{"boot_progress_start": 2.1, "network_ok": 12, "payload_ok": 30.25, "some_future_mark": 40, "boot_ready": 4}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("%v", m)
	}
	// A half-written last line is skipped.
	if m := ParseMarks([]byte("network_ok 11.5\ndashboard_u")); len(m) != 1 {
		t.Fatalf("%v", m)
	}
	// At most 32 names.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("m" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + " 1\n")
	}
	if m := ParseMarks([]byte(b.String())); len(m) != agentproto.StartupMaxExternalMarks {
		t.Fatalf("%d names", len(m))
	}
}

// The watcher: a missing file is normal, an atomically replaced file is read
// again, a huge file is ignored, and it stops after 5 minutes or 60 s
// without a change.
func TestWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "startup-marks")
	w := &Watcher{Path: path}
	at := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	if m, ch, done, warn := w.Poll(at(20)); m != nil || ch || done || warn != "" {
		t.Fatalf("missing file: %v %v %v %q", m, ch, done, warn)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	// Written by its owner: write to a temporary name, rename over.
	replace := func(text string) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	replace("network_ok 11.5\n")
	m, ch, done, _ := w.Poll(at(22))
	if !ch || done || m["network_ok"] != 11.5 {
		t.Fatalf("%v %v %v", m, ch, done)
	}
	if _, ch, _, _ := w.Poll(at(24)); ch {
		t.Fatal("unchanged file reported as changed")
	}
	replace("network_ok 11.5\npayload_ok 30\n")
	if m, ch, _, _ := w.Poll(at(70)); !ch || len(m) != 2 {
		t.Fatalf("%v %v", m, ch)
	}
	// Too large: ignored, said once, the last marks stay.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", agentproto.StartupMarksFileMaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, ch, _, warn := w.Poll(at(72)); ch || m != nil || warn == "" {
		t.Fatalf("huge file: %v %v %q", m, ch, warn)
	}
	if _, _, _, warn := w.Poll(at(74)); warn != "" {
		t.Fatal("the warning repeats")
	}
	// 60 s without a change: done, and nothing is read any more.
	if _, _, done, _ := w.Poll(at(131)); !done {
		t.Fatal("not done after 60 s without a change")
	}
	reads := 0
	w.Read = func(string) ([]byte, error) { reads++; return nil, nil }
	if _, _, done, _ := w.Poll(at(132)); !done || reads != 0 {
		t.Fatal("a stopped watcher reads")
	}
	// The file and its folder are still there: the watcher never deletes.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	// 5 minutes at most, even while the file keeps changing.
	n := 0
	w2 := &Watcher{Path: "x", Read: func(string) ([]byte, error) { n++; return []byte("m " + strconv.Itoa(n) + "\n"), nil }}
	var stopped bool
	for s := 0.0; s <= 400 && !stopped; s += 10 {
		_, _, stopped, _ = w2.Poll(at(s))
		if stopped && s < 300 {
			t.Fatalf("stopped at %v s", s)
		}
	}
	if !stopped {
		t.Fatal("never stopped")
	}
	// A file that goes away: an empty set of marks, once.
	w3 := &Watcher{Path: "x", Read: func(string) ([]byte, error) { return []byte("a 1\n"), nil }}
	w3.Poll(at(1))
	w3.Read = func(string) ([]byte, error) { return nil, nil }
	if m, ch, _, _ := w3.Poll(at(2)); !ch || len(m) != 0 {
		t.Fatalf("%v %v", m, ch)
	}
}

func TestRecorderMarksAndAcks(t *testing.T) {
	r := NewRecorder("b1", 20500*time.Millisecond)
	rep := r.Report()
	if rep.BootID != "b1" || rep.AgentStartS != 20.5 || rep.Marks["agent_start"] != 20.5 || rep.Count() != 1 {
		t.Fatalf("%+v", rep)
	}
	// Potluck acknowledges the registration's report.
	r.Answered(1, true)
	if r.ForHeartbeat() != nil {
		t.Fatal("an acknowledged report is sent again")
	}
	if !r.Mark("registered", 23123*time.Millisecond) || r.Mark("registered", 99*time.Second) {
		t.Fatal("a mark is recorded once")
	}
	hb := r.ForHeartbeat()
	if hb == nil || hb.Marks["registered"] != 23.123 {
		t.Fatalf("%+v", hb)
	}
	// A mark arrives before the answer: the answer acknowledges the older
	// report only, so the next heartbeat still carries it.
	r.Mark("programs_ready", 30*time.Second)
	r.Answered(2, false)
	if hb := r.ForHeartbeat(); hb == nil || hb.Count() != 3 {
		t.Fatalf("%+v", hb)
	}
	r.Answered(3, false)
	if r.Pending() || r.ForHeartbeat() != nil {
		t.Fatal("still pending after the last ack")
	}
	// A changed value in the marks file is a change too.
	r.SetExternal(map[string]float64{"network_ok": 11})
	if r.ForHeartbeat() == nil {
		t.Fatal("new external mark not sent")
	}
	r.Answered(4, false)
	r.SetExternal(map[string]float64{"network_ok": 11})
	if r.Pending() {
		t.Fatal("the same marks again are not a change")
	}
	r.SetExternal(map[string]float64{"network_ok": 12})
	if !r.Pending() {
		t.Fatal("a corrected value is not sent")
	}
	// An older Potluck answers without startup_ack: no more heartbeats carry
	// it until the next registration.
	r.Answered(0, false)
	if r.ForHeartbeat() != nil {
		t.Fatal("sent to a Potluck that takes no reports")
	}
	r.Report()
	r.Answered(4, true) // the registration's report is acknowledged whole
	if r.ForHeartbeat() != nil {
		t.Fatal("an acknowledged registration report is sent again")
	}
	r.Mark("first_stats", 40*time.Second)
	if r.ForHeartbeat() == nil {
		t.Fatal("after a registration that Potluck acknowledged, a new mark is not sent")
	}
	// The copy is the caller's.
	c := r.Report()
	c.Marks["x"] = 1
	if r.Has("x") {
		t.Fatal("the report shares its map")
	}
	// systemd facts.
	up, soft := 3.5, int64(0)
	r.SetSystemd(Facts{UserspaceStartS: &up, SoftReboots: &soft})
	if c := r.Report(); *c.UserspaceStartS != 3.5 || *c.SoftReboots != 0 {
		t.Fatalf("%+v", c)
	}
}

// Stale marks after a soft restart (/run survives it): marks earlier than
// the current userspace start are dropped, and a file whose
// boot_progress_start is earlier is ignored whole until a fresh one appears.
func TestWatcherDropsMarksOfAnEarlierUserspace(t *testing.T) {
	at := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	type tc struct {
		file string
		us   float64
		ok   bool
		want map[string]float64
	}
	for name, c := range map[string]tc{
		"stale whole file": {"boot_progress_start 1.95\nnetwork_ok 9.5\npayload_ok 18.2\n", 500, true, map[string]float64{}},
		"some marks older": {"boot_progress_start 501\nnetwork_ok 9.5\npayload_ok 520\n", 500, true,
			map[string]float64{"boot_progress_start": 501, "payload_ok": 520}},
		"userspace start unknown": {"boot_progress_start 1.95\nnetwork_ok 9.5\n", 0, false,
			map[string]float64{"boot_progress_start": 1.95, "network_ok": 9.5}},
		"full start": {"boot_progress_start 1.95\nnetwork_ok 9.5\n", 1.5, true,
			map[string]float64{"boot_progress_start": 1.95, "network_ok": 9.5}},
	} {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			w := &Watcher{Path: "x", Read: func(string) ([]byte, error) { return []byte(c.file), nil },
				UserspaceStart: func() (float64, bool) { return c.us, c.ok },
				Logger:         slog.New(slog.NewTextHandler(&logs, nil))}
			m, ch, _, _ := w.Poll(at(530))
			if !ch || !reflect.DeepEqual(m, c.want) {
				t.Fatalf("marks %v changed %v, want %v", m, ch, c.want)
			}
			stale := strings.Count(logs.String(), `level=INFO msg="start-up marks file is from an earlier start; ignored"`)
			if name == "stale whole file" {
				if stale != 1 || !strings.Contains(logs.String(), "boot_progress_start_s=1.95") || !strings.Contains(logs.String(), "userspace_start_s=500") {
					t.Fatalf("log\n%s", logs.String())
				}
			} else if stale != 0 {
				t.Fatalf("log\n%s", logs.String())
			}
		})
	}
}

func TestWatcherReadsAFreshFileAfterAStaleOne(t *testing.T) {
	at := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	file := "boot_progress_start 1.95\nnetwork_ok 9.5\n"
	var logs strings.Builder
	w := &Watcher{Path: "x", Read: func(string) ([]byte, error) { return []byte(file), nil },
		UserspaceStart: func() (float64, bool) { return 500, true },
		Logger:         slog.New(slog.NewTextHandler(&logs, nil))}
	if m, ch, _, _ := w.Poll(at(510)); !ch || len(m) != 0 {
		t.Fatalf("stale: %v %v", m, ch)
	}
	if _, ch, _, _ := w.Poll(at(512)); ch {
		t.Fatal("unchanged stale file reported again")
	}
	file = "boot_progress_start 540\n" // the writer started late
	if m, ch, _, _ := w.Poll(at(541)); !ch || m["boot_progress_start"] != 540 || len(m) != 1 {
		t.Fatalf("fresh: %v %v", m, ch)
	}
	file = "boot_progress_start 540\nnetwork_ok 548\n"
	if m, ch, _, _ := w.Poll(at(549)); !ch || len(m) != 2 {
		t.Fatalf("fresh, more: %v %v", m, ch)
	}
	if n := strings.Count(logs.String(), "earlier start"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
}

// The cut-off is the userspace start, and only when this boot had a soft
// restart (or the count is unknown): /run is new on a full boot.
func TestRecorderMarksCutoff(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		facts Facts
		us    float64
		ok    bool
	}{
		{Facts{UserspaceStartS: f(500), SoftReboots: i(1)}, 500, true},
		{Facts{UserspaceStartS: f(500)}, 500, true},
		{Facts{UserspaceStartS: f(1.5), SoftReboots: i(0)}, 0, false},
		{Facts{SoftReboots: i(2)}, 0, false},
		{Facts{}, 0, false},
	} {
		r := NewRecorder("b", time.Second)
		r.SetSystemd(c.facts)
		if us, ok := r.MarksCutoff(); us != c.us || ok != c.ok {
			t.Errorf("%+v: %v %v", c.facts, us, ok)
		}
	}
}
