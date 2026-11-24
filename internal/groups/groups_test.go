package groups

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	db, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func names(gs []Group) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

// Groups are kept in the order they were made, each zone in at most one;
// adding a zone to a group moves it out of the one it was in.
func TestGroups_CreateMoveRenameDelete(t *testing.T) {
	s := open(t, t.TempDir())
	if _, err := s.Create("rack-a", []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("home", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("Rack-A", nil); !errors.Is(err, ErrExists) {
		t.Fatalf("same name, other case: %v", err)
	}
	for _, bad := range []string{"", " rack", "rack/a", "a b", string(make([]byte, 65))} {
		if _, err := s.Create(bad, nil); !errors.Is(err, ErrBadName) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	moved, err := s.AddMembers("home", []string{"02:00:00:00:00:02", "02:00:00:00:00:03"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(moved, map[string]string{"02:00:00:00:00:02": "rack-a"}) {
		t.Fatalf("moved %v", moved)
	}
	if g, ok := s.GroupOf("02:00:00:00:00:02"); !ok || g != "home" {
		t.Fatalf("group of 02: %q %v", g, ok)
	}
	if err := s.Rename("rack-a", "rack-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("rack-b", "home"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto another: %v", err)
	}
	if err := s.Rename("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}
	removed, err := s.RemoveMembers("home", []string{"02:00:00:00:00:03", "02:00:00:00:00:01"})
	if err != nil || !reflect.DeepEqual(removed, []string{"02:00:00:00:00:03"}) {
		t.Fatalf("removed %v %v (01 is not in home)", removed, err)
	}
	gs := s.List()
	if !reflect.DeepEqual(names(gs), []string{"rack-b", "home"}) || !reflect.DeepEqual(gs[0].Members, []string{"02:00:00:00:00:01"}) ||
		!reflect.DeepEqual(gs[1].Members, []string{"02:00:00:00:00:02"}) || gs[0].CreatedAt.IsZero() {
		t.Fatalf("list %+v", gs)
	}
	if err := s.Delete("rack-b"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GroupOf("02:00:00:00:00:01"); ok {
		t.Fatal("a deleted group's members are in no group")
	}
	if err := s.Delete("rack-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

// Everything is kept across a restart (a second Open of the same file), and
// a second Open migrates nothing.
func TestGroups_SurviveRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Create("rack-a", []string{"02:00:00:00:00:01"})
	s.Create("home", []string{"02:00:00:00:00:02", "02:00:00:00:00:03"})
	s.Rename("home", "desk")
	db.Close()
	s2 := open(t, dir)
	gs := s2.List()
	if !reflect.DeepEqual(names(gs), []string{"rack-a", "desk"}) || len(gs[1].Members) != 2 {
		t.Fatalf("after restart %+v", gs)
	}
	var v int
	s2.db.DB().QueryRow(`SELECT MAX(version) FROM group_migrations`).Scan(&v)
	if v != len(migrations) {
		t.Fatalf("ledger at %d", v)
	}
}

// Without the database the groups are kept in memory.
func TestGroups_Memory(t *testing.T) {
	s := Memory()
	s.Create("rack-a", []string{"02:00:00:00:00:01"})
	if g, ok := s.GroupOf("02:00:00:00:00:01"); !ok || g != "rack-a" {
		t.Fatal(g, ok)
	}
}

// A newer ledger than this build knows is refused, as every store does.
func TestGroups_NewerLedgerRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	db.DB().Exec(`INSERT INTO group_migrations (version, applied_at) VALUES (99, 0)`)
	if _, err := Open(db); err == nil {
		t.Fatal("a newer ledger was accepted")
	}
}

// Revision moves on every change and only then.
func TestGroups_Revision(t *testing.T) {
	s := Memory()
	r0 := s.Revision()
	s.Create("a", nil)
	r1 := s.Revision()
	s.AddMembers("a", nil)
	s.RemoveMembers("a", []string{"x"}) // nothing to remove
	if r1 == r0 || s.Revision() != r1 {
		t.Fatalf("%d %d %d", r0, r1, s.Revision())
	}
}
