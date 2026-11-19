package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/api"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/bench"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/benchspec"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/history"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// The seed definitions pass the real validators.
func TestSeedDefinitionsValid(t *testing.T) {
	ok := func(id string) bool { _, found := profileByID(id); return found }
	byID := map[string]*benchspec.Benchmark{}
	for _, e := range seedBenchmarks() {
		raw, _ := json.Marshal(e.spec)
		b, err := benchspec.ParseBenchmarkJSON(raw)
		if err != nil {
			t.Fatalf("%s: %v", e.spec["id"], err)
		}
		if err := b.Validate(ok); err != nil {
			t.Errorf("%s: %v", b.ID, err)
		}
		byID[b.ID] = b
	}
	for _, e := range seedSuites() {
		raw, _ := json.Marshal(e.spec)
		s, err := benchspec.ParseSuiteJSON(raw)
		if err != nil {
			t.Fatalf("%s: %v", e.spec["id"], err)
		}
		for _, it := range s.Benchmarks {
			if it.Ref != "" && byID[it.Ref] == nil {
				t.Errorf("suite %s names unknown benchmark %s", s.ID, it.Ref)
			}
		}
	}
}

// A real manager runs the nightly suite against the scripted scheduler: every
// step ends as expected, the refusal is refused, and a slower decode on the
// second run is a regression of that benchmark only, and a 22% slower load
// on the third is a regression that still meets its expectation.
func TestScriptedNightly(t *testing.T) {
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	for _, e := range seedBenchmarks() {
		raw, _ := json.Marshal(e.spec)
		b, _ := benchspec.ParseBenchmarkJSON(raw)
		if err := st.SaveBenchmark(b, "api", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range seedSuites() {
		raw, _ := json.Marshal(e.spec)
		s, _ := benchspec.ParseSuiteJSON(raw)
		if err := st.SaveSuite(s, "api", now); err != nil {
			t.Fatal(err)
		}
	}
	clock := &vclock{}
	clock.set(time.Now().Add(-48 * time.Hour))
	sa := newScriptedAPI(clock)
	defer sa.stop()
	m, err := bench.NewManager(bench.ManagerOptions{Store: st, API: sa, Version: devVersion, Logger: quiet, Now: clock.Now,
		ProfileOK: func(id string) bool { _, ok := profileByID(id); return ok },
		IdlePoll:  20 * time.Millisecond, LoadPoll: 20 * time.Millisecond, GoneTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m.SetModelFile(modelFileOf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())
	run := func(slow, slowLoad map[string]float64) *bench.Run {
		sa.mu.Lock()
		sa.factor, sa.loadBy = slow, slowLoad
		sa.mu.Unlock()
		r, err := m.Enqueue("nightly", bench.EnqueueOptions{Trigger: "schedule"})
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(30 * time.Millisecond) {
			got, err := st.GetRun(r.ID)
			if err == nil && bench.IsFinal(got.State) {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s did not finish", r.ID)
			}
		}
	}
	first := run(map[string]float64{}, nil)
	if first.State != bench.RunCompleted || len(first.Steps) != 8 {
		t.Fatalf("first run: %s, %d steps, %s", first.State, len(first.Steps), first.Error)
	}
	for _, s := range first.Steps {
		want := bench.OutcomeReady
		if s.BenchmarkID == "llama70b-refused" {
			want = bench.OutcomeRefused
		}
		if s.Outcome != want || s.Expect == nil || !s.Expect.Pass {
			b, _ := json.Marshal(s.Expect)
			t.Errorf("%s: outcome %s, expect %s", s.BenchmarkID, s.Outcome, b)
		}
	}
	second := run(map[string]float64{"bench-devstral-24b": 0.82}, nil)
	for _, s := range second.Steps {
		reg := s.Comparison != nil && s.Comparison.Regression
		if reg != (s.BenchmarkID == "devstral-load-requests") {
			b, _ := json.Marshal(s.Comparison)
			t.Errorf("%s: regression %v: %s", s.BenchmarkID, reg, b)
		}
	}
	third := run(map[string]float64{}, map[string]float64{"bench-gemma-12b-3w": 1.22})
	for _, s := range third.Steps {
		reg := s.Comparison != nil && s.Comparison.Regression
		if reg != (s.BenchmarkID == "gemma-load-3w") || (s.BenchmarkID == "gemma-load-3w" && (s.Expect == nil || !s.Expect.Pass)) {
			b, _ := json.Marshal(s.Comparison)
			t.Errorf("third run %s: regression %v: %s", s.BenchmarkID, reg, b)
		}
	}
	if second.Steps[0].StartedAt-first.Steps[0].StartedAt < 60_000 {
		t.Errorf("the virtual clock did not move the second run past the first one's steps")
	}
}

// The whole harness: the real server with the import, the seed writes, a
// frozen scenario and MCP. It builds fakellama, so it is skipped under -short.
func TestHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the full harness")
	}
	c, err := startCluster(quiet, clusterOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	liveProfiles = c.profileDefs
	defer func() { liveProfiles = nil }()
	if err := seedConfig(c, quiet); err != nil {
		t.Fatal(err)
	}
	if err := seedBenchDefs(c); err != nil {
		t.Fatal(err)
	}
	get := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(c.url + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %v", path, resp.StatusCode, m)
		}
		return m
	}
	if cfg := get("/api/config"); cfg["revision"].(float64) < 14 || cfg["configured"] != true {
		t.Fatalf("config: revision %v configured %v", cfg["revision"], cfg["configured"])
	}
	sources := map[string]bool{}
	for _, e := range get("/api/audit?limit=500")["entries"].([]any) {
		sources[e.(map[string]any)["source"].(string)] = true
	}
	for _, s := range []string{"import", "ui", "rest", "mcp"} {
		if !sources[s] {
			t.Errorf("no audit entry with source %s: %v", s, sources)
		}
	}
	if len(get("/api/bench/benchmarks")["benchmarks"].([]any)) != len(seedBenchmarks()) {
		t.Errorf("benchmarks missing")
	}
	// a profile created through the API is in the scripted profile list
	if _, ok := profileByID("devstral-24b:16k"); !ok {
		t.Errorf("the profile created by the seed is not in the live profile list")
	}
	// a frozen scenario on a snapshot
	sc, _ := scenarioByName("bench-load")
	fw, err := startFrozen(c, quiet, sc.Bench, func(map[string]any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer fw.close()
	fw.settle(20 * time.Second)
	p := fw.mgr.Progress()
	cur, _ := p["current"].(map[string]any)
	if cur == nil || cur["benchmark_id"] != "kimi-72b-load-3w" {
		t.Fatalf("frozen run not at the kimi load: %v", p)
	}
	d := cur["detail"].(map[string]any)
	var load struct {
		Progress float64
		Workers  []struct{ State string }
	}
	raw, _ := json.Marshal(d["load"])
	json.Unmarshal(raw, &load)
	if load.Progress != 0.4 || len(load.Workers) != 3 || load.Workers[0].State != "done" || load.Workers[1].State != "receiving" || load.Workers[2].State != "waiting" {
		t.Fatalf("frozen load: %s", raw)
	}
	// MCP answers on the real root
	resp, err := http.Post(c.url+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("mcp: %v %v", err, resp)
	}
	resp.Body.Close()
}

// The recording of the real cluster is complete enough to replay: the index
// names files that exist and parse, the profile list is the large real one,
// and the catalog built from it has one entry per model file, none of them a
// generated configuration.
func TestRealRecordingReplays(t *testing.T) {
	start := time.Now()
	rp, err := loadReplay(start)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"/api/status", "/api/profiles", "/api/config", "/api/config/schema", "/api/history", "/api/audit", "/api/system",
		"/api/bench/state", "/api/bench/runs", "/api/bench/aggregates"} {
		if _, ok := rp.doc(key); !ok {
			t.Errorf("the recording has no %s", key)
		}
	}
	ps := rp.profiles()
	if len(ps) < 100 {
		t.Fatalf("only %d profiles in the recording", len(ps))
	}
	for _, f := range []string{"status.json", "status-loading.json"} {
		st, err := rp.status(f)
		if err != nil {
			t.Fatal(err)
		}
		at, _ := time.Parse(time.RFC3339Nano, st["time"].(string))
		if d := time.Since(at); d < -time.Minute || d > time.Minute {
			t.Errorf("%s: its time %v was not moved to now", f, at)
		}
	}
	// timestamps age with the harness: the newest recorded event is as old as it was at the recording
	hv, _ := rp.doc("/api/history")
	newest := hv.(map[string]any)["events"].([]any)[0].(map[string]any)["ts"].(float64)
	if age := float64(start.UnixMilli()) - newest; age < 0 || age > 3*3600e3 {
		t.Errorf("the newest recorded event is %v ms old after the shift", age)
	}
	status, _ := rp.status("status.json")
	in := api.CatalogInput{Status: status, Profiles: ps, HistoryOn: true}
	b, _ := json.Marshal(hv)
	var doc struct{ Events []history.Event }
	json.Unmarshal(b, &doc)
	in.Events, in.Files = doc.Events, rp.modelFiles(doc.Events)
	models := api.BuildModelCatalog(in, "")
	if len(models) < 5 || len(models) > 40 {
		t.Fatalf("%d models from %d profiles", len(models), len(ps))
	}
	total := 0
	for _, m := range models {
		total += m["profile_count"].(int)
		if strings.HasPrefix(m["key"].(string), "bench-") {
			t.Errorf("a profile is listed as a model: %v", m["key"])
		}
	}
	if total != len(ps) {
		t.Errorf("the models hold %d profiles, the recording %d", total, len(ps))
	}
	if p, ok := rp.plan(ps[0]["id"].(string), status); !ok || p["action"] == nil || p["need_basis"] == nil {
		t.Errorf("derived plan: %v", p)
	}
}
