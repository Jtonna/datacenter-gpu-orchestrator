package e2e

import (
	"strings"
	"testing"
)

// A configuration change through REST takes effect in the running binary
// without a restart, and is audited with the source the server decided.
func TestLiveConfigChangeWithoutRestart(t *testing.T) {
	c := withPS5(t, nil)
	st := c.jsonGet("/api/config")
	if st["configured"] != true || st["revision"] != float64(1) {
		t.Fatalf("imported seed should be revision 1: %v", st["revision"])
	}
	// A new profile that reuses an existing model file.
	code, b, _ := c.do("POST", "/api/config/profiles", `{"id":"fresh","model":"tiny.gguf","ctx":1024,"needs_mib":500,"allow_tiers":[1]}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	if status, body := c.chat("fresh", false); status != 200 {
		t.Fatalf("the new profile does not serve: %d %s", status, body)
	}
	models := c.jsonGet("/v1/models")
	if !strings.Contains(mustJSON(models), `"fresh"`) {
		t.Fatalf("/v1/models: %v", models)
	}
	status := c.jsonGet("/api/status")
	if status["config_revision"] != float64(2) {
		t.Fatalf("status revision: %v", status["config_revision"])
	}
	// The loaded instance is flagged stale once its profile changes, and keeps serving.
	if code, b, _ = c.do("PUT", "/api/config/profiles/fresh", `{"model":"tiny.gguf","ctx":2048,"needs_mib":500,"allow_tiers":[1]}`); code != 200 {
		t.Fatalf("update: %d %s", code, b)
	}
	stale := false
	for _, in := range c.jsonGet("/api/status")["instances"].([]any) {
		im := in.(map[string]any)
		if im["profile"] == "fresh" && im["stale_profile"] == true {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("instance not flagged stale: %v", c.jsonGet("/api/status")["instances"])
	}
	if status, _ := c.chat("fresh", false); status != 200 {
		t.Fatalf("a stale instance stopped serving: %d", status)
	}
	// Delete: new requests are refused as unknown.
	if code, b, _ = c.do("DELETE", "/api/config/profiles/fresh", ""); code != 200 {
		t.Fatalf("delete: %d %s", code, b)
	}
	if status, _ := c.chat("fresh", false); status != 404 {
		t.Fatalf("deleted profile answered %d, want 404", status)
	}
	// Audit trail.
	log := c.jsonGet("/api/audit?target=fresh&order=asc")
	es := log["entries"].([]any)
	if len(es) != 3 || es[0].(map[string]any)["action"] != "create" || es[2].(map[string]any)["action"] != "delete" {
		t.Fatalf("%v", log)
	}
	// The import was audited as such.
	imp := c.jsonGet("/api/audit?action=import")
	if imp["total_matching"].(float64) < 1 || imp["entries"].([]any)[0].(map[string]any)["source"] != "import" {
		t.Fatalf("%v", imp)
	}
}
