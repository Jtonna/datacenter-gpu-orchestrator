package cfgstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/groups"
)

// scheduler.db holds three ledgers side by side: cfg_migrations (stays at 3),
// access_migrations (the access keys) and group_migrations (zone groups,
// internal/groups). Each must run whatever the others have done to the file.

func ledgerVersions(t *testing.T, s *Store) string {
	t.Helper()
	return fmt.Sprintf("cfg=%d access=%d groups=%d",
		countRows(t, s, `SELECT COALESCE(MAX(version),0) FROM cfg_migrations`),
		countRows(t, s, `SELECT COALESCE(MAX(version),0) FROM access_migrations`),
		countRows(t, s, `SELECT COALESCE(MAX(version),0) FROM group_migrations`))
}

func openBoth(t *testing.T, db DB, groupsFirst bool) (*Store, *groups.Store) {
	t.Helper()
	var (
		s   *Store
		g   *groups.Store
		err error
	)
	if groupsFirst {
		if g, err = groups.Open(db); err != nil {
			t.Fatalf("groups: %v", err)
		}
	}
	if s, err = Open(db); err != nil {
		t.Fatalf("config and access: %v", err)
	}
	if !groupsFirst {
		if g, err = groups.Open(db); err != nil {
			t.Fatalf("groups: %v", err)
		}
	}
	return s, g
}

func TestLedgers_AccessAndGroupsOnAFreshFile(t *testing.T) {
	for _, groupsFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("groupsFirst=", groupsFirst), func(t *testing.T) {
			db, err := bench.OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			s, g := openBoth(t, db, groupsFirst)
			got := ledgerVersions(t, s)
			if countRows(t, s, `SELECT MAX(version) FROM access_migrations`) != AccessVersion() ||
				countRows(t, s, `SELECT MAX(version) FROM cfg_migrations`) != v36Migrations ||
				countRows(t, s, `SELECT COUNT(*) FROM group_migrations`) == 0 {
				t.Fatalf("ledgers after a fresh open: %s", got)
			}
			if _, err := g.Create("rack-a", []string{"node-a"}); err != nil {
				t.Fatal(err)
			}
			// Opening again in either order changes no ledger and keeps the group.
			s2, g2 := openBoth(t, db, !groupsFirst)
			if again := ledgerVersions(t, s2); again != got {
				t.Fatalf("a second open moved a ledger: %s then %s", got, again)
			}
			if gr, ok := g2.Get("rack-a"); !ok || len(gr.Members) != 1 {
				t.Fatalf("the group did not survive: %+v %v", gr, ok)
			}
		})
	}
}

// A file the live image (zone groups, no access tables) has already used:
// cfg_migrations at 3 with old keys, group_migrations applied, a group with a
// member. This build adds the access tables and touches nothing else; then
// the live image opens the file again and still works.
func TestLedgers_FileThatAlreadyHasTheGroupTables(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The live image: config tables as before, plus the groups.
	old, err := openAsV36(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.CreateAgent(Agent{ID: "ag_old1", Name: "deploy-check", KeyHash: "h-old1", KeyPrefix: "lls_kBFyEk", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	g, err := groups.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Create("rack-a", []string{"node-a", "node-b"}); err != nil {
		t.Fatal(err)
	}
	groupLedger := countRows(t, old, `SELECT COUNT(*) FROM group_migrations`)
	if groupLedger == 0 {
		t.Fatal("the groups ledger is empty")
	}
	if n := countRows(t, old, `SELECT COUNT(*) FROM sqlite_master WHERE name='access_migrations'`); n != 0 {
		t.Fatal("the live image must not have the access ledger yet")
	}
	keysBefore := fmt.Sprint(v36Agents(t, old))

	// This build.
	s, g2 := openBoth(t, db, false)
	if v := countRows(t, s, `SELECT MAX(version) FROM access_migrations`); v != AccessVersion() {
		t.Fatalf("access_migrations at %d", v)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM cfg_migrations`); v != v36Migrations {
		t.Fatalf("cfg_migrations at %d", v)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM group_migrations`); n != groupLedger {
		t.Fatalf("group_migrations went from %d to %d rows", groupLedger, n)
	}
	if gr, ok := g2.Get("rack-a"); !ok || len(gr.Members) != 2 {
		t.Fatalf("the group did not survive: %+v %v", gr, ok)
	}
	if after := fmt.Sprint(v36Agents(t, s)); after != keysBefore {
		t.Fatalf("the old keys changed:\n%s\n%s", keysBefore, after)
	}
	list, err := s.Agents(context.Background())
	if err != nil || len(list) != 1 || list[0].Access != nil {
		t.Fatalf("the old key should stay a legacy key: %+v %v", list, err)
	}

	// Back to the live image on the migrated file: it opens and sees its group.
	if _, err := openAsV36(db); err != nil {
		t.Fatalf("the live image's config open refused the migrated file: %v", err)
	}
	g3, err := groups.Open(db)
	if err != nil {
		t.Fatalf("the live image's groups open refused the migrated file: %v", err)
	}
	if gr, ok := g3.Get("rack-a"); !ok || len(gr.Members) != 2 {
		t.Fatalf("the group did not survive the round trip: %+v %v", gr, ok)
	}
}
