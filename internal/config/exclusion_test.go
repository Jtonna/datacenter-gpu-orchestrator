package config

import (
	"reflect"
	"strings"
	"testing"
)

// A profile that forces an excluded node is valid configuration (0.1.7): the
// profile is kept and refused at run time. ExclusionBlocks lists each such
// profile once, with the forced name and the matching exclude_nodes entry,
// sorted by profile; the warning names all three.
func TestExclusionBlocks(t *testing.T) {
	llama := func(name, force string) string {
		return "  " + name + ":\n    model: m.gguf\n    ctx: 4096\n    force_nodes: [" + force + "]\n"
	}
	y := baseHeader + "exclude_nodes: [\"02:00:00:00:00:01\", \"laptop-*\"]\nprofiles:\n" +
		llama("b-one", "02:00:00:00:00:01") + llama("a-two", "laptop-2") + llama("c-free", "ps5-1") +
		"  d-two:\n    model: m.gguf\n    ctx: 4096\n    force_nodes: [ps5-1, laptop-3]\n"
	c := mustParse(t, y)
	got := c.ExclusionBlocks()
	want := []ExclusionBlock{
		{Profile: "a-two", Node: "laptop-2", Entry: "laptop-*"},
		{Profile: "b-one", Node: "02:00:00:00:00:01", Entry: "02:00:00:00:00:01"},
		{Profile: "d-two", Node: "laptop-3", Entry: "laptop-*"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	w := got[0].Warning()
	for _, s := range []string{`profile "a-two"`, `force_nodes`, `"laptop-2"`, `exclude_nodes entry "laptop-*"`, "kept", "refused"} {
		if !strings.Contains(w, s) {
			t.Errorf("warning %q lacks %s", w, s)
		}
	}
	if b, ok := c.BlockedByExclusion("b-one"); !ok || b.Entry != "02:00:00:00:00:01" {
		t.Fatalf("%+v %v", b, ok)
	}
	if _, ok := c.BlockedByExclusion("c-free"); ok {
		t.Fatal("c-free is not blocked")
	}
	// The import of such a file is valid too, and says so in its notes.
	imp, err := ParseYAMLImport([]byte(y), Bootstrap{ModelsDir: "/models"}, Options{FileExists: existsAll})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, note := range imp.Notes {
		if strings.Contains(note, "exclude_nodes entry") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("notes %v", imp.Notes)
	}
}
