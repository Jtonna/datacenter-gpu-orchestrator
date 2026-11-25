package api

import (
	"fmt"
	"testing"
)

// POST /api/plan says which other profiles share the profile's shape (and
// whether each was generated), with its ctx and parallel: what a
// context_fill step checks before it loads anything (B8).
func TestPlanSaysWhichProfilesShareTheShape(t *testing.T) {
	e, _, _ := cfgEnv(t)
	for _, js := range []string{
		`{"id":"p1","model":"p1.gguf","ctx":2048,"needs_mib":100}`,
		`{"id":"p2","model":"p1.gguf","ctx":2048,"needs_mib":100}`,
		`{"id":"bench-p1","model":"p1.gguf","ctx":2048,"needs_mib":100,"generated":true}`,
		`{"id":"p3","model":"p1.gguf","ctx":4096,"needs_mib":100}`,
	} {
		if st, m := e.cdo("POST", "/api/config/profiles", js, nil); st >= 300 {
			t.Fatalf("%s: %d %v", js, st, m)
		}
	}
	st, m := e.cdo("POST", "/api/plan", `{"model":"p1"}`, nil)
	if st != 200 {
		t.Fatalf("%d %v", st, m)
	}
	if fmt.Sprint(m["shape_shared_with"]) != "[map[generated:true id:bench-p1] map[generated:false id:p2]]" || m["ctx"] != float64(2048) || m["parallel"] != float64(1) {
		t.Fatalf("%v %v %v", m["shape_shared_with"], m["ctx"], m["parallel"])
	}
	_, m = e.cdo("POST", "/api/plan", `{"model":"p3"}`, nil)
	if fmt.Sprint(m["shape_shared_with"]) != "[]" || m["ctx"] != float64(4096) {
		t.Fatalf("%v %v", m["shape_shared_with"], m["ctx"])
	}
}
