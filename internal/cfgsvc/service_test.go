package cfgsvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

type env struct {
	t       *testing.T
	svc     *Service
	cs      *cfgstore.Store
	applied []*config.Config
	events  []ChangeEvent
	refs    map[string][]string
}

func newEnv(t *testing.T) *env {
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
	e := &env{t: t, cs: cs, refs: map[string][]string{}}
	e.svc = New(context.Background(), Options{Store: cs, Parse: config.Options{FileExists: func(string) bool { return true }},
		Apply:       func(c *config.Config) { e.applied = append(e.applied, c) },
		OnChange:    func(ev ChangeEvent) { e.events = append(e.events, ev) },
		ProfileRefs: func(id string) []string { return e.refs[id] }})
	return e
}

func ui(actor string) WriteOpts {
	return WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: actor, Comment: "test"}}
}

func llama(model string, ctx int) *config.ProfileSpec {
	p, err := ParseProfile([]byte(fmt.Sprintf(`{"model":%q,"ctx":%d,"needs_mib":100}`, model, ctx)))
	if err != nil {
		panic(err)
	}
	return p
}

func TestEmptyDatabaseStartsUnconfigured(t *testing.T) {
	e := newEnv(t)
	v := e.svc.View()
	if v.Configured || v.Revision != 0 || len(v.Profiles) != 0 || e.svc.Config().Configured {
		t.Fatalf("%+v", v)
	}
	if v.Settings["ttl"] != "5m0s" {
		t.Fatalf("defaults missing: %v", v.Settings["ttl"])
	}
}

func TestProfileLifecycleRevisionsAndAudit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, err := e.svc.CreateProfile(ctx, ui("jacob"), "a", llama("a.gguf", 1024))
	if err != nil || res.Revision != 1 || len(res.AuditEntryIDs) != 1 || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	if len(e.applied) != 1 || e.applied[0].Revision != 1 || e.applied[0].Profiles["a"] == nil {
		t.Fatalf("not applied: %+v", e.applied)
	}
	if _, err := e.svc.CreateProfile(ctx, ui("x"), "a", llama("a.gguf", 1024)); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	res, err = e.svc.UpdateProfile(ctx, ui("jacob"), "a", llama("a.gguf", 2048))
	if err != nil || res.Revision != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	found := false
	for _, d := range res.Diff {
		if d.Path == "profiles.a.ctx" && d.Old == float64(1024) && d.New == float64(2048) {
			found = true
		}
	}
	if !found {
		t.Fatalf("diff %+v", res.Diff)
	}
	es, total, err := e.cs.Query(ctx, cfgstore.Filter{Section: "profiles", Target: "a", Ascending: true})
	if err != nil || total != 2 || es[0].Action != "create" || es[1].Action != "update" || es[1].Actor != "jacob" || es[1].Source != "ui" || es[1].Revision != 2 || es[1].Comment != "test" {
		t.Fatalf("%+v %v", es, err)
	}
	// An unchanged write makes no revision.
	res, _ = e.svc.UpdateProfile(ctx, ui("jacob"), "a", llama("a.gguf", 2048))
	if res.Changed || res.Revision != 2 || e.svc.Revision() != 2 {
		t.Fatalf("no-op wrote a revision: %+v", res)
	}
	if len(e.events) != 2 || e.events[1].Section != "profiles" || e.events[1].Target != "a" {
		t.Fatalf("events %+v", e.events)
	}
}

func TestInvalidChangeIsRejectedAndWritesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad, _ := ParseProfile([]byte(`{"model":"a.gguf","ctx":1024,"kv_quant":"q8_0"}`)) // q8_0 needs flash_attn
	_, err := e.svc.CreateProfile(ctx, ui("x"), "a", bad)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Messages) == 0 || ve.Messages[0] != `profile "a": kv_quant: q8_0 requires flash_attn: true` {
		t.Fatalf("%v", err)
	}
	if e.svc.Revision() != 0 || len(e.applied) != 0 {
		t.Fatal("a rejected change was applied")
	}
	if _, total, _ := e.cs.Query(ctx, cfgstore.Filter{}); total != 0 {
		t.Fatalf("audit entries for a rejected change: %d", total)
	}
	_, err = e.svc.PatchSettings(ctx, ui("x"), map[string]any{"max_hold": "1s", "min_hold": "5s"})
	if !errors.As(err, &ve) {
		t.Fatalf("min_hold > max_hold accepted: %v", err)
	}
	_, err = e.svc.PatchSettings(ctx, ui("x"), map[string]any{"nonsense": 1.0})
	if !errors.As(err, &ve) {
		t.Fatalf("unknown key accepted: %v", err)
	}
}

func TestDryRunIfMatchAndSettingsReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := ui("a")
	o.DryRun = true
	res, err := e.svc.PatchSettings(ctx, o, map[string]any{"ttl": "10m"})
	if err != nil || !res.DryRun || len(res.Diff) != 1 || e.svc.Revision() != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.svc.PatchSettings(ctx, ui("a"), map[string]any{"ttl": "10m", "max_pool": 2.0}); err != nil {
		t.Fatal(err)
	}
	if e.svc.View().Settings["ttl"] != "10m0s" || e.svc.Config().Defaults.TTL.Minutes() != 10 {
		t.Fatal("setting not applied")
	}
	stale := int64(0)
	o = ui("a")
	o.IfMatch = &stale
	_, err = e.svc.PatchSettings(ctx, o, map[string]any{"ttl": "1m"})
	var st *cfgstore.ErrStale
	if !errors.As(err, &st) || st.Current != 1 {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.PatchSettings(ctx, ui("a"), map[string]any{"ttl": nil}); err != nil {
		t.Fatal(err)
	}
	if e.svc.View().Settings["ttl"] != "5m0s" {
		t.Fatalf("not reset: %v", e.svc.View().Settings["ttl"])
	}
}

func TestDeleteProfileWarnsAboutBenchmarksAndNeedsForce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.CreateProfile(ctx, ui("a"), "a", llama("a.gguf", 1024))
	e.refs["a"] = []string{"bench-1"}
	_, err := e.svc.DeleteProfile(ctx, ui("a"), "a")
	var re *RefError
	if !errors.As(err, &re) || len(re.Warnings) != 1 {
		t.Fatalf("%v", err)
	}
	o := ui("a")
	o.Force = true
	res, err := e.svc.DeleteProfile(ctx, o, "a")
	if err != nil || len(res.Warnings) != 1 || len(e.svc.View().Profiles) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRollbackCreatesNewRevisionAndRestartKeysAreReported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.CreateProfile(ctx, ui("a"), "a", llama("a.gguf", 1024)) // 1
	res, _ := e.svc.PatchSettings(ctx, ui("a"), map[string]any{"ws_max_clients": 8.0})
	if len(res.RestartRequired) != 1 || res.RestartRequired[0] != "settings.ws_max_clients" {
		t.Fatalf("%+v", res.RestartRequired)
	}
	e.svc.DeleteProfile(ctx, ui("a"), "a") // 3
	res, err := e.svc.Rollback(ctx, ui("a"), 2)
	if err != nil || res.Revision != 4 || e.svc.View().Profiles["a"] == nil {
		t.Fatalf("%+v %v", res, err)
	}
	es, _, _ := e.cs.Query(ctx, cfgstore.Filter{Action: "rollback"})
	if len(es) == 0 || es[0].Revision != 4 {
		t.Fatalf("%+v", es)
	}
	if _, err := e.svc.Rollback(ctx, ui("a"), 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestImportRefusesNonEmptyWithoutReplace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	doc := config.NewDocument()
	doc.Profiles["a"] = llama("a.gguf", 1024)
	o := WriteOpts{Origin: audit.Origin{Source: audit.SourceImport, Actor: "cli"}}
	if res, err := e.svc.Import(ctx, o, doc, false); err != nil || res.Revision != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.svc.Import(ctx, o, doc, false); !errors.Is(err, cfgstore.ErrNotEmpty) {
		t.Fatalf("%v", err)
	}
	doc.Profiles["b"] = llama("b.gguf", 1024)
	if res, err := e.svc.Import(ctx, o, doc, true); err != nil || res.Revision != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestStartsFromTheStoredRevision(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.CreateProfile(ctx, ui("a"), "a", llama("a.gguf", 1024))
	again := New(ctx, Options{Store: e.cs, Parse: config.Options{FileExists: func(string) bool { return true }}})
	if again.Revision() != 1 || again.Config().Profiles["a"] == nil || !again.Config().Configured || again.Config().Revision != 1 {
		t.Fatalf("%d %+v", again.Revision(), again.Config())
	}
}
