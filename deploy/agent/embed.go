// Package agentdeploy holds the agent's install scripts; Potluck serves the
// setup script to a machine joining as a zone (internal/api/pending.go).
package agentdeploy

import _ "embed"

// Setup is potluck-agent-setup, as shipped in this directory.
//
//go:embed potluck-agent-setup
var Setup []byte
