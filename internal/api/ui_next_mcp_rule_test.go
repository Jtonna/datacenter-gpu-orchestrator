package api

import (
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
)

// The fixture's GET /api/mcp/tools carries the same group notes as the
// server, so pictures of the MCP page show what live shows.
func TestUIFixtureGroupNotesMatchTheServer(t *testing.T) {
	get := fixtureServer(t, "--keys", "some")
	var fx struct {
		Groups []struct{ ID, Title, Note string } `json:"groups"`
	}
	get("/api/mcp/tools", &fx)
	srv := mcpserver.New(nil, mcpserver.Options{Version: "test"}).ToolInfo().(map[string]any)
	want := map[string]string{}
	for _, g := range srv["groups"].([]map[string]any) {
		want[g["id"].(string)] = g["note"].(string)
	}
	if len(fx.Groups) != len(want) {
		t.Fatalf("fixture has %d groups, the server %d", len(fx.Groups), len(want))
	}
	for _, g := range fx.Groups {
		if want[g.ID] != g.Note {
			t.Errorf("group %s: fixture %q, server %q", g.ID, g.Note, want[g.ID])
		}
	}
}
