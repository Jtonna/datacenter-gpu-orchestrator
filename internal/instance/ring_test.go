package instance

import (
	"reflect"
	"sync"
	"testing"
)

func TestRingKeepsLastLines(t *testing.T) {
	r := newRing(3)
	if got := r.Lines(); len(got) != 0 {
		t.Fatalf("empty ring = %q", got)
	}
	r.Add("a")
	r.Add("b")
	if got := r.Lines(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %q", got)
	}
	for _, s := range []string{"c", "d", "e"} {
		r.Add(s)
	}
	if got := r.Lines(); !reflect.DeepEqual(got, []string{"c", "d", "e"}) {
		t.Fatalf("got %q", got)
	}
	if got := r.Last(2); !reflect.DeepEqual(got, []string{"d", "e"}) {
		t.Fatalf("Last(2) = %q", got)
	}
	if got := r.Last(10); len(got) != 3 {
		t.Fatalf("Last(10) = %q", got)
	}
	if got := r.Last(0); len(got) != 0 {
		t.Fatalf("Last(0) = %q", got)
	}
}

func TestRingConcurrent(t *testing.T) {
	r := newRing(50)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Add("x")
				r.Last(5)
			}
		}()
	}
	wg.Wait()
	if len(r.Lines()) != 50 {
		t.Fatalf("len = %d", len(r.Lines()))
	}
}
