package api

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/access"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// routeScopes is what each route needs from a request that carries an access
// key, keyed by its ServeMux pattern exactly as registered. It is the one
// table: ServeHTTP enforces it, the MCP server reads it for its tools (a tool
// needs what its route needs), GET /api/agents lists it per permission, the
// docs table is written from it (TestRouteScopes_DocsTable), and
// TestRouteScopes_EveryRouteHasAnEntry fails on a route that is missing.
//
// A request without a key is the default user and is never checked. A
// pattern missing from the table fails closed: GET needs observe, anything
// else settings.
var routeScopes = map[string]access.Scope{
	// always open
	"/":           access.Open, // the catch-all: it only answers "no such path"
	"GET /health": access.Open,
	"GET /{$}":    access.Open,
	"GET /ui":     access.Open,
	"GET /ui/":    access.Open,
	"GET /ui/{$}": access.Open,

	// the model API: a key there is checked (unknown, expired, revoked:
	// refused) but grants nothing yet (07 P7)
	"/v1/":                        access.ModelAPI,
	"GET /v1/models":              access.ModelAPI,
	"/upstream/{model}":           access.ModelAPI,
	"/upstream/{model}/{rest...}": access.ModelAPI,

	// the console agents' protocol: they call without a key
	"POST " + agentproto.PathRegister:                    access.Agents,
	"GET " + agentproto.PathRelease:                      access.Agents,
	"POST /api/pool/nodes/{id}/heartbeat":                access.Agents,
	"POST /api/pool/nodes/{id}/deregister":               access.Agents,
	"GET /api/pool/nodes/{id}/logs":                      access.Agents,
	"POST /api/pool/nodes/{id}/logs":                     access.Agents,
	"GET /api/pool/programs/{release}/{platform}/{file}": access.Agents,
	"GET " + agentproto.PathClientLogsConfig:             access.Agents,
	"POST " + agentproto.PathClientLogs:                  access.Agents,

	// managing access keys: the default user only
	"GET /api/agents":              access.Owner,
	"POST /api/agents":             access.Owner,
	"PATCH /api/agents/{id}":       access.Owner,
	"POST /api/agents/{id}/rotate": access.Owner,
	"DELETE /api/agents/{id}":      access.Owner,

	// observe: reading, and the read-only plan
	"GET /api/about": access.Observe, "GET /api/audit": access.Observe, "GET /api/audit/{id}": access.Observe,
	"GET /api/bench/aggregates": access.Observe, "GET /api/bench/benchmarks": access.Observe, "GET /api/bench/benchmarks/{id}": access.Observe,
	"GET /api/bench/benchmarks/{id}/results": access.Observe, "GET /api/bench/external": access.Observe, "GET /api/bench/external/{id}": access.Observe,
	"GET /api/bench/groups": access.Observe, "GET /api/bench/groups/{id}": access.Observe, "GET /api/bench/groups/{id}/expand": access.Observe,
	"GET /api/bench/measurements": access.Observe, "GET /api/bench/results/models": access.Observe, "GET /api/bench/metrics": access.Observe, "GET /api/bench/profiles": access.Observe,
	"GET /api/bench/removals": access.Observe, "GET /api/bench/removals/preview": access.Observe, "GET /api/bench/runs": access.Observe,
	"GET /api/bench/runs/{id}": access.Observe, "GET /api/bench/runs/{id}/export": access.Observe, "GET /api/bench/runs/{id}/measurements": access.Observe,
	"GET /api/bench/runs/{id}/report": access.Observe, "GET /api/bench/runs/{id}/scores": access.Observe,
	"GET /api/bench/runs/{id}/steps/{step}/transcript": access.Observe, "GET /api/bench/runs/{id}/tasks": access.Observe,
	"GET /api/bench/runs/{id}/tasks/{result}": access.Observe, "GET /api/bench/runs/{id}/transcripts": access.Observe,
	"GET /api/bench/schema": access.Observe, "GET /api/bench/state": access.Observe, "GET /api/bench/suites": access.Observe,
	"GET /api/bench/suites/{id}": access.Observe, "GET /api/bench/suites/{id}/expand": access.Observe, "GET /api/bench/task-sets": access.Observe,
	"GET /api/bench/task-sets/{id}": access.Observe, "GET /api/bench/trends": access.Observe,
	"GET /api/config": access.Observe, "GET /api/config/exclude-nodes": access.Observe, "GET /api/config/export": access.Observe,
	"GET /api/config/fleet": access.Observe, "GET /api/config/no-auto-nodes": access.Observe, "GET /api/config/node-overrides": access.Observe,
	"GET /api/config/profiles": access.Observe, "GET /api/config/profiles/{id...}": access.Observe, "GET /api/config/recovery": access.Observe,
	"GET /api/config/revisions": access.Observe, "GET /api/config/revisions/{n}": access.Observe, "GET /api/config/schema": access.Observe,
	"GET /api/config/settings": access.Observe, "GET /api/config/tiers": access.Observe,
	"GET /api/events": access.Observe, "GET /api/health": access.Observe, "GET /api/history": access.Observe,
	"GET /api/history/export": access.Observe, "GET /api/history/summary": access.Observe, "GET /api/mcp/tools": access.Observe,
	"GET /api/metrics/activity": access.Observe, "GET /api/models": access.Observe, "GET /api/models/{key...}": access.Observe,
	"GET /api/needs": access.Observe, "GET /api/nodes": access.Observe, "GET /api/nodes/availability": access.Observe,
	"GET /api/nodes/{id}/startups": access.Observe, "GET /api/profiles": access.Observe, "GET /api/requests": access.Observe,
	"GET /api/requests/summary": access.Observe, "GET /api/requests/{id}": access.Observe, "GET /api/startups": access.Observe,
	"GET /api/status": access.Observe, "GET /api/system": access.Observe, "GET /running": access.Observe, "GET /ws": access.Observe,
	"GET " + agentproto.PathLogsQuery: access.Observe, agentproto.PathLogsPages: access.Observe,
	"POST /api/plan":  access.Observe, // what would happen, changes nothing
	"GET /api/groups": access.Observe, "GET /api/nodes/uptime": access.Observe,
	"GET /api/zones/pending": access.Observe, "GET /api/zones/pending/{id}": access.Observe,

	// the install script a joining machine fetches with curl, without a key
	// (pending.go): nothing secret in it, as the agent binary under /api/pool
	"GET /install.sh": access.Agents, "GET /install/potluck-agent-setup": access.Agents,

	// zones: switch zones on or off
	"POST /api/nodes/{id}/pause": access.Zones, "POST /api/nodes/{id}/unpause": access.Zones,
	"POST /api/nodes/{id}/readmit": access.Zones, "POST /api/nodes/{id}/expect-restart": access.Zones,
	"PUT /api/config/exclude-nodes": access.Zones, "PUT /api/config/no-auto-nodes": access.Zones,
	// zone groups: pausing a group pauses its zones; arranging groups changes no zone
	"POST /api/groups/{name}/pause": access.Zones, "POST /api/groups/{name}/unpause": access.Zones,
	"POST /api/groups": access.Zones, "PATCH /api/groups/{name}": access.Zones, "DELETE /api/groups/{name}": access.Zones,
	"POST /api/groups/{name}/members": access.Zones, "DELETE /api/groups/{name}/members": access.Zones,
	// a selection of zones, a zone made before its machine
	"POST /api/zone-actions/pause": access.Zones, "POST /api/zone-actions/unpause": access.Zones,
	"POST /api/zones/pending": access.Zones, "DELETE /api/zones/pending/{id}": access.Zones,

	// models: load and unload (a benchmark run loads models)
	"POST /api/load": access.Models, "POST /api/unload": access.Models, "POST /api/models/unload": access.Models,
	"POST /api/models/unload/{model...}": access.Models, "GET /unload": access.Models, "POST /unload": access.Models,
	"POST /api/nodes/{id}/force-unload": access.Models, "POST /api/cache/evict": access.Models,
	"POST /api/models/drain/{model...}": access.Models, "POST /api/models/undrain/{model...}": access.Models,
	"PUT /api/models/pin/{key...}": access.Models, "DELETE /api/models/pin/{key...}": access.Models, // pins (models page)
	"POST /api/models/delete/{key...}": access.Models, "POST /api/models/trash/{id}/restore": access.Models, "POST /api/models/trash/{id}/remove": access.Models, "GET /api/models/trash": access.Observe, // the models trash
	"POST /api/zone-actions/force-unload": access.Models, "POST /api/zone-actions/drain": access.Models,
	"POST /api/bench/benchmarks/{id}/run": access.Models, "POST /api/bench/groups/{id}/run": access.Models,
	"POST /api/bench/suites/{id}/run": access.Models, "POST /api/bench/runs/{id}/cancel": access.Models,
	"PUT /api/bench/runs/{id}/pin": access.Models, "DELETE /api/bench/runs/{id}/pin": access.Models, // run pins, as model pins (slice 2; was settings)

	// settings: configuration, profiles, benchmark definitions, scores
	"PATCH /api/config/fleet": access.Settings, "PATCH /api/config/recovery": access.Settings, "PATCH /api/config/settings": access.Settings,
	"PUT /api/config/fleet": access.Settings, "PUT /api/config/node-overrides": access.Settings, "PUT /api/config/recovery": access.Settings,
	"PUT /api/config/tiers": access.Settings, "POST /api/config/import": access.Settings, "POST /api/config/profiles": access.Settings,
	"PUT /api/config/profiles/{id...}": access.Settings, "DELETE /api/config/profiles/{id...}": access.Settings,
	"POST /api/config/profiles/{rest...}":     access.Settings, // {id}/promote and {id}/demote: a profile edit
	"POST /api/config/revisions/{n}/rollback": access.Settings,
	"PUT /api/models/{key...}":                access.Settings, "DELETE /api/models/{key...}": access.Settings,
	"POST /api/models/{key...}": access.Settings, // {file}/standard-set: writes profiles
	"POST /api/forget-need":     access.Settings, "POST /api/needs/forget": access.Settings, "DELETE /api/nodes/{id}": access.Settings,
	"POST /api/bench/benchmarks": access.Settings, "PUT /api/bench/benchmarks/{id}": access.Settings, "DELETE /api/bench/benchmarks/{id}": access.Settings,
	"POST /api/bench/external": access.Settings, "PUT /api/bench/external/{id}": access.Settings, "DELETE /api/bench/external/{id}": access.Settings,
	"POST /api/bench/groups": access.Settings, "PUT /api/bench/groups/{id}": access.Settings, "DELETE /api/bench/groups/{id}": access.Settings,
	"POST /api/bench/suites": access.Settings, "PUT /api/bench/suites/{id}": access.Settings, "DELETE /api/bench/suites/{id}": access.Settings,
	"POST /api/bench/task-sets": access.Settings, "DELETE /api/bench/task-sets/{id}": access.Settings,
	"PUT /api/bench/runs/{id}/scores/{scorer}": access.Settings, "DELETE /api/bench/runs/{id}/scores/{scorer}": access.Settings,
}

// scopeOf is what a matched pattern needs; a pattern missing from the table
// fails closed.
func scopeOf(pattern, method string) access.Scope {
	if sc, ok := routeScopes[pattern]; ok {
		return sc
	}
	if method == http.MethodGet || method == http.MethodHead {
		return access.Observe
	}
	return access.Settings
}

// routesOf lists the patterns that need a scope, sorted by path then method.
func routesOf(sc access.Scope) []string {
	out := []string{}
	for p, s := range routeScopes {
		if s == sc {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := patternPath(out[i]), patternPath(out[j])
		if pi != pj {
			return pi < pj
		}
		return out[i] < out[j]
	})
	return out
}

func patternPath(p string) string {
	if i := strings.IndexByte(p, ' '); i >= 0 {
		return p[i+1:]
	}
	return p
}

var placeholder = regexp.MustCompile(`\{[^}]*\}`)

// ScopeOf is what a request to method and path needs; path may hold {name}
// placeholders (an MCP tool's route). The MCP server asks it for each tool.
// NoteRefusal counts a refusal another transport made for a keyed request
// (an MCP tool call refused by the key's permissions), as authorize does.
func (s *Server) NoteRefusal(ctx context.Context, ref *access.Refusal) {
	if g, ok := access.GrantFrom(ctx); ok && ref != nil {
		s.agents.refused(g.AgentID, ref.Type, s.opts.Now())
	}
}

func (s *Server) ScopeOf(method, path string) access.Scope {
	r, err := http.NewRequest(method, "http://in-process"+placeholder.ReplaceAllString(path, "x"), nil)
	if err != nil {
		return scopeOf("", method)
	}
	_, pattern := s.mux.Handler(r)
	return scopeOf(pattern, method)
}

// authorize refuses a request whose key lacks what its route needs. A
// request without a key passes: it is the default user.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	g, ok := access.GrantFrom(r.Context())
	if !ok {
		return true
	}
	_, pattern := s.mux.Handler(r)
	ref := access.Check(g, scopeOf(pattern, r.Method))
	if ref == nil {
		return true
	}
	s.agents.refused(g.AgentID, ref.Type, s.opts.Now()) // P6
	writeJSON(w, ref.Status, map[string]any{"error": map[string]any{"message": ref.Message, "type": ref.Type, "scope": ref.Scope}})
	return false
}
