package cfgsvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
)

// stage and display_name are written through profile create and update
// (slice B2). An update that leaves both out keeps the stored ones (a
// client that does not know them does not wipe them); one that sets either
// writes both. Validation is the configuration's (section 5).
func TestProfileMarksAreWritable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	p := llama("m.gguf", 4096)
	p.Stage, p.DisplayName = "test", "Mine"
	if _, err := e.svc.CreateProfile(ctx, ui("x"), "p", p); err != nil {
		t.Fatal(err)
	}
	if q := e.svc.Config().Profiles["p"]; q.Stage != "test" || q.DisplayName != "Mine" {
		t.Fatalf("created %+v", q)
	}
	// an edit without them keeps them
	if _, err := e.svc.UpdateProfile(ctx, ui("x"), "p", llama("m.gguf", 8192)); err != nil {
		t.Fatal(err)
	}
	if q := e.svc.Config().Profiles["p"]; q.Ctx != 8192 || q.Stage != "test" || q.DisplayName != "Mine" {
		t.Fatalf("after an edit without marks %+v", q)
	}
	// an edit with them writes them
	other := llama("m.gguf", 8192)
	other.Stage, other.DisplayName = "production", "Gemma fast"
	if _, err := e.svc.UpdateProfile(ctx, ui("x"), "p", other); err != nil {
		t.Fatal(err)
	}
	if q := e.svc.Config().Profiles["p"]; q.Stage != "production" || q.DisplayName != "Gemma fast" {
		t.Fatalf("after an edit with marks %+v", q)
	}
	// validation (section 5)
	for _, c := range []struct{ stage, name, want string }{
		{"beta", "", "stage"},
		{"", "TEST - x", "TEST - "},
		{"", strings.Repeat("n", 81), "at most 80"},
		{"", "p", "is the id of profile"},
	} {
		bad := llama("m.gguf", 4096)
		bad.Stage, bad.DisplayName = c.stage, c.name
		_, err := e.svc.CreateProfile(ctx, ui("x"), "q", bad)
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Error(), c.want) {
			t.Errorf("stage %q name %q: %v, want %q", c.stage, c.name, err, c.want)
		}
	}
}

// Promote and demote: one revision each, audited as promote / demote with
// the comment; nothing written (no revision, no entry) when the profile is
// already at that stage, by mark or by rule.
func TestPromoteAndDemote(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	forced := llama("m.gguf", 4096)
	forced.ForceNodes = []string{"ps5-1"} // test by rule
	if _, err := e.svc.CreateProfile(ctx, ui("x"), "f", forced); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateProfile(ctx, ui("x"), "plain", llama("m.gguf", 4096)); err != nil { // production by rule
		t.Fatal(err)
	}
	rev := e.svc.Revision()
	o := WriteOpts{Origin: audit.Origin{Source: audit.SourceUI, Actor: "alex", Comment: "ready for users"}}
	res, err := e.svc.SetStage(ctx, o, "f", "production")
	if err != nil || !res.Changed || res.Revision != rev+1 || len(res.AuditEntryIDs) != 1 {
		t.Fatalf("promote: %+v %v", res, err)
	}
	if q := e.svc.Config().Profiles["f"]; q.Stage != "production" || len(q.ForceNodes) != 1 {
		t.Fatalf("promoted %+v", q)
	}
	es, _, err := e.cs.Query(ctx, cfgstore.Filter{Section: "profiles", Target: "f", Ascending: true})
	if err != nil || len(es) != 2 || es[1].Action != audit.ActionPromote || es[1].Comment != "ready for users" || es[1].Actor != "alex" {
		t.Fatalf("audit %+v %v", es, err)
	}
	// again: nothing changes, nothing written
	res, err = e.svc.SetStage(ctx, o, "f", "production")
	if err != nil || res.Changed || e.svc.Revision() != rev+1 {
		t.Fatalf("second promote: %+v %v", res, err)
	}
	// production by rule: promote is a no-op
	if res, err := e.svc.SetStage(ctx, o, "plain", "production"); err != nil || res.Changed {
		t.Fatalf("promote of a production profile: %+v %v", res, err)
	}
	// demote
	res, err = e.svc.SetStage(ctx, o, "plain", "test")
	if err != nil || !res.Changed {
		t.Fatalf("demote: %+v %v", res, err)
	}
	es, _, _ = e.cs.Query(ctx, cfgstore.Filter{Section: "profiles", Target: "plain", Ascending: true})
	if len(es) != 2 || es[1].Action != audit.ActionDemote {
		t.Fatalf("demote audit %+v", es)
	}
	if _, err := e.svc.SetStage(ctx, o, "nope", "test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := e.svc.SetStage(ctx, o, "plain", "beta"); err == nil {
		t.Fatal("an unknown stage was taken")
	}
}
