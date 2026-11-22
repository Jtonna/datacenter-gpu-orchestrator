package scheduler

import "testing"

// A grant whose profile was deleted between the grant and building the
// lease (request goroutines see a new configuration at once) gets the
// default stall timeout instead of panicking on a nil profile.
func TestLeaseOfADeletedProfileUsesTheDefaultStallTimeout(t *testing.T) {
	f := startScheduler(t, nil)
	l, err := f.s.lease(answer{grant: &grant{ID: 1, Addr: "127.0.0.1:5800", Profile: "deleted-meanwhile"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := f.s.Config().Defaults.StallTimeout; l.StallTimeout != want {
		t.Fatalf("stall timeout %v, want %v", l.StallTimeout, want)
	}
}
