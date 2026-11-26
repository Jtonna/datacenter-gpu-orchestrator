package api

import (
	"fmt"
	"strings"
	"testing"
)

// p33: the two profile fields through the API: a write sets them, GET
// /api/profiles lists them only when set (absent: the cluster value), and
// the flags in extra_args are refused with the sentence that points to the
// fields.
func TestProfileHostMemoryThroughTheAPI(t *testing.T) {
	e, _, _ := cfgEnv(t)
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"own","model":"own.gguf","ctx":2048,"needs_mib":100,"ctx_checkpoints":3,"cache_ram_mib":0}`, nil); st != 201 {
		t.Fatalf("%d %v", st, m)
	}
	if st, m := e.cdo("POST", "/api/config/profiles", `{"id":"plain","model":"plain.gguf","ctx":2048,"needs_mib":100}`, nil); st != 201 {
		t.Fatalf("%d %v", st, m)
	}
	st, m := e.cdo("POST", "/api/config/profiles", `{"id":"flag","model":"f.gguf","ctx":2048,"needs_mib":100,"extra_args":["--ctx-checkpoints","3"]}`, nil)
	if st != 400 || !strings.Contains(fmt.Sprint(m), "set this profile's field ctx_checkpoints") {
		t.Fatalf("flag: %d %v", st, m)
	}
	ps := jsonOf(t, body(t, e.get("/api/profiles")))
	seen := 0
	for _, p := range ps["profiles"].([]any) {
		pm := p.(map[string]any)
		switch pm["id"] {
		case "own":
			seen++
			if pm["ctx_checkpoints"] != 3.0 || pm["cache_ram_mib"] != 0.0 {
				t.Errorf("own %v %v", pm["ctx_checkpoints"], pm["cache_ram_mib"])
			}
		case "plain":
			seen++
			if _, ok := pm["ctx_checkpoints"]; ok {
				t.Errorf("plain lists ctx_checkpoints")
			}
			if _, ok := pm["cache_ram_mib"]; ok {
				t.Errorf("plain lists cache_ram_mib")
			}
		}
	}
	if seen != 2 {
		t.Fatalf("profiles %v", ps)
	}
}
