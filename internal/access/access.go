// Package access is what an MCP access key may do: the four permissions of
// the MCP page (Observe, Switch zones, Load and unload models, Change
// settings), each Allow, Ask me or Off, and the grant a request carries once
// its key is resolved.
//
// Access is open: the web UI and the REST API need no key, and a request
// without one acts as the default user with full access. A key is the only
// restriction: a request that carries one gets exactly its key's
// permissions, so what an agent does is limited and attributed.
package access

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Scope is what a route or an MCP tool needs.
type Scope string

// The four permissions of a key, in the order the MCP page lists them.
const (
	Observe  Scope = "observe"  // read anything: zones, runs, tokens, logs, prompts and answers
	Zones    Scope = "zones"    // switch zones on or off (pause, unpause, readmit, exclude)
	Models   Scope = "models"   // load and unload models (benchmark runs load models too)
	Settings Scope = "settings" // change settings, profiles, benchmark definitions
)

// Scopes of routes that no key permission covers.
const (
	// Owner: managing access keys. A request with a key is refused; without
	// one it is the default user, who may.
	Owner Scope = "owner"
	// Agents: the console agents' own protocol (/api/pool/...). Never
	// checked: they call without a key.
	Agents Scope = "agents"
	// ModelAPI: /v1 and /upstream. A key names the caller and is checked
	// (unknown, expired, revoked: refused), but grants nothing here yet.
	ModelAPI Scope = "model_api"
	// Open: the web UI's files and /health.
	Open Scope = "open"
)

// Permissions lists the four permissions, in order.
var Permissions = []Scope{Observe, Zones, Models, Settings}

// Level is how much of a permission a key has.
type Level string

// Levels.
const (
	Allow Level = "allow"
	Ask   Level = "ask" // "Ask me": the owner approves each request (approvals are not built yet: refused)
	Off   Level = "off"
)

// Label is the permission's name as the MCP page shows it.
func Label(s Scope) string {
	switch s {
	case Observe:
		return "Observe"
	case Zones:
		return "Switch zones on or off"
	case Models:
		return "Load and unload models"
	case Settings:
		return "Change settings"
	case Owner:
		return "Manage access keys"
	}
	return string(s)
}

// short is the permission in the page's "Can" column ("Observe · switch zones").
func short(s Scope) string {
	switch s {
	case Zones:
		return "switch zones"
	case Models:
		return "load and unload"
	case Settings:
		return "change settings"
	}
	return strings.ToLower(Label(s))
}

// Grants is a key's level for each permission.
type Grants struct {
	Observe  Level `json:"observe"`
	Zones    Level `json:"zones"`
	Models   Level `json:"models"`
	Settings Level `json:"settings"`
}

// Full is every permission allowed: what a key made before permissions
// existed has.
func Full() Grants { return Grants{Allow, Allow, Allow, Allow} }

// Default is a new key's permissions when none are given: observe only
// (the New key panel's defaults, least privilege).
func Default() Grants { return Grants{Allow, Off, Off, Off} }

// Of is the level of one permission; Off for any other scope.
func (g Grants) Of(s Scope) Level {
	switch s {
	case Observe:
		return g.Observe
	case Zones:
		return g.Zones
	case Models:
		return g.Models
	case Settings:
		return g.Settings
	}
	return Off
}

func (g *Grants) set(s Scope, l Level) {
	switch s {
	case Observe:
		g.Observe = l
	case Zones:
		g.Zones = l
	case Models:
		g.Models = l
	case Settings:
		g.Settings = l
	}
}

// Validate checks every level: observe is allow or off (the page offers no
// Ask me for it), the others allow, ask or off.
func (g Grants) Validate() error {
	for _, s := range Permissions {
		l := g.Of(s)
		switch {
		case l == Allow, l == Off:
		case l == Ask && s != Observe:
		case s == Observe:
			return fmt.Errorf("permission observe must be allow or off, not %q", l)
		default:
			return fmt.Errorf("permission %s must be allow, ask or off, not %q", s, l)
		}
	}
	return nil
}

// Merge applies a partial change ({"models": "ask"}) to g and validates the
// result. Unknown names are refused.
func (g Grants) Merge(change map[string]any) (Grants, error) {
	for k, v := range change {
		s := Scope(k)
		if !isPermission(s) {
			return g, fmt.Errorf("no permission %q: the permissions are observe, zones, models and settings", k)
		}
		l, ok := v.(string)
		if !ok {
			return g, fmt.Errorf("permission %s must be a string: allow, ask or off", k)
		}
		g.set(s, Level(strings.ToLower(strings.TrimSpace(l))))
	}
	return g, g.Validate()
}

func isPermission(s Scope) bool {
	for _, p := range Permissions {
		if p == s {
			return true
		}
	}
	return false
}

// Can is the "Can" column: the allowed permissions joined, the first one
// capitalised ("Observe · switch zones"); "Nothing" when none is allowed.
func (g Grants) Can() string {
	var parts []string
	for _, s := range Permissions {
		if g.Of(s) == Allow {
			parts = append(parts, short(s))
		}
	}
	if len(parts) == 0 {
		return "Nothing"
	}
	parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
	return strings.Join(parts, " · ")
}

// Asks lists the permissions set to Ask me.
func (g Grants) Asks() []Scope {
	out := []Scope{}
	for _, s := range Permissions {
		if g.Of(s) == Ask {
			out = append(out, s)
		}
	}
	return out
}

// JSON is the stored form.
func (g Grants) JSON() string {
	b, _ := json.Marshal(g)
	return string(b)
}

// ParseJSON reads the stored form.
func ParseJSON(s string) (Grants, error) {
	var g Grants
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		return g, err
	}
	return g, g.Validate()
}

// Grant is what a request that carried a key may do.
type Grant struct {
	AgentID, Name string
	Grants        Grants
}

type grantKey struct{}

// WithGrant marks a context with the grant of the key its request carried.
func WithGrant(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// GrantFrom is the grant of a context; ok false when the request carried no
// key (the default user: full access).
func GrantFrom(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}

// Refusal is why a keyed request may not do something.
type Refusal struct {
	Status  int    // 403
	Type    string // permission_denied, approval_required, owner_only
	Scope   Scope
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// Check decides whether a request with grant g may do something that needs
// scope. Scopes outside the four permissions: Owner is refused to every key,
// the others (Agents, ModelAPI, Open) are not checked.
func Check(g Grant, scope Scope) *Refusal {
	switch scope {
	case Agents, ModelAPI, Open:
		return nil
	case Owner:
		return &Refusal{Status: 403, Type: "owner_only", Scope: scope, Message: fmt.Sprintf(
			"access keys are managed in the web UI, not with an access key: the key of %s cannot list, create or change keys", g.Name)}
	}
	switch g.Grants.Of(scope) {
	case Allow:
		return nil
	case Ask:
		return &Refusal{Status: 403, Type: "approval_required", Scope: scope, Message: fmt.Sprintf(
			"the key of %s is set to ask before it may %s (%q is Ask me), and approvals are not available yet: ask the owner to do it, or to set the permission to Allow under MCP in the web UI",
			g.Name, verb(scope), string(scope))}
	}
	return &Refusal{Status: 403, Type: "permission_denied", Scope: scope, Message: fmt.Sprintf(
		"the key of %s may not %s (%q is Off): ask the owner to allow it under MCP in the web UI",
		g.Name, verb(scope), string(scope))}
}

func verb(s Scope) string {
	switch s {
	case Observe:
		return "read anything (observe)"
	case Zones:
		return "switch zones on or off"
	case Models:
		return "load or unload models (or run benchmarks, which load them)"
	case Settings:
		return "change settings"
	}
	return string(s)
}
