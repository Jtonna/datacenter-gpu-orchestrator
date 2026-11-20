package cfgstore

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

func TestNodeAdminSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeAdmin("02:00:00:00:00:01", NodeAdmin{Paused: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeAdmin("02:00:00:00:00:02", NodeAdmin{Removed: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeAdmin("02:00:00:00:00:02", NodeAdmin{}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.NodeAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["02:00:00:00:00:01"].Paused {
		t.Fatalf("admin %+v", got)
	}
}
