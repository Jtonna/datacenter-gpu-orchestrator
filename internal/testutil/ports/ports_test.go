package ports

import (
	"net"
	"strconv"
	"testing"
)

// Free gives distinct ports outside the ephemeral ranges, each bindable.
func TestFreeIsOutsideTheEphemeralRangeAndDistinct(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		p := Free(t, "127.0.0.1")
		if p < lo || p >= hi || seen[p] {
			t.Fatalf("port %d (seen %v)", p, seen[p])
		}
		seen[p] = true
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
}

// An address the machine does not have is an error, not a port.
func TestFreeOnAMissingAddress(t *testing.T) {
	if _, err := FreeOn("192.0.2.77"); err == nil {
		t.Fatal("want an error")
	}
}
