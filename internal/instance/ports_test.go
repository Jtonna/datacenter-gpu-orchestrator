package instance

import (
	"errors"
	"math/rand"
	"net"
	"strconv"
	"testing"
)

// contiguousRange finds n consecutive ports that are free right now. It picks
// below the OS ephemeral range (outbound connections of parallel test
// packages take ports from there) so the range stays free for the test.
func contiguousRange(t *testing.T, n int) int {
	t.Helper()
	for try := 0; try < 50; try++ {
		base := 20000 + rand.Intn(10000)
		var ls []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(base+i))
			if err != nil {
				ok = false
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Skip("could not find a free contiguous port range")
	return 0
}

func TestNewPortAllocatorValidates(t *testing.T) {
	for _, r := range [][2]int{{0, 10}, {10, 5}, {1, 70000}, {-1, 5}} {
		if _, err := NewPortAllocator(r[0], r[1]); err == nil {
			t.Errorf("range %v accepted", r)
		}
	}
}

func TestPortAllocatorHandsOutDistinctPortsInRange(t *testing.T) {
	base := contiguousRange(t, 4)
	a, err := NewPortAllocator(base, base+3)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		p, err := a.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if p < base || p > base+3 || seen[p] {
			t.Fatalf("bad or repeated port %d", p)
		}
		seen[p] = true
	}
	if a.InUse() != 4 {
		t.Fatalf("InUse = %d", a.InUse())
	}
	if _, err := a.Acquire(); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("err = %v, want ErrNoPorts", err)
	}
}

func TestPortAllocatorReleaseMakesPortAvailable(t *testing.T) {
	base := contiguousRange(t, 1)
	a, _ := NewPortAllocator(base, base)
	p, err := a.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	a.Release(p)
	a.Release(p) // idempotent
	a.Release(base + 100)
	if a.InUse() != 0 {
		t.Fatalf("InUse = %d", a.InUse())
	}
	if p2, err := a.Acquire(); err != nil || p2 != p {
		t.Fatalf("got %d, %v", p2, err)
	}
}

func TestPortAllocatorSkipsPortsInUseByOthers(t *testing.T) {
	base := contiguousRange(t, 3)
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(base))
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	a, _ := NewPortAllocator(base, base+2)
	for i := 0; i < 2; i++ {
		p, err := a.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if p == base {
			t.Fatalf("handed out busy port %d", p)
		}
	}
	if _, err := a.Acquire(); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("err = %v, want ErrNoPorts (one port is busy)", err)
	}
}

func TestPortAllocatorDoesNotImmediatelyReuse(t *testing.T) {
	base := contiguousRange(t, 3)
	a, _ := NewPortAllocator(base, base+2)
	p1, _ := a.Acquire()
	a.Release(p1)
	p2, _ := a.Acquire()
	if p2 == p1 {
		t.Fatalf("reused %d at once; a just-stopped child's port may linger", p1)
	}
}
