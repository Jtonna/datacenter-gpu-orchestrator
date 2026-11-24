package cfgstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// v36Migrations is how many cfg_migrations steps the v36 image knows. The
// access tables must not add one: v36 refuses to open a database whose
// cfg_migrations is ahead of it.
const v36Migrations = 3

// openAsV36 is the v36 image's Open (cfgstore/store.go at 311213b): it
// refuses a newer cfg_migrations and applies the steps it knows. It knows
// nothing of agent_access or access_migrations.
func openAsV36(db DB) (*Store, error) {
	s := &Store{db: db, now: func() time.Time { return time.UnixMilli(5000) }}
	err := db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS cfg_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM cfg_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > v36Migrations {
			return fmt.Errorf("the configuration tables are at version %d but this build knows %d", have, v36Migrations)
		}
		for i := have; i < v36Migrations; i++ {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO cfg_migrations (version, applied_at) VALUES (?,?)`, i+1, 5000); err != nil {
				return err
			}
		}
		return nil
	})
	return s, err
}

// v36Agents is the v36 image's agent list query (no join).
func v36Agents(t *testing.T, s *Store) []Agent {
	t.Helper()
	rows, err := s.db.DB().Query(`SELECT ` + agentCols + ` FROM agents ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func countRows(t *testing.T, s *Store, q string) int {
	t.Helper()
	var n int
	if err := s.db.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The access migration on a database that holds old-style keys, then the
// previous image (v36) on the migrated file, then this build again:
//   - migrating adds agent_access and its own ledger, keeps cfg_migrations at
//     3, and leaves every old key exactly as it was, with no access row (old
//     style: full permissions, no expiry);
//   - v36 opens the migrated file (it does not refuse to start), lists every
//     key, new ones included, and can create, rotate, revoke and count keys;
//     it ignores the access tables, so nothing in them is lost;
//   - this build again: the key v36 made is old style, the key v36 rotated
//     keeps its expiry and permissions (its hint falls back to the prefix),
//     the key v36 revoked stays revoked.
func TestAccessMigration_OldKeysV36AndForwardAgain(t *testing.T) {
	if len(migrations) != v36Migrations {
		t.Fatalf("cfg_migrations has %d steps: a new step would make the v36 image refuse to start; put access changes in accessMigrations", len(migrations))
	}
	dir := t.TempDir()
	db, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	old, err := openAsV36(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []Agent{
		{ID: "ag_old1", Name: "deploy-check", KeyHash: "h-old1", KeyPrefix: "lls_kBFyEk", CreatedAt: 1000, CreatedSource: "rest", CreatedActor: "anonymous"},
		{ID: "ag_old2", Name: "retired", KeyHash: "h-old2", KeyPrefix: "lls_aaaaaa", CreatedAt: 1100},
	} {
		if err := old.CreateAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	old.TouchAgent("ag_old1", 7, 1500)
	old.RevokeAgent("ag_old2", 1600)
	before := fmt.Sprint(v36Agents(t, old))

	// This build: migrate.
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM cfg_migrations`); v != v36Migrations {
		t.Fatalf("cfg_migrations at %d", v)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM access_migrations`); v != AccessVersion() {
		t.Fatalf("access_migrations at %d", v)
	}
	if after := fmt.Sprint(v36Agents(t, s)); after != before {
		t.Fatalf("the old keys changed:\n%s\n%s", before, after)
	}
	list, err := s.Agents(context.Background())
	if err != nil || len(list) != 2 || list[0].Access != nil || list[1].Access != nil || list[0].RequestCount != 7 || list[1].RevokedAt != 1600 {
		t.Fatalf("%+v %v", list, err)
	}
	// A new key with access.
	x := AgentAccess{KeyHash: "h-new", KeyLast4: "7c2e", ExpiresAt: 9_000_000, Grants: `{"observe":"allow","zones":"allow","models":"ask","settings":"off"}`, UpdatedAt: 2000}
	if err := s.CreateAgentAccess(Agent{ID: "ag_new", Name: "ops-bot", KeyHash: "h-new", KeyPrefix: "ptl_9f2c41", CreatedAt: 2000}, x); err != nil {
		t.Fatal(err)
	}

	// The v36 image on the migrated file.
	v36, err := openAsV36(db)
	if err != nil {
		t.Fatalf("v36 refuses the migrated database: %v", err)
	}
	got := v36Agents(t, v36)
	if len(got) != 3 || got[2].ID != "ag_new" || got[2].KeyHash != "h-new" {
		t.Fatalf("v36 sees %+v", got)
	}
	if err := v36.CreateAgent(Agent{ID: "ag_v36", Name: "made-by-v36", KeyHash: "h-v36", KeyPrefix: "lls_bbbbbb", CreatedAt: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := v36.RotateAgentKey("ag_new", "h-rotated", "lls_cccccc", 3100); err != nil {
		t.Fatal(err)
	}
	if err := v36.TouchAgent("ag_new", 2, 3200); err != nil {
		t.Fatal(err)
	}
	if err := v36.RevokeAgent("ag_old1", 3300); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, v36, `SELECT COUNT(*) FROM agent_access`); n != 1 {
		t.Fatalf("%d access rows after v36", n)
	}

	// Forward again.
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if v := countRows(t, s, `SELECT COUNT(*) FROM access_migrations`); v != AccessVersion() {
		t.Fatalf("access_migrations rows %d", v)
	}
	by := map[string]Agent{}
	list, _ = s.Agents(context.Background())
	for _, a := range list {
		by[a.ID] = a
	}
	if a := by["ag_v36"]; a.Access != nil || a.KeyHash != "h-v36" {
		t.Fatalf("made by v36: %+v", a)
	}
	if a := by["ag_new"]; a.Access == nil || a.Access.ExpiresAt != 9_000_000 || a.Access.Grants != x.Grants || a.Access.KeyHash == a.KeyHash ||
		a.KeyPrefix != "lls_cccccc" || a.RequestCount != 2 {
		t.Fatalf("rotated by v36: %+v %+v", a, a.Access)
	}
	if a := by["ag_old1"]; a.RevokedAt != 3300 || a.Access != nil {
		t.Fatalf("revoked by v36: %+v", a)
	}
}

// Access rows: update and rotate write them whole; a revoked agent cannot
// be changed or rotated.
func TestAccess_UpdateRotateAndRevoked(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	x := AgentAccess{KeyHash: "h1", KeyLast4: "aaaa", Grants: `{"observe":"allow","zones":"off","models":"off","settings":"off"}`, UpdatedAt: 1}
	if err := s.CreateAgentAccess(Agent{ID: "ag_1", Name: "a", KeyHash: "h1", KeyPrefix: "ptl_aaaaaa", CreatedAt: 1}, x); err != nil {
		t.Fatal(err)
	}
	x.ExpiresAt, x.Warned = 500, "7d"
	if err := s.UpdateAgentAccess("ag_1", "b", "for the dashboard", x); err != nil {
		t.Fatal(err)
	}
	x.KeyHash, x.KeyLast4, x.Warned = "h2", "bbbb", ""
	if err := s.RotateAgentAccess("ag_1", "h2", "ptl_bbbbbb", 2, x); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAgentWarned("ag_1", "1d"); err != nil {
		t.Fatal(err)
	}
	a, err := s.Agent(context.Background(), "ag_1")
	if err != nil || a.Name != "b" || a.Description != "for the dashboard" || a.KeyHash != "h2" || a.Access.KeyLast4 != "bbbb" || a.Access.ExpiresAt != 500 || a.Access.Warned != "1d" {
		t.Fatalf("%+v %+v %v", a, a.Access, err)
	}
	s.RevokeAgent("ag_1", 3)
	if err := s.UpdateAgentAccess("ag_1", "c", "", x); err != ErrNotFound {
		t.Fatalf("update of a revoked agent: %v", err)
	}
	if err := s.RotateAgentAccess("ag_1", "h3", "ptl_cccccc", 4, x); err != ErrNotFound {
		t.Fatalf("rotate of a revoked agent: %v", err)
	}
}
