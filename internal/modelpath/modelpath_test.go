package modelpath

import (
	"strings"
	"testing"
)

// The one rule for a profile's model path: relative, forward slashes, no
// part that starts with a dot. A plain sub-folder stays allowed.
func TestCheck(t *testing.T) {
	for _, ok := range []string{
		"a.gguf",
		"Qwen_Qwen3.5-9B-Q4_K_M.gguf",
		"sub/a.gguf",
		"sub/deeper/a.gguf",
		"a.b.gguf",     // a dot inside a name is fine
		"sub.d/a.gguf", // and inside a folder name
		"a..gguf",      // two dots inside a name are not a ".." part
		"name with space.gguf",
	} {
		if err := Check(ok); err != nil {
			t.Errorf("Check(%q) = %v, want allowed", ok, err)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"", "no model file named"},
		{".trash/0123/a.gguf", "starts with a dot"},
		{".incoming/a.gguf", "starts with a dot"},
		{".potluck-uploads/a.gguf", "starts with a dot"},
		{"sub/.hidden/a.gguf", "starts with a dot"},
		{".a.gguf", "starts with a dot"},
		{"./a.gguf", "starts with a dot"},
		{".", "starts with a dot"},
		{"..", ".."},
		{"../a.gguf", ".."},
		{"sub/../a.gguf", ".."},
		{"sub/../../etc/a.gguf", ".."},
		{"/models/a.gguf", "absolute"},
		{"/etc/passwd", "absolute"},
		{`\a.gguf`, "backslash"},
		{`sub\a.gguf`, "backslash"},
		{`..\a.gguf`, "backslash"},
		{`C:\models\a.gguf`, "drive"},
		{"C:/models/a.gguf", "drive"},
		{"c:a.gguf", "drive"},
		{"a\x00.gguf", "NUL"},
		{"sub/", "empty part"},
		{"sub//a.gguf", "empty part"},
	} {
		err := Check(c.in)
		if err == nil {
			t.Errorf("Check(%q) = nil, want refused (%s)", c.in, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%q) = %q, want it to say %q", c.in, err, c.want)
		}
	}
}

// Join joins a checked path to the models folder with the separator the
// folder already uses, and refuses what Check refuses.
func TestJoin(t *testing.T) {
	for _, c := range []struct{ dir, model, want string }{
		{"/models", "a.gguf", "/models/a.gguf"},
		{"/models/", "sub/a.gguf", "/models/sub/a.gguf"},
		{`C:\m`, "sub/a.gguf", `C:\m\sub\a.gguf`},
		{"", "a.gguf", "a.gguf"},
	} {
		got, err := Join(c.dir, c.model)
		if err != nil || got != c.want {
			t.Errorf("Join(%q, %q) = %q, %v; want %q", c.dir, c.model, got, err, c.want)
		}
	}
	for _, bad := range []string{".trash/x/a.gguf", "../a.gguf", "/etc/a.gguf", `sub\a.gguf`} {
		if got, err := Join("/models", bad); err == nil {
			t.Errorf("Join(/models, %q) = %q, want refused", bad, got)
		}
	}
}
