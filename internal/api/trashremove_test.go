package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// trashed deletes file (a comment from alex) and returns its trash id.
func (te *trashEnv) trashed(file string) string {
	te.t.Helper()
	st, m := te.cdo("POST", "/api/models/delete/"+file, `{"comment":"not used"}`, trashUI)
	if st != 200 {
		te.t.Fatalf("delete %s: %d %v", file, st, m)
	}
	return m["trash_id"].(string)
}

func (te *trashEnv) setting(body string) {
	te.t.Helper()
	if st, m := te.cdo("PATCH", "/api/config/settings", body, nil); st != 200 {
		te.t.Fatalf("settings %s: %d %v", body, st, m)
	}
}

func (te *trashEnv) removedEvents() []history.Event {
	return te.hist.Query(history.Query{Types: []string{history.TModelRemoved}, Limit: 100}).Events
}

func (te *trashEnv) inTrash(id, file string) bool { return te.has(filepath.Join(".trash", id, file)) }

// Remove for good: the dry run answers what is freed and that it cannot be
// undone; the removal erases only .trash/<id>/<file> (a new file of the
// same name in the folder stays), keeps a tombstone listed apart from the
// trash, and is audited (section models, action remove) and in the history.
func TestModelRemove_ForGood(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	id := te.trashed("a.gguf")
	te.file("a.gguf", "a new file of the same name")
	rev := te.svc.Revision()

	// refused: no comment, a bad id, an unknown id
	if st, m := te.cdo("POST", "/api/models/trash/"+id+"/remove", ``, trashUI); st != 400 {
		t.Fatalf("no comment: %d %v", st, m)
	}
	for _, bad := range []string{"..%2F..%2Fa.gguf", "a.gguf", "0123456789AB", "%2E%2E"} {
		if st, m := te.cdo("POST", "/api/models/trash/"+bad+"/remove", `{"comment":"x"}`, trashUI); st != 404 {
			t.Errorf("%s: %d %v", bad, st, m)
		}
	}
	if st, _ := te.cdo("POST", "/api/models/trash/0123456789ab/remove", `{"comment":"x"}`, trashUI); st != 404 {
		t.Fatalf("unknown id: %d", st)
	}
	// dry run: the figures, nothing changes
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x","dry_run":true}`, trashUI)
	if st != 200 || m["dry_run"] != true || m["size_bytes"] != float64(6) || len(m["profiles"].([]any)) != 2 || m["cannot_undo"] != true ||
		m["days_early"] != float64(30) || m["read_only"] != false || !strings.Contains(m["message"].(string), "cannot be undone") {
		t.Fatalf("dry run: %d %v", st, m)
	}
	if !te.inTrash(id, "a.gguf") {
		t.Fatal("the dry run removed the file")
	}
	// remove
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"free the space"}`, trashUI)
	if st != 200 || m["freed_bytes"] != float64(6) || len(m["profiles_removed"].([]any)) != 2 || m["reason"] != "owner" {
		t.Fatalf("remove: %d %v", st, m)
	}
	if te.inTrash(id, "a.gguf") || te.has(filepath.Join(".trash", id)) {
		t.Fatal("the trashed file or its folder is still there")
	}
	if b, _ := os.ReadFile(filepath.Join(te.dir, "a.gguf")); string(b) != "a new file of the same name" {
		t.Fatalf("the file of the same name in the folder: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(te.dir, "b.gguf")); string(b) != "GGUF-b" {
		t.Fatalf("b.gguf: %q", b)
	}
	if te.svc.Revision() != rev {
		t.Fatal("a removal wrote a configuration revision")
	}
	// the trash list: no item; the tombstone apart
	_, m = te.cdo("GET", "/api/models/trash", "", nil)
	if len(m["items"].([]any)) != 0 || m["count"] != float64(0) {
		t.Fatalf("items after remove: %v", m)
	}
	rm := m["removed"].([]any)
	if len(rm) != 1 {
		t.Fatalf("removed: %v", m["removed"])
	}
	tb := rm[0].(map[string]any)
	if tb["id"] != id || tb["file"] != "a.gguf" || tb["state"] != "removed" || tb["reason"] != "owner" || tb["removed_by"] != "alex" || tb["comment"] != "free the space" ||
		tb["deleted_by"] != "alex" || len(tb["profile_ids"].([]any)) != 2 || tb["removed_at"] == nil || tb["profiles"] != nil {
		t.Fatalf("tombstone: %v", tb)
	}
	if c := catalogKeys(t, te.env); c["__disk"]["trash_count"] != float64(0) || c["__disk"]["trash_bytes"] != float64(0) {
		t.Fatalf("disk: %v", c["__disk"])
	}
	// audit and history
	es := auditOf(t, te.env, "section=models")
	if es[0]["action"] != "remove" || es[0]["target"] != "model:a.gguf" || es[0]["actor"] != "alex" || es[0]["comment"] != "free the space" {
		t.Fatalf("audit: %v", es[0])
	}
	if ev := te.removedEvents(); len(ev) != 1 || ev[0].ModelFile != "a.gguf" || ev[0].Data["reason"] != "owner" {
		t.Fatalf("history: %+v", ev)
	}
	// again: gone; restore: no record
	if st, m := te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI); st != 410 || errOf(m)["reason"] != "already_removed" {
		t.Fatalf("again: %d %v", st, m)
	}
	if st, _ := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI); st != 404 {
		t.Fatalf("restore of a removed file: %d", st)
	}
	if ents, _ := os.ReadDir(te.outer); len(ents) != 0 {
		t.Fatalf("outside the models folder: %v", ents)
	}
}

// What is not removed, with nothing changed: a record that needs a look
// (its own answer), one under way, a file missing from its trash folder, a
// file of another size than the one deleted, a file that is a directory.
func TestModelRemove_Refused(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	te.file("c.gguf", "GGUF-c")
	idA, idB, idC := te.trashed("a.gguf"), te.trashed("b.gguf"), te.trashed("c.gguf")
	remove := func(id string) (int, map[string]any) {
		return te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
	}

	te.cs.TrashSetState(idA, cfgstore.TrashInterrupted, 0, nil, "a restore stopped and the file is not in exactly one place", 1)
	st, m := remove(idA)
	if st != 409 || errOf(m)["reason"] != "needs_a_look" || errOf(m)["in_trash_folder"] != true || errOf(m)["in_folder"] != false ||
		!strings.Contains(errOf(m)["message"].(string), "Nothing was changed") {
		t.Fatalf("interrupted: %d %v", st, m)
	}
	te.cs.TrashSetState(idA, cfgstore.TrashPending, 0, nil, "", 1)
	if st, m := remove(idA); st != 409 || errOf(m)["reason"] != "busy" {
		t.Fatalf("pending: %d %v", st, m)
	}
	if !te.inTrash(idA, "a.gguf") {
		t.Fatal("a refused removal erased the file")
	}
	// another size than the one deleted
	te.file(filepath.Join(".trash", idB, "b.gguf"), "GGUF-b plus more")
	if st, m := remove(idB); st != 409 || errOf(m)["reason"] != "changed" {
		t.Fatalf("changed: %d %v", st, m)
	}
	if !te.inTrash(idB, "b.gguf") {
		t.Fatal("removed a changed file")
	}
	// a directory where the file was
	os.Remove(filepath.Join(te.dir, ".trash", idC, "c.gguf"))
	os.Mkdir(filepath.Join(te.dir, ".trash", idC, "c.gguf"), 0o755)
	te.file(filepath.Join(".trash", idC, "c.gguf", "inner"), "x")
	if st, m := remove(idC); st != 409 || errOf(m)["reason"] != "not_regular" {
		t.Fatalf("directory: %d %v", st, m)
	}
	if !te.has(filepath.Join(".trash", idC, "c.gguf", "inner")) {
		t.Fatal("the directory's content went")
	}
	// missing
	os.RemoveAll(filepath.Join(te.dir, ".trash", idC, "c.gguf"))
	if st, m := remove(idC); st != 409 || errOf(m)["reason"] != "missing_from_trash" {
		t.Fatalf("missing: %d %v", st, m)
	}
	for _, id := range []string{idA, idB, idC} {
		if _, err := te.cs.RemovedGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("a tombstone for a refused removal: %s %v", id, err)
		}
	}
	if es := auditOf(t, te.env, "section=models&action=remove"); len(es) != 0 {
		t.Fatalf("audit of refusals: %v", es)
	}
}

// A read-only models folder: the removal answers 409 read_only before
// anything is written (no tombstone, the record stays trashed, the file
// stays); the dry run says read_only.
func TestModelRemove_ReadOnly(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	id := te.trashed("a.gguf")
	te.ro = true
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x","dry_run":true}`, trashUI)
	if st != 200 || m["read_only"] != true {
		t.Fatalf("dry run, read-only: %d %v", st, m)
	}
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "read_only" || !readOnlyOnce(errOf(m)["message"].(string), "access") {
		t.Fatalf("read-only: %d %v", st, m)
	}
	if r, _ := te.cs.TrashGet(ctx, id); r.State != cfgstore.TrashTrashed || r.Problem != "" {
		t.Fatalf("record: %+v", r)
	}
	if _, err := te.cs.RemovedGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
		t.Fatalf("tombstone: %v", err)
	}
	if !te.inTrash(id, "a.gguf") {
		t.Fatal("erased on a read-only folder")
	}
}

// A kill at each step of a removal; the next start settles it from whether
// the file is still there, and never erases a file itself.
func TestModelRemove_KillAndRepair(t *testing.T) {
	ctx := context.Background()
	t.Run("killed before the record is written", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		te.stop = "remove:before-record"
		te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
		te.stop = ""
		s2 := te.restart()
		if r, _ := te.cs.TrashGet(ctx, id); r == nil || r.State != cfgstore.TrashTrashed {
			t.Fatalf("after the start: %+v", r)
		}
		if _, err := te.cs.RemovedGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("tombstone: %v", err)
		}
		if !te.inTrash(id, "a.gguf") || len(s2.trash.repairList()) != 0 {
			t.Fatalf("file %v, repairs %v", te.inTrash(id, "a.gguf"), s2.trash.repairList())
		}
	})
	t.Run("killed after the record, before the unlink", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		te.stop = "remove:after-record"
		te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
		te.stop = ""
		if r, _ := te.cs.TrashGet(ctx, id); r.State != cfgstore.TrashRemoving {
			t.Fatalf("before the start: %+v", r)
		}
		s2 := te.restart()
		if r, _ := te.cs.TrashGet(ctx, id); r == nil || r.State != cfgstore.TrashTrashed {
			t.Fatalf("after the start: %+v", r)
		}
		if _, err := te.cs.RemovedGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("tombstone: %v", err)
		}
		if !te.inTrash(id, "a.gguf") {
			t.Fatal("the start erased the file")
		}
		if n := s2.trash.repairList(); len(n) != 1 || !strings.Contains(n[0], "stays in the trash") {
			t.Fatalf("repairs: %v", n)
		}
		// and it can be removed now
		if st, m := te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI); st != 200 {
			t.Fatalf("remove after the repair: %d %v", st, m)
		}
	})
	t.Run("killed after the unlink, before the record is completed", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		te.stop = "remove:after-unlink"
		te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"free it"}`, trashUI)
		te.stop = ""
		if te.inTrash(id, "a.gguf") {
			t.Fatal("not erased")
		}
		s2 := te.restart()
		if _, err := te.cs.TrashGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("record after the start: %v", err)
		}
		if tb, err := te.cs.RemovedGet(ctx, id); err != nil || tb.State != cfgstore.RemovedRemoved || tb.RemovedBy != "alex" {
			t.Fatalf("tombstone: %+v %v", tb, err)
		}
		if te.has(filepath.Join(".trash", id)) {
			t.Fatal("the empty trash folder stayed")
		}
		if n := s2.trash.repairList(); len(n) != 1 || !strings.Contains(n[0], "completed") {
			t.Fatalf("repairs: %v", n)
		}
		es := auditOf(t, te.env, "section=models&action=remove")
		if len(es) != 1 || es[0]["actor"] != "potluck" || !strings.Contains(es[0]["comment"].(string), "completed at start") {
			t.Fatalf("audit: %v", es)
		}
		if ev := te.removedEvents(); len(ev) != 1 {
			t.Fatalf("history: %+v", ev)
		}
	})
	t.Run("killed after the record is completed", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		te.stop = "remove:after-finish"
		te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
		te.stop = ""
		s2 := te.restart()
		if _, err := te.cs.TrashGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("record: %v", err)
		}
		if tb, err := te.cs.RemovedGet(ctx, id); err != nil || tb.State != cfgstore.RemovedRemoved || tb.RemovedAt == 0 {
			t.Fatalf("tombstone: %+v %v", tb, err)
		}
		// the kill came before the event and the audit entry: the start writes
		// both, signed potluck, saying so, and sets the mark
		es := auditOf(t, te.env, "section=models&action=remove")
		if len(es) != 1 || es[0]["actor"] != "potluck" || !strings.Contains(es[0]["comment"].(string), "written at start for a removal made at") || !strings.Contains(es[0]["comment"].(string), "by alex") {
			t.Fatalf("audit: %v", es)
		}
		ev := te.removedEvents()
		if len(ev) != 1 || ev[0].Data["written_at_start"] != true || !strings.Contains(ev[0].Summary, "Written at start") {
			t.Fatalf("history: %+v", ev)
		}
		if tb, _ := te.cs.RemovedGet(ctx, id); !tb.Audited {
			t.Fatalf("mark not set: %+v", tb)
		}
		if n := s2.trash.repairList(); len(n) != 1 || !strings.Contains(n[0], "written at start") {
			t.Fatalf("repairs: %v", n)
		}
		// a second start writes nothing
		s3 := te.restart()
		if es := auditOf(t, te.env, "section=models&action=remove"); len(es) != 1 {
			t.Fatalf("audit after a second start: %v", es)
		}
		if ev := te.removedEvents(); len(ev) != 1 {
			t.Fatalf("history after a second start: %+v", ev)
		}
		if n := s3.trash.repairList(); len(n) != 0 {
			t.Fatalf("repairs at the second start: %v", n)
		}
	})
	t.Run("the clearing killed after the unlink", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		te.setting(`{"model_trash_purge":true}`)
		te.advance(31 * 24 * time.Hour)
		te.stop = "remove:after-unlink"
		te.api.clearTrash(ctx)
		te.stop = ""
		if te.inTrash(id, "a.gguf") {
			t.Fatal("not erased")
		}
		if r, _ := te.cs.TrashGet(ctx, id); r == nil || r.State != cfgstore.TrashRemoving {
			t.Fatalf("before the start: %+v", r)
		}
		s2 := te.restart()
		if _, err := te.cs.TrashGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("record after the start: %v", err)
		}
		if tb, err := te.cs.RemovedGet(ctx, id); err != nil || tb.State != cfgstore.RemovedRemoved || tb.Reason != cfgstore.RemovedByClearing {
			t.Fatalf("tombstone: %+v %v", tb, err)
		}
		if n := s2.trash.repairList(); len(n) != 1 || !strings.Contains(n[0], "completed") {
			t.Fatalf("repairs: %v", n)
		}
	})
	// Not a state a kill can produce (step 1 writes the record's removing and
	// the tombstone in one transaction), but a damaged or hand-edited
	// database can: the record removing, no tombstone, the file gone. The
	// start completes it (a tombstone with reason repair, the event, an audit
	// entry signed potluck), once: it never stays removing.
	t.Run("record removing, no tombstone, file gone (hand-made)", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		if err := te.cs.TrashSetState(id, cfgstore.TrashRemoving, 0, nil, "", 1); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(te.dir, ".trash", id, "a.gguf"))
		s2 := te.restart()
		if _, err := te.cs.TrashGet(ctx, id); !errors.Is(err, cfgstore.ErrNotFound) {
			t.Fatalf("the record stayed after the start: %v", err)
		}
		tb, err := te.cs.RemovedGet(ctx, id)
		if err != nil || tb.State != cfgstore.RemovedRemoved || tb.Reason != "repaired at start" || tb.RemovedBy != "potluck" || tb.File != "a.gguf" || tb.RemovedAt == 0 {
			t.Fatalf("tombstone: %+v %v", tb, err)
		}
		if n := s2.trash.repairList(); len(n) != 1 || !strings.Contains(n[0], "completed") {
			t.Fatalf("repairs: %v", n)
		}
		es := auditOf(t, te.env, "section=models&action=remove")
		if len(es) != 1 || es[0]["actor"] != "potluck" {
			t.Fatalf("audit: %v", es)
		}
		if ev := te.removedEvents(); len(ev) != 1 {
			t.Fatalf("history: %+v", ev)
		}
		if s3 := te.restart(); len(s3.trash.repairList()) != 0 {
			t.Fatalf("a second start repaired again: %v", s3.trash.repairList())
		}
		_, m := te.cdo("GET", "/api/models/trash", "", nil)
		if items, _ := m["items"].([]any); len(items) != 0 {
			t.Fatalf("still listed in the trash: %v", items)
		}
	})
	t.Run("killed after the record, the file now somewhere else", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		id := te.trashed("a.gguf")
		te.stop = "remove:after-record"
		te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
		te.stop = ""
		os.Rename(filepath.Join(te.dir, ".trash", id, "a.gguf"), filepath.Join(te.dir, "a.gguf")) // moved back by hand
		te.restart()
		if r, _ := te.cs.TrashGet(ctx, id); r == nil || r.State != cfgstore.TrashInterrupted || r.Problem == "" {
			t.Fatalf("after the start: %+v", r)
		}
		if b, _ := os.ReadFile(filepath.Join(te.dir, "a.gguf")); string(b) != "GGUF-a" {
			t.Fatalf("the file: %q", b)
		}
	})
}

// The timed clearing: off unless model_trash_purge; a file is cleared at the
// later of the date it was given and its deletion plus model_trash_days now
// (raising the setting keeps files longer, lowering it never clears one
// sooner); at most trashClearPerPass a pass, the soonest first; each with a
// history event and an audit entry signed potluck; nothing on a read-only
// folder, and that is logged once, not each pass.
func TestTrashClearing(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	day := 24 * time.Hour
	idA := te.trashed("a.gguf") // given t0+30
	te.setting(`{"model_trash_days":7}`)
	idB := te.trashed("b.gguf") // given t0+7

	te.advance(40 * day)
	if got := te.api.clearTrash(ctx); len(got) != 0 {
		t.Fatalf("cleared with model_trash_purge off: %v", got)
	}
	_, m := te.cdo("GET", "/api/models/trash", "", nil)
	if m["purge_on"] != false || !te.inTrash(idA, "a.gguf") {
		t.Fatalf("off: %v", m["purge_on"])
	}
	te.advance(-32 * day) // t0+8

	te.setting(`{"model_trash_purge":true,"model_trash_days":30}`)
	_, m = te.cdo("GET", "/api/models/trash", "", nil)
	if m["purge_on"] != true || catalogKeys(t, te.env)["__disk"]["trash_purge_on"] != true {
		t.Fatalf("on: %v", m)
	}
	// raised to 30: B, given t0+7, now waits until t0+30
	if got := te.api.clearTrash(ctx); len(got) != 0 {
		t.Fatalf("cleared with the date raised: %v", got)
	}
	// lowered to 7: A, given t0+30, keeps its date; B is due
	te.setting(`{"model_trash_days":7}`)
	if got := te.api.clearTrash(ctx); strings.Join(got, ",") != "b.gguf" {
		t.Fatalf("cleared at t0+8 with 7 days: %v", got)
	}
	if !te.inTrash(idA, "a.gguf") || te.inTrash(idB, "b.gguf") {
		t.Fatal("wrong file cleared")
	}
	_, m = te.cdo("GET", "/api/models/trash", "", nil)
	if it := m["items"].([]any)[0].(map[string]any); it["purge_at"] != float64(te.api.opts.Now().Add(22*day).UnixMilli()) {
		t.Fatalf("A's date: %v", it["purge_at"])
	}
	te.advance(22 * day) // t0+30
	if got := te.api.clearTrash(ctx); strings.Join(got, ",") != "a.gguf" {
		t.Fatalf("cleared at t0+30: %v", got)
	}

	// audit (potluck, system) and history, one per file
	es := auditOf(t, te.env, "section=models&action=remove")
	if len(es) != 2 || es[0]["actor"] != "potluck" || es[0]["source"] != "system" || !strings.Contains(es[0]["comment"].(string), "cleared after") {
		t.Fatalf("audit: %v", es)
	}
	if ev := te.removedEvents(); len(ev) != 2 || ev[0].Data["reason"] != "clearing" {
		t.Fatalf("history: %+v", ev)
	}
	_, m = te.cdo("GET", "/api/models/trash", "", nil)
	if len(m["removed"].([]any)) != 2 || m["removed"].([]any)[0].(map[string]any)["reason"] != "clearing" {
		t.Fatalf("tombstones: %v", m["removed"])
	}

	// at most trashClearPerPass a pass, the soonest first
	var ids []string
	for _, f := range []string{"c.gguf", "d.gguf", "e.gguf", "f.gguf"} {
		te.file(f, "GGUF")
		ids = append(ids, te.trashed(f))
		te.advance(time.Hour)
	}
	te.advance(8 * day)
	// read-only: nothing, logged once
	te.ro = true
	if got := te.api.clearTrash(ctx); len(got) != 0 {
		t.Fatalf("cleared on a read-only folder: %v", got)
	}
	te.api.clearTrash(ctx)
	if n := strings.Count(te.logs.String(), "clearing waits"); n != 1 {
		t.Fatalf("read-only logged %d times", n)
	}
	if strings.Contains(te.logs.String(), "level=WARN msg=\"the models trash clearing") {
		t.Fatal("read-only warned")
	}
	for i, id := range ids {
		if r, _ := te.cs.TrashGet(ctx, id); r == nil || r.State != cfgstore.TrashTrashed {
			t.Fatalf("record %d on a read-only folder: %+v", i, r)
		}
	}
	te.ro = false
	if got := te.api.clearTrash(ctx); strings.Join(got, ",") != "c.gguf,d.gguf,e.gguf" {
		t.Fatalf("first pass: %v", got)
	}
	if got := te.api.clearTrash(ctx); strings.Join(got, ",") != "f.gguf" {
		t.Fatalf("second pass: %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(te.dir, "a.gguf")); b != nil {
		t.Fatalf("a.gguf at the top: %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Join(te.dir, ".trash")); len(ents) != 0 {
		t.Fatalf("trash folders left: %v", ents)
	}
}

// The clearing's "waits" line (Info, once) is written by a pass that finds a
// due file on a read-only folder: the first pass runs at start, right after
// the start's repair, then hourly. With nothing due there is nothing to
// wait for and no line, purge on or not.
func TestTrashClearing_ReadOnlyLineOnlyWhenSomethingIsDue(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	te.trashed("a.gguf")
	te.setting(`{"model_trash_purge":true}`)
	te.ro = true
	te.api.clearTrash(ctx) // nothing due yet
	if strings.Contains(te.logs.String(), "clearing waits") {
		t.Fatal("a waits line with nothing due")
	}
	te.advance(31 * 24 * time.Hour)
	te.api.clearTrash(ctx) // now a.gguf is due
	if n := strings.Count(te.logs.String(), "clearing waits"); n != 1 {
		t.Fatalf("waits lines: %d", n)
	}
}

// Before model_trash_purge is turned on the owner sees, per item, the date it
// would be cleared (purge_at: the later of the date it was given at the
// delete and its deletion plus model_trash_days now), the list says
// purge_on false, and nothing is cleared, even when the date has passed.
func TestTrashList_ClearingDateShownWhilePurgeOff(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	id := te.trashed("a.gguf")
	rec, _ := te.cs.TrashGet(ctx, id)
	want := rec.DeletedAt + 30*dayMS
	_, m := te.cdo("GET", "/api/models/trash", "", nil)
	items, _ := m["items"].([]any)
	if m["purge_on"] != false || len(items) != 1 || int64(items[0].(map[string]any)["purge_at"].(float64)) != want || int64(m["next_purge_at"].(float64)) != want || m["trash_days"] != float64(30) {
		t.Fatalf("list with purge off: %v", m)
	}
	te.setting(`{"model_trash_days":60}`) // raised: the date moves with it
	_, m = te.cdo("GET", "/api/models/trash", "", nil)
	if got := int64(m["items"].([]any)[0].(map[string]any)["purge_at"].(float64)); got != rec.DeletedAt+60*dayMS {
		t.Fatalf("after raising model_trash_days: %d", got)
	}
	te.advance(90 * 24 * time.Hour) // overdue
	if got := te.api.clearTrash(ctx); len(got) != 0 || !te.inTrash(id, "a.gguf") {
		t.Fatalf("cleared while purge is off: %v", got)
	}
}

// Every removal path sets the audited mark itself, so a start adds nothing:
// the owner's removal, the clearing, and the start's own completion of a
// removal a kill cut after the unlink.
func TestModelRemove_EveryPathMarksAudited(t *testing.T) {
	ctx := context.Background()
	te := newTrashEnv(t)
	te.setup()
	a := te.trashed("a.gguf")
	b := te.trashed("b.gguf")
	te.file("c.gguf", "GGUF-c")
	c := te.trashed("c.gguf")
	if st, m := te.cdo("POST", "/api/models/trash/"+a+"/remove", `{"comment":"x"}`, trashUI); st != 200 {
		t.Fatalf("remove: %d %v", st, m)
	}
	te.stop = "remove:after-unlink"
	te.cdo("POST", "/api/models/trash/"+c+"/remove", `{"comment":"x"}`, trashUI)
	te.stop = ""
	te.setting(`{"model_trash_purge":true}`)
	te.advance(31 * 24 * time.Hour)
	te.api.clearTrash(ctx) // b
	for _, id := range []string{a, b} {
		if tb, err := te.cs.RemovedGet(ctx, id); err != nil || !tb.Audited {
			t.Fatalf("%s: %+v %v", id, tb, err)
		}
	}
	te.setting(`{"model_trash_purge":false}`)
	s2 := te.restart() // completes c, marked by the completion
	if tb, err := te.cs.RemovedGet(ctx, c); err != nil || !tb.Audited || tb.State != cfgstore.RemovedRemoved {
		t.Fatalf("c: %+v %v", tb, err)
	}
	if es := auditOf(t, te.env, "section=models&action=remove"); len(es) != 3 {
		t.Fatalf("audit: %d entries %v", len(es), es)
	}
	if ev := te.removedEvents(); len(ev) != 3 {
		t.Fatalf("history: %d", len(ev))
	}
	for _, n := range s2.trash.repairList() {
		if strings.Contains(n, "written at start") {
			t.Fatalf("the start wrote again: %v", s2.trash.repairList())
		}
	}
	te.restart()
	if es := auditOf(t, te.env, "section=models&action=remove"); len(es) != 3 {
		t.Fatalf("audit after another start: %d", len(es))
	}
}

// A kill between writing the audit entry and setting the mark never gives
// two entries: the start looks the entry up first (target model:<file>,
// action remove, the tombstone trash_id) and the event by its trash_id,
// writes only what is missing, then sets the mark. Both places the entry is
// written are killed: the owner removal, and the start writing it.
func TestModelRemove_KillBetweenAuditAndMark(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, stop string }{
		{"the owner removal", "remove:after-audit"},
		{"the start writing it", "audit-at-start:after-audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTrashEnv(t)
			te.setup()
			id := te.trashed("a.gguf")
			if tc.stop == "remove:after-audit" {
				te.stop = tc.stop
				te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
			} else {
				te.stop = "remove:after-finish" // no entry, no event: the start writes them
				te.cdo("POST", "/api/models/trash/"+id+"/remove", `{"comment":"x"}`, trashUI)
				te.stop = tc.stop
				te.restart()
			}
			te.stop = ""
			if tb, _ := te.cs.RemovedGet(ctx, id); tb == nil || tb.Audited {
				t.Fatalf("the mark is set before the start: %+v", tb)
			}
			if es := auditOf(t, te.env, "section=models&action=remove"); len(es) != 1 {
				t.Fatalf("audit before the start: %v", es)
			}
			te.restart()
			if es := auditOf(t, te.env, "section=models&action=remove"); len(es) != 1 {
				t.Fatalf("audit after the start: %d entries %v", len(es), es)
			}
			if ev := te.removedEvents(); len(ev) != 1 {
				t.Fatalf("history: %d events", len(ev))
			}
			if tb, _ := te.cs.RemovedGet(ctx, id); !tb.Audited {
				t.Fatalf("mark not set: %+v", tb)
			}
		})
	}
}

// The removal sentences count profiles in the singular for one:
// "its 1 profile", never "its 1 profiles".
func TestItsProfiles(t *testing.T) {
	for n, want := range map[int]string{0: "its 0 profiles", 1: "its 1 profile", 2: "its 2 profiles"} {
		if got := itsProfiles(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}
