package mcpserver

import (
	"strings"
	"testing"
)

// The "Load and unload" group note is shown on the MCP page and to agents.
// Since the owner's decision the web UI loads, unloads and force-unloads
// (Zones, Machine details). The note must not state the old rule, and it
// ends there: the clause about an on/off switch is dropped (the owner's word, 25 Nov).
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
	for _, want := range []string{"web UI", "force-unload"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not say %q: %s", want, note)
		}
	}
	if strings.Contains(note, "on/off") || !strings.HasSuffix(note, "(load, unload and force-unload, from Zones and Machine details).") {
		t.Errorf("the note must end at the Zones and Machine details clause: %s", note)
	}
}

// list_model_files and list_deleted_models name models_writable, so an agent
// knows before it tries whether a delete or a restore can work here.
func TestModelToolsNameTheWritableFlag(t *testing.T) {
	for _, tl := range allTools() {
		if tl.Name == "list_model_files" || tl.Name == "list_deleted_models" {
			if !strings.Contains(tl.Desc, "models_writable") {
				t.Errorf("%s does not name models_writable: %s", tl.Name, tl.Desc)
			}
		}
	}
}
