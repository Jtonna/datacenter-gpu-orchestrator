package api

import (
	"encoding/json"
	"testing"
)

// withHostMemory is the /api/models answer with host_memory on every
// profile of model key (GET /api/profiles' host_memory, scheduler hostmem.go).
func withHostMemory(t *testing.T, models any, key string, hm map[string]any) any {
	t.Helper()
	b, _ := json.Marshal(models)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range jlist(out["models"]) {
		mm := jmap(m)
		if jstr(mm["key"]) != key {
			continue
		}
		for _, p := range jlist(mm["profiles"]) {
			jmap(p)["host_memory"] = hm
			found = true
		}
	}
	if !found {
		t.Fatalf("no profile of %s in the answer", key)
	}
	return out
}

// The Models panel says, for a recurrent or windowed family, what the
// model keeps on the machine that hosts it, and whether what it needs
// counts it; nothing for a plain family. The plan's memory table names the
// term inside the context column when it is counted.
func TestUIHostMemoryLine(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	rec := map[string]any{"family": "recurrent", "cell_mib": 50.25, "checkpoints": 32, "checkpoints_mib": 1608, "prompt_cache_mib": 0, "on_node": true, "counted": false}
	counted := map[string]any{}
	for k, v := range rec {
		counted[k] = v
	}
	counted["counted"] = true
	plain := map[string]any{"family": "plain", "cell_mib": 0, "checkpoints": 32, "checkpoints_mib": 0, "on_node": false, "counted": false}
	unknown := map[string]any{"family": "windowed", "cell_mib": 0, "checkpoints": 32, "checkpoints_mib": 0, "prompt_cache_mib": 2048, "on_node": false, "counted": false, "size_unknown": true, "estimate": false}
	window := map[string]any{"family": "windowed", "cell_mib": 320, "checkpoints": 32, "checkpoints_mib": 10240, "prompt_cache_mib": 2048, "on_node": false, "counted": false, "estimate": true}
	with := func(hm map[string]any) map[string]any {
		f := map[string]any{}
		for k, v := range fetch {
			f[k] = v
		}
		f["/api/models"] = withHostMemory(t, fetch["/api/models"], mdlLoaded, hm)
		return f
	}
	const line = "Conversation memory: this model keeps restore points on the machine that hosts it: about 50 MB each, up to 32 (1,608 MB)"
	// one run per answer: the app keeps the catalog it fetched first
	for _, c := range []struct {
		hm   map[string]any
		want map[string]any
	}{
		{rec, map[string]any{"name": "host memory: not yet counted", "page": []string{line + ", not yet counted."}, "pageHas": []string{"[data-convmem]"}}},
		{counted, map[string]any{"name": "host memory: counted", "page": []string{line + ", counted in what it needs."}}},
		{plain, map[string]any{"name": "host memory: nothing for a plain family", "pageNone": []string{"[data-convmem]"}, "notOnPage": []string{"Conversation memory"}}},
		{unknown, map[string]any{"name": "host memory: a window size not yet known shows no number",
			"page":      []string{"Conversation memory: this model keeps restore points on the machine that hosts it: size not yet known: read at the first load."},
			"notOnPage": []string{" MB each", "(0 MB)"}}},
		{window, map[string]any{"name": "host memory: a window model says at most",
			"page": []string{"Conversation memory: this model keeps restore points on the machine that hosts it: at most 320 MB each, up to 32 (10,240 MB), not yet counted."}}},
	} {
		f := with(c.hm)
		c.want["route"], c.want["status"] = "#/models?model="+mdlLoaded, st
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{c.want}})
	}
}
