package history

import "testing"

// A node filter matches an event that names the node by any of its names:
// events carry the hostname, callers often hold the id (B1).
func TestQueryNodeMatchesAnyAlias(t *testing.T) {
	byName := &Event{Type: TNodeAdmin, Nodes: []string{"console-1"}}
	byID := &Event{Type: TNodeAdmin, Nodes: []string{"02:00:00:00:00:01"}}
	other := &Event{Type: TNodeAdmin, Nodes: []string{"console-2"}}
	q := Query{Node: "02:00:00:00:00:01", NodeAliases: []string{"console-1"}}
	if !q.Match(byName) || !q.Match(byID) || q.Match(other) {
		t.Fatalf("alias match: name %v id %v other %v", q.Match(byName), q.Match(byID), q.Match(other))
	}
	plain := Query{Node: "console-1"}
	if !plain.Match(byName) || plain.Match(byID) {
		t.Fatal("a query without aliases matches the given name only")
	}
}
