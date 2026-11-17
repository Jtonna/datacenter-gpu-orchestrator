package instance

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
)

// ErrNoPorts means every port in the range is held by a live child or is in
// use by another process.
var ErrNoPorts = errors.New("instance: no free port in range")

// PortAllocator hands out free localhost TCP ports from [lo, hi]. A port is
// never handed out while a live child holds it (until Release), nor while any
// other process is listening on it. Ports are tried round-robin, so a port
// just released is the last candidate, not the first. Safe for concurrent use.
type PortAllocator struct {
	mu   sync.Mutex
	lo   int
	hi   int
	next int
	used map[int]bool
}

// NewPortAllocator returns an allocator for the inclusive range [lo, hi].
func NewPortAllocator(lo, hi int) (*PortAllocator, error) {
	if lo < 1 || hi > 65535 || lo > hi {
		return nil, fmt.Errorf("instance: bad port range %d-%d", lo, hi)
	}
	return &PortAllocator{lo: lo, hi: hi, next: lo, used: map[int]bool{}}, nil
}

// Acquire returns a port that is free now and reserves it until Release.
func (a *PortAllocator) Acquire() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	size := a.hi - a.lo + 1
	for i := 0; i < size; i++ {
		p := a.lo + (a.next-a.lo+i)%size
		if a.used[p] || !portFree(p) {
			continue
		}
		a.used[p] = true
		a.next = p + 1
		if a.next > a.hi {
			a.next = a.lo
		}
		return p, nil
	}
	return 0, ErrNoPorts
}

// Release gives a port back. Releasing a port that is not held is a no-op.
func (a *PortAllocator) Release(port int) {
	a.mu.Lock()
	delete(a.used, port)
	a.mu.Unlock()
}

// InUse reports how many ports are currently reserved.
func (a *PortAllocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.used)
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		return false
	}
	l.Close()
	return true
}
