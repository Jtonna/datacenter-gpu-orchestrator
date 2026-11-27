package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/ggufgen"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/testutil/progtree"
)

func TestScanLog(t *testing.T) {
	log := strings.Join([]string{
		`time=t level=INFO msg="llm-scheduler started"`,
		`time=t level=ERROR msg="the program release is not served; agents will be refused"`,
		`time=t level=ERROR msg="a write failed"`,
		`{"time":"t","level":"ERROR","msg":"json handler"}`,
		`panic: runtime error: index out of range [3] with length 3`,
		`fatal error: concurrent map writes`,
		`time=t level=WARN msg="the word ERROR in a message is not an error line"`,
		"time=t level=ERROR msg=\"crlf\"\r",
	}, "\n")
	s := scanLog(log, []*regexp.Regexp{regexp.MustCompile(`program release is not served`)})
	if s.Panics != 2 || s.Errors != 3 || s.Allowed != 1 || len(s.Samples) != 5 {
		t.Fatalf("%+v", s)
	}
}

func TestNextOpTurnsCreateChangeDelete(t *testing.T) {
	cur := ""
	var methods []string
	for i := 0; i < 4; i++ {
		w := nextOp("g", "m.gguf", cur, true)
		if w.before != cur {
			t.Fatalf("step %d: before %q, want %q", i, w.before, cur)
		}
		methods = append(methods, w.method)
		cur = w.after
	}
	if got := strings.Join(methods, " "); got != "POST PUT DELETE POST" {
		t.Fatal(got)
	}
	// without the change, a cycle is the old test's pair; a changed profile left by a kill is deleted
	if w := nextOp("g", "m.gguf", nextOp("g", "m.gguf", "", false).after, false); w.method != "DELETE" {
		t.Fatal(w.method)
	}
	two := nextOp("g", "m.gguf", nextOp("g", "m.gguf", "", true).after, true).after
	if w := nextOp("g", "m.gguf", two, false); w.method != "DELETE" {
		t.Fatal(w.method)
	}
}

func TestCopyDirMakesACopyAndRefusesItsOwnInside(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "a.bin"), []byte("data"), 0o600)
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "a.bin")); string(b) != "data" {
		t.Fatalf("copy %q", b)
	}
	if err := copyDir(src, dst); err == nil {
		t.Fatal("copied into a folder that is not empty")
	}
	if err := copyDir(src, filepath.Join(src, "inside")); err == nil {
		t.Fatal("copied into a folder inside the source")
	}
}

// A dump keys memory rows by their node, leaves out timestamps, and a
// comparison lists each differing field once, with a profile missing on one
// side as one row.
func TestDumpAndCompare(t *testing.T) {
	old := []byte(`{"profiles":[
	 {"id":"a","needs_mib":4000,"needs_basis":"measured","plan":{"action":"start","need_mib":4000,"memory":{"nodes":[
	   {"node":"node-a","will_take_mib":4100,"basis":"measured","host_overhead_mib":1024,"host_overhead_source":"setting","host_overhead_measured_at":"2026-01-01"},
	   {"node":"node-b","will_take_mib":4100,"basis":"measured","host_overhead_mib":0}]}}},
	 {"id":"gone","plan":{"action":"reject","nodes":[]}}]}`)
	new := []byte(`{"profiles":[
	 {"id":"a","needs_mib":4000,"needs_basis":"measured","plan":{"action":"start","need_mib":4000,"memory":{"nodes":[
	   {"node":"node-b","will_take_mib":4100,"basis":"measured","host_overhead_mib":0},
	   {"node":"node-a","will_take_mib":4612,"basis":"measured","host_overhead_mib":1536,"host_overhead_source":"measured","host_overhead_measured_at":"2026-02-02"}]}}},
	 {"id":"added","plan":{"action":"start"}}]}`)
	ign := regexp.MustCompile(`(^|\.)[a-z_]*(_at|_at_ms)$`)
	a, err := dumpOf(old, ign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := dumpOf(new, ign)
	if err != nil {
		t.Fatal(err)
	}
	if a["a"]["plan.memory.nodes[node-a].will_take_mib"] != "4100" || a["gone"]["plan.nodes"] != "[]" || a["gone"]["needs_mib"] != "(absent)" {
		t.Fatalf("dump %v", a)
	}
	for k := range a["a"] {
		if strings.HasSuffix(k, "_at") {
			t.Fatalf("timestamp kept: %s", k)
		}
	}
	c := compare("old-new", "old-1", "new-1", a, b)
	var got []string
	for _, r := range c.Rows {
		got = append(got, r.ID+" "+r.Field+" "+r.Old+" "+r.New)
	}
	want := []string{
		"a plan.memory.nodes[node-a].host_overhead_mib 1024 1536",
		`a plan.memory.nodes[node-a].host_overhead_source "setting" "measured"`,
		"a plan.memory.nodes[node-a].will_take_mib 4100 4612",
		"added (profile) absent present",
		"gone (profile) present absent",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || c.Profiles != 3 {
		t.Fatalf("rows (%d profiles):\n%s\nwant:\n%s", c.Profiles, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := compare("same", "x", "y", a, a); c.Profiles != 0 || len(c.Rows) != 0 {
		t.Fatalf("a dump differs from itself: %+v", c)
	}
}

// crashData is a data folder of the fake scheduler (it holds nothing; the
// fake seeds one profile).
func fakeCrash(t *testing.T, mode string, extra ...string) (*crashResult, int, string) {
	t.Helper()
	data, models := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(data, "keep.txt"), []byte("source"), 0o644)
	before := hashDir(t, data)
	work := filepath.Join(t.TempDir(), "work")
	args := append([]string{"crash", "--scheduler", fakeBin(t, "sched"), "--data", data, "--models", models, "--work", work,
		"--cycles", "15", "--kills", "4", "--kill-window", "1ms", "--seed", "3", "--ready", "20s", "--env", "GATE_FAKE_SCHED=" + mode}, extra...)
	code, out, errOut := runCLI(args...)
	if hashDir(t, data) != before {
		t.Fatal("the source data folder was written")
	}
	b, err := os.ReadFile(filepath.Join(work, "crash.json"))
	if err != nil {
		t.Fatalf("no result: exit %d\n%s%s", code, out, errOut)
	}
	var res crashResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	return &res, code, out
}

// A build that stores each write whole passes; the line carries the figures.
func TestCrashPassesABuildThatKeepsItsWrites(t *testing.T) {
	res, code, out := fakeCrash(t, "file")
	if code != 0 || res.Result != "PASS" || res.Cycles != 15 || res.Kills != 4 || res.BadReads+res.BadWrites != 0 {
		t.Fatalf("exit %d %+v\n%s", code, res, out)
	}
	if res.ListingCounts["profiles"] != res.ListingCounts["config_profiles"] || res.ListingCounts["catalog_profiles"] != res.ListingCounts["profiles"] {
		t.Fatalf("listing counts %v", res.ListingCounts)
	}
	if res.Writes < 3*15-4 || res.Listings["list"] == 0 || res.Listings["models"] == 0 || res.Listings["needs"] == 0 {
		t.Fatalf("writes %d, readers' reads %v", res.Writes, res.Listings)
	}
	if !regexp.MustCompile(`crash: PASS cycles=15 writes=\d+ kills=4 listings\{list=\d+ models=\d+ needs=\d+\} bad_reads=0 bad_writes=0 counts\[.*config_profiles=\d+.*\] panics=0 errors=0 integrity=ok`).MatchString(out) {
		t.Fatalf("line %q", out)
	}
}

// A build that forgets acknowledged writes at a restart fails with bad
// writes; an ERROR line fails it too, unless the caller allows it.
func TestCrashFailsALostWriteAndAnErrorLine(t *testing.T) {
	res, code, _ := fakeCrash(t, "memory")
	if code != 1 || res.Result != "FAIL" || res.BadWrites == 0 || !strings.Contains(strings.Join(res.Problems, "\n"), "no acknowledged write made it") &&
		!strings.Contains(strings.Join(res.Problems, "\n"), "not stored whole") && !strings.Contains(strings.Join(res.Problems, "\n"), "the acknowledged writes say") {
		t.Fatalf("exit %d %+v", code, res)
	}
	res, code, _ = fakeCrash(t, "file", "--env", "GATE_FAKE_LOG_ERROR=1")
	if code != 1 || res.Log.Errors != 5 { // one per start: the first and four restarts
		t.Fatalf("an ERROR line: exit %d %+v", code, res.Log)
	}
	res, code, _ = fakeCrash(t, "file", "--env", "GATE_FAKE_LOG_ERROR=1", "--allow-log", "a fake error")
	if code != 0 || res.Log.Errors != 0 || res.Log.Allowed != 5 {
		t.Fatalf("an allowed ERROR line: exit %d %+v", code, res.Log)
	}
	res, code, _ = fakeCrash(t, "file", "--env", "GATE_FAKE_BAD_MODELS=1")
	if code != 1 || res.BadReads == 0 || !strings.Contains(strings.Join(res.Problems, "\n"), "reader /v1/models: HTTP 500") {
		t.Fatalf("a reader's bad answer: exit %d bad reads %d %v", code, res.BadReads, res.Problems)
	}
}

// The old test's shape: POST-and-DELETE pairs for a time, no kill, a read
// per cycle; a cycle is one pair, so the figures compare.
func TestCrashTheOldTestsShape(t *testing.T) {
	data, models := t.TempDir(), t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	code, out, errOut := runCLI("crash", "--scheduler", fakeBin(t, "sched"), "--data", data, "--models", models, "--work", work,
		"--duration", "2s", "--change=false", "--kills", "0", "--verify", "cycle", "--ready", "20s", "--env", "GATE_FAKE_SCHED=file")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var res crashResult
	b, _ := os.ReadFile(filepath.Join(work, "crash.json"))
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.Cycles < 5 || res.Writes != 2*res.Cycles || res.Kills != 0 || res.DurationS < 2 || res.DurationS > 10 {
		t.Fatalf("%+v", &res)
	}
}

// The plans of two builds: what NEW changes, and the way back. NEW adds 512
// MiB to every will_take and marks the data; OLD on the marked data reports
// another need basis, so the way back differs for OLD only.
func TestPlansComparesTwoBuildsAndTheWayBack(t *testing.T) {
	data, models := t.TempDir(), t.TempDir()
	before := hashDir(t, data)
	work := filepath.Join(t.TempDir(), "work")
	code, out, errOut := runCLI("plans", "--old", fakeBin(t, "old"), "--new", fakeBin(t, "new"), "--data", data, "--models", models,
		"--work", work, "--settle", "10s", "--ready", "20s", "--env", "GATE_FAKE_SCHED=file")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if hashDir(t, data) != before {
		t.Fatal("the source data folder was written")
	}
	var rep plansReport
	b, _ := os.ReadFile(filepath.Join(work, "plans.json"))
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OldNew.Profiles != 1 || len(rep.OldNew.Rows) != 2 || rep.OldNew.Rows[0].Field != "plan.memory.nodes[node-a].will_take_mib" ||
		rep.OldNew.Rows[0].Old != "4000" || rep.OldNew.Rows[0].New != "4512" {
		t.Fatalf("old-new %+v\n%s", rep.OldNew, out)
	}
	if rep.WayBackOld.Profiles != 1 || rep.WayBackOld.Rows[0].Field != "needs_basis" || rep.WayBackNew.Profiles != 0 {
		t.Fatalf("way back old %+v new %+v", rep.WayBackOld, rep.WayBackNew)
	}
	for _, f := range []string{"old-1.json", "new-1.json", "old-2.json", "new-2.json"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out, "plans: old-new 1 profiles differ | way back OLD 1 | way back NEW 0") {
		t.Fatalf("summary line missing:\n%s", out)
	}
	// the caller says what is expected
	code, _, _ = runCLI("plans", "--old", fakeBin(t, "old"), "--new", fakeBin(t, "new"), "--data", data, "--models", models,
		"--settle", "10s", "--ready", "20s", "--env", "GATE_FAKE_SCHED=file", "--fail-on", "way-back")
	if code != 1 {
		t.Fatalf("--fail-on way-back: exit %d", code)
	}
}

// realData is a data folder of the real scheduler: two profiles on generated
// model files, imported with import-config, and a program release.
func realData(t *testing.T, sched string) (data, models, programs string) {
	t.Helper()
	dir := t.TempDir()
	models = filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	if err := ggufgen.Write(filepath.Join(models, "alpha.gguf"), ggufgen.Spec{Blocks: 8, Context: 32768}); err != nil {
		t.Fatal(err)
	}
	if err := ggufgen.Write(filepath.Join(models, "beta.gguf"), ggufgen.Qwen35_4B()); err != nil {
		t.Fatal(err)
	}
	programs = progtree.Write(t, filepath.Join(dir, "programs"), progtree.Tree{Release: agentproto.Release, Llama: "a01665d30",
		Folders: map[string]map[string][]byte{
			"agent":              {"potluck-agent-linux-amd64": []byte("agent")},
			"linux-amd64-vulkan": {"llama-server": []byte("L"), "rpc-server": []byte("R")},
		}})
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte(`fleet: {local_node: gate-host}
defaults: {membership: agents, ttl: 0s}
tiers:
  - {match: {hostname: "node-*"}, priority: 1, headroom_mib: 300, shared_memory: true}
profiles:
  alpha:
    model: alpha.gguf
    ctx: 4096
    needs_mib: 3000
  beta:
    model: beta.gguf
    ctx: 8192
`), 0o644)
	data = filepath.Join(dir, "data")
	if out, err := exec.Command(sched, "import-config", "--data-dir", data, "--models-dir", models, cfg).CombinedOutput(); err != nil {
		t.Fatalf("import-config: %v\n%s", err, out)
	}
	return data, models, programs
}

// The real scheduler, killed at random moments of its writes, keeps every
// acknowledged write and never a half one.
func TestCrashOnTheRealScheduler(t *testing.T) {
	sched, _ := built(t)
	data, models, programs := realData(t, sched)
	before := hashDir(t, data)
	work := filepath.Join(t.TempDir(), "work")
	o := crashOpts{data: data, models: models, programs: programs, work: work, cycles: 12, kills: 3, seed: 5, slots: 9, change: true, verify: "write",
		readers: map[string]int{"list": 3, "models": 1, "needs": 1},
		window:  300 * time.Millisecond, ready: time.Minute,
		// the test release has no host program: the same-build check refuses loads, which this test does not make
		allow: []*regexp.Regexp{regexp.MustCompile(`same-build rule`)}}
	o.spec, _ = parseSpec(sched)
	res, err := runCrash(context.Background(), o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "PASS" || res.Kills != 3 || res.Writes < 3*12-3 || res.ListingCounts["profiles"] < 2 || res.Listings["list"] == 0 || res.Listings["models"] == 0 || len(res.Databases) == 0 || !dbsOK(res.Databases) {
		t.Fatalf("%s\n%+v\n%s", res.line(), res.Problems, res.Log.Samples)
	}
	if hashDir(t, data) != before {
		t.Fatal("the source data folder was written")
	}
}

// The real scheduler as OLD and NEW, with a fake fleet: the same build gives
// the same plans, both ways, and the memory rows of the fake nodes are in
// the dump.
func TestPlansOnTheRealSchedulerWithAFakeFleet(t *testing.T) {
	sched, agents := built(t)
	data, models, programs := realData(t, sched)
	nodes := filepath.Join(t.TempDir(), "nodes.json")
	os.WriteFile(nodes, []byte(`{"nodes":[
	 {"id":"02:00:00:00:00:01","name":"node-a","mem_total_mib":16000,"mem_available_mib":14000},
	 {"id":"02:00:00:00:00:02","name":"node-b","mem_total_mib":16000,"mem_available_mib":14000}]}`), 0o644)
	if _, err := freePort("127.0.0.2"); err != nil {
		t.Skipf("no second loopback address: %v", err)
	}
	work := filepath.Join(t.TempDir(), "work")
	o := plansOpts{data: data, models: models, programs: programs, nodes: nodes, fakeAgents: agents, addrBase: "127.0.0.2",
		work: work, settle: 30 * time.Second, ready: time.Minute,
		// the test release has no host program: the same-build check refuses loads, which this test does not make
		allow:  []*regexp.Regexp{regexp.MustCompile(`same-build rule`)},
		ignore: regexp.MustCompile(`(^|\.)[a-z_]*(_at|_at_ms)$`), print: 20}
	o.old, _ = parseSpec(sched)
	o.new = o.old
	var out strings.Builder
	rep, err := runPlans(context.Background(), o, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if rep.OldNew.Profiles != 0 || rep.WayBackOld.Profiles != 0 || rep.WayBackNew.Profiles != 0 || len(rep.Unsettled) != 0 {
		t.Fatalf("%+v\n%s", rep, out.String())
	}
	for _, name := range []string{"old-1", "new-1", "old-2", "new-2"} {
		r := rep.Runs[name]
		if r == nil || r.Version == "" || r.MemoryRows == 0 || r.Actions == nil || r.bad() || len(r.Databases) == 0 {
			t.Fatalf("run %s: %+v", name, r)
		}
		if !strings.Contains(out.String(), name+": ") || !strings.Contains(out.String(), "integrity scheduler.db ok") {
			t.Fatalf("no run line for %s:\n%s", name, out.String())
		}
	}
	var d dump
	b, _ := os.ReadFile(filepath.Join(work, "old-1.json"))
	json.Unmarshal(b, &d)
	if d["alpha"]["plan.action"] == "" || d["alpha"]["plan.memory.nodes[node-a].will_take_mib"] == "" {
		t.Fatalf("alpha's dump has no memory row of node-a: %v", d["alpha"])
	}
}

// A database written by a running scheduler is copied whole: the rows still
// in its write-ahead log are in the copy, and the copy passes its check.
func TestCopyDirCopiesALiveDatabaseWhole(t *testing.T) {
	src := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(src, "scheduler.db"))+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"CREATE TABLE schema_migrations (version INTEGER)", "INSERT INTO schema_migrations VALUES (1), (2)",
		"CREATE TABLE needs_shapes (k TEXT)", "INSERT INTO needs_shapes VALUES ('a'), ('b'), ('c')"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(filepath.Join(src, "scheduler.db-wal")); err != nil || fi.Size() == 0 {
		t.Fatalf("the test wants rows in the write-ahead log: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "scheduler.db-wal")); err == nil {
		t.Fatal("the write-ahead log was copied beside the database")
	}
	c := checkDB(filepath.Join(dst, "scheduler.db"))
	if c.Integrity != "ok" || strings.Join(c.Tables, ",") != "needs_shapes" || fmt.Sprint(c.Ledgers["schema_migrations"]) != "[1 2]" {
		t.Fatalf("%+v", c)
	}
	cp, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dst, "scheduler.db"))+"?mode=ro")
	defer cp.Close()
	var n int
	if err := cp.QueryRow("SELECT count(*) FROM needs_shapes").Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows in the copy: %d %v", n, err)
	}
}
