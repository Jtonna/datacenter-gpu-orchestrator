package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
)

// A query whose caller gives up (its context ends while the event loop is
// busy) must not touch the caller's memory when the loop runs it later: the
// method returns the context error and a zero result, and the loop's
// answer goes to storage the query owns. Run under -race: the loop is held
// while every query-based method is abandoned, then released, and the
// results are read again after the loop ran the abandoned queries.
func TestScheduler_AnAbandonedQueryDoesNotWriteTheCallersResult(t *testing.T) {
	f := startScheduler(t, []core.Node{ps5("a", 2, 15000)}, llamaP("m", 8000))
	s := f.s
	held, release := make(chan struct{}), make(chan struct{})
	go s.query(context.Background(), func(*schedCore) { close(held); <-release }) //nolint:errcheck
	<-held

	abandoned := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	type result struct {
		name string
		err  error
		zero bool
	}
	var got []result
	{
		st, err := s.Status(abandoned())
		got = append(got, result{"Status", err, reflect.ValueOf(st).IsZero()})
	}
	{
		r, err := s.Plan(abandoned(), "m")
		got = append(got, result{"Plan", err, reflect.ValueOf(r).IsZero()})
	}
	{
		names, err := s.UnloadAllIdle(abandoned())
		got = append(got, result{"UnloadAllIdle", err, names == nil})
	}
	{
		err := s.Unload(abandoned(), "m", false)
		got = append(got, result{"Unload", err, true})
	}
	{
		had, err := s.ForgetNeed(abandoned(), "m")
		got = append(got, result{"ForgetNeed", err, !had})
	}
	{
		rows, err := s.LearnedRows(abandoned())
		got = append(got, result{"LearnedRows", err, rows == nil})
	}
	{
		had, err := s.ForgetShape(abandoned(), "x")
		got = append(got, result{"ForgetShape", err, !had})
	}
	close(release)
	if _, err := s.Status(ctxT(t)); err != nil { // the loop has run every abandoned query
		t.Fatal(err)
	}
	for _, r := range got {
		if !errors.Is(r.err, context.DeadlineExceeded) || !r.zero {
			t.Errorf("%s abandoned: err %v, zero result %v", r.name, r.err, r.zero)
		}
	}
}
