package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The test binary doubles as a fake scheduler: started with GATE_FAKE_SCHED
// set it serves the few endpoints the gate reads, so a test can make a build
// that loses a write or changes a plan, and see the gate say so. Its
// behaviour is chosen by the environment and by its own file name.
//
//	GATE_FAKE_SCHED=memory  configuration kept in memory only: a restart loses
//	                        every acknowledged write
//	GATE_FAKE_SCHED=file    configuration in <data-dir>/fake.json, replaced
//	                        whole (write a temporary file, rename)
//	GATE_FAKE_LOG_ERROR=1   writes one level=ERROR line at start
//
// A copy of the binary named new* adds 512 MiB to every will_take of the
// plans and marks the data folder (a migration); a binary named old* that
// finds the mark reports its needs as estimated.
func TestMain(m *testing.M) {
	if mode := os.Getenv("GATE_FAKE_SCHED"); mode != "" {
		os.Exit(fakeScheduler(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

// built returns the real llm-scheduler and potluck-fake-agents binaries,
// built once per test run.
func built(t *testing.T) (sched, agents string) {
	t.Helper()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "potluck-gate-test-")
		if buildErr != nil {
			return
		}
		for _, p := range []string{"llm-scheduler", "potluck-fake-agents"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, p+exe), "./cmd/"+p)
			cmd.Dir = filepath.Join("..", "..")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("building %s: %v\n%s", p, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(buildDir, "llm-scheduler"+exe), filepath.Join(buildDir, "potluck-fake-agents"+exe)
}

// fakeBin is a copy of this test binary named name (old, new, ...), to run
// as a fake scheduler.
func fakeBin(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

// fakeScheduler is the fake: GET /health, the configuration of profiles
// (GET, POST, PUT, DELETE), the three listings, the needs and /api/status.
func fakeScheduler(mode string, args []string) int {
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	listen := fs.String("listen", "", "")
	data := fs.String("data-dir", "", "")
	fs.String("models-dir", "", "")
	fs.String("programs-dir", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Getenv("GATE_FAKE_LOG_ERROR") != "" {
		fmt.Println(`time=2026-01-01T00:00:00Z level=ERROR msg="a fake error"`)
	}
	name := strings.ToLower(filepath.Base(os.Args[0]))
	isNew, isOld := strings.HasPrefix(name, "new"), strings.HasPrefix(name, "old")
	mark := filepath.Join(*data, "migrated-by-new")
	if isNew {
		os.WriteFile(mark, []byte("1"), 0o644)
	}
	_, migrated := os.Stat(mark)
	store := filepath.Join(*data, "fake.json")
	type state struct {
		Revision int                        `json:"revision"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	var mu sync.Mutex
	st := state{Revision: 1, Profiles: map[string]json.RawMessage{"alpha": json.RawMessage(`{"model":"alpha.gguf","ctx":4096}`)}}
	if mode == "file" {
		if b, err := os.ReadFile(store); err == nil {
			json.Unmarshal(b, &st)
		}
	}
	save := func() {
		if mode != "file" {
			return
		}
		b, _ := json.Marshal(st)
		os.WriteFile(store+".tmp", b, 0o644)
		os.Rename(store+".tmp", store)
	}
	ans := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /api/config/profiles", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		ans(w, st)
	})
	write := func(w http.ResponseWriter, id string, spec json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		if spec == nil {
			delete(st.Profiles, id)
		} else {
			st.Profiles[id] = spec
		}
		st.Revision++
		save()
		ans(w, map[string]any{"revision": st.Revision})
	}
	m.HandleFunc("POST /api/config/profiles", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["id"].(string)
		delete(body, "id")
		b, _ := json.Marshal(body)
		write(w, id, b)
	})
	m.HandleFunc("PUT /api/config/profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		write(w, r.PathValue("id"), b)
	})
	m.HandleFunc("DELETE /api/config/profiles/{id}", func(w http.ResponseWriter, r *http.Request) { write(w, r.PathValue("id"), nil) })
	entries := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for id := range st.Profiles {
			take := 4000
			if isNew {
				take += 512
			}
			basis := "measured"
			if isOld && migrated == nil {
				basis = "estimated"
			}
			out = append(out, map[string]any{"id": id, "needs_mib": 4000, "needs_basis": basis, "plan": map[string]any{
				"action": "start", "need_mib": 4000, "memory": map[string]any{"nodes": []any{
					map[string]any{"node": "node-b", "will_take_mib": take, "basis": "measured", "checked_at": "now"},
					map[string]any{"node": "node-a", "will_take_mib": take, "basis": "measured", "checked_at": "now"}}}}})
		}
		return out
	}
	m.HandleFunc("GET /api/profiles", func(w http.ResponseWriter, r *http.Request) { ans(w, map[string]any{"profiles": entries()}) })
	m.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		ans(w, map[string]any{"models": []any{map[string]any{"key": "alpha.gguf", "profiles": entries()}}})
	})
	m.HandleFunc("GET /api/needs", func(w http.ResponseWriter, r *http.Request) { ans(w, map[string]any{"needs": []any{}}) })
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { ans(w, map[string]any{"agents": []any{}}) })
	if err := http.ListenAndServe(*listen, m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// hashDir is a digest of every file under dir, to show a source folder was
// not written. A database's -shm file is left out: it is SQLite's shared
// index of the write-ahead log, which every reader of a database in WAL mode
// updates (the live scheduler's own readers do too); the database and its
// log are compared.
func hashDir(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".db-shm") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s %d\n", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--help"}, []string{"potluck-gate crash", "potluck-gate plans"}},
		{[]string{"crash", "--help"}, []string{"It asserts:", "-kills", "-allow-log", "-dry-run"}},
		{[]string{"plans", "--help"}, []string{"the way back", "-fail-on", "-nodes", "-dry-run"}},
	} {
		code, out, _ := runCLI(tc.args...)
		if code != 0 {
			t.Errorf("%v: exit %d", tc.args, code)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: the help has no %q:\n%s", tc.args, w, out)
			}
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"crash"},
		{"crash", "--scheduler", "x", "--data", "d"},
		{"crash", "--scheduler", "image:", "--data", "d", "--models", "m"},
		{"crash", "--scheduler", "image:x", "--data", "d", "--models", "m", "--cycles", "3", "--kills", "3"},
		{"plans", "--old", "a", "--data", "d", "--models", "m"},
		{"plans", "--old", "image:a", "--new", "image:b", "--data", "d", "--models", "m", "--nodes", "n.json"},
		{"plans", "--old", "image:a", "--new", "image:b", "--data", "d", "--models", "m", "--fail-on", "everything"},
	} {
		if code, _, _ := runCLI(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestDryRunsStartNothing(t *testing.T) {
	data, models := t.TempDir(), t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	code, out, errOut := runCLI("crash", "--dry-run", "--scheduler", "image:candidate", "--data", data, "--models", models,
		"--work", work, "--seed", "7", "--listen", "127.0.0.1:20999")
	if code != 0 || !strings.Contains(out, "dry run: start docker run --rm") || !strings.Contains(out, "--listen 127.0.0.1:20999 --data-dir /gate/data") {
		t.Fatalf("crash dry run: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = runCLI("plans", "--dry-run", "--old", "image:live", "--new", "image:candidate", "--data", data, "--models", models, "--work", work)
	if code != 0 || strings.Count(out, ": start image:") != 4 || !strings.Contains(out, "way back") {
		t.Fatalf("plans dry run: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(work); err == nil {
		t.Fatal("a dry run made the work folder")
	}
}
