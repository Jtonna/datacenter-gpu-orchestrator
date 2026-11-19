package history

import (
	"io"
	"log/slog"
	"testing"
)

func TestSubscribeSeesEveryRecordedEventInOrder(t *testing.T) {
	s := Open(Options{Dir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer s.Close()
	var got []Event
	cancel := s.Subscribe(func(e Event) { got = append(got, e) })
	s.Record(Event{Ts: 1, Type: "a", Severity: Info, Summary: "A."})
	s.Record(Event{Ts: 2, Type: "b", Severity: Warn, Summary: "B."})
	cancel()
	s.Record(Event{Ts: 3, Type: "c", Severity: Info, Summary: "C."})
	if len(got) != 2 || got[0].Type != "a" || got[1].Type != "b" || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Nodes == nil || got[0].Data == nil {
		t.Fatalf("subscriber got a record that differs from the stored shape: %+v", got[0])
	}
}

func TestSubscribeOnAnOffStoreIsHarmless(t *testing.T) {
	s := Open(Options{})
	cancel := s.Subscribe(func(Event) { t.Fatal("an off store delivered an event") })
	s.Record(Event{Type: "x"})
	cancel()
}
