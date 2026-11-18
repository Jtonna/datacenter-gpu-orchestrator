package instance

import "errors"

// ByteCounter is implemented by a Child whose upload progress can be read
// from outside: how many bytes it has sent to the given worker addresses
// ("ip:port", as passed to --rpc). It is optional; the scheduler falls back
// to an estimate when a child does not implement it or it fails.
//
// SentBytes may take a moment and must be called off the scheduler's event
// loop.
type ByteCounter interface {
	SentBytes(peers []string) (SentCount, error)
}

// SentCount is one reading of a child's upload.
type SentCount struct {
	// Bytes is the sum over every worker.
	Bytes int64
	// Sockets is how many TCP connections to those peers were found; zero
	// means the child has not connected yet, so the figures carry no
	// information.
	Sockets int
	// Peers is the acknowledged byte count per worker, keyed by the address
	// exactly as it was passed in. A worker with no connection is absent.
	Peers map[string]int64
}

// ErrSentBytesUnsupported means this platform cannot count a child's sent
// bytes (anything but Linux).
var ErrSentBytesUnsupported = errors.New("instance: counting sent bytes is not supported on this platform")

// SentBytes implements ByteCounter for a running child.
func (p *proc) SentBytes(peers []string) (SentCount, error) {
	return socketBytesAcked(p.PID(), peers)
}
