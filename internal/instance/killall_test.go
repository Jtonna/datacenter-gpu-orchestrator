package instance

import "testing"

// KillAll (the scheduler's fatal path) kills every live child, and a reaped
// child leaves the live set.
func TestKillAllKillsLiveChildren(t *testing.T) {
	l := newLauncher(t)
	a, b := start(t, l, fakeSpec(t)), start(t, l, fakeSpec(t))
	waitHealthy(t, a)
	waitHealthy(t, b)
	if n := l.KillAll(); n != 2 {
		t.Fatalf("KillAll killed %d, want 2", n)
	}
	for _, c := range []Child{a, b} {
		if st := recvExit(t, c); !st.Requested {
			t.Fatalf("status %+v", st)
		}
	}
	if n := l.KillAll(); n != 0 {
		t.Fatalf("reaped children still live: %d", n)
	}
}
