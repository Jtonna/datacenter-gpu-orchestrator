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
	madeUp := map[string]any{"family": "recurrent", "cell_mib": 0, "checkpoints": 32, "checkpoints_mib": 0, "prompt_cache_mib": 0, "on_node": true, "counted": false, "size_unknown": true}
	logged := map[string]any{"family": "windowed", "cell_mib": 42.5, "checkpoints": 32, "checkpoints_mib": 1360, "prompt_cache_mib": 2048, "on_node": false, "counted": false,
		"from_log": true, "logged_file": "gemma-x.gguf", "logged_file_bytes": 7000000000, "logged_ctx": 8192}
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
		{window, map[string]any{"name": "host memory: a proven window model (Gemma 4 12B) says at most",
			"page": []string{"Conversation memory: this model keeps restore points on the machine that hosts it: at most 320 MB each, up to 32 (10,240 MB), not yet counted."}}},
		{madeUp, map[string]any{"name": "host memory: an architecture without an exact rule shows no number",
			"page":      []string{"Conversation memory: this model keeps restore points on the machine that hosts it: size not yet known: read at the first load."},
			"notOnPage": []string{" MB each", "(0 MB)"}}},
		{logged, map[string]any{"name": "host memory: a size read from a load's log says which file and context",
			"page": []string{"Conversation memory: this model keeps restore points on the machine that hosts it: 43 MB each as the server logged it at a load of gemma-x.gguf at ctx 8,192, up to 32 (1,360 MB), not yet counted."}}},
	} {
		f := with(c.hm)
		c.want["route"], c.want["status"] = "#/models?model="+mdlLoaded, st
		uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": f, "cases": []any{c.want}})
	}
}

// p33: the Models panel names where the figures come from, and lists the
// profiles of one file by group when their own extra arguments differ.
func TestUIHostMemorySources(t *testing.T) {
	fetch := fixtureAnswers(t, []string{"--models", "five"}, "/api/models", "/api/status")
	st := fetch["/api/status"]
	cluster := map[string]any{"family": "recurrent", "cell_mib": 50.25, "checkpoints": 32, "checkpoints_mib": 1608, "prompt_cache_mib": 2048, "on_node": false, "counted": true,
		"per_slot": 32, "checkpoints_from": "the cluster default", "prompt_cache_from": "the cluster default"}
	own := map[string]any{"family": "recurrent", "cell_mib": 50.25, "checkpoints": 3, "checkpoints_mib": 151, "prompt_cache_mib": 0, "on_node": false, "counted": true,
		"per_slot": 3, "checkpoints_from": "from this profile's extra arguments", "prompt_cache_from": "from this profile's extra arguments"}
	const lead = "Conversation memory: this model keeps restore points on the machine that hosts it: about 50 MB each"
	all := map[string]any{}
	for k, v := range fetch {
		all[k] = v
	}
	all["/api/models"] = withHostMemory(t, fetch["/api/models"], mdlLoaded, cluster)
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": all, "cases": []any{map[string]any{"name": "host memory: one source",
		"route": "#/models?model=" + mdlLoaded, "status": st,
		"page": []string{lead + ", up to 32 (1,608 MB; the cluster default), counted in what it needs."}}}})

	// the first profile carries its own flags, the rest the cluster default
	mixed := withHostMemory(t, fetch["/api/models"], mdlLoaded, cluster).(map[string]any)
	var first string
	for _, m := range jlist(mixed["models"]) {
		if mm := jmap(m); jstr(mm["key"]) == mdlLoaded {
			ps := jlist(mm["profiles"])
			if len(ps) < 2 {
				t.Skip("the fixture model has one profile")
			}
			jmap(ps[0])["host_memory"] = own
			first = jstr(jmap(ps[0])["id"])
		}
	}
	all["/api/models"] = mixed
	uiExec(t, map[string]any{"shell": "next", "app": true, "fetch": all, "cases": []any{map[string]any{"name": "host memory: by profile",
		"route": "#/models?model=" + mdlLoaded, "status": st,
		"page": []string{lead + "; by profile: ", "up to 3 (151 MB; from this profile's extra arguments) for " + first,
			"up to 32 (1,608 MB; the cluster default) for "}}}})
}

// p33: the placement page's Host block shows the scheduler machine's
// budget for a model hosted there, and says when it was not checked.
func TestUIHostRoomLine(t *testing.T) {
	e := newEnv(t, chatOK)
	var status map[string]any
	json.Unmarshal([]byte(body(t, e.get("/api/status"))), &status)
	plan := func() map[string]any {
		var p map[string]any
		if err := json.Unmarshal([]byte(body(t, e.post("/api/plan", `{"model":"alpha"}`))), &p); err != nil {
			t.Fatal(err)
		}
		p["host"] = "server"
		p["host_memory"] = map[string]any{"family": "windowed", "cell_mib": 320, "checkpoints": 32, "checkpoints_mib": 10240, "prompt_cache_mib": 2048,
			"counted": true, "machine": "host-1", "checkpoints_from": "the cluster default", "prompt_cache_from": "the cluster default", "estimate": true}
		return p
	}
	short := plan()
	short["host_room"] = map[string]any{"machine": "host-1", "known": true, "mem_available_mib": 15400, "floor_mib": 1000, "headroom_mib": 200, "budget_mib": 14200,
		"held_mib": 12288, "this_mib": 12288, "fits": false, "unload": []any{"gemma-4-12b"}}
	unknown := plan()
	unknown["host_room"] = map[string]any{"machine": "host-1", "known": false, "note": "host memory not checked: available memory of host-1 unknown (no reading yet)"}
	cases := []any{
		map[string]any{"name": "host room: short, an idle model unloaded first", "route": "#/placement/alpha", "status": status, "fetch": map[string]any{"/api/plan": short},
			"expect": []string{"restore points and prompt cache the cluster default.", "host-1 has ", " for conversation memory (available ", "gemma-4-12b would be unloaded first."}},
		map[string]any{"name": "host room: not checked", "route": "#/placement/alpha", "status": status, "fetch": map[string]any{"/api/plan": unknown},
			"expect": []string{"host memory not checked: available memory of host-1 unknown (no reading yet)."}},
	}
	uiExec(t, map[string]any{"modules": []string{"views/placement.js"}, "fetch": map[string]any{}, "cases": cases})
}
