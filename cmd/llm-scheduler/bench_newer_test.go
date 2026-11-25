package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// importSeed makes a database with two profiles (import-config), as a
// running install has, and returns the data directory.
func importSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data, models := filepath.Join(dir, "data"), filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	file := filepath.Join(dir, "seed.yaml")
	os.WriteFile(file, []byte("listen: \"127.0.0.1:0\"\nlocal_node: host-1\ndefaults: {membership: agents}\nprofiles:\n  qwen: {model: qwen.gguf, ctx: 4096, needs_mib: 3000}\n  gemma: {model: gemma.gguf, ctx: 2048, needs_mib: 3000}\n"), 0o644)
	var out, errb strings.Builder
	if code := realMain([]string{"import-config", "--data-dir", data, "--models-dir", models, "--no-model-check", file}, &out, &errb); code != 0 {
		t.Fatalf("import: %d %s", code, errb.String())
	}
	return data
}

func openDB(t *testing.T, data string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(data, bench.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// benchTables are the tables the bench migrations create (and migrations):
// read from a database made by the bench store alone.
func benchTables(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	st, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	db := openDB(t, dir)
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	if len(out) < 5 {
		t.Fatalf("bench tables %v", out)
	}
	return out
}

// dumpBench is every bench table's definition and rows, as text.
func dumpBench(t *testing.T, data string, tables []string) string {
	t.Helper()
	db := openDB(t, data)
	defer db.Close()
	var b strings.Builder
	for _, tb := range tables {
		var def sql.NullString
		db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, tb).Scan(&def)
		fmt.Fprintf(&b, "== %s\n%s\n", tb, def.String)
		rows, err := db.Query(`SELECT * FROM "` + tb + `"`)
		if err != nil {
			fmt.Fprintf(&b, "error %v\n", err)
			continue
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			lines = append(lines, fmt.Sprint(vals...))
		}
		rows.Close()
		sort.Strings(lines)
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return b.String()
}

func benchVersion(t *testing.T, data string) int {
	t.Helper()
	db := openDB(t, data)
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return r.StatusCode, sb.String()
}

func waitStarted(t *testing.T, logs *syncBuf) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(logs.String(), "llm-scheduler started") {
		if time.Now().After(deadline) {
			t.Fatalf("not started:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// F1: a database whose bench migrations a newer version moved past what
// this build knows switches off the benchmarks only. The scheduler starts
// healthy with its profiles; plans, requests, the configuration and the UI
// answer; every benchmark route (and MCP bench tool) answers 503 naming
// both versions; /api/health has one row saying so; one WARN at start;
// nothing of the bench tables is written. A build that knows the version
// (here: the row lowered back) has benchmarks again.
func TestNewerBenchTablesSwitchOffOnlyBenchmarks(t *testing.T) {
	data := importSeed(t)
	know := benchVersion(t, data)
	db := openDB(t, data)
	if _, err := db.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?, 'from a newer version', 0)`, know+1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	tables := benchTables(t)
	before := dumpBench(t, data, tables)

	addr, logs, stop := startOn(t, data)
	waitStarted(t, logs)
	base := "http://" + addr
	reason := fmt.Sprintf("benchmarks are off: this database's benchmark tables are at version %d, this version knows up to %d; start a version that knows %d or newer", know+1, know, know+1)
	if code, body := get(t, base+"/health"); code != 200 {
		t.Fatalf("/health %d %s", code, body)
	}
	for _, p := range []string{"/api/profiles", "/api/config", "/api/system", "/v1/models", "/ui/"} {
		if code, body := get(t, base+p); code != 200 {
			t.Fatalf("%s %d %s", p, code, body)
		}
	}
	if _, body := get(t, base+"/api/profiles"); !strings.Contains(body, "qwen") || !strings.Contains(body, "gemma") {
		t.Fatalf("profiles %s", body)
	}
	if code, body := post(t, base+"/api/plan", `{"model":"qwen"}`); code != 200 || !strings.Contains(body, "qwen") {
		t.Fatalf("plan %d %s", code, body)
	}
	// a request reaches the request path (an unknown model: 404, not 503)
	if code, body := post(t, base+"/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`); code != 404 {
		t.Fatalf("request %d %s", code, body)
	}
	for _, p := range []string{"/api/bench/state", "/api/bench/runs", "/api/bench/benchmarks", "/api/bench/suites", "/api/bench/trends?benchmark=x&metric=load_s", "/api/bench/external", "/api/bench/task-sets"} {
		code, body := get(t, base+p)
		if code != 503 || !strings.Contains(body, reason) {
			t.Fatalf("%s %d %s, want 503 with %q", p, code, body, reason)
		}
	}
	if code, body := get(t, base+"/api/bench/state"); !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("state %d %s", code, body)
	}
	if code, body := post(t, base+"/api/bench/benchmarks", `{"id":"b1","profile":"qwen","phases":[{"type":"load"}]}`); code != 503 || !strings.Contains(body, reason) {
		t.Fatalf("create %d %s", code, body)
	}
	if code, body := post(t, base+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_bench_state","arguments":{}}}`); code != 200 || !strings.Contains(body, "benchmarks are off") {
		t.Fatalf("mcp %d %s", code, body)
	}
	if _, body := get(t, base+"/api/health"); !strings.Contains(body, `"id":"benchmarks"`) || !strings.Contains(body, reason) {
		t.Fatalf("health %s", body)
	}
	if _, body := get(t, base+"/api/system"); !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("system bench %s", body)
	}
	if c := stop(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	if n := strings.Count(logs.String(), "level=WARN msg=\"benchmarks are off"); n != 1 {
		t.Fatalf("%d WARN lines:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "unhealthy") || strings.Contains(logs.String(), "not serving") {
		t.Fatalf("unhealthy:\n%s", logs.String())
	}
	if after := dumpBench(t, data, tables); after != before {
		t.Fatalf("the bench tables changed:\n--- before\n%s\n--- after\n%s", before, after)
	}

	// The build that knows the version: benchmarks are back.
	db = openDB(t, data)
	if _, err := db.Exec(`DELETE FROM migrations WHERE version = ?`, know+1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	addr, logs, stop = startOn(t, data)
	waitStarted(t, logs)
	if code, body := get(t, "http://"+addr+"/api/bench/state"); code != 200 {
		t.Fatalf("state after %d %s", code, body)
	}
	if _, body := get(t, "http://"+addr+"/api/health"); strings.Contains(body, `"id":"benchmarks"`) {
		t.Fatalf("health still has the row %s", body)
	}
	stop()
}

// Every other reason stays as it was: the configuration ledger ahead of
// this build (with or without the bench one ahead too) is unhealthy as a
// whole, with the configuration's reason.
func TestNewerConfigurationTablesStayUnhealthy(t *testing.T) {
	for _, benchToo := range []bool{false, true} {
		data := importSeed(t)
		know := benchVersion(t, data)
		db := openDB(t, data)
		var cfgHave int
		db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM cfg_migrations`).Scan(&cfgHave)
		if _, err := db.Exec(`INSERT INTO cfg_migrations (version, applied_at) VALUES (?, 0)`, cfgHave+1); err != nil {
			t.Fatal(err)
		}
		if benchToo {
			db.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?, 'from a newer version', 0)`, know+1)
		}
		db.Close()
		addr, logs, stop := startOn(t, data)
		want := fmt.Sprintf("the configuration tables are at version %d but this build knows %d", cfgHave+1, cfgHave)
		for _, p := range []string{"/health", "/api/profiles", "/api/bench/state"} {
			if code, body := get(t, "http://"+addr+p); code != 503 || !strings.Contains(body, want) {
				t.Fatalf("bench too %v: %s %d %q", benchToo, p, code, body)
			}
		}
		if c := stop(); c != 1 {
			t.Fatalf("exit %d:\n%s", c, logs.String())
		}
	}
}

// The model and access ledgers ahead of this build are left alone, as
// before: the scheduler starts healthy with benchmarks on.
func TestNewerModelAndAccessLedgersStillStart(t *testing.T) {
	data := importSeed(t)
	db := openDB(t, data)
	for _, tb := range []string{"model_migrations", "access_migrations"} {
		if _, err := db.Exec(`INSERT INTO ` + tb + ` (version, applied_at) VALUES (99, 0)`); err != nil {
			t.Fatal(tb, err)
		}
	}
	db.Close()
	addr, logs, stop := startOn(t, data)
	waitStarted(t, logs)
	for _, p := range []string{"/health", "/api/profiles", "/api/bench/state"} {
		if code, body := get(t, "http://"+addr+p); code != 200 {
			t.Fatalf("%s %d %s", p, code, body)
		}
	}
	if c := stop(); c != 0 {
		t.Fatalf("exit %d", c)
	}
}
