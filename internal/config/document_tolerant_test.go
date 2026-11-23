package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A stored document written by a newer version may carry fields this
// version does not know: a new optional profile field, a new setting, a
// field of a tier. ParseStoredDocument keeps everything it understands,
// drops the rest, and names each unknown field (map keys as *); ParseDocument
// (what clients send) still refuses them.
func TestParseStoredDocumentToleratesUnknownFields(t *testing.T) {
	doc := `{
	  "settings": {"queue_timeout": "90s", "x_setting_from_the_future": 7},
	  "tiers": [{"match": {"hostname": "ps5-*", "rack": "r1"}, "priority": 1, "glow": true}],
	  "profiles": {
	    "qwen": {"kind": "llama", "model": "qwen.gguf", "ctx": 4096, "x_field_from_the_future": "a small model", "ttl": "5m"},
	    "gemma": {"kind": "llama", "model": "gemma.gguf", "ctx": 2048, "x_field_from_the_future": "another"}
	  },
	  "exclude_nodes": ["a"],
	  "node_overrides": {"a": {"rpc": "192.0.2.1:50052", "weight": 2}},
	  "recovery": {},
	  "fleet": {},
	  "x_top_from_the_future": {"x": 1}
	}`
	if _, err := ParseDocument([]byte(doc)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("a client document with unknown fields is accepted: %v", err)
	}
	d, unknown, err := ParseStoredDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node_overrides.*.weight", "profiles.*.x_field_from_the_future", "settings.x_setting_from_the_future", "tiers[].glow", "tiers[].match.rack", "x_top_from_the_future"}
	if !reflect.DeepEqual(unknown, want) {
		t.Fatalf("unknown fields %q, want %q", unknown, want)
	}
	if len(d.Profiles) != 2 || d.Profiles["qwen"].Model != "qwen.gguf" || *d.Profiles["qwen"].Ctx != 4096 || d.Profiles["qwen"].TTL == nil {
		t.Fatalf("known profile fields lost: %+v", d.Profiles["qwen"])
	}
	if d.Settings["queue_timeout"] != "90s" || d.Settings["x_setting_from_the_future"] != nil {
		t.Fatalf("settings %v", d.Settings)
	}
	if d.Tiers[0].Match.Hostname != "ps5-*" || d.Tiers[0].Priority != 1 || d.NodeOverrides["a"].RPC != "192.0.2.1:50052" {
		t.Fatalf("tiers or overrides lost: %+v %+v", d.Tiers, d.NodeOverrides)
	}
	// what is kept builds: the unknown setting no longer fails validation
	if _, err := Build(d, Bootstrap{}, Options{FileExists: func(string) bool { return true }}); err != nil {
		t.Fatalf("the kept document does not build: %v", err)
	}
	// a document with no unknown field reports none, and round-trips
	b, _ := json.Marshal(d)
	d2, unknown2, err := ParseStoredDocument(b)
	if err != nil || len(unknown2) != 0 || !reflect.DeepEqual(d, d2) {
		t.Fatalf("round trip: %v %q", err, unknown2)
	}
	// broken JSON is still an error
	if _, _, err := ParseStoredDocument([]byte(`{"profiles": `)); err == nil {
		t.Fatal("broken JSON accepted")
	}
}
