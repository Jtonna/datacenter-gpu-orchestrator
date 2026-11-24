package pool

// ZoneFacts are the server facts the zone views read besides memory (UI
// slice 1a: S4, S6, P6b), embedded in each node entry of GET /api/nodes and
// of /api/status's agents (NodeFacts) and in the local entry (LocalFacts).
// The pool leaves them empty; the API fills them from the scheduler's
// status and the audit log (internal/api/zonefacts.go). Every field is
// additive and absent when it does not apply.
type ZoneFacts struct {
	// Member is the node's part in the instance placed on it.
	Member *MemberFacts `json:"member,omitempty"`
	// SetAside is why the node is set aside (quarantined) and until when.
	SetAside *SetAsideFacts `json:"set_aside,omitempty"`
	// PausedAt and PausedActor: when and by whom the node was paused, from
	// the newest pause in the audit log (absent when the log has none).
	PausedAt    string `json:"paused_at,omitempty"`
	PausedActor string `json:"paused_actor,omitempty"`
	// ExcludedAt and ExcludedActor: when and by whom the exclude_nodes
	// entry that matches the node (excluded_by) was added, from the newest
	// configuration change in the audit log that added it.
	ExcludedAt    string `json:"excluded_at,omitempty"`
	ExcludedActor string `json:"excluded_actor,omitempty"`
	// FreeAt is when a claimed or settling node is expected to be free;
	// FreeInS the seconds left (0 when due); FreeBasis how it was worked
	// out (settling, fetching, stopping, draining: no time, claimed). Next
	// is the model that claimed it.
	FreeAt    string   `json:"free_at,omitempty"`
	FreeInS   *float64 `json:"free_in_s,omitempty"`
	FreeBasis string   `json:"free_basis,omitempty"`
	Next      string   `json:"next,omitempty"`
	// Group is the zone group the node is in (absent when in none).
	Group string `json:"group,omitempty"`
	// ZoneState is the state the zone is counted in (free, in_use,
	// loading, lost, set_aside, off; removed for a removed one, which is
	// not counted): the same classification as the Zones row of
	// /api/health and the counts of GET /api/nodes.
	ZoneState string `json:"zone_state,omitempty"`
	// AttentionRank orders "needs attention first" (0 down, 1 trouble, 2
	// loading, 3 in use, 4 paused, 5 free, 6 excluded, 7 removed); the
	// page and MCP list_zones use the same order (internal/api/counts.go).
	AttentionRank int `json:"attention_rank"`
}

// MemberFacts is a node's part in an instance: its role (host runs
// llama-server, helper is an RPC worker) and its share of the model (0 to
// 1, its tensor split weight over the sum; the shares of an instance add up
// to 1).
type MemberFacts struct {
	Profile  string  `json:"profile"`
	State    string  `json:"state"` // the instance's state
	Role     string  `json:"role"`
	Share    float64 `json:"share"`
	Host     string  `json:"host"` // server or node
	HostNode string  `json:"host_node,omitempty"`
}

// SetAsideFacts is a quarantine: its reason word (preflight_failed,
// agent_unreachable, worker_hung, node_lost, other), the cause in words,
// the load it happened in, since and until when, and the level (how many
// times in a row).
type SetAsideFacts struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	Profile string `json:"profile,omitempty"`
	Since   string `json:"since,omitempty"`
	Until   string `json:"until"`
	Level   int    `json:"level"`
}
