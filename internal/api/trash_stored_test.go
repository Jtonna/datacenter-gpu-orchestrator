package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// liveCase is the live file: trained at 2,048 (known only after the
// profiles were written, as on live before v79), with stored profiles at
// 2,304, 8,192 and 32,768 and one at 2,048 with an alias.
var liveCase = []string{
	`{"id":"bench-tinyllama-1a-2304","model":"t.gguf","ctx":2304,"needs_mib":100}`,
	`{"id":"bench-tinyllama-1a-8192","model":"t.gguf","ctx":8192,"needs_mib":100}`,
	`{"id":"bench-tinyllama-1a-32768","model":"t.gguf","ctx":32768,"needs_mib":100}`,
	`{"id":"tiny","model":"t.gguf","ctx":2048,"needs_mib":100,"aliases":["tinyllama"]}`,
}

func (te *trashEnv) liveCase() map[string]*config.ProfileSpec {
	te.t.Helper()
	te.file("t.gguf", "GGUF-t")
	for _, p := range liveCase {
		if st, m := te.cdo("POST", "/api/config/profiles", p, nil); st != 201 && st != 200 {
			te.t.Fatalf("profile %s: %d %v", p, st, m)
		}
	}
	te.trained.Store("t.gguf", 2048)
	return cfgsvc.ProfilesOfFile(te.svc.Document(), "t.gguf")
}

const liveWarning = "3 of the 4 restored profiles of t.gguf have a context above the model's trained 2,048 and run at 2,048"

// The exact live case: delete, restore: all four back with the same
// fields, in one revision, and the restore's answer warns about the three
// above the trained context; never a refusal.
func TestModelRestore_StoredProfilesAboveTrained(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	want := te.liveCase()
	// an ordinary write of the same is refused (no hole)
	if st, _ := te.cdo("POST", "/api/config/profiles", `{"id":"bench-new","model":"t.gguf","ctx":8192,"needs_mib":100}`, nil); st == 200 || st == 201 {
		t.Fatal("a new profile above the trained context was accepted")
	}
	id := te.trashed("t.gguf")
	rev := te.svc.Revision()
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"first restore test"}`, trashUI)
	if st != 200 {
		t.Fatalf("restore: %d %v", st, m)
	}
	if got := cfgsvc.ProfilesOfFile(te.svc.Document(), "t.gguf"); !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles after the restore:\n got %s\nwant %s", js(got), js(want))
	}
	if int64(m["revision"].(float64)) != rev+1 || te.svc.Revision() != rev+1 {
		t.Fatalf("revisions: answer %v, now %d, before %d", m["revision"], te.svc.Revision(), rev)
	}
	ws, _ := m["warnings"].([]any)
	if len(ws) != 1 || !strings.Contains(ws[0].(string), liveWarning) || !strings.Contains(ws[0].(string), "bench-tinyllama-1a-32768") || strings.Contains(ws[0].(string), "tiny,") {
		t.Fatalf("warnings: %v", m["warnings"])
	}
	if !te.has("t.gguf") || !te.noRecords() {
		t.Fatalf("file %v, records left %v", te.has("t.gguf"), !te.noRecords())
	}
}

// rename_taken still works with such profiles: a taken id comes back as
// <id>-restored, still above the trained context, still a warning.
func TestModelRestore_StoredProfilesRenameTaken(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	want := te.liveCase()
	id := te.trashed("t.gguf")
	if st, m := te.cdo("POST", "/api/config/profiles", `{"id":"bench-tinyllama-1a-8192","model":"a.gguf","ctx":2048,"needs_mib":100}`, nil); st != 201 && st != 200 {
		t.Fatalf("taking the id: %d %v", st, m)
	}
	if st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI); st != 409 || errOf(m)["reason"] != "profiles_taken" {
		t.Fatalf("without rename: %d %v", st, m)
	}
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x","rename_taken":true}`, trashUI)
	if st != 200 {
		t.Fatalf("restore with rename_taken: %d %v", st, m)
	}
	got := te.svc.Document().Profiles["bench-tinyllama-1a-8192-restored"]
	if got == nil || !reflect.DeepEqual(got, want["bench-tinyllama-1a-8192"]) {
		t.Fatalf("renamed profile: %s", js(got))
	}
	ws, _ := m["warnings"].([]any)
	if len(ws) != 1 || !strings.Contains(ws[0].(string), liveWarning) || !strings.Contains(ws[0].(string), "bench-tinyllama-1a-8192-restored") {
		t.Fatalf("warnings: %v", m["warnings"])
	}
}

// The record already on live: written by v86 (state trashed, its profiles
// as v86 stores them), the file in .trash/<id>/; the new image restores it.
// After the restore the stored configuration has nothing an older image
// does not know: it loads with no unknown field and no load error.
func TestModelRestore_StoredRecordFromV86(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	te.trained.Store("t.gguf", 2048)
	const id = "4f3c2b1a0d9e"
	// exactly as v86 writes the profiles column (json of config.ProfileSpec)
	recJSON := `{"bench-tinyllama-1a-2304":{"model":"t.gguf","ctx":2304,"needs_mib":100},` +
		`"bench-tinyllama-1a-32768":{"model":"t.gguf","ctx":32768,"needs_mib":100},` +
		`"bench-tinyllama-1a-8192":{"model":"t.gguf","ctx":8192,"needs_mib":100},` +
		`"tiny":{"model":"t.gguf","ctx":2048,"needs_mib":100,"aliases":["tinyllama"]}}`
	if _, err := te.bs.DB().Exec(`INSERT INTO model_trash (id, file, size_bytes, deleted_at, purge_at, state, profiles, revision, actor, comment, updated_at)
		VALUES (?, 't.gguf', 6, 1795000000000, 1797592000000, 'trashed', ?, 3, 'owner', 'first delete test', 1795000000000)`, id, recJSON); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(te.dir, ".trash", id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(te.dir, ".trash", id, "t.gguf"), []byte("GGUF-t"), 0o644); err != nil {
		t.Fatal(err)
	}
	te.restart()
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"restore under the new image"}`, trashUI)
	if st != 200 {
		t.Fatalf("restore: %d %v", st, m)
	}
	var want map[string]*config.ProfileSpec
	if err := json.Unmarshal([]byte(recJSON), &want); err != nil {
		t.Fatal(err)
	}
	if got := cfgsvc.ProfilesOfFile(te.svc.Document(), "t.gguf"); !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles:\n got %s\nwant %s", js(got), js(want))
	}
	if ws, _ := m["warnings"].([]any); len(ws) != 1 || !strings.Contains(ws[0].(string), liveWarning) {
		t.Fatalf("warnings: %v", m["warnings"])
	}
	if _, err := te.cs.TrashGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) || !te.has("t.gguf") {
		t.Fatalf("record %v, file %v", err, te.has("t.gguf"))
	}
	// the older image reads the configuration: the document holds only what
	// was stored before the delete (no new field), so it loads
	svc2 := cfgsvc.New(ctx, cfgsvc.Options{Store: te.cs, Boot: config.Bootstrap{ModelsDir: te.dir}, Parse: config.Options{TrainedCtx: te.trainedCtx}})
	if e := svc2.LoadError(); e != "" || len(svc2.UnknownFields()) != 0 {
		t.Fatalf("load: %q unknown %v", e, svc2.UnknownFields())
	}
	b, _ := json.Marshal(svc2.Document().Profiles["bench-tinyllama-1a-32768"])
	if string(b) != `{"model":"t.gguf","ctx":32768,"needs_mib":100}` {
		t.Fatalf("stored profile as written: %s", b)
	}
}

// The delete's own put-backs (the rename failed: move_failed, read_only;
// something loaded it again: loaded_again) add the profiles back as stored:
// they cannot be refused for a ctx above the trained context, so the
// profiles are never lost and the file never moved.
func TestModelDelete_PutBackKeepsStoredProfiles(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"move_failed", "move_failed"},
		{"read_only", "read_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTrashEnvWith(t, func(te *trashEnv, step string) bool {
				if step == "delete:after-config" {
					if tc.reason == "read_only" {
						te.ro = true
					} else {
						te.renameErr = &fs.PathError{Op: "rename", Path: "t.gguf", Err: syscall.EXDEV}
					}
				}
				return false
			})
			te.setup()
			want := te.liveCase()
			st, m := te.cdo("POST", "/api/models/delete/t.gguf", `{"comment":"x"}`, trashUI)
			if st != 409 || errOf(m)["reason"] != tc.reason {
				t.Fatalf("delete: %d %v", st, m)
			}
			te.ro, te.renameErr = false, nil
			if got := cfgsvc.ProfilesOfFile(te.svc.Document(), "t.gguf"); !reflect.DeepEqual(got, want) {
				t.Fatalf("profiles not put back:\n got %s\nwant %s", js(got), js(want))
			}
			if !te.has("t.gguf") || !te.noRecords() {
				t.Fatalf("file %v, records left %v", te.has("t.gguf"), !te.noRecords())
			}
		})
	}
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

// loaded_again: a load started between the interrupt and the revision; the
// revision is undone by putting the profiles back, here with a stored
// profile above the trained context (a2, 8,192 on a file trained at 2,048).
func TestModelDelete_LoadedAgainPutsBackStoredProfiles(t *testing.T) {
	te := newTrashEnvWith(t, func(te *trashEnv, step string) bool {
		if step == "delete:after-interrupt" {
			te.chatAsync("a2")
			eventually(t, "a2 loading", func() bool { return te.instState("a2") == "starting" })
		}
		return false
	})
	te.setup()
	want := cfgsvc.ProfilesOfFile(te.svc.Document(), "a.gguf")
	te.trained.Store("a.gguf", 2048)
	body(t, te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`))
	eventually(t, "a1 ready", func() bool { return te.instState("a1") == "ready" })
	hold := make(chan struct{})
	defer close(hold)
	te.launcher.setHold(hold)
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","interrupt":true}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "loaded_again" {
		t.Fatalf("loaded again: %d %v", st, m)
	}
	if got := cfgsvc.ProfilesOfFile(te.svc.Document(), "a.gguf"); !reflect.DeepEqual(got, want) || !te.has("a.gguf") || !te.noRecords() {
		t.Fatalf("after: profiles %s, file %v", js(got), te.has("a.gguf"))
	}
}

// A name taken meanwhile: by default the restore is refused, nothing moves,
// and the sentence names both ways (rename_taken, hold_back_refused). With
// hold_back_refused the file comes back with the other profiles; the taken
// ones stay in the record (state held_back, listed with why, not counted as
// trash); a second restore adds them once the name is free.
func TestModelRestore_TakenNameHoldBack(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	want := te.liveCase()
	id := te.trashed("t.gguf")
	// the id of one and the alias of another are taken meanwhile
	if st, m := te.cdo("POST", "/api/config/profiles", `{"id":"bench-tinyllama-1a-8192","model":"a.gguf","ctx":2048,"needs_mib":100,"aliases":["tinyllama"]}`, nil); st != 201 && st != 200 {
		t.Fatalf("taking the names: %d %v", st, m)
	}
	rev := te.svc.Revision()
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI)
	msg, _ := errOf(m)["message"].(string)
	if st != 409 || errOf(m)["reason"] != "profiles_taken" || !strings.Contains(msg, "rename_taken: true") || !strings.Contains(msg, "hold_back_refused: true") || !strings.Contains(msg, "Nothing was restored") {
		t.Fatalf("default: %d %v", st, m)
	}
	if te.has("t.gguf") || te.svc.Revision() != rev {
		t.Fatal("something moved")
	}
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x","hold_back_refused":true}`, trashUI)
	if st != 200 || m["state"] != "held_back" {
		t.Fatalf("hold back: %d %v", st, m)
	}
	held, _ := m["held_back"].([]any)
	if len(held) != 2 || held[0].(map[string]any)["id"] != "bench-tinyllama-1a-8192" || held[1].(map[string]any)["id"] != "tiny" ||
		!strings.Contains(held[1].(map[string]any)["reason"].(string), "alias tinyllama") {
		t.Fatalf("held back: %v", m["held_back"])
	}
	if !te.has("t.gguf") || te.svc.Revision() != rev+1 {
		t.Fatalf("file %v, revision %d (was %d)", te.has("t.gguf"), te.svc.Revision(), rev)
	}
	doc := te.svc.Document()
	for _, pid := range []string{"bench-tinyllama-1a-2304", "bench-tinyllama-1a-32768"} {
		if !reflect.DeepEqual(doc.Profiles[pid], want[pid]) {
			t.Fatalf("%s: %s", pid, js(doc.Profiles[pid]))
		}
	}
	if doc.Profiles["tiny"] != nil || doc.Profiles["bench-tinyllama-1a-8192"].Model != "a.gguf" {
		t.Fatal("a held-back profile was written")
	}
	rec, err := te.cs.TrashGet(ctx, id)
	if err != nil || rec.State != cfgstore.TrashHeldBack || len(rec.Profiles) != 2 || !reflect.DeepEqual(rec.Profiles["tiny"], want["tiny"]) ||
		!strings.Contains(rec.Problem, "2 profiles were held back") {
		t.Fatalf("record: %+v %v", rec, err)
	}
	_, l := te.cdo("GET", "/api/models/trash", "", nil)
	items, _ := l["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["state"] != "held_back" || items[0].(map[string]any)["profile_count"] != float64(2) ||
		l["count"] != float64(0) || l["total_bytes"] != float64(0) {
		t.Fatalf("list: %v", l)
	}
	// remove for good is refused for it (the file is restored)
	if st, _ := te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI); st == 200 || !te.has("t.gguf") {
		t.Fatalf("remove for good of a held-back record: %d", st)
	}
	// a second call while the names are still taken: nothing to add yet
	if st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"again","hold_back_refused":true}`, trashUI); st != 200 || m["state"] != "held_back" {
		t.Fatalf("second call: %d %v", st, m)
	}
	// free the names, restore again: the record goes
	if st, m := te.cdo("DELETE", "/api/config/profiles/bench-tinyllama-1a-8192", "", nil); st != 200 {
		t.Fatalf("freeing: %d %v", st, m)
	}
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"names free"}`, trashUI)
	if st != 200 || m["state"] != "restored" {
		t.Fatalf("after freeing: %d %v", st, m)
	}
	if got := cfgsvc.ProfilesOfFile(te.svc.Document(), "t.gguf"); !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles:\n got %s\nwant %s", js(got), js(want))
	}
	if _, err := te.cs.TrashGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
		t.Fatalf("record: %v", err)
	}
}

// A restore after a restart of the scheduler: a new configuration service
// and a new server read everything from the store, then restore.
func TestModelRestore_AfterRestart(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	want := te.liveCase()
	id := te.trashed("t.gguf")
	svc2 := cfgsvc.New(context.Background(), cfgsvc.Options{Store: te.cs, Boot: config.Bootstrap{ModelsDir: te.dir}, Parse: config.Options{TrainedCtx: te.trainedCtx}})
	if e := svc2.LoadError(); e != "" {
		t.Fatalf("load: %s", e)
	}
	s2 := New(te.cfg, te.sched, Options{Config: svc2, Audit: te.cs, BenchStore: te.bs, Logger: te.api.log, Now: te.api.opts.Now, History: te.hist, trashClearEvery: -1})
	t.Cleanup(s2.Close)
	s2.RepairTrash(context.Background())
	rq := httptest.NewRequest("POST", "/api/models/trash/"+id+"/restore", strings.NewReader(`{"comment":"after a restart"}`))
	for k, v := range trashUI {
		rq.Header.Set(k, v)
	}
	rw := httptest.NewRecorder()
	s2.ServeHTTP(rw, rq)
	var m map[string]any
	json.Unmarshal(rw.Body.Bytes(), &m)
	if rw.Code != 200 || m["state"] != "restored" {
		t.Fatalf("restore after a restart: %d %v", rw.Code, m)
	}
	if got := cfgsvc.ProfilesOfFile(svc2.Document(), "t.gguf"); !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles:\n got %s\nwant %s", js(got), js(want))
	}
	if ws, _ := m["warnings"].([]any); len(ws) != 1 || !strings.Contains(ws[0].(string), liveWarning) {
		t.Fatalf("warnings: %v", m["warnings"])
	}
}

// The delete's dry run says what a later restore meets: every profile comes
// back as it is, with the ones a new write would refuse named (a note).
func TestModelDelete_DryRunRestoreNote(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	te.liveCase()
	st, m := te.cdo("POST", "/api/models/delete/t.gguf", `{"comment":"x","dry_run":true}`, trashUI)
	note, _ := m["restore_note"].(string)
	if st != 200 || !strings.HasPrefix(note, "all 4 profiles will come back as they are; 3 of the 4 profiles of t.gguf have a context above the model's trained 2,048 and run at 2,048") {
		t.Fatalf("dry run: %d %q", st, note)
	}
	if st, m := te.cdo("POST", "/api/models/delete/b.gguf", `{"comment":"x","dry_run":true}`, trashUI); st != 200 || m["restore_note"] != "its 1 profile will come back as it is" {
		t.Fatalf("b.gguf dry run: %d %v", st, m["restore_note"])
	}
}

// Any rule that refuses a profile even as stored (here a managed flag in
// extra_args, which no stored profile may keep) never keeps the file in the
// trash: the file comes back, the other profiles are added, the refused one
// is held back in the record with the refusal, without any option.
func TestModelRestore_RuleRefusalHoldsBack(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	const id = "5a4b3c2d1e0f"
	recJSON := `{"good":{"model":"t.gguf","ctx":2048,"needs_mib":100},"bad":{"model":"t.gguf","ctx":2048,"needs_mib":100,"extra_args":["--port","9"]}}`
	if _, err := te.bs.DB().Exec(`INSERT INTO model_trash (id, file, size_bytes, deleted_at, purge_at, state, profiles, revision, actor, comment, updated_at)
		VALUES (?, 't.gguf', 6, 1795000000000, 1797592000000, 'trashed', ?, 3, 'owner', 'x', 1795000000000)`, id, recJSON); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(te.dir, ".trash", id), 0o755)
	os.WriteFile(filepath.Join(te.dir, ".trash", id, "t.gguf"), []byte("GGUF-t"), 0o644)
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI)
	if st != 200 || m["state"] != "held_back" || !te.has("t.gguf") {
		t.Fatalf("restore: %d %v (file %v)", st, m, te.has("t.gguf"))
	}
	held, _ := m["held_back"].([]any)
	if len(held) != 1 || held[0].(map[string]any)["id"] != "bad" || !strings.Contains(held[0].(map[string]any)["reason"].(string), "managed flag") {
		t.Fatalf("held back: %v", m["held_back"])
	}
	if te.svc.Document().Profiles["good"] == nil || te.svc.Document().Profiles["bad"] != nil {
		t.Fatal("profiles")
	}
	if rec, err := te.cs.TrashGet(ctx, id); err != nil || rec.State != cfgstore.TrashHeldBack || rec.Profiles["bad"] == nil || len(rec.Profiles) != 1 {
		t.Fatalf("record: %+v %v", rec, err)
	}
}

// A hold-back restore killed after the file moved back: the start adds the
// planned profiles and keeps the held-back one in the record.
func TestModelRestore_HoldBackKilledAfterMove(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	te.liveCase()
	id := te.trashed("t.gguf")
	if st, m := te.cdo("POST", "/api/config/profiles", `{"id":"tiny","model":"a.gguf","ctx":2048,"needs_mib":100}`, nil); st != 201 && st != 200 {
		t.Fatalf("taking the id: %d %v", st, m)
	}
	te.stop = "restore:after-rename"
	te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x","hold_back_refused":true}`, trashUI)
	te.stop = ""
	te.restart()
	rec, err := te.cs.TrashGet(ctx, id)
	if err != nil || rec.State != cfgstore.TrashHeldBack || len(rec.Profiles) != 1 || rec.Profiles["tiny"] == nil || !te.has("t.gguf") {
		t.Fatalf("record after the start: %+v %v", rec, err)
	}
	if got := cfgsvc.ProfilesOfFile(te.svc.Document(), "t.gguf"); len(got) != 3 {
		t.Fatalf("profiles added at start: %s", js(got))
	}
}
