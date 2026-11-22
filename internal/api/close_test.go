package api

import (
	"testing"
	"time"
)

// Close stops every background user of the store before it returns: the
// catalog build Warm started is cancelled and waited for, so nothing reads
// or writes the database after the caller closes it (a late reader opens a
// new connection, which recreates the database's side files).
func TestCloseWaitsForTheWarmBuild(t *testing.T) {
	e := newEnv(t, okUpstream())
	s := e.api
	s.catalog.buildMu.Lock() // the warm build waits here, as on a slow store
	s.Warm()
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
		s.catalog.buildMu.Unlock()
		t.Fatal("Close returned while the warm catalog build was still running")
	case <-time.After(200 * time.Millisecond):
	}
	s.catalog.buildMu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the build ended")
	}
}
