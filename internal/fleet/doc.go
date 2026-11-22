// Package fleet is the fleet console client (SSE, polling fallback, single-node fetch), the local GPU probe, and the merged node view in MiB.
//
// LEGACY: the fleet console client (Client, Member, the feed and the poll)
// is the old membership mode (defaults.membership: fleet). It is kept until
// phase 7 and gets no new features. View, ErrNotFound, AmbiguousError,
// NeverReceived and ParseSMI are also used by the agent membership
// (internal/pool) and the scheduler core; they move out of this package
// before the client is removed.
package fleet
