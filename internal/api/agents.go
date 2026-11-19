package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/audit"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/cfgstore"
)

// Agents (design 9.6). This system has no login and no access control: the
// owner decided so. An agent key is therefore IDENTITY for the audit log and
// nothing else:
//
//   - a request without a key works exactly as before and is recorded as an
//     unnamed caller;
//   - a request with a valid key (Authorization: Bearer lls_..., or the header
//     X-Agent-Key) is recorded under that agent's name, actor "agent:<name>",
//     with the agent's id on the audit entry; the source stays mcp or rest;
//   - a request with an unknown or revoked key is refused with 401, so a typo
//     cannot silently lose the attribution.
//
// Only the SHA-256 of a key is stored; the key is shown once, when it is made.
//
//	GET    /api/agents                  the agents (revoked ones included)
//	POST   /api/agents                  {name, description} -> the agent and its key, once
//	PATCH  /api/agents/{id}             {name?, description?}
//	POST   /api/agents/{id}/rotate      a new key, once; the old one stops working
//	DELETE /api/agents/{id}             revoke (the row stays, so old audit entries keep a name)
//	GET    /api/mcp/tools               what an agent can do: the MCP tool list

// AgentKeyPrefix starts every agent key.
const AgentKeyPrefix = "lls_"

type agentUse struct {
	n       int64
	last    int64
	flushed time.Time
}

// agentRegistry resolves keys in memory and keeps the use counters, which are
// written to the database at most once a second per agent.
type agentRegistry struct {
	store *cfgstore.Store
	mu    sync.Mutex
	list  []cfgstore.Agent // nil: not loaded
	use   map[string]*agentUse
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newAgentKey() (key, hash, prefix string, err error) {
	var b [24]byte
	if _, err = rand.Read(b[:]); err != nil {
		return
	}
	key = AgentKeyPrefix + base64.RawURLEncoding.EncodeToString(b[:]) // 32 url-safe characters
	return key, hashKey(key), key[:len(AgentKeyPrefix)+6], nil
}

func (g *agentRegistry) load(ctx context.Context) ([]cfgstore.Agent, error) {
	if g.list != nil {
		return g.list, nil
	}
	l, err := g.store.Agents(ctx)
	if err != nil {
		return nil, err
	}
	if l == nil {
		l = []cfgstore.Agent{}
	}
	g.list = l
	return l, nil
}

func (g *agentRegistry) reset() {
	g.mu.Lock()
	g.list = nil
	g.mu.Unlock()
}

var (
	errUnknownKey = errors.New("unknown agent key: no agent has this key (it may be mistyped, or it was replaced by a newer one). Requests without a key are accepted; a wrong key is refused so that its actions are not recorded as nobody's")
	errRevokedKey = errors.New("this agent key was revoked. Create a new agent (or ask the owner for a new key) under MCP in the web UI")
)

// resolve finds the agent of a key and counts the request. Every stored hash
// is compared in constant time, so the time taken says nothing about a key.
func (g *agentRegistry) resolve(ctx context.Context, key string, now time.Time) (*cfgstore.Agent, error) {
	want := []byte(hashKey(key))
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.load(ctx)
	if err != nil {
		return nil, err
	}
	var hit *cfgstore.Agent
	for i := range list {
		if subtle.ConstantTimeCompare(want, []byte(list[i].KeyHash)) == 1 {
			hit = &list[i]
		}
	}
	if hit == nil {
		return nil, errUnknownKey
	}
	if hit.RevokedAt > 0 {
		return nil, errRevokedKey
	}
	if g.use == nil {
		g.use = map[string]*agentUse{}
	}
	u := g.use[hit.ID]
	if u == nil {
		u = &agentUse{}
		g.use[hit.ID] = u
	}
	u.n++
	u.last = now.UnixMilli()
	if time.Since(u.flushed) >= time.Second {
		g.flushOne(hit.ID, u)
	}
	a := *hit
	return &a, nil
}

// flushOne writes the pending counter of one agent (the caller holds mu).
func (g *agentRegistry) flushOne(id string, u *agentUse) {
	if u.n == 0 {
		return
	}
	if err := g.store.TouchAgent(id, u.n, u.last); err != nil {
		return // keep the count for the next try
	}
	for i := range g.list {
		if g.list[i].ID == id {
			g.list[i].RequestCount += u.n
			if u.last > g.list[i].LastUsedAt {
				g.list[i].LastUsedAt = u.last
			}
		}
	}
	u.n, u.flushed = 0, time.Now()
}

func (g *agentRegistry) flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, u := range g.use {
		g.flushOne(id, u)
	}
}

// keyOf reads an agent key from a request: X-Agent-Key, or a Bearer token
// that starts with lls_. Any other Authorization value is not an agent key
// (OpenAI clients always send one) and is ignored, as it always was.
func keyOf(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-Agent-Key")); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		if k := strings.TrimSpace(a[7:]); strings.HasPrefix(k, AgentKeyPrefix) {
			return k
		}
	}
	return ""
}

// identify resolves the agent key of a request, if it carries one. ok false
// means the request was answered (an unknown or revoked key).
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	ctx, status, err := s.IdentifyAgent(r)
	if err != nil {
		typ := "invalid_agent_key"
		if status == http.StatusServiceUnavailable {
			typ = "database_unavailable"
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="llm-scheduler agent identity"`)
		writeJSON(w, status, map[string]any{"error": map[string]any{"message": err.Error(), "type": typ}})
		return nil, false
	}
	if ctx != r.Context() {
		r = r.WithContext(ctx)
	}
	return r, true
}

// IdentifyAgent is identify for another transport on the same listener (the
// MCP endpoint): it returns the request's context, marked with the agent when
// the request carries a valid key, or the HTTP status and error to answer.
// A request that already acts for an agent (an in-process MCP tool call) and
// a request without a key pass unchanged.
func (s *Server) IdentifyAgent(r *http.Request) (context.Context, int, error) {
	ctx := r.Context()
	if _, _, ok := audit.AgentFrom(ctx); ok {
		return ctx, 0, nil
	}
	key := keyOf(r)
	if key == "" {
		return ctx, 0, nil
	}
	if s.store == nil {
		return ctx, http.StatusServiceUnavailable, errors.New("agent keys need the database, which is not available; send the request without a key")
	}
	a, err := s.agents.resolve(ctx, key, time.Now())
	if errors.Is(err, errUnknownKey) || errors.Is(err, errRevokedKey) {
		return ctx, http.StatusUnauthorized, err
	}
	if err != nil {
		return ctx, http.StatusServiceUnavailable, err
	}
	return audit.WithAgent(ctx, a.ID, a.Name), 0, nil
}

// ---- REST ----

func (s *Server) agentRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/agents", s.agentsList)
	m.HandleFunc("POST /api/agents", s.agentCreate)
	m.HandleFunc("PATCH /api/agents/{id}", s.agentPatch)
	m.HandleFunc("POST /api/agents/{id}/rotate", s.agentRotate)
	m.HandleFunc("DELETE /api/agents/{id}", s.agentRevoke)
	m.HandleFunc("GET /api/mcp/tools", s.mcpTools)
}

func (s *Server) agentStore(w http.ResponseWriter) bool {
	if s.store == nil {
		apiErr(w, http.StatusServiceUnavailable, "agents need the database, which is not available")
		return false
	}
	return true
}

func agentView(a cfgstore.Agent, actions int) map[string]any {
	v := map[string]any{"id": a.ID, "name": a.Name, "description": a.Description, "key_prefix": a.KeyPrefix, "created_at": a.CreatedAt,
		"created_by": map[string]any{"source": a.CreatedSource, "actor": a.CreatedActor}, "rotated_at": nilIfZero(a.RotatedAt),
		"last_used_at": nilIfZero(a.LastUsedAt), "revoked_at": nilIfZero(a.RevokedAt), "revoked": a.RevokedAt > 0,
		"request_count": a.RequestCount, "actor": audit.AgentPrefix + a.Name, "audit_entries": actions}
	return v
}

// agentsNow lists the agents with the counters that are not written yet.
func (s *Server) agentsNow(ctx context.Context) ([]cfgstore.Agent, error) {
	g := &s.agents
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.load(ctx)
	if err != nil {
		return nil, err
	}
	out := append([]cfgstore.Agent(nil), list...)
	for i := range out {
		if u := g.use[out[i].ID]; u != nil && u.n > 0 {
			out[i].RequestCount += u.n
			if u.last > out[i].LastUsedAt {
				out[i].LastUsedAt = u.last
			}
		}
	}
	return out, nil
}

func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) {
	if !s.agentStore(w) {
		return
	}
	list, err := s.agentsNow(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	actions, _ := s.store.AgentActions(r.Context())
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, agentView(a, actions[a.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out, "key_prefix": AgentKeyPrefix,
		"headers": []string{"Authorization: Bearer <key>", "X-Agent-Key: <key>"},
		"note":    "Keys are identity for the audit log, not access control: requests without a key keep working and are recorded as an unnamed caller."})
}

// agentName checks a name: 1 to 64 printable characters, and not the name of
// another agent that is still active.
func (s *Server) agentName(ctx context.Context, name, exceptID string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("the agent needs a name (it is what the audit log shows)")
	}
	if len(name) > 64 {
		return "", errors.New("the name may be at most 64 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("the name must not contain control characters")
		}
	}
	list, err := s.agentsNow(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range list {
		if a.ID != exceptID && a.RevokedAt == 0 && strings.EqualFold(a.Name, name) {
			return "", errConflict{"an agent named " + a.Name + " already exists: two agents with one name could not be told apart in the audit log"}
		}
	}
	return name, nil
}

type errConflict struct{ msg string }

func (e errConflict) Error() string { return e.msg }

func agentErr(w http.ResponseWriter, err error) {
	var c errConflict
	switch {
	case errors.As(err, &c):
		apiErr(w, http.StatusConflict, "%v", err)
	case errors.Is(err, cfgstore.ErrNotFound):
		apiErr(w, http.StatusNotFound, "no such agent (or it is revoked)")
	default:
		apiErr(w, http.StatusBadRequest, "%v", err)
	}
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Server) agentCreate(w http.ResponseWriter, r *http.Request) {
	if !s.agentStore(w) {
		return
	}
	obj, _, ok := bodyObject(w, r)
	if !ok {
		return
	}
	name, _ := obj["name"].(string)
	desc, _ := obj["description"].(string)
	name, err := s.agentName(r.Context(), name, "")
	if err != nil {
		agentErr(w, err)
		return
	}
	key, hash, prefix, err := newAgentKey()
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "no random key could be made: %v", err)
		return
	}
	var idb [6]byte
	rand.Read(idb[:])
	o := audit.FromRequest(r)
	a := cfgstore.Agent{ID: "ag_" + hex.EncodeToString(idb[:]), Name: name, Description: clipText(desc, 500), KeyHash: hash, KeyPrefix: prefix,
		CreatedAt: time.Now().UnixMilli(), CreatedSource: o.Source, CreatedActor: o.Actor}
	if err := s.store.CreateAgent(a); err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.agents.reset()
	id := s.recordAudit(r, audit.ActionCreate, audit.SectionAgents, a.ID, nil, map[string]any{"name": a.Name, "description": a.Description, "key_prefix": a.KeyPrefix})
	s.announce(r, audit.SectionAgents, a.ID, id)
	writeJSON(w, http.StatusCreated, map[string]any{"agent": agentView(a, 0), "key": key, "audit_id": id,
		"note": "This is the only time the key is shown. Only its SHA-256 is stored."})
}

func (s *Server) agentPatch(w http.ResponseWriter, r *http.Request) {
	if !s.agentStore(w) {
		return
	}
	id := r.PathValue("id")
	old, err := s.store.Agent(r.Context(), id)
	if err != nil {
		agentErr(w, err)
		return
	}
	obj, _, ok := bodyObject(w, r)
	if !ok {
		return
	}
	name, desc := old.Name, old.Description
	if v, has := obj["name"].(string); has {
		if name, err = s.agentName(r.Context(), v, id); err != nil {
			agentErr(w, err)
			return
		}
	}
	if v, has := obj["description"].(string); has {
		desc = clipText(v, 500)
	}
	if name == old.Name && desc == old.Description {
		writeJSON(w, http.StatusOK, map[string]any{"agent": agentView(*old, 0), "changed": false})
		return
	}
	if err := s.store.UpdateAgent(id, name, desc); err != nil {
		agentErr(w, err)
		return
	}
	s.agents.reset()
	aid := s.recordAudit(r, audit.ActionUpdate, audit.SectionAgents, id, map[string]any{"name": old.Name, "description": old.Description}, map[string]any{"name": name, "description": desc})
	s.announce(r, audit.SectionAgents, id, aid)
	old.Name, old.Description = name, desc
	writeJSON(w, http.StatusOK, map[string]any{"agent": agentView(*old, 0), "changed": true, "audit_id": aid})
}

func (s *Server) agentRotate(w http.ResponseWriter, r *http.Request) {
	if !s.agentStore(w) {
		return
	}
	id := r.PathValue("id")
	old, err := s.store.Agent(r.Context(), id)
	if err != nil {
		agentErr(w, err)
		return
	}
	key, hash, prefix, err := newAgentKey()
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "no random key could be made: %v", err)
		return
	}
	now := time.Now().UnixMilli()
	if err := s.store.RotateAgentKey(id, hash, prefix, now); err != nil {
		agentErr(w, err)
		return
	}
	s.agents.reset()
	aid := s.recordAudit(r, "rotate", audit.SectionAgents, id, map[string]any{"name": old.Name, "key_prefix": old.KeyPrefix}, map[string]any{"name": old.Name, "key_prefix": prefix})
	s.announce(r, audit.SectionAgents, id, aid)
	old.KeyPrefix, old.RotatedAt = prefix, now
	writeJSON(w, http.StatusOK, map[string]any{"agent": agentView(*old, 0), "key": key, "audit_id": aid,
		"note": "This is the only time the key is shown. The old key no longer works."})
}

func (s *Server) agentRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.agentStore(w) {
		return
	}
	id := r.PathValue("id")
	old, err := s.store.Agent(r.Context(), id)
	if err != nil {
		agentErr(w, err)
		return
	}
	s.agents.flush()
	now := time.Now().UnixMilli()
	if err := s.store.RevokeAgent(id, now); err != nil {
		agentErr(w, err)
		return
	}
	s.agents.reset()
	aid := s.recordAudit(r, "revoke", audit.SectionAgents, id, map[string]any{"name": old.Name, "revoked": false}, map[string]any{"name": old.Name, "revoked": true})
	s.announce(r, audit.SectionAgents, id, aid)
	old.RevokedAt = now
	writeJSON(w, http.StatusOK, map[string]any{"agent": agentView(*old, 0), "revoked": true, "audit_id": aid})
}

// SetMCPTools gives the API the MCP server's tool list (the MCP server wraps
// the API, so it is handed over after both exist).
func (s *Server) SetMCPTools(f func() any) { s.mcpToolList.Store(&f) }

func (s *Server) mcpTools(w http.ResponseWriter, r *http.Request) {
	f := s.mcpToolList.Load()
	if f == nil {
		apiErr(w, http.StatusServiceUnavailable, "the MCP server is not part of this process")
		return
	}
	writeJSON(w, http.StatusOK, (*f)())
}
