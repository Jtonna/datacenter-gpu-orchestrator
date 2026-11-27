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

// Every save or delete of a benchmark or a group requires comment, says
// what it is for and how it is sent, and the arguments error names it: an
// agent can correct itself from the error alone.
func TestBenchSaveAndDeleteToolsRequireAComment(t *testing.T) {
	names := []string{"create_benchmark", "update_benchmark", "delete_benchmark", "create_suite", "update_suite", "delete_suite",
		"create_bench_group", "update_bench_group", "delete_bench_group"}
	by := map[string]*tool{}
	for _, tl := range allTools() {
		by[tl.Name] = tl
	}
	h := newTest(&fakeInner{})
	for _, n := range names {
		tl := by[n]
		if tl == nil {
			t.Fatalf("no tool %s", n)
		}
		req := false
		for _, r := range tl.Required {
			req = req || r == "comment"
		}
		if !req || !tl.saveComment {
			t.Errorf("%s: comment not required (required %v, saveComment %v)", n, tl.Required, tl.saveComment)
		}
		d, _ := tl.Props["comment"].(P)
		desc, _ := d["description"].(string)
		if !strings.Contains(desc, "Required") || !strings.Contains(desc, "audit log") || !strings.Contains(desc, "X-Comment") {
			t.Errorf("%s: the comment's description does not say what it is for and how it is sent: %q", n, desc)
		}
		args := `{"id":"x"}`
		if strings.HasPrefix(n, "create_") {
			args = `{"` + tl.BodyArg + `":{}}`
		} else if strings.HasPrefix(n, "update_") {
			args = `{"id":"x","` + tl.BodyArg + `":{}}`
		}
		out := callTool(t, h, nil, n, args)
		if !out.IsError || !strings.Contains(out.Content[0].Text, `missing required argument "comment"`) {
			t.Errorf("%s without a comment: %+v", n, out)
		}
	}
}
