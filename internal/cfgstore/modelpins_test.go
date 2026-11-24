package cfgstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// Models slice 2: pins live in model_pins under their own ledger
// (model_migrations v1); cfg_migrations does not move, so the image before
// (v55) opens the same file, ignores the pins, and they are there again
// when this image comes back.
func TestModelPins_LedgerAndWayBack(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM model_migrations`); v != 1 || ModelVersion() != 1 {
		t.Fatalf("model_migrations at %d (know %d)", v, ModelVersion())
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM cfg_migrations`); v != len(migrations) {
		t.Fatalf("cfg_migrations at %d, want %d (unchanged)", v, len(migrations))
	}
	ctx := context.Background()
	if at, already, err := s.PinModel("a.gguf", 100); err != nil || already || at != 100 {
		t.Fatalf("pin: %d %v %v", at, already, err)
	}
	if at, already, err := s.PinModel("a.gguf", 200); err != nil || !already || at != 100 {
		t.Fatalf("pin again: %d %v %v (want the first time kept)", at, already, err)
	}
	s.PinModel("command:whisper", 300)
	if err := s.UnpinModel("command:whisper"); err != nil {
		t.Fatal(err)
	}
	if err := s.UnpinModel("command:whisper"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpin twice: %v", err)
	}

	// the way back: the v55 image's Open on this file, then this one again
	if err := db.Write(func(tx *sql.Tx) error {
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM cfg_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			t.Fatalf("the v55 image would refuse: cfg_migrations at %d", have)
		}
		return migrateAccessList(tx, 1, accessMigrations)
	}); err != nil {
		t.Fatalf("v55 open: %v", err)
	}
	openAsV44(t, db)
	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := s2.ModelPins(ctx)
	if err != nil || len(pins) != 1 || pins["a.gguf"] != 100 {
		t.Fatalf("pins after the way back: %v %v", pins, err)
	}
	if v := countRows(t, s2, `SELECT COUNT(*) FROM model_migrations`); v != 1 {
		t.Fatalf("model_migrations rows %d after reopening", v)
	}
}
