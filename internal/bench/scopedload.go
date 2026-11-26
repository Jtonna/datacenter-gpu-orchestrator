package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A run-scoped load (scheduler/scoped.go, profiles plan slice A): POST
// /api/load with a scope loads the production profile with the run's
// settings under its own name, "<production id>@<scope>". An OLDER
// scheduler does not know scoped loads and ignores the scope: it loads the
// production profile itself (on the zones given) and answers 202 naming the
// production id. LoadScoped is the one helper a caller uses for a scoped
// load: it checks that the answer names the scoped profile and, when it does
// not, unloads what that load started and fails with ErrNoScopedLoads.

// ErrNoScopedLoads is the failure of a scoped load on a scheduler that does
// not know them.
var ErrNoScopedLoads = errors.New("this scheduler does not know run-scoped loads; nothing is left loaded")

// ScopedLoadRequest is the body of a scoped /api/load. Nil settings keep the
// production profile's.
type ScopedLoadRequest struct {
	Model     string   `json:"model"` // the production profile (an id or an alias)
	Scope     string   `json:"scope"`
	Nodes     []string `json:"nodes,omitempty"`
	Ctx       *int     `json:"ctx,omitempty"`
	Parallel  *int     `json:"parallel,omitempty"`
	KVQuant   *string  `json:"kv_quant,omitempty"`
	FlashAttn *bool    `json:"flash_attn,omitempty"`
	Host      *string  `json:"host,omitempty"`
	Thinking  *bool    `json:"thinking,omitempty"`
}

// ScopedLoadAnswer is the answer of a scoped load.
type ScopedLoadAnswer struct {
	Status     int
	Msg        string // the scheduler's words for a refusal
	Reason     string
	Model      string // the scoped name, "<production id>@<scope>"
	Production string
	Scope      string
}

// IsScopedAnswer reports whether a 2xx answer to a scoped load for scope
// names the scoped profile: "model" is "<production>@<scope>" and "scope"
// is the scope. The same test for a script, with jq on the answer body:
//
//	jq -e --arg s "$SCOPE" '.scope == $s and .production != null and .model == (.production + "@" + $s)'
func IsScopedAnswer(a ScopedLoadAnswer, scope string) bool {
	return scope != "" && a.Scope == scope && a.Production != "" && a.Model == a.Production+"@"+scope
}

// LoadScoped sends a scoped load. A refusal (no 2xx) is returned as the
// answer, as Load returns one. A 2xx answer that does not name the scoped
// profile comes from a scheduler that does not know scoped loads: what that
// load started is unloaded (force: a load just started is still loading),
// unless the profile it named was loaded before (then the load started
// nothing and nothing is unloaded), and the error is ErrNoScopedLoads. The
// status read before the load is what tells the two apart; when it cannot
// be read nothing is loaded.
func (h *HTTPAPI) LoadScoped(ctx context.Context, r ScopedLoadRequest) (*ScopedLoadAnswer, error) {
	if r.Model == "" || r.Scope == "" {
		return nil, errors.New("a scoped load needs a model and a scope")
	}
	before, err := h.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("scoped load of %s: the status before it cannot be read, nothing loaded: %w", r.Model, err)
	}
	loaded := map[string]bool{}
	for _, in := range before.Instances {
		loaded[in.Profile] = true
	}
	b, _ := json.Marshal(r)
	resp, err := h.do(ctx, "POST", "/api/load", b, nil)
	if err != nil {
		return nil, err
	}
	raw := readLimited(resp.Body)
	resp.Body.Close()
	out := &ScopedLoadAnswer{Status: resp.StatusCode}
	if resp.StatusCode >= 300 {
		var eb errBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Message != "" {
			out.Msg, out.Reason = eb.Error.Message, eb.Error.Reason
		} else {
			out.Msg = strings.TrimSpace(Head(string(raw), 1000))
		}
		return out, nil
	}
	var m struct {
		Model      string `json:"model"`
		Production string `json:"production"`
		Scope      string `json:"scope"`
	}
	json.Unmarshal(raw, &m)
	out.Model, out.Production, out.Scope = m.Model, m.Production, m.Scope
	if IsScopedAnswer(*out, r.Scope) {
		return out, nil
	}
	// An older scheduler: it loaded the profile the answer names (else the
	// one asked for) as a plain load.
	started := m.Model
	if started == "" {
		started = r.Model
	}
	if loaded[started] {
		return out, fmt.Errorf("scoped load of %s@%s: %w (%s was loaded before and is left as it was)", r.Model, r.Scope, ErrNoScopedLoads, started)
	}
	u, err := h.Unload(ctx, started, true)
	if err != nil || u.Status >= 300 {
		why := ""
		if err != nil {
			why = err.Error()
		} else {
			why = fmt.Sprintf("%d %s", u.Status, u.Msg)
		}
		return out, fmt.Errorf("scoped load of %s@%s: this scheduler does not know run-scoped loads, and the unload of %s it started failed (%s): unload it by hand", r.Model, r.Scope, started, why)
	}
	return out, fmt.Errorf("scoped load of %s@%s: %w (%s, which it loaded instead, was unloaded)", r.Model, r.Scope, ErrNoScopedLoads, started)
}
