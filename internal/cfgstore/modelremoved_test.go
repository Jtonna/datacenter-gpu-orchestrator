package cfgstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// af916f4ModelMigrations is how many model_migrations steps the image before
// slice 4 (af916f4, v66) knows: pins and the trash.
const af916f4ModelMigrations = 2

func trashedRec(t *testing.T, s *Store, id, file string) {
	t.Helper()
	ctxv := 4096
	rec := &TrashRecord{ID: id, File: file, SizeBytes: 42, DeletedAt: 1000, PurgeAt: 2000, State: TrashTrashed,
		Profiles: map[string]*config.ProfileSpec{"beta": {Model: file}, "alpha": {Model: file, Ctx: &ctxv}}, Revision: 7, Actor: "alex", Comment: "old", UpdatedAt: 1000}
	if err := s.TrashInsert(rec); err != nil {
		t.Fatal(err)
	}
}

// The three steps of a removal, and the way back of the first.
func TestModelRemoved_Steps(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	trashedRec(t, s, "0123456789ab", "a.gguf")
	// begin: trash record removing, tombstone removing with the profile ids, the deleter and the remover
	tomb, err := s.RemoveBegin("0123456789ab", RemovedByOwner, "sam", "free the space", 3000)
	if err != nil || tomb.State != TrashRemoving || len(tomb.ProfileIDs) != 2 || tomb.ProfileIDs[0] != "alpha" || tomb.DeletedBy != "alex" || tomb.RemovedBy != "sam" {
		t.Fatalf("begin: %+v %v", tomb, err)
	}
	if r, _ := s.TrashGet(ctx, "0123456789ab"); r.State != TrashRemoving {
		t.Fatalf("trash record: %+v", r)
	}
	// only from trashed
	if _, err := s.RemoveBegin("0123456789ab", RemovedByOwner, "sam", "x", 3001); !errors.Is(err, ErrNotTrashed) {
		t.Fatalf("begin twice: %v", err)
	}
	if _, err := s.RemoveBegin("ffffffffffff", RemovedByOwner, "sam", "x", 3001); !errors.Is(err, ErrNotFound) {
		t.Fatalf("begin of none: %v", err)
	}
	// abort: back to trashed with the problem, no tombstone
	if err := s.RemoveAbort("0123456789ab", "it could not be erased", 3002); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.TrashGet(ctx, "0123456789ab"); r.State != TrashTrashed || r.Problem != "it could not be erased" || len(r.Profiles) != 2 {
		t.Fatalf("after abort: %+v", r)
	}
	if _, err := s.RemovedGet(ctx, "0123456789ab"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstone after abort: %v", err)
	}
	// begin, finish: no trash record, a removed tombstone
	if _, err := s.RemoveBegin("0123456789ab", RemovedByClearing, "potluck", "cleared", 4000); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFinish("0123456789ab", 4001); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrashGet(ctx, "0123456789ab"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash record after finish: %v", err)
	}
	g, err := s.RemovedGet(ctx, "0123456789ab")
	if err != nil || g.State != RemovedRemoved || g.RemovedAt != 4001 || g.Reason != RemovedByClearing || g.SizeBytes != 42 || g.Revision != 7 || g.DeletedComment != "old" {
		t.Fatalf("tombstone: %+v %v", g, err)
	}
	if err := s.RemoveFinish("0123456789ab", 4002); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish twice: %v", err)
	}
	if l, _ := s.RemovedList(ctx); len(l) != 1 {
		t.Fatalf("list: %+v", l)
	}
}

// Models slice 4's tombstones are in model_trash_removed, step 3 of the
// model_migrations ledger; cfg_migrations does not move. The image before
// (af916f4) opens the same file and leaves step 3 alone; its trash list
// (model_trash) never shows a removed file, and a record whose removal was
// cut shows as removing, which it does not restore (it restores only
// trashed). This image again: all of it is there.
func TestModelRemoved_LedgerAndWayBack(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if ModelVersion() != 3 {
		t.Fatalf("model_migrations knows %d steps, want 3 (pins, trash, removed)", ModelVersion())
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM cfg_migrations`); v != len(migrations) {
		t.Fatalf("cfg_migrations at %d, want %d (unchanged)", v, len(migrations))
	}
	ctx := context.Background()
	trashedRec(t, s, "0123456789ab", "a.gguf") // removed for good
	trashedRec(t, s, "1123456789ab", "b.gguf") // removal cut after step 1
	trashedRec(t, s, "2123456789ab", "c.gguf") // still in the trash
	s.RemoveBegin("0123456789ab", RemovedByOwner, "sam", "x", 3000)
	s.RemoveFinish("0123456789ab", 3001)
	s.RemoveBegin("1123456789ab", RemovedByClearing, "potluck", "x", 3002)

	// the way back: af916f4's Open (cfg check, access ledger, its two models steps)
	if err := db.Write(func(tx *sql.Tx) error {
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM cfg_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			t.Fatalf("af916f4 would refuse: cfg_migrations at %d", have)
		}
		if err := migrateAccessList(tx, 1, accessMigrations); err != nil {
			return err
		}
		return migrateModelsList(tx, 1, modelMigrations[:af916f4ModelMigrations])
	}); err != nil {
		t.Fatalf("af916f4 open: %v", err)
	}
	// af916f4's trash list is this TrashList (model_trash, unchanged since)
	old := &Store{db: db}
	l, err := old.TrashList(ctx)
	if err != nil || len(l) != 2 {
		t.Fatalf("af916f4's trash list: %+v %v", l, err)
	}
	states := map[string]string{}
	for _, r := range l {
		states[r.File] = r.State
	}
	if states["a.gguf"] != "" || states["b.gguf"] != TrashRemoving || states["c.gguf"] != TrashTrashed {
		t.Fatalf("af916f4 sees: %v", states)
	}
	if v := countRows(t, old, `SELECT MAX(version) FROM model_migrations`); v != 3 {
		t.Fatalf("af916f4 moved the ledger: %d", v)
	}

	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := s2.RemovedGet(ctx, "0123456789ab"); err != nil || g.State != RemovedRemoved {
		t.Fatalf("tombstone after the way back: %+v %v", g, err)
	}
	if g, err := s2.RemovedGet(ctx, "1123456789ab"); err != nil || g.State != TrashRemoving {
		t.Fatalf("cut removal after the way back: %+v %v", g, err)
	}
	if v := countRows(t, s2, `SELECT COUNT(*) FROM model_migrations`); v != 3 {
		t.Fatalf("model_migrations rows %d", v)
	}
}

// RemoveFinish settles every leftover of a removal, so a record never stays
// removing: the real state after a kill between the steps (record removing,
// tombstone removing), a record removing without its tombstone (damaged or
// hand-edited: completed with reason repair, signed potluck), and a record
// removing whose tombstone already says removed (only the record goes).
// Neither a record nor a tombstone removing: ErrNotFound.
func TestRemoveFinish_SettlesEveryLeftover(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gone := func(id string) {
		t.Helper()
		if _, err := s.TrashGet(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: the record stayed: %v", id, err)
		}
	}
	// the real state after a kill between step 2 and step 3
	trashedRec(t, s, "aaaaaaaaaaaa", "a.gguf")
	if _, err := s.RemoveBegin("aaaaaaaaaaaa", RemovedByOwner, "sam", "x", 3000); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFinish("aaaaaaaaaaaa", 4000); err != nil {
		t.Fatal(err)
	}
	gone("aaaaaaaaaaaa")
	if tb, _ := s.RemovedGet(ctx, "aaaaaaaaaaaa"); tb.State != RemovedRemoved || tb.Reason != RemovedByOwner || tb.RemovedBy != "sam" || tb.RemovedAt != 4000 {
		t.Fatalf("real: %+v", tb)
	}
	// record removing, no tombstone
	trashedRec(t, s, "bbbbbbbbbbbb", "b.gguf")
	if err := s.TrashSetState("bbbbbbbbbbbb", TrashRemoving, 0, nil, "", 3000); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFinish("bbbbbbbbbbbb", 4000); err != nil {
		t.Fatalf("no tombstone: %v", err)
	}
	gone("bbbbbbbbbbbb")
	tb, err := s.RemovedGet(ctx, "bbbbbbbbbbbb")
	if err != nil || tb.State != RemovedRemoved || tb.Reason != "repaired at start" || tb.RemovedBy != "potluck" || tb.File != "b.gguf" || tb.SizeBytes != 42 ||
		tb.DeletedBy != "alex" || tb.RemovedAt != 4000 || len(tb.ProfileIDs) != 2 || tb.Revision != 7 {
		t.Fatalf("no tombstone: %+v %v", tb, err)
	}
	// record removing, tombstone already removed
	trashedRec(t, s, "cccccccccccc", "c.gguf")
	if _, err := s.RemoveBegin("cccccccccccc", RemovedByOwner, "sam", "x", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE model_trash_removed SET state = ?, removed_at = 3500 WHERE id = ?`, RemovedRemoved, "cccccccccccc"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFinish("cccccccccccc", 4000); err != nil {
		t.Fatalf("tombstone already removed: %v", err)
	}
	gone("cccccccccccc")
	if tb, _ := s.RemovedGet(ctx, "cccccccccccc"); tb.RemovedAt != 3500 {
		t.Fatalf("the tombstone was rewritten: %+v", tb)
	}
	// nothing to settle
	if err := s.RemoveFinish("dddddddddddd", 4000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("none: %v", err)
	}
	trashedRec(t, s, "eeeeeeeeeeee", "e.gguf") // trashed, not removing: not finished
	if err := s.RemoveFinish("eeeeeeeeeeee", 4000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed: %v", err)
	}
	if r, _ := s.TrashGet(ctx, "eeeeeeeeeeee"); r == nil || r.State != TrashTrashed {
		t.Fatalf("a trashed record was touched: %+v", r)
	}
}
