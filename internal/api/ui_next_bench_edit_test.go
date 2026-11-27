package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// The editors (board bench-editors/editors.html, five states) on the
// --bench editors fixture: the start page, the benchmark editor drawn from
// the server's schema, the save with its comment and the server's refusal
// ("Not saved." and the sentences verbatim, the parts marked), the group
// editor with each entry's dry read, Leave without saving, Duplicate, and a
// run's "definition changed since this run".

// The fixture's schema is the server's: GET /api/bench/schema as
// benchspec.Schema() answers it (POTLUCK_WRITE_FIXTURES=1 writes it).
func TestUIBenchEditorsFixtureSchemaIsTheServers(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(here), "..", "..", "scripts", "ui-fixture", "bench-schema.json")
	want, err := json.MarshalIndent(benchspec.Schema(), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("POTLUCK_WRITE_FIXTURES") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from benchspec.Schema(): run this test with POTLUCK_WRITE_FIXTURES=1", path)
	}
}

// fixtureSend asks the fixture server a POST or PUT (a save).
func fixtureSend(t *testing.T, base, method, path, body string) any {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s %s: %s", method, path, b)
	}
	if resp.StatusCode >= 300 {
		return map[string]any{"$status": resp.StatusCode, "$body": v}
	}
	return v
}

// benchEditors: the GETs of the editors fixture, the plans, the per-entry
// dry reads, and the saves by their body.
func benchEditors(t *testing.T) map[string]any {
	t.Helper()
	f := benchRunCancel(t, "editors")
	get, base := fixtureStart(t, "--bench", "editors")
	for _, p := range []string{"/api/bench/schema", "/api/bench/task-sets", "/api/profiles", "/api/bench/benchmarks/qwen4b-2con", "/api/bench/groups/s3a", "/api/bench/groups/smoke"} {
		var v any
		get(p, &v)
		f[p] = v
	}
	f["/api/profiles/test"] = map[string]any{"profiles": []any{}}
	plan := func(model string) any { return fixtureAsk(t, base, "/api/plan", `{"model":"`+model+`"}`) }
	f["POST /api/plan"] = map[string]any{"$match": []any{
		[]any{`test-s3-qwen4b-3con`, plan("test-s3-qwen4b-3con")}, []any{`test-s1-qwen4b-host`, plan("test-s1-qwen4b-host")}, []any{`.`, plan("test-s3-qwen4b-2con")}}}
	for _, id := range []string{"qwen4b-1con", "qwen4b-2con", "qwen4b-3con", "qwen4b-4con", "qwen4b-5con", "host-gpu-alone"} {
		p := "/api/bench/benchmarks/" + id + "/run"
		f["POST "+p] = fixtureAsk(t, base, p+"?dry_run=1", `{}`)
	}
	for _, id := range []string{"s1-ref", "smoke"} {
		p := "/api/bench/groups/" + id + "/run"
		f["POST "+p] = fixtureAsk(t, base, p+"?dry_run=1", `{}`)
	}
	send := func(m, p, body string) any { return fixtureSend(t, base, m, p, body) }
	// a new benchmark: a prompt left empty is refused; else it is saved
	f["POST /api/bench/benchmarks"] = map[string]any{"$match": []any{
		[]any{`"prompt":""`, send("POST", "/api/bench/benchmarks", `{"id":"x","profile":"p","phases":[{"type":"load"},{"type":"warmup","count":1},{"type":"requests","requests":[{"name":"explain","prompt":""}]},{"type":"unload"}],"comment":"x"}`)},
		[]any{`.`, send("POST", "/api/bench/benchmarks", `{"id":"qwen-new","name":"Qwen new","profile":"test-s3-qwen4b-3con","phases":[{"type":"load"}],"comment":"x"}`)}}}
	f["PUT /api/bench/benchmarks/qwen4b-2con"] = send("PUT", "/api/bench/benchmarks/qwen4b-2con", `{"id":"qwen4b-2con","profile":"test-s3-qwen4b-2con","phases":[{"type":"load"}],"comment":"x"}`)
	f["PUT /api/bench/groups/s3a"] = map[string]any{"$match": []any{
		[]any{`"nope"`, send("PUT", "/api/bench/groups/s3a", `{"id":"s3a","benchmarks":["nope"],"comment":"x"}`)},
		[]any{`.`, send("PUT", "/api/bench/groups/s3a", `{"id":"s3a","benchmarks":["qwen4b-1con"],"comment":"x"}`)}}}
	f["POST /api/bench/groups"] = send("POST", "/api/bench/groups", `{"id":"s3a-copy","benchmarks":["qwen4b-1con"],"comment":"x"}`)
	return f
}
