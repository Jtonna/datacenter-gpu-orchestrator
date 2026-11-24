package cfgstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// b162acdModelMigrations is how many model_migrations steps the image before
// slice 3 (b162acd, v62) knows: the pins only.
const b162acdModelMigrations = 1

// Models slice 3: the trash records live in model_trash, step 2 of the
// model_migrations ledger; cfg_migrations does not move. The image before
// (b162acd) opens the same file: its cfg check passes, its models ledger
// sees version 2 ahead of its 1 and leaves it alone, its pins still read.
// This image again: the trash record is there as it was, one row per step.
func TestModelTrash_LedgerAndWayBack(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if ModelVersion() != 2 {
		t.Fatalf("model_migrations knows %d steps, want 2 (pins, trash)", ModelVersion())
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM model_migrations`); v != 2 {
		t.Fatalf("model_migrations at %d", v)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM cfg_migrations`); v != len(migrations) {
		t.Fatalf("cfg_migrations at %d, want %d (unchanged)", v, len(migrations))
	}
	ctx := context.Background()
	ctxv := 4096
	rec := &TrashRecord{ID: "0123456789ab", File: "a.gguf", SizeBytes: 42, DeletedAt: 1000, PurgeAt: 2000, State: TrashPending,
		Profiles: map[string]*config.ProfileSpec{"alpha": {Model: "a.gguf", Ctx: &ctxv, Aliases: []string{"al"}}}, Actor: "alex", Comment: "old", UpdatedAt: 1000}
	if err := s.TrashInsert(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.TrashSetState(rec.ID, TrashTrashed, 7, nil, "", 1001); err != nil {
		t.Fatal(err)
	}
	s.PinModel("b.gguf", 5)

	// the way back: b162acd's Open (cfg check, access ledger, its one models step), then the older ones
	if err := db.Write(func(tx *sql.Tx) error {
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM cfg_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			t.Fatalf("b162acd would refuse: cfg_migrations at %d", have)
		}
		if err := migrateAccessList(tx, 1, accessMigrations); err != nil {
			return err
		}
		return migrateModelsList(tx, 1, modelMigrations[:b162acdModelMigrations])
	}); err != nil {
		t.Fatalf("b162acd open: %v", err)
	}
	old := &Store{db: db}
	if p, err := old.ModelPins(ctx); err != nil || p["b.gguf"] != 5 {
		t.Fatalf("b162acd's pins: %v %v", p, err)
	}
	openAsV44(t, db)

	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.TrashGet(ctx, rec.ID)
	if err != nil || got.State != TrashTrashed || got.Revision != 7 || got.File != "a.gguf" || got.SizeBytes != 42 || got.PurgeAt != 2000 ||
		*got.Profiles["alpha"].Ctx != 4096 || got.Profiles["alpha"].Aliases[0] != "al" || got.Actor != "alex" {
		t.Fatalf("after the way back: %+v %v", got, err)
	}
	if v := countRows(t, s2, `SELECT COUNT(*) FROM model_migrations`); v != 2 {
		t.Fatalf("model_migrations rows %d", v)
	}

	// restore bookkeeping, list, delete
	if err := s2.TrashSetState(rec.ID, TrashRestoring, 0, map[string]*config.ProfileSpec{"alpha-restored": {Model: "a.gguf"}}, "", 1002); err != nil {
		t.Fatal(err)
	}
	l, _ := s2.TrashList(ctx)
	if len(l) != 1 || l[0].State != TrashRestoring || l[0].RestoreProfiles["alpha-restored"] == nil || l[0].Revision != 7 {
		t.Fatalf("list: %+v", l)
	}
	if err := s2.TrashDelete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.TrashGet(ctx, rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s2.TrashSetState(rec.ID, TrashTrashed, 0, nil, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set state of none: %v", err)
	}
}
