package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Figures, ids and file names are in the mono face at 400 (--w-num), never
// heavier (the reviewer, build-92: the figures of Benchmarks > Overview's
// "Latest result for each model" and the Zones list drew heavier than the
// names beside them). The bundled mono face has a 400 and a 500 cut only: a
// mono run at the body weight (500) takes the Medium cut, at 600 or more the
// browser fakes a bold. So:
//   - every rule that sets the mono family sets font-weight var(--w-num);
//   - no rule gives an element matched as .mono another weight, and .mono's
//     own weight is !important, so no other class on a .mono element raises it;
//   - the figure cells of the tables (td.r, td .fig, the results' figures) are
//     among those rules;
//   - no view writes another weight inline on a .mono element.
//
// The computed weights were checked once in a real browser (status file).
func TestUINextMonoIsRegular(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("ui", "next", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	weight := regexp.MustCompile(`font-weight\s*:\s*([^;}]+)`)
	monoClass := regexp.MustCompile(`\.mono([^\w-]|$)`)
	figureCells := map[string]bool{".tbl td.r": false, ".tbl td .fig": false, ".bp-fv": false, ".rate-n": false, ".cbar .cv": false}
	monoOwn := ""
	for _, r := range cssRules(string(raw)) {
		if strings.HasPrefix(r.sel, "@font-face") {
			continue
		}
		w := ""
		if m := weight.FindStringSubmatch(r.body); m != nil {
			w = strings.TrimSpace(m[1])
		}
		val := strings.TrimSpace(strings.TrimSuffix(w, "!important"))
		if strings.Contains(r.body, "font-family:var(--mono)") {
			if val != "var(--w-num)" {
				t.Errorf("%s %s: sets the mono family with font-weight %q, want var(--w-num)", r.at, r.sel, w)
			}
			if _, ok := figureCells[r.sel]; ok {
				figureCells[r.sel] = true
			}
		}
		if r.sel == ".mono" {
			monoOwn = w
		}
		if w == "" || val == "var(--w-num)" || val == "inherit" {
			continue
		}
		for _, part := range strings.Split(r.sel, ",") {
			parts := regexp.MustCompile(`[\s>+~]+`).Split(strings.TrimSpace(part), -1)
			if subject := parts[len(parts)-1]; monoClass.MatchString(subject) {
				t.Errorf("%s %s: gives a .mono element font-weight %q, want var(--w-num)", r.at, strings.TrimSpace(part), w)
			}
		}
	}
	if monoOwn != "var(--w-num) !important" {
		t.Errorf(".mono: font-weight %q, want var(--w-num) !important", monoOwn)
	}
	for sel, ok := range figureCells {
		if !ok {
			t.Errorf("%s: no rule sets this figure cell in the mono face at var(--w-num)", sel)
		}
	}
	// no view writes another weight inline on a .mono element
	inline := regexp.MustCompile(`h\('[a-z0-9]*[.\w-]*\.mono[^']*',\s*\{[^}]*font-weight:\s*([^;'"]+)`)
	err = fs.WalkDir(uiFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, err := fs.ReadFile(uiFS, p)
		if err != nil {
			return err
		}
		for _, m := range inline.FindAllStringSubmatch(string(b), -1) {
			if strings.TrimSpace(m[1]) != "var(--w-num)" {
				t.Errorf("%s: a .mono element with font-weight %s inline", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
