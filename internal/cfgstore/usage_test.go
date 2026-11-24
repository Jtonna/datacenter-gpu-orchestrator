package cfgstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

const dayMS = 86400000

// v44Access is how many access_migrations steps the v44 image knows.
const v44Access = 1

// openAsV44 is the v44 image's Open: the config steps it knows and the
// first access step; it ignores an access ledger ahead of it.
func openAsV44(t *testing.T, db DB) *Store {
	t.Helper()
	s, err := openAsV36(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(func(tx *sql.Tx) error { return migrateAccessList(tx, 1, accessMigrations[:v44Access]) }); err != nil {
		t.Fatalf("the v44 image's access migration: %v", err)
	}
	return s
}

func usageOf(t *testing.T, s *Store, since int64) string {
	t.Helper()
	u, err := s.AgentUsage(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(u)
}

// Per key and UTC day: requests, denied, asked. A flush adds to the day's
// row and moves the agent's request_count and last_used_at as before; old
// days are trimmed.
func TestAgentUsage_RecordAndTrim(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM access_migrations`); v != 2 || AccessVersion() != 2 {
		t.Fatalf("access_migrations at %d (know %d)", v, AccessVersion())
	}
	if err := s.CreateAgent(Agent{ID: "ag_1", Name: "ops-bot", KeyHash: "h1", KeyPrefix: "ptl_aaaaaa", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	d1, d2 := int64(100*dayMS), int64(101*dayMS)
	for _, u := range [][]AgentDayUse{
		{{Day: d1, Requests: 3, Denied: 1}},
		{{Day: d1, Requests: 2, Asked: 1}, {Day: d2, Requests: 4, Denied: 2}},
	} {
		n := int64(0)
		for _, x := range u {
			n += x.Requests
		}
		if err := s.RecordAgentUse("ag_1", n, d2+5, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordAgentUse("ag_1", 0, 0, []AgentDayUse{{Day: d2, Denied: 1}}); err != nil { // a refusal alone
		t.Fatal(err)
	}
	if got := usageOf(t, s, d1); got != "map[ag_1:[{8640000000 5 1 1} {8726400000 4 3 0}]]" {
		t.Fatalf("usage %s", got)
	}
	if got := usageOf(t, s, d2); got != "map[ag_1:[{8726400000 4 3 0}]]" {
		t.Fatalf("usage since d2 %s", got)
	}
	list, _ := s.Agents(context.Background())
	if len(list) != 1 || list[0].RequestCount != 9 || list[0].LastUsedAt != d2+5 {
		t.Fatalf("agent %+v", list)
	}
	if err := s.TrimAgentUsage(d2); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, s, 0); got != "map[ag_1:[{8726400000 4 3 0}]]" {
		t.Fatalf("after the trim %s", got)
	}
}

// The way back: v44 opens the file this build migrated (its access ledger
// check lets a newer ledger be), resolves and counts keys as before, and
// leaves agent_usage alone; forward again, the days are all there. A file
// only v44 used gets agent_usage when this build opens it.
func TestAgentUsage_TheWayBackToV44AndForward(t *testing.T) {
	db, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	old := openAsV44(t, db) // a file v44 made
	if err := old.CreateAgent(Agent{ID: "ag_1", Name: "ops-bot", KeyHash: "h1", KeyPrefix: "ptl_aaaaaa", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, old, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'agent_usage'`); n != 0 {
		t.Fatal("v44 has no agent_usage")
	}

	s, err := Open(db) // forward
	if err != nil {
		t.Fatal(err)
	}
	if v := countRows(t, s, `SELECT MAX(version) FROM access_migrations`); v != AccessVersion() {
		t.Fatalf("access_migrations at %d", v)
	}
	if err := s.RecordAgentUse("ag_1", 2, 10*dayMS+1, []AgentDayUse{{Day: 10 * dayMS, Requests: 2, Denied: 1}}); err != nil {
		t.Fatal(err)
	}
	before := usageOf(t, s, 0)

	back := openAsV44(t, db) // back to v44
	if err := back.TouchAgent("ag_1", 5, 11*dayMS); err != nil {
		t.Fatalf("v44's flush: %v", err)
	}
	list, err := back.Agents(context.Background())
	if err != nil || len(list) != 1 || list[0].RequestCount != 7 {
		t.Fatalf("v44's list %+v %v", list, err)
	}
	if v := countRows(t, back, `SELECT MAX(version) FROM access_migrations`); v != AccessVersion() {
		t.Fatalf("v44 moved the ledger to %d", v)
	}

	again, err := Open(db) // forward again
	if err != nil {
		t.Fatal(err)
	}
	if after := usageOf(t, again, 0); after != before {
		t.Fatalf("the days changed: %s then %s", before, after)
	}
	list, _ = again.Agents(context.Background())
	if list[0].RequestCount != 7 || list[0].LastUsedAt != 11*dayMS {
		t.Fatalf("%+v", list[0])
	}
}
