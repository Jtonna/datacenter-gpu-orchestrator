package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// plantProfile stores a profile the way an older version could have: written
// to the configuration store directly (the path rule refuses it as a write),
// then read back by a new configuration service, as at the next start.
func (te *trashEnv) plantProfile(id, model string) {
	te.t.Helper()
	ctx := context.Background()
	d := te.svc.Document()
	if d.Profiles == nil {
		d.Profiles = map[string]*config.ProfileSpec{}
	}
	ctxLen := 2048
	d.Profiles[id] = &config.ProfileSpec{Model: model, Ctx: &ctxLen, NeedsMiB: 100}
	if _, _, err := te.cs.Commit(ctx, cfgstore.Change{Doc: d, Origin: audit.Origin{Source: "test", Actor: "older-version"}}); err != nil {
		te.t.Fatal(err)
	}
	svc := cfgsvc.New(ctx, cfgsvc.Options{Store: te.cs, Boot: config.Bootstrap{ModelsDir: te.dir},
		Apply:    func(c *config.Config) { te.sched.ApplyConfig(c) },
		OnChange: func(ev cfgsvc.ChangeEvent) { te.api.PublishConfig(ev) }})
	if e := svc.LoadError(); e != "" {
		te.t.Fatalf("the planted configuration was not used: %s", e)
	}
	if _, ok := svc.Document().Profiles[id]; !ok {
		te.t.Fatalf("profile %s not kept", id)
	}
	te.svc, te.api.opts.Config = svc, svc
	te.sched.ApplyConfig(svc.Config())
}

// While a stored profile names a file inside a trash folder (a path the
// path rule now refuses, kept from an older version), neither "Remove for
// good" nor the automatic clearing erases that folder's file: 409
// in_use_by_profile naming the profiles; the dry run says the same. Once
// the profile is gone, the removal goes through.
func TestModelRemove_RefusedWhileAProfileNamesTheTrash(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	id := te.trashed("a.gguf")
	idB := te.trashed("b.gguf")
	te.plantProfile("t1", ".trash/"+id+"/a.gguf")
	te.plantProfile("t2", "./.trash/"+id+"//a.gguf") // the same file by an uncleaned path
	remove := func(id, body string) (int, map[string]any) {
		return te.cdo("POST", "/api/models/trash/"+id+"/remove", body, trashUI)
	}
	for _, body := range []string{`{"comment":"x","dry_run":true}`, `{"comment":"x"}`} {
		st, m := remove(id, body)
		if st != 409 || errOf(m)["reason"] != "in_use_by_profile" {
			t.Fatalf("%s: %d %v", body, st, m)
		}
		msg, _ := errOf(m)["message"].(string)
		if !strings.Contains(msg, "t1") || !strings.Contains(msg, "t2") {
			t.Fatalf("the refusal does not name the profiles: %q", msg)
		}
		if ps, _ := errOf(m)["profiles"].([]any); len(ps) != 2 {
			t.Fatalf("profiles: %v", errOf(m)["profiles"])
		}
	}
	if !te.inTrash(id, "a.gguf") {
		t.Fatal("a refused removal erased the file")
	}
	if _, err := te.cs.RemovedGet(ctx, id); err == nil {
		t.Fatal("a tombstone for a refused removal")
	}

	// the clearing: due, purge on; it erases b's file and leaves a's
	te.setting(`{"model_trash_purge":true}`)
	te.advance(40 * 24 * time.Hour)
	got := te.api.clearTrash(ctx)
	if len(got) != 1 || got[0] != "b.gguf" || te.inTrash(idB, "b.gguf") {
		t.Fatalf("clearing: %v", got)
	}
	if !te.inTrash(id, "a.gguf") {
		t.Fatal("the clearing erased a file a profile names")
	}

	// the profiles go: the removal goes through
	for _, p := range []string{"t1", "t2"} {
		if st, m := te.cdo("DELETE", "/api/config/profiles/"+p, "", nil); st != 200 {
			t.Fatalf("delete profile %s: %d %v", p, st, m)
		}
	}
	if st, m := remove(id, `{"comment":"x"}`); st != 200 {
		t.Fatalf("after the profiles went: %d %v", st, m)
	}
	if te.inTrash(id, "a.gguf") {
		t.Fatal("not erased")
	}
}

// Slice 3's delete also counts a profile that names the file by another
// path that resolves to it (a link): loaded, it blocks the delete the way a
// profile naming the file does.
func TestModelTrash_BusyMatchesAProfileByAnyPathToTheFile(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	if err := os.Symlink(filepath.Join(te.dir, "a.gguf"), filepath.Join(te.dir, "link.gguf")); err != nil {
		t.Skipf("links cannot be made here: %v", err)
	}
	if st, m := te.cdo("POST", "/api/config/profiles", `{"id":"l1","model":"link.gguf","ctx":2048,"needs_mib":100}`, nil); st != 201 && st != 200 {
		t.Fatalf("profile l1: %d %v", st, m)
	}
	resp := te.post("/v1/chat/completions", `{"model":"l1","messages":[{"role":"user","content":"hi"}]}`)
	body(t, resp)
	eventually(t, "l1 loaded", func() bool {
		st, _ := te.sched.Status(context.Background())
		return len(st.Instances) > 0
	})
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "in_use" || !strings.Contains(errOf(m)["message"].(string), "l1 is") {
		t.Fatalf("loaded by a link: %d %v", st, m)
	}
	if !te.has("a.gguf") {
		t.Fatal("a refused delete moved the file")
	}
}
