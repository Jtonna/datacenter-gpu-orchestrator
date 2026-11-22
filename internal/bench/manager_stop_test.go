package bench

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// The loop ends when its context is cancelled without Stop being called;
// it must not wait for itself.
func TestLoopEndsWhenItsContextIsCancelled(t *testing.T) {
	store, _ := openTemp(t)
	m, err := NewManager(ManagerOptions{Store: store, API: newFake(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not end after its context was cancelled")
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	m.Stop(sctx) // a later Stop returns at once
	if sctx.Err() != nil {
		t.Fatal("Stop after the loop ended waited for its deadline")
	}
}
