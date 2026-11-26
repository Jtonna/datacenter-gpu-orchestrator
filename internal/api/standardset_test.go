package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/placer"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// ssFiles is the catalog of the standard-set tests.
var ssFiles = []ModelFile{
	{File: "tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf", Name: "TinyLlama 1.1B Chat v1.0", Quant: "Q4_K_M", ContextLength: 2048, MetaSource: "gguf"},
	{File: "nometa.gguf", Name: "Nometa", MetaSource: "file_name"},
	{File: "gemma-3-12b-Q4_K_M.gguf", Name: "Gemma 3 12B", Quant: "Q4_K_M", ContextLength: 131072, MetaSource: "gguf"},
	{File: "huge.gguf", Name: "Huge", ContextLength: 65536, MetaSource: "gguf"},
	{File: "unknown.gguf", Name: "Unknown", ContextLength: 32768, MetaSource: "gguf"},
	{File: "qwen-dots.gguf", Name: "Qwen2.5 72B Instruct", ContextLength: 32768, MetaSource: "gguf"},
	{File: "small-measured.gguf", Name: "Small Measured", ContextLength: 32768, MetaSource: "gguf"},
	{File: "big-measured.gguf", Name: "Big Measured", ContextLength: 32768, MetaSource: "gguf"},
	{File: "deep.gguf", Name: "Deep", ContextLength: 163840, MetaSource: "gguf"},
}

// ssNeed is each file's estimate: weights plus a slope per 1k of context.
func ssNeed(p *core.Profile) (scheduler.NeedInfo, error) {
	k := p.Ctx / 1024
	switch p.Model {
	case "tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf":
		return scheduler.NeedInfo{Need: placer.Estimated(700 + k*20), FileBytes: 600 << 20}, nil
	case "nometa.gguf":
		return scheduler.NeedInfo{Need: placer.Estimated(2000 + k*50), FileBytes: 1 << 30}, nil
	case "gemma-3-12b-Q4_K_M.gguf":
		return scheduler.NeedInfo{Need: placer.Estimated(6000 + k*250), FileBytes: 6 << 30}, nil
	case "huge.gguf":
		return scheduler.NeedInfo{Need: placer.Estimated(20000 + k*800), FileBytes: 18 << 30}, nil
	case "unknown.gguf":
		return scheduler.NeedInfo{}, errors.New("the header cannot be read")
	case "qwen-dots.gguf":
		return scheduler.NeedInfo{Need: placer.Estimated(3000 + k*20), FileBytes: 2 << 30}, nil
	case "small-measured.gguf":
		return scheduler.NeedInfo{Need: placer.Measured(3000), FileBytes: 2 << 30}, nil
	case "big-measured.gguf":
		return scheduler.NeedInfo{Need: placer.Measured(60000), FileBytes: 2 << 30}, nil
	case "deep.gguf":
		return scheduler.NeedInfo{Need: placer.Estimated(1000 + k*10), FileBytes: 1 << 30}, nil
	}
	return scheduler.NeedInfo{Need: placer.Measured(max(p.NeedsMiB, 1)), FileBytes: 1 << 20}, nil
}

// ssNeeds is ssNeed with the trained contexts of ssFiles (the scheduler's
// real-context rule reads them).
type ssNeeds struct{}

func (ssNeeds) Need(p *core.Profile) (scheduler.NeedInfo, error) { return ssNeed(p) }
func (ssNeeds) TrainedCtx(model string) (int, string, bool) {
	for _, f := range ssFiles {
		if f.File == model && f.ContextLength > 0 {
			return f.ContextLength, "llama", true
		}
	}
	return 0, "", false
}

// ssEnv is the consoles fleet (host-1 excluded: three zones) with the
// configuration service, the catalog above and the needs of ssNeed.
func ssEnv(t *testing.T) (*env, *cfgsvc.Service, *cfgstore.Store) {
	t.Helper()
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseWithOptions([]byte(testYAML), config.Options{FileExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(okUpstream())
	t.Cleanup(up.Close)
	port, _ := strconv.Atoi(up.URL[strings.LastIndex(up.URL, ":")+1:])
	e := &env{t: t, cfg: cfg, upstream: up, logs: &syncBuf{}, clock: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC), launcher: &fakeLauncher{port: port}}
	svc := cfgsvc.New(context.Background(), cfgsvc.Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }},
		Apply:    func(c *config.Config) { e.sched.ApplyConfig(c) },
		OnChange: func(ev cfgsvc.ChangeEvent) { e.api.PublishConfig(ev) }})
	sched, err := scheduler.New(cfg, scheduler.Options{
		View: &consolesView{ch: make(chan struct{}, 1)}, Prober: noProber{}, Launcher: e.launcher,
		Needs: ssNeeds{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.sched = sched
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	e.api = New(cfg, sched, Options{Logger: slog.New(slog.NewTextHandler(e.logs, nil)), Config: svc, Audit: cs,
		Now: func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.clock }, PingInterval: 40 * time.Millisecond,
		modelFiles: func() []ModelFile { return append([]ModelFile(nil), ssFiles...) }})
	e.front = httptest.NewServer(e.api)
	t.Cleanup(func() {
		e.front.CloseClientConnections()
		e.front.Close()
		cancel()
		<-done
		e.api.Close()
	})
	doc, err := config.ParseDocument([]byte(`{
		"fleet": {"url": "http://127.0.0.1:1", "local_node": "host-1"},
		"settings": {"max_pool": 4},
		"exclude_nodes": ["host-1"],
		"tiers": [{"match": {"local": true}, "priority": 1, "headroom_mib": 256}, {"match": {"local": false}, "priority": 1, "headroom_mib": 256}],
		"profiles": {
			"gemma-3-12b:64k": {"model": "gemma-3-12b-Q4_K_M.gguf", "ctx": 65536, "needs_mib": 22000, "description": "a person's"},
			"older": {"model": "nometa.gguf", "ctx": 32768, "needs_mib": 3600, "aliases": ["nometa:16k"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Import(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t", Comment: "start"}}, doc, true); err != nil {
		t.Fatal(err)
	}
	return e, svc, cs
}

func ssGet(t *testing.T, e *env, file string) standardSet {
	t.Helper()
	resp := e.get("/api/models/" + file + "/standard-set")
	b := body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", file, resp.StatusCode, b)
	}
	var s standardSet
	if err := json.Unmarshal([]byte(b), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func ssRow(t *testing.T, s standardSet, ctx int) standardRow {
	t.Helper()
	for _, r := range s.Rows {
		if r.Ctx == ctx {
			return r
		}
	}
	t.Fatalf("%s: no row at ctx %d: %+v", s.File, ctx, s.Rows)
	return standardRow{}
}

// The read call: candidates capped at the trained context, each planned on
// the fleet as it is, the default, the need's source, every zone, exists.
func TestStandardSet_Read(t *testing.T) {
	e, svc, _ := ssEnv(t)
	rev := svc.Revision()

	// trained at 2,048: only that context, and it is the default
	tiny := ssGet(t, e, "tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf")
	if len(tiny.Rows) != 1 || tiny.Rows[0].Ctx != 2048 || !tiny.Rows[0].Kept || tiny.Rows[0].ID != "tinyllama-1.1b-chat-v1.0" || tiny.ShortSource != "file" || !tiny.Rows[0].Default || tiny.TrainedCtx != 2048 {
		t.Fatalf("tinyllama: %+v", tiny)
	}
	if tiny.Rows[0].NeedSource != "estimate" || tiny.Rows[0].NeedMiB == nil {
		t.Fatalf("tinyllama need: %+v", tiny.Rows[0])
	}

	// no trained context: 16k and 32k; the file's production family is
	// "older" (its one profile, at 32k): the name is older, its 32k exists
	nm := ssGet(t, e, "nometa.gguf")
	if nm.TrainedSet || len(nm.Rows) != 2 || nm.Rows[0].Ctx != 16384 || nm.Rows[1].Ctx != 32768 || nm.Short != "older" || nm.ShortSource != "family" {
		t.Fatalf("nometa: %+v", nm)
	}
	if r := nm.Rows[0]; r.ID != "older:16k" || r.Exists {
		t.Fatalf("nometa 16k: %+v", r)
	}
	if r := nm.Rows[1]; r.ID != "older" || !r.Default || !r.Exists || r.ExistsNote != "exists, kept as it is" ||
		!reflect.DeepEqual(r.Carries, []string{"a hand-written need of 3,600 MiB", "aliases (nometa:16k)"}) || r.Selected || r.Tickable {
		t.Fatalf("nometa 32k: %+v", r)
	}
	if !reflect.DeepEqual(nm.ToWrite, []string{"older:16k"}) {
		t.Fatalf("nometa to write: %v", nm.ToWrite)
	}

	// 128k: 256k capped to it (one row); 64k exists; 128k only on every zone
	g := ssGet(t, e, "gemma-3-12b-Q4_K_M.gguf")
	if len(g.Rows) != 4 || g.Rows[3].Ctx != 131072 || g.DefaultID != "gemma-3-12b" || g.DefaultCtx != 32768 {
		t.Fatalf("gemma: %+v", g)
	}
	if r := ssRow(t, g, 65536); r.ID != "gemma-3-12b:64k" || !r.Exists || r.ExistsAs != "gemma-3-12b:64k" || r.ExistsNote != "exists, kept as it is" {
		t.Fatalf("gemma 64k: %+v", r)
	}
	if r := ssRow(t, g, 16384); r.UsesEveryZone || len(r.Zones) != 1 || r.MayUse != 3 {
		t.Fatalf("gemma 16k: %+v", r)
	}
	big := ssRow(t, g, 131072)
	if !big.Kept || !big.UsesEveryZone || len(big.Zones) != 3 || !strings.Contains(big.EveryZoneNote, "every zone") || big.ID != "gemma-3-12b:128k" {
		t.Fatalf("gemma 128k: %+v", big)
	}
	// a candidate EQUAL to the trained context is not above it (realctx.go
	// realCtx: p.Ctx <= trained runs as written): kept, no reason, no cap
	if g.TrainedCtx != 131072 || big.Ctx != g.TrainedCtx || big.Reason != "" || big.NeedMiB == nil || big.NeedSource != "estimate" {
		t.Fatalf("gemma 128k = trained: %+v (trained %d)", big, g.TrainedCtx)
	}
	// a kept row always has a need: kept means the idle-fleet plan placed it,
	// and a plan places nothing without a need (an unknown need is never kept)
	for _, s := range []standardSet{tiny, nm, g} {
		for _, r := range s.Rows {
			if r.Kept && (r.NeedMiB == nil || r.NeedSource == "unknown") {
				t.Fatalf("kept without a need: %+v", r)
			}
		}
	}
	if !reflect.DeepEqual(g.ToWrite, []string{"gemma-3-12b:16k", "gemma-3-12b", "gemma-3-12b:128k"}) {
		t.Fatalf("gemma to write: %v", g.ToWrite)
	}

	// a rejected candidate keeps its reason; the kept ones stay
	h := ssGet(t, e, "huge.gguf")
	if r := ssRow(t, h, 65536); r.State != "estimate-does-not-fit" || !r.Tickable || r.Selected ||
		!strings.HasPrefix(r.Reason, "not measured: by the file estimate it does not fit even with nothing loaded (71,200 MiB on 3 zones); the plan says: placer: need 71200 MiB ") {
		t.Fatalf("huge 64k reason: %q", r.Reason)
	}
	if r := ssRow(t, h, 65536); r.Kept || r.Action != "reject" || r.Reason == "" {
		t.Fatalf("huge 64k: %+v", r)
	}
	if r := ssRow(t, h, 16384); !r.Kept {
		t.Fatalf("huge 16k: %+v", r)
	}

	// an unknown need stays unknown: no figure, not kept
	u := ssGet(t, e, "unknown.gguf")
	for _, r := range u.Rows {
		if r.Kept || r.NeedMiB != nil || r.NeedSource != "unknown" || !strings.Contains(r.Reason, "memory need unknown") {
			t.Fatalf("unknown: %+v", r)
		}
	}
	if len(u.ToWrite) != 0 || u.DefaultID != "" {
		t.Fatalf("unknown writes %v default %q", u.ToWrite, u.DefaultID)
	}

	// the read wrote nothing and loaded nothing
	if svc.Revision() != rev {
		t.Fatalf("a read wrote revision %d", svc.Revision())
	}
	stat, err := e.sched.Status(context.Background())
	if err != nil || len(stat.Instances) != 0 || len(stat.Queue) != 0 {
		t.Fatalf("after the reads: %+v %v", stat, err)
	}
	if resp := e.get("/api/models/missing.gguf/standard-set"); resp.StatusCode != 404 {
		t.Fatalf("missing file: %d", resp.StatusCode)
	}
}

// The write: exactly the read's kept rows that do not exist, in one
// revision, audited; existing profiles untouched; a rollback of that
// revision removes them again.
func TestStandardSet_Write(t *testing.T) {
	e, svc, cs := ssEnv(t)
	file := "gemma-3-12b-Q4_K_M.gguf"
	read := ssGet(t, e, file)
	before := svc.Document()
	if st, m := e.cdo("POST", "/api/models/"+file+"/standard-set", `{}`, nil); st != 400 || !strings.Contains(fmt.Sprint(m), "comment is required") {
		t.Fatalf("no comment: %d %v", st, m)
	}
	rev := svc.Revision()
	st, m := e.cdo("POST", "/api/models/"+file+"/standard-set", `{"comment":"standard set for gemma"}`, nil)
	if st != 200 || m["revision"] != float64(rev+1) || m["changed"] != true {
		t.Fatalf("write: %d %v", st, m)
	}
	var written []string
	for _, v := range m["written"].([]any) {
		written = append(written, v.(string))
	}
	if !reflect.DeepEqual(written, read.ToWrite) {
		t.Fatalf("written %v, the read said %v", written, read.ToWrite)
	}
	if !strings.Contains(fmt.Sprint(m["skipped"]), "id:gemma-3-12b:64k reason:exists, kept as it is") {
		t.Fatalf("skipped %v", m["skipped"])
	}
	cfg := svc.Config()
	for _, row := range read.Rows {
		p := cfg.Profiles[row.ID]
		if row.Exists {
			if !reflect.DeepEqual(svc.Document().Profiles[row.ID], before.Profiles[row.ID]) {
				t.Fatalf("%s was edited", row.ID)
			}
			continue
		}
		if p == nil || p.Ctx != row.Ctx || p.Model != file || p.Stage != "production" || p.Description != "standard set" ||
			p.NeedsMiB != 0 || len(p.ForceNodes) != 0 || len(p.ExtraArgs) != 0 || p.Generated != nil {
			t.Fatalf("%s written as %+v", row.ID, p)
		}
	}
	es, _, err := cs.Query(context.Background(), cfgstore.Filter{Section: "profiles", Ascending: true, Since: 1})
	n := 0
	for _, en := range es {
		if en.Revision == rev+1 {
			n++
			if en.Action != audit.ActionCreate || en.Comment != "standard set for gemma" {
				t.Fatalf("entry %+v", en)
			}
		}
	}
	if err != nil || n != len(written) {
		t.Fatalf("entries at revision %d: %d, want %d (%v)", rev+1, n, len(written), err)
	}
	// again: nothing left to write, no revision
	if st, m := e.cdo("POST", "/api/models/"+file+"/standard-set", `{"comment":"again"}`, nil); st != 200 || m["changed"] != false || svc.Revision() != rev+1 {
		t.Fatalf("second write: %d %v", st, m)
	}
	// the rollback of that revision removes them
	if _, err := svc.Rollback(context.Background(), cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "t", Comment: "back"}}, rev); err != nil {
		t.Fatal(err)
	}
	for _, id := range written {
		if svc.Config().Profiles[id] != nil {
			t.Fatalf("%s still there after the rollback", id)
		}
	}
	if svc.Config().Profiles["gemma-3-12b:64k"] == nil {
		t.Fatal("the rollback removed the person's profile")
	}
	if st, _ := e.cdo("POST", "/api/models/"+file+"/other", `{"comment":"x"}`, nil); st != 404 {
		t.Fatalf("other route: %d", st)
	}
}

// The host memory sentence says plainly what is not counted.
func TestStandardSet_HostMemorySentence(t *testing.T) {
	got := hostMemorySentence(&scheduler.HostMemory{Machine: "the scheduler's machine", CheckpointsMiB: 6144, PromptCacheMiB: 4096})
	want := "restore points and prompt cache on the scheduler's machine: up to 10,240 MiB, not counted in what it needs (restore points 6,144 MiB, prompt cache 4,096 MiB)"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if s := hostMemorySentence(&scheduler.HostMemory{OnNode: true, Counted: true, SizeUnknown: true, PromptCacheMiB: 2048}); !strings.Contains(s, "the host node: at least 2,048 MiB, counted") {
		t.Fatal(s)
	}
	if got := standardCtxs(40960); !reflect.DeepEqual(got, []int{16384, 32768, 40960}) {
		t.Fatalf("40k trained: %v", got)
	}
}

var _ = http.StatusOK

// Kept is decided on an idle fleet (team-lead ruling, 26 Nov): with another
// model loaded on one zone, the 128k candidate that needs every zone cannot
// start now (its plan would unload or wait), yet it is kept, with the plan
// of now said beside it; it uses the three zones it would use with nothing
// loaded. The read unloads nothing.
func TestStandardSet_KeptOnAnIdleFleet(t *testing.T) {
	e, _, _ := ssEnv(t)
	resp := e.post("/v1/chat/completions", chat("older"))
	if resp.StatusCode != 200 {
		t.Fatalf("load older: %d %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	eventually(t, "older ready", func() bool {
		st, err := e.sched.Status(context.Background())
		return err == nil && len(st.Instances) == 1 && st.Instances[0].State == "ready"
	})
	g := ssGet(t, e, "gemma-3-12b-Q4_K_M.gguf")
	big := ssRow(t, g, 131072)
	if !big.Kept || big.Action == "start" || big.NowNote == "" || len(big.Zones) != 3 || !big.UsesEveryZone || big.Reason != "" {
		t.Fatalf("128k with older loaded: %+v", big)
	}
	if big.Action == "evict" && !strings.Contains(big.NowNote, "fits only when other models are unloaded: now it would first unload older") {
		t.Fatalf("128k now note: %q", big.NowNote)
	}
	if !slices.Contains(g.ToWrite, "gemma-3-12b:128k") {
		t.Fatalf("to write: %v", g.ToWrite)
	}
	if r := ssRow(t, g, 16384); !r.Kept || r.Action != "start" || r.NowNote != "" {
		t.Fatalf("16k: %+v", r)
	}
	// a candidate that does not fit even with nothing loaded is not kept
	h := ssGet(t, e, "huge.gguf")
	if r := ssRow(t, h, 65536); r.Kept || r.Reason == "" {
		t.Fatalf("huge 64k: %+v", r)
	}
	st, err := e.sched.Status(context.Background())
	if err != nil || len(st.Instances) != 1 || st.Instances[0].Profile != "older" || st.Instances[0].State != "ready" {
		t.Fatalf("after the read: %+v %v", st.Instances, err)
	}
}
