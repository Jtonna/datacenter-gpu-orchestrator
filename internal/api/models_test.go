package api

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

func TestIsGeneratedProfile(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		id     string
		marker *bool
		want   bool
	}{
		{"bench-gemma12b-3-2048", nil, true}, {"b-qwen4b-1ps5", nil, true}, {"m-gemma-48k", nil, true},
		{"gemma-4-12b", nil, false}, {"benchmark-model", nil, false}, {"mixtral", nil, false}, {"big-model", nil, false},
		{"gemma-4-12b", &yes, true},           // the field says so
		{"bench-kept-by-hand-8k", &no, false}, // the field wins over the name
	} {
		if got := IsGeneratedProfile(c.id, c.marker); got != c.want {
			t.Errorf("IsGeneratedProfile(%q, %v) = %v, want %v", c.id, c.marker, got, c.want)
		}
	}
}

func TestModelFileFromName(t *testing.T) {
	for _, c := range []struct{ file, name, publisher, params, quant string }{
		{"gemma-4-12b-it-Q4_K_M.gguf", "Gemma 4 12B IT", "", "12B", "Q4_K_M"},
		{"Qwen_Qwen3.5-4B-Q4_K_M.gguf", "Qwen3.5 4B", "Qwen", "4B", "Q4_K_M"},
		{"moonshotai_Kimi-Dev-72B-IQ2_S.gguf", "Kimi Dev 72B", "Moonshot AI", "72B", "IQ2_S"},
		{"tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf", "TinyLlama 1.1B Chat v1.0", "", "1.1B", "Q4_K_M"},
		{"mixtral-8x7b-instruct-v0.1.Q4_K_M.gguf", "Mixtral 8x7B Instruct v0.1", "", "8x7B", "Q4_K_M"},
		{"Llama-3.3-70B-Instruct-Q2_K.gguf", "Llama 3.3 70B Instruct", "", "70B", "Q2_K"},
		{"plain.gguf", "Plain", "", "", ""},
	} {
		mf := ModelFileFromName(c.file)
		if mf.Name != c.name || mf.Publisher != c.publisher || mf.Parameters != c.params || mf.Quant != c.quant || mf.MetaSource != "file_name" {
			t.Errorf("%s: got name %q publisher %q parameters %q quant %q source %q", c.file, mf.Name, mf.Publisher, mf.Parameters, mf.Quant, mf.MetaSource)
		}
	}
}

// ggufBytes writes a minimal GGUF header with string metadata only.
func ggufBytes(kv map[string]string) []byte {
	le32 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	le64 := func(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }
	str := func(s string) []byte { return append(le64(uint64(len(s))), s...) }
	out := append([]byte("GGUF"), le32(3)...)
	out = append(out, le64(0)...)
	out = append(out, le64(uint64(len(kv)))...)
	for k, v := range kv {
		out = append(out, str(k)...)
		out = append(out, le32(8)...) // string
		out = append(out, str(v)...)
	}
	return out
}

// The metadata of a file comes from its GGUF header when it can be read, and
// from its name when the file is missing or is not a GGUF; the cache answers
// the second listing without reading again.
func TestModelFilesFromDirectory(t *testing.T) {
	dir := t.TempDir()
	good := ggufBytes(map[string]string{"general.architecture": "gemma3", "general.name": "Gemma 4 12B It", "general.size_label": "12B"})
	if err := os.WriteFile(filepath.Join(dir, "gemma-4-12b-it-Q4_K_M.gguf"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "broken-7b-Q8_0.gguf"), []byte("not a gguf"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	var c modelFileCache
	for pass := 0; pass < 2; pass++ {
		got := map[string]ModelFile{}
		for _, f := range c.list(dir, []string{"missing-3b-Q4_K_M.gguf", ""}) {
			got[f.File] = f
		}
		if len(got) != 3 {
			t.Fatalf("pass %d: files %v, want the two .gguf in the directory and the named missing one", pass, got)
		}
		g := got["gemma-4-12b-it-Q4_K_M.gguf"]
		if !g.Exists || g.SizeBytes != int64(len(good)) || g.Name != "Gemma 4 12B IT" || g.RawName != "Gemma 4 12B It" || g.Brand != "gemma" || g.Family != "gemma3" || g.Parameters != "12B" || g.Quant != "Q4_K_M" || g.MetaSource != "gguf" {
			t.Errorf("pass %d: gguf file: %+v", pass, g)
		}
		if b := got["broken-7b-Q8_0.gguf"]; !b.Exists || b.MetaSource != "file_name" || b.Name != "Broken 7B" || b.Quant != "Q8_0" {
			t.Errorf("pass %d: unreadable file: %+v", pass, b)
		}
		if m := got["missing-3b-Q4_K_M.gguf"]; m.Exists || m.SizeBytes != 0 || m.Parameters != "3B" {
			t.Errorf("pass %d: missing file: %+v", pass, m)
		}
	}
	if len(c.seen) != 2 {
		t.Errorf("cache holds %d entries, want the 2 files that exist", len(c.seen))
	}
}

func catalogFixture() CatalogInput {
	var status map[string]any
	json.Unmarshal([]byte(`{
	 "instances": [
	  {"profile": "gemma:8k", "model_file": "gemma.gguf", "state": "ready", "nodes": [{"id": "aa:01"}], "device_total_mib": 7850.4,
	   "in_flight": 1, "waiters": 0, "requests_served": 4, "ready_since": "2026-11-19T08:00:00Z", "last_used": "2026-11-19T08:05:00Z",
	   "load_duration": 28.1, "last_request": {"tokens_per_second": 47.8}},
	  {"profile": "bench-gemma-2-2048", "model_file": "gemma.gguf", "state": "starting", "phase": "loading", "nodes": [{"id": "aa:02"}, {"id": "aa:03"}],
	   "created": "2026-11-19T08:06:00Z", "load_progress": 0.4, "load_eta_s": 12, "in_flight": 0, "waiters": 1, "requests_served": 0}
	 ],
	 "learned_needs": {"gemma:8k": {"measured_mib": 7850, "nodes": [{"node": "aa:01", "mib": 7850}]}},
	 "last_refusals": [{"profile": "big:8k", "at": "2026-11-19T07:00:00Z", "status": 503, "reason": "forced nodes do not fit", "message": "does not fit"}],
	 "view": {"nodes": [
	  {"id": "aa:01", "hostname": "ps5-a", "eligible": true, "excluded": false, "plan_budget_mib": 13000},
	  {"id": "aa:02", "hostname": "ps5-b", "eligible": true, "excluded": false, "plan_budget_mib": 13000},
	  {"id": "aa:03", "hostname": "ps5-c", "eligible": true, "excluded": false, "plan_budget_mib": 13000},
	  {"id": "aa:04", "hostname": "desk", "eligible": false, "excluded": true, "plan_budget_mib": 8000}]}
	}`), &status)
	prof := func(id, model string, ctx int, need float64, generated bool, force []string, start bool) map[string]any {
		return map[string]any{"id": id, "kind": "llama", "model": model, "ctx": ctx, "parallel": 1, "aliases": []string{}, "force_nodes": force,
			"tensor_split": []int{}, "generated": generated, "needs_mib": need, "needs_basis": "estimated", "needs_measured": false,
			"plan": map[string]any{"action": "start", "would_start": start, "nodes": []string{}, "reason": ""}}
	}
	return CatalogInput{
		HistoryOn: true, Status: status,
		Profiles: []map[string]any{
			prof("gemma:8k", "gemma.gguf", 8192, 8100, false, nil, true),
			prof("bench-gemma-2-2048", "gemma.gguf", 2048, 8000, true, []string{"ps5-b", "ps5-c"}, false),
			prof("big:8k", "big-72B-Q2_K.gguf", 8192, 28650, false, nil, false),
			prof("huge:8k", "huge-200B-Q2_K.gguf", 8192, 90000, false, nil, false),
			{"id": "whisper", "kind": "command", "model": "", "aliases": []string{}, "force_nodes": []string{}, "tensor_split": []int{}, "generated": false,
				"plan": map[string]any{"action": "start", "would_start": true}},
		},
		Files: []ModelFile{
			{File: "gemma.gguf", Exists: true, SizeBytes: 7 << 30, Name: "Gemma 4 12B", Family: "gemma3", Parameters: "12B", Quant: "Q4_K_M", MetaSource: "gguf"},
			{File: "unused-3b-Q4_K_M.gguf", Exists: true, SizeBytes: 2 << 30, Name: "unused-3b", MetaSource: "file_name"},
		},
		Events: []history.Event{ // newest first
			{Ts: 5000, Type: history.TInstanceUnloaded, Profile: "bench-gemma-2-2048", ModelFile: "gemma.gguf", Nodes: []string{"ps5-b", "ps5-c"}, Data: map[string]any{"requests_served": 3.0}},
			{Ts: 4000, Type: history.TLoadFinished, Profile: "bench-gemma-2-2048", ModelFile: "gemma.gguf", Nodes: []string{"ps5-b", "ps5-c"},
				Data: map[string]any{"outcome": "ready", "load_s": 30.0, "avg_mb_s": 250.0, "nodes_detail": []any{map[string]any{"memory_mib": 4000.0}, map[string]any{"memory_mib": 3900.0}}}},
			{Ts: 3000, Type: history.TRequestRefused, Profile: "big:8k", ModelFile: "big-72B-Q2_K.gguf", Data: map[string]any{"reason": "older", "message": "older", "status": 503.0}},
			{Ts: 2000, Type: history.TLoadFinished, Profile: "gemma:8k", ModelFile: "gemma.gguf", Nodes: []string{"ps5-a"}, Data: map[string]any{"outcome": "failed", "reason": "stalled", "load_s": 71.0}},
			{Ts: 1000, Type: history.TLoadFinished, Profile: "gemma:8k", ModelFile: "gemma.gguf", Nodes: []string{"ps5-a"}, Data: map[string]any{"outcome": "ready", "load_s": 26.0, "avg_mb_s": 100.0, "nodes_detail": []any{map[string]any{"memory_mib": 7900.0}}}},
			{Ts: 900, Type: history.TNodeMemory, Profile: "gemma:8k", ModelFile: "gemma.gguf", Nodes: []string{"ps5-a"}},
		},
		Aggregates: map[string]any{
			"by_model_nodes": []any{map[string]any{"model_file": "gemma.gguf", "nodes": 2.0, "runs": 2.0, "steps": 2.0,
				"metrics": map[string]any{"load_s": map[string]any{"median": 29.0, "last": 30.0, "count": 2.0}, "decode_tps": map[string]any{"median": 21.0, "last": 22.0, "count": 2.0}}}},
			"benchmarks": []any{map[string]any{"benchmark_id": "gemma-2ps5", "name": "Gemma on two", "profile": "bench-gemma-2-2048", "runs": 2.0, "pass_rate": 1.0,
				"metrics": map[string]any{"load_s": map[string]any{"median": 29.0, "last": 30.0, "count": 2.0}}}},
		},
	}
}

// One entry per model file: profiles are grouped under their file, a file no
// profile uses is listed too, a profile whose file is missing still gets an
// entry, and a command profile gets one of its own.
func TestModelCatalogGrouping(t *testing.T) {
	out := BuildModelCatalog(catalogFixture(), "")
	by := map[string]map[string]any{}
	var order []string
	for _, m := range out {
		by[m["key"].(string)] = m
		order = append(order, m["key"].(string))
	}
	if len(out) != 5 {
		t.Fatalf("got %d entries %v, want 5 (gemma, big, huge, unused, command:whisper)", len(out), order)
	}
	g := by["gemma.gguf"]
	if g["name"] != "Gemma 4 12B" || g["exists"] != true || g["size_bytes"].(int64) != 7<<30 || g["quant"] != "Q4_K_M" || g["meta_source"] != "gguf" {
		t.Errorf("gemma: file facts %v", g)
	}
	if g["profile_count"] != 2 || g["generated_count"] != 1 || g["startable_count"] != 1 {
		t.Errorf("gemma: counts %v %v %v", g["profile_count"], g["generated_count"], g["startable_count"])
	}
	ps := g["profiles"].([]map[string]any)
	if ps[0]["id"] != "bench-gemma-2-2048" || ps[0]["generated"] != true || ps[0]["placement"] != "forced" || len(ps[0]["force_nodes"].([]string)) != 2 ||
		ps[1]["id"] != "gemma:8k" || ps[1]["generated"] != false || ps[1]["placement"] != "automatic" {
		t.Errorf("gemma: profiles %v", ps)
	}
	// a file that is not there
	b := by["big-72B-Q2_K.gguf"]
	if b["exists"] != false || b["name"] != "Big 72B" || b["raw_name"] != "big-72B" || b["parameters"] != "72B" || b["quant"] != "Q2_K" || b["meta_source"] != "file_name" || b["state"] != "not_loaded" {
		t.Errorf("missing file: %v", b)
	}
	if u := by["unused-3b-Q4_K_M.gguf"]; u == nil || u["profile_count"] != 0 || u["exists"] != true {
		t.Errorf("a file without a profile: %v", u)
	}
	if c := by["command:whisper"]; c == nil || c["kind"] != "command" || c["name"] != "whisper" || c["file"] != "" {
		t.Errorf("command profile: %v", c)
	}
	if _, has := g["events"]; has {
		t.Error("the list must not carry the events (they are in the detail)")
	}
}

// The instances of every profile of a model hang under that model, each with
// the hostnames it is on, and decide its state.
func TestModelCatalogInstances(t *testing.T) {
	g := BuildModelCatalog(catalogFixture(), "gemma.gguf")[0]
	insts := g["instances"].([]map[string]any)
	if len(insts) != 2 || g["state"] != "ready" {
		t.Fatalf("state %v, instances %v", g["state"], insts)
	}
	r, l := insts[0], insts[1]
	if r["profile"] != "gemma:8k" || r["state"] != "ready" || r["nodes"].([]string)[0] != "ps5-a" || r["memory_mib"].(float64) != 7850 ||
		r["in_flight"] != 1 || r["requests_served"] != 4 || r["since"] != "2026-11-19T08:00:00Z" || r["last_request_tps"].(float64) != 47.8 {
		t.Errorf("ready instance: %v", r)
	}
	if l["profile"] != "bench-gemma-2-2048" || l["state"] != "starting" || len(l["nodes"].([]string)) != 2 || l["nodes"].([]string)[1] != "ps5-c" ||
		l["load_progress"].(float64) != 0.4 || l["since"] != "2026-11-19T08:06:00Z" || l["waiters"] != 1 {
		t.Errorf("loading instance: %v", l)
	}
	if got := BuildModelCatalog(catalogFixture(), "big-72B-Q2_K.gguf")[0]; len(got["instances"].([]map[string]any)) != 0 || got["state"] != "not_loaded" {
		t.Errorf("a model without an instance: %v", got["instances"])
	}
	if len(BuildModelCatalog(catalogFixture(), "nope.gguf")) != 0 {
		t.Error("an unknown key must give no entry")
	}
}

// History, measurements, refusals and benchmarks are summed per model.
func TestModelCatalogHistoryAndFit(t *testing.T) {
	in := catalogFixture()
	g := BuildModelCatalog(in, "gemma.gguf")[0]
	loads := g["loads"].(map[string]any)
	if loads["count"] != 3 || loads["ready"] != 2 || loads["failed"] != 1 || loads["unloads"] != 1 {
		t.Errorf("loads %v", loads)
	}
	if last := loads["last"].(map[string]any); last["profile"] != "bench-gemma-2-2048" || last["outcome"] != "ready" || last["load_s"].(float64) != 30 {
		t.Errorf("last load %v", last)
	}
	by := loads["by_nodes"].([]map[string]any)
	if len(by) != 2 || by[0]["nodes"] != 1 || by[0]["median_s"].(float64) != 26 || by[1]["nodes"] != 2 || by[1]["avg_mb_s"].(float64) != 250 {
		t.Errorf("loads by node count %v", by)
	}
	if g["last_loaded"].(int64) != 4000 || g["last_active"].(int64) != 5000 || g["requests_served"] != 7 {
		t.Errorf("last_loaded %v last_active %v served %v (3 from the history + 4 of the loaded instance)", g["last_loaded"], g["last_active"], g["requests_served"])
	}
	ms := g["measured"].([]map[string]any)
	if len(ms) != 2 || ms[0]["nodes"] != 1 || ms[0]["memory_mib"].(float64) != 7850 || ms[0]["source"] != "this run" || ms[1]["nodes"] != 2 || ms[1]["memory_mib"].(float64) != 7900 {
		t.Errorf("measured %v", ms)
	}
	fit := g["fit"].(map[string]any)
	if fit["basis"] != "measured" || fit["need_mib"].(float64) != 7850 || fit["min_nodes"] != 1 || fit["ran_on_nodes"] != 1 {
		t.Errorf("fit %v", fit)
	}
	if ev := g["events"].([]history.Event); len(ev) != 4 {
		t.Errorf("detail events: %d, want 4 (node memory samples are left out)", len(ev))
	}
	bm := g["bench"].(map[string]any)
	if bm["runs"] != 2 || bm["benchmarks"].([]string)[0] != "gemma-2ps5" || bm["by_nodes"].([]map[string]any)[0]["nodes"] != 2 || len(bm["detail"].([]map[string]any)) != 1 {
		t.Errorf("bench %v", bm)
	}

	// estimated: needs more than one node of 13000, fits on three
	big := BuildModelCatalog(in, "big-72B-Q2_K.gguf")[0]
	if fit := big["fit"].(map[string]any); fit["basis"] != "estimated" || fit["min_nodes"] != 3 || fit["need_mib"].(float64) != 28650 || fit["cluster_budget_mib"].(float64) != 39000 {
		t.Errorf("big fit %v", fit)
	}
	// the status refusal is newer than the one in the history
	if r := big["last_refusal"].(map[string]any); r["reason"] != "forced nodes do not fit" || r["profile"] != "big:8k" {
		t.Errorf("last refusal %v", r)
	}
	if big["bench"] != nil || g["last_refusal"] != nil {
		t.Errorf("empty parts must be null: bench %v, refusal %v", big["bench"], g["last_refusal"])
	}
	// more than the whole cluster: min_nodes 0
	if fit := BuildModelCatalog(in, "huge-200B-Q2_K.gguf")[0]["fit"].(map[string]any); fit["min_nodes"] != 0 {
		t.Errorf("huge fit %v", fit)
	}
}

// The endpoints on a real server: the list groups the test profiles by file
// (none of which exists under /models), the detail answers by key, an unknown
// key is 404, and the unload paths next to them keep working as POST only.
func TestModelCatalogEndpoints(t *testing.T) {
	e := newEnv(t, chatOK)
	resp := e.get("/api/models")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/models: %d %s", resp.StatusCode, body(t, resp))
	}
	var doc struct {
		Models []struct {
			Key, File, State string
			Exists           bool
			Profiles         []struct {
				ID        string
				Generated bool
				Plan      map[string]any
			}
			Instances []any
		}
		ModelsDir string `json:"models_dir"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Models) != 3 || doc.ModelsDir != "/models" {
		t.Fatalf("%+v", doc)
	}
	for _, m := range doc.Models {
		if m.Exists || m.State != "not_loaded" || len(m.Profiles) != 1 || m.Profiles[0].Generated || m.Profiles[0].Plan["action"] == nil || m.Instances == nil {
			t.Errorf("model %+v", m)
		}
	}
	if doc.Models[0].Key != "a.gguf" || doc.Models[0].Profiles[0].ID != "alpha" {
		t.Errorf("first model %+v", doc.Models[0])
	}
	resp = e.get("/api/models/b.gguf")
	var one map[string]any
	if err := json.Unmarshal([]byte(body(t, resp)), &one); err != nil || resp.StatusCode != 200 || one["key"] != "b.gguf" || one["events"] == nil {
		t.Errorf("GET /api/models/b.gguf: %d %v %v", resp.StatusCode, err, one)
	}
	if resp = e.get("/api/models/nope.gguf"); resp.StatusCode != 404 {
		t.Errorf("unknown model: status %d", resp.StatusCode)
	}
	body(t, resp)
	if resp = e.get("/api/models/unload"); resp.StatusCode != 404 {
		t.Errorf("GET /api/models/unload must not unload anything: status %d", resp.StatusCode)
	}
	body(t, resp)
}
