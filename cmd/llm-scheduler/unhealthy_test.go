package main

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	_ "modernc.org/sqlite"
)

// startOn starts the scheduler on data, waits for its listen
// address, and returns it with a stop function.
func startOn(t *testing.T, data string) (addr string, logs *syncBuf, stop func() int) {
	t.Helper()
	logs = &syncBuf{}
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	boot := config.Bootstrap{Listen: "127.0.0.1:0", DataDir: data, ModelsDir: t.TempDir()}
	go func() { done <- serveUntil(boot, slog.New(slog.NewTextHandler(logs, nil)), sigs) }()
	re := regexp.MustCompile(`listen=(127\.0\.0\.1:\d+)`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if m := re.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
			break
		}
		select {
		case code := <-done:
			t.Fatalf("stopped (exit %d) before listening:\n%s", code, logs.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("not listening within 30 s:\n%s", logs.String())
		}
	}
	return addr, logs, func() int {
		sigs <- os.Interrupt
		select {
		case c := <-done:
			return c
		case <-time.After(time.Minute):
			t.Fatalf("did not stop:\n%s", logs.String())
			return -1
		}
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// A database a newer version migrated (schema one higher than this build
// knows): the scheduler does not come up healthy with an empty
// configuration. It stays up, /health answers 503 with the reason, every
// other route answers 503, and it loads nothing. Stopping it is clean.
func TestNewerDatabaseIsUnhealthyNotEmpty(t *testing.T) {
	data := t.TempDir()
	st, err := bench.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	have, _ := st.SchemaVersion()
	st.Close()
	db, err := sql.Open("sqlite", filepath.Join(data, bench.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?, 'from a newer version', 0)`, have+1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	addr, logs, stop := startOn(t, data)
	code, body := get(t, "http://"+addr+"/health")
	want := "database is newer than this version: " + itoa(have+1) + " > " + itoa(have)
	if code != 503 || !strings.Contains(body, want) {
		t.Fatalf("/health %d %q, want 503 with %q", code, body, want)
	}
	for _, p := range []string{"/api/config", "/api/profiles", "/v1/models", "/api/system"} {
		if code, body := get(t, "http://"+addr+p); code != 503 || !strings.Contains(body, want) {
			t.Fatalf("%s %d %q", p, code, body)
		}
	}
	r, err := http.Post("http://"+addr+"/api/config/profiles", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatalf("a write answered %d", r.StatusCode)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), want) {
		t.Fatalf("the reason is not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "llm-scheduler started") || strings.Contains(logs.String(), "built-in defaults") {
		t.Fatalf("started anyway:\n%s", logs.String())
	}
	if c := stop(); c != 1 {
		t.Fatalf("exit %d, want 1 (it never served)", c)
	}
	// the database is left as it was
	st2, err := bench.OpenStore(data)
	if err == nil {
		st2.Close()
		t.Fatal("the newer database was changed")
	}
}

// A database that cannot be opened for another reason (here: the file is
// not a database) is unhealthy the same way, with its reason.
func TestUnreadableDatabaseIsUnhealthy(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, bench.DBFile), []byte("this is not a database, it is plain text and long enough to be read"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, _, stop := startOn(t, data)
	code, body := get(t, "http://"+addr+"/health")
	if code != 503 || !strings.Contains(body, "the database cannot be opened") {
		t.Fatalf("/health %d %q", code, body)
	}
	stop()
}

// No database file yet (first start): it starts, healthy, unconfigured.
func TestFirstStartIsHealthy(t *testing.T) {
	addr, logs, stop := startOn(t, filepath.Join(t.TempDir(), "data"))
	for !strings.Contains(logs.String(), "llm-scheduler started") {
		time.Sleep(20 * time.Millisecond)
	}
	if code, body := get(t, "http://"+addr+"/health"); code != 200 {
		t.Fatalf("/health %d %q", code, body)
	}
	if c := stop(); c != 0 {
		t.Fatalf("exit %d", c)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// The readiness round trip for a document: a newer version stored a profile
// field this version does not know. This version starts healthy with every
// profile (not revision 0 and no profiles), warns once, and GET /api/system
// shows the field.
func TestStoredFieldFromANewerVersionKeepsTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	data, models := filepath.Join(dir, "data"), filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	file := filepath.Join(dir, "seed.yaml")
	os.WriteFile(file, []byte("listen: \"127.0.0.1:0\"\nlocal_node: host-1\ndefaults: {membership: agents}\nprofiles:\n  qwen: {model: qwen.gguf, ctx: 4096, needs_mib: 3000}\n  gemma: {model: gemma.gguf, ctx: 2048, needs_mib: 3000}\n"), 0o644)
	var out, errb strings.Builder
	if code := realMain([]string{"import-config", "--data-dir", data, "--models-dir", models, "--no-model-check", file}, &out, &errb); code != 0 {
		t.Fatalf("import: %d %s", code, errb.String())
	}
	db, err := sql.Open("sqlite", filepath.Join(data, bench.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE config_revisions SET doc = replace(replace(doc, '"model":"qwen.gguf"', '"model":"qwen.gguf","x_field_from_the_future":"from a newer version"'), '"settings":{', '"x_top_from_the_future":{"a":1},"settings":{"x_setting_from_the_future":true,')`)
	if n, _ := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("edit: %v %d", err, n)
	}
	db.Close()

	addr, logs, stop := startOn(t, data)
	for !strings.Contains(logs.String(), "llm-scheduler started") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "profiles=2") || strings.Count(logs.String(), "field=profiles.*.x_field_from_the_future") != 1 || strings.Count(logs.String(), "field=settings.x_setting_from_the_future") != 1 || strings.Count(logs.String(), "field=x_top_from_the_future") != 1 {
		t.Fatalf("not started with both profiles and one warning:\n%s", logs.String())
	}
	if code, _ := get(t, "http://"+addr+"/health"); code != 200 {
		t.Fatalf("/health %d", code)
	}
	if code, body := get(t, "http://"+addr+"/api/system"); code != 200 || !strings.Contains(body, `"unknown_fields":["profiles.*.x_field_from_the_future","settings.x_setting_from_the_future","x_top_from_the_future"]`) {
		t.Fatalf("/api/system %d %s", code, body)
	}
	stop()
}

// A stored configuration this version finds invalid is not replaced by an
// empty one either: unhealthy, with the reason.
func TestInvalidStoredConfigurationIsUnhealthy(t *testing.T) {
	dir := t.TempDir()
	data, models := filepath.Join(dir, "data"), filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	file := filepath.Join(dir, "seed.yaml")
	os.WriteFile(file, []byte("listen: \"127.0.0.1:0\"\nprofiles:\n  qwen: {model: qwen.gguf, ctx: 4096, needs_mib: 3000}\n"), 0o644)
	var out, errb strings.Builder
	if code := realMain([]string{"import-config", "--data-dir", data, "--models-dir", models, "--no-model-check", file}, &out, &errb); code != 0 {
		t.Fatalf("import: %d %s", code, errb.String())
	}
	db, _ := sql.Open("sqlite", filepath.Join(data, bench.DBFile))
	if _, err := db.Exec(`UPDATE config_revisions SET doc = replace(doc, '"ctx":4096', '"ctx":-1')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	addr, _, stop := startOn(t, data)
	if code, body := get(t, "http://"+addr+"/health"); code != 503 || !strings.Contains(body, "is invalid") {
		t.Fatalf("/health %d %q", code, body)
	}
	stop()
}
