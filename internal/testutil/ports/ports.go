// Package ports picks a TCP port for a test whose server binds it later
// (another process, or a fake that is handed a port number).
//
// The old helpers bound port 0, read the port and closed the listener:
// the port came from the system's ephemeral range, the same range every
// port-0 listener and every outgoing connection of the other test
// packages running in parallel draws from, so one of those could take it
// before the test bound it ("bind: address already in use", once in a full
// parallel run). Free picks outside the ephemeral range instead (Linux
// 32768-60999, Windows and macOS 49152-65535), never hands out the same
// port twice in one process, and checks the port binds on the address the
// test will use. Where the test owns the server, binding port 0 at the
// server and reading the port back is better still (fakeagent.AutoPort).
package ports

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"testing"
)

// The range Free picks from: below every common ephemeral range.
const (
	lo = 20000
	hi = 30000
)

var (
	mu    sync.Mutex
	given = map[int]bool{}
)

// FreeOn is a port that binds on host now, outside the ephemeral range and
// not handed out before in this process; an error when host cannot be
// bound at all (e.g. no second loopback address) or no port is free.
func FreeOn(host string) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	start := lo + rand.IntN(hi-lo)
	var last error
	for i := 0; i < hi-lo; i++ {
		p := lo + (start-lo+i)%(hi-lo)
		if given[p] {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err != nil {
			last = err
			if i == 0 {
				// the address itself may be missing: try port 0 to tell
				l0, err0 := net.Listen("tcp", net.JoinHostPort(host, "0"))
				if err0 != nil {
					return 0, err0
				}
				l0.Close()
			}
			continue
		}
		l.Close()
		given[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("ports: no free port in %d-%d on %s: %v", lo, hi-1, host, last)
}

// Free is FreeOn, failing the test on an error.
func Free(tb testing.TB, host string) int {
	tb.Helper()
	p, err := FreeOn(host)
	if err != nil {
		tb.Fatalf("ports.Free: %v", err)
	}
	return p
}
