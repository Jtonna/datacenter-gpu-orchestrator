package mcpserver

import (
	"strings"
	"testing"
)

// The "Load and unload" group note is shown on the MCP page and to agents.
// Since the owner's decision the web UI loads, unloads and force-unloads
// (Zones, Machine details); what it has not is an on/off switch for the
// scheduler. The note must not state the old rule.
func TestRuntimeGroupNoteStatesTheUIRule(t *testing.T) {
	info := newTest(nil).ToolInfo().(map[string]any)
	var note string
	for _, g := range info["groups"].([]map[string]any) {
		if g["id"] == GroupRuntime {
			note = g["note"].(string)
		}
	}
	if note == "" {
		t.Fatal("no runtime group")
	}
	for _, old := range []string{"no such control", "Only agents", "only agents", "only the API", "cannot load", "UI has no"} {
		if strings.Contains(note, old) {
			t.Errorf("the note states the old rule (%q): %s", old, note)
		}
	}
	for _, want := range []string{"web UI", "force-unload", "on/off switch"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not say %q: %s", want, note)
		}
	}
}
