package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// NeverReceived is the Age reported before the first byte has arrived.
const NeverReceived time.Duration = 1<<63 - 1

// View is what the scheduler core depends on. The implementation is
// *Client; the core's tests fake this interface. Every method is safe for
// concurrent use.
type View interface {
	// Nodes is a snapshot of every usable node (remote ones from the fleet
	// console, the local one from the probe), sorted by ID. Members that
	// match no tier are absent. Node.Online is the fleet's own flag (the
	// local node is always online); whether the view as a whole is fresh
	// enough is the caller's call, using Age and Connected.
	Nodes() []core.Node

	// Age is the time since the last byte arrived from the fleet console
	// (stream events and pings, or a polled response), design 8.4. It is
	// NeverReceived before the first byte.
	Age() time.Duration

	// Connected reports whether the SSE stream is currently open. While it
	// is false the client polls GET /api/fleet instead.
	Connected() bool

	// LastReconnect is when the stream last (re)connected; zero if never.
	// The scheduler's node-loss counter resets when this changes.
	LastReconnect() time.Time

	// FetchNode reads one node fresh (GET /api/nodes/:id, or the local
	// probe for the local node). Any error, including a non-200 answer,
	// means the caller should treat the node as offline. A member that
	// exists but is offline returns a Node with Online false and no error.
	FetchNode(ctx context.Context, id core.NodeID) (core.Node, error)

	// Changed returns a channel that receives a value after the node set,
	// any node's figures, or the connected state changed. It has capacity
	// one and sends never block, so bursts coalesce into one wake-up; read
	// Nodes after each wake-up. Pings and unchanged snapshots do not fire it.
	Changed() <-chan struct{}

	// Resolve maps a config name (fleet id or hostname) to a node id, with
	// the design 6 ambiguity rule. The name of the local node resolves too.
	Resolve(name string) (core.NodeID, error)
}

// ErrNotFound is wrapped by Resolve and FetchNode when no such node exists.
var ErrNotFound = errors.New("fleet: node not found")

// AmbiguousError is returned by Resolve when a hostname is shared by two or
// more online members, which makes it match none of them.
type AmbiguousError struct {
	Name string
	IDs  []core.NodeID
}

func (e *AmbiguousError) Error() string {
	ids := make([]string, len(e.IDs))
	for i, id := range e.IDs {
		ids[i] = string(id)
	}
	sort.Strings(ids)
	return fmt.Sprintf("fleet: hostname %q is ambiguous: online members %s", e.Name, strings.Join(ids, ", "))
}
