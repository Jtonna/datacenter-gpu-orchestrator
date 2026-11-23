package config

import (
	"strings"
	"testing"
)

// A profile may carry a free-text description: optional, for people only.
func TestProfileDescription(t *testing.T) {
	llama := "  p:\n    model: m.gguf\n    ctx: 4096\n"
	c := mustParse(t, prof(llama+"    description: Qwen 4B hosted on one console, for the canary\n"))
	if p := c.Profiles["p"]; p.Description != "Qwen 4B hosted on one console, for the canary" {
		t.Fatalf("%q", p.Description)
	}
	if p := mustParse(t, prof(llama)).Profiles["p"]; p.Description != "" {
		t.Fatalf("%q", p.Description)
	}
	// kind command takes one too
	c = mustParse(t, prof("  c:\n    kind: command\n    cmd: run ${PORT}\n    needs_mib: 100\n    description: a helper\n"))
	if c.Profiles["c"].Description != "a helper" {
		t.Fatal(c.Profiles["c"])
	}
	for _, bad := range []string{strings.Repeat("x", 501), `two\nlines`} {
		err := parseErr(prof(llama+"    description: \""+bad+"\"\n"), nil)
		if err == nil || !strings.Contains(err.Error(), "description") {
			t.Errorf("%.20q: %v", bad, err)
		}
	}
	found := false
	for _, f := range profileFields {
		found = found || f.Key == "description"
	}
	if !found {
		t.Error("description is not in the profile schema")
	}
}
