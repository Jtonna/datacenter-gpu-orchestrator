package mcpserver

// Reshaping of GET /api/status for agents: a model-centric summary instead of
// the full object. Everything here reads the REST JSON, never scheduler types.

func arr(v any) []any         { a, _ := v.([]any); return a }
func mp(v any) map[string]any { m, _ := v.(map[string]any); return m }

// pick copies the listed keys that are present (not null, not an empty string).
func pick(src map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := src[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	return out
}

func hostnames(doc map[string]any) map[string]string {
	h := map[string]string{}
	for _, n := range arr(mp(doc["view"])["nodes"]) {
		nm := mp(n)
		id, _ := nm["id"].(string)
		if hn, _ := nm["hostname"].(string); hn != "" {
			h[id] = hn
		}
	}
	return h
}

func deriveStatus(doc map[string]any, args map[string]any) any {
	if v, _ := args["verbose"].(bool); v {
		return doc
	}
	host := hostnames(doc)
	insts := []any{}
	for _, x := range arr(doc["instances"]) {
		in := mp(x)
		o := pick(in, "profile", "model_file", "state", "phase", "in_flight", "waiters", "requests_served", "ready_since", "last_used",
			"load_duration", "device_total_mib", "victim_of", "last_request",
			"load_elapsed_s", "load_progress", "load_eta_s", "load_bytes_sent", "load_bytes_total", "load_progress_source")
		nodes := []any{}
		for _, y := range arr(in["nodes"]) {
			pn := mp(y)
			id, _ := pn["id"].(string)
			n := map[string]any{"id": id}
			if hn := host[id]; hn != "" {
				n["hostname"] = hn
			}
			if v, ok := pn["budget_mib"]; ok {
				n["budget_mib"] = v
			}
			var mem float64
			for _, d := range arr(pn["memory"]) {
				if f, ok := mp(d)["total_mib"].(float64); ok {
					mem += f
				}
			}
			if mem > 0 {
				n["memory_mib"] = mem
			}
			nodes = append(nodes, n)
		}
		o["nodes"] = nodes
		insts = append(insts, o)
	}
	// capabilities: the agent's words from the status agents (sorted there);
	// an empty list for a node with no agent entry (the local node).
	caps := map[string]any{}
	for _, x := range arr(doc["agents"]) {
		a := mp(x)
		if id, _ := a["id"].(string); id != "" && a["capabilities"] != nil {
			caps[id] = a["capabilities"]
		}
	}
	nodes := []any{}
	for _, x := range arr(mp(doc["view"])["nodes"]) {
		n := pick(mp(x), "id", "hostname", "local", "online", "excluded", "excluded_by", "auto", "eligible", "unplaced", "unplaced_message", "total_mib", "free_mib",
			"live_budget_mib", "holder", "claimed_by", "settling", "quarantined", "health", "recovery")
		id, _ := n["id"].(string)
		if c, ok := caps[id]; ok {
			n["capabilities"] = c
		} else {
			n["capabilities"] = []any{}
		}
		nodes = append(nodes, n)
	}
	recent := arr(doc["recent_loads"])
	if len(recent) > 5 {
		recent = recent[:5]
	}
	out := map[string]any{
		"instances": insts, "nodes": nodes, "queue": queueOf(doc),
		"quarantines": orEmpty(doc["quarantines"]), "backoffs": orEmpty(doc["backoffs"]),
		"recent_loads": orEmpty(recent),
		"fleet":        pick(mp(doc["view"]), "age", "connected", "stale"),
	}
	for _, k := range []string{"time", "shutting_down"} {
		if v, ok := doc[k]; ok {
			out[k] = v
		}
	}
	return out
}

func orEmpty(v any) any {
	if a, ok := v.([]any); ok && a != nil {
		return a
	}
	return []any{}
}

func queueOf(doc map[string]any) []any {
	q := []any{}
	for _, x := range arr(doc["queue"]) {
		q = append(q, pick(mp(x), "profile", "age", "waiters", "claimed", "claim", "victims", "victims_stopped", "wait_reason"))
	}
	return q
}

func deriveNodes(doc map[string]any, args map[string]any) any {
	return map[string]any{"nodes": orEmpty(mp(doc["view"])["nodes"]), "fleet": pick(mp(doc["view"]), "age", "connected", "stale")}
}

func deriveQueue(doc map[string]any, args map[string]any) any {
	return map[string]any{"queue": queueOf(doc), "quarantines": orEmpty(doc["quarantines"]), "backoffs": orEmpty(doc["backoffs"])}
}

// profileFromList answers get_profile on a build without /api/config/profiles:
// the entry of GET /api/profiles with that id.
func profileFromList(doc map[string]any, args map[string]any) (any, bool) {
	id, _ := args["id"].(string)
	for _, p := range arr(doc["profiles"]) {
		if pm := mp(p); pm["id"] == id {
			return pm, true
		}
	}
	return nil, false
}
