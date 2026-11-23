package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/access"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
)

// MCP access keys (design 9.6; the MCP page). Access is open: the web UI and
// the REST API need no key, and a request without one is the default user
// with full access. A key is the one restriction there is:
//
//   - a request with a valid key (Authorization: Bearer ptl_..., or the
//     header X-Agent-Key) gets exactly its key's permissions (Observe,
//     Switch zones, Load and unload models, Change settings: allow, ask or
//     off; access.go) and is recorded under the agent's name, actor
//     "agent:<name>", with the agent's id on the audit entry;
//   - a request with an unknown, expired or revoked key is refused with 401,
//     never treated as keyless;
//   - a key made before permissions existed (lls_..., or by an older image)
//     keeps working as it did: every permission allowed, no expiry.
//
// Only the SHA-256 of a key is stored; the key is shown once, when it is
// made or rotated. Its hint is the prefix and the last four characters.
//
//	GET    /api/agents                  the keys (revoked ones included)
//	POST   /api/agents                  {name, description, expires_at, permissions} -> the key, once
//	PATCH  /api/agents/{id}             {name?, description?, expires_at?, permissions?}
//	POST   /api/agents/{id}/rotate      a new key, once; the old one stops working
//	DELETE /api/agents/{id}             revoke (the row stays, so old audit entries keep a name)
//	GET    /api/mcp/tools               what an agent can do: the MCP tool list

// AgentKeyPrefix starts every key made now.
const AgentKeyPrefix = "ptl_"

// LegacyKeyPrefix started the keys made before permissions existed; they
// are still accepted.
const LegacyKeyPrefix = "lls_"

// Expiry: the New key panel's default, and how soon a key counts as expiring.
const (
	DefaultKeyLife   = 30 * 24 * time.Hour
	KeyExpiringSoon  = 7 * 24 * time.Hour
	keyHintMask      = "••••"
	pasteKeyHere     = "<paste your key>"
	expiryWarnFirst  = "7d"
	expiryWarnSecond = "1d"
	expiryWarnGone   = "expired"
)

// Key statuses, as the page's status tag says them.
const (
	KeyActive   = "active"
	KeyExpiring = "expiring" // within KeyExpiringSoon
	KeyExpired  = "expired"
	KeyRevoked  = "revoked"
)

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

// newAgentKey makes a key: ptl_ and 24 random bytes (32 url-safe
// characters, 192 bits). prefix is its first 10 characters, last4 its end.
func newAgentKey() (key, hash, prefix, last4 string, err error) {
	var b [24]byte
	if _, err = rand.Read(b[:]); err != nil {
		return
	}
	key = AgentKeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return key, hashKey(key), key[:len(AgentKeyPrefix)+6], key[len(key)-4:], nil
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
	func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.list = nil
	}()
}

var (
	errUnknownKey = errors.New("unknown agent key: no agent has this key (it may be mistyped, or it was replaced by a newer one). Requests without a key are accepted; a wrong key is refused so that its actions are not recorded as nobody's")
	errRevokedKey = errors.New("this agent key was revoked. Create a new key (or ask the owner for one) under MCP in the web UI")
	errExpiredKey = errors.New("this agent key has expired. The owner can extend it or rotate it under MCP in the web UI")
)

// grantsOf is what an agent's key may do: an old-style key (no access row)
// everything; a stored set that cannot be read nothing (fail closed).
func grantsOf(a *cfgstore.Agent) access.Grants {
	if a.Access == nil {
		return access.Full()
	}
	g, err := access.ParseJSON(a.Access.Grants)
	if err != nil {
		return access.Grants{Observe: access.Off, Zones: access.Off, Models: access.Off, Settings: access.Off}
	}
	return g
}

func expiresAt(a *cfgstore.Agent) int64 {
	if a.Access == nil {
		return 0
	}
	return a.Access.ExpiresAt
}

// keyStatus is active, expiring (within KeyExpiringSoon), expired or
// revoked, and for an expiry the whole days left, rounded up ("Expires in 3
// days").
func keyStatus(a *cfgstore.Agent, now time.Time) (status string, daysLeft int) {
	if a.RevokedAt > 0 {
		return KeyRevoked, 0
	}
	exp := expiresAt(a)
	if exp == 0 {
		return KeyActive, 0
	}
	left := time.UnixMilli(exp).Sub(now)
	if left <= 0 {
		return KeyExpired, 0
	}
	days := int(math.Ceil(left.Hours() / 24))
	if left <= KeyExpiringSoon {
		return KeyExpiring, days
	}
	return KeyActive, days
}

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
	if st, _ := keyStatus(hit, now); st == KeyExpired {
		return nil, errExpiredKey
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
// that starts with ptl_ (or lls_, the older keys). Any other Authorization
// value is not an agent key (OpenAI clients always send one) and is
// ignored, as it always was.
func keyOf(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-Agent-Key")); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		if k := strings.TrimSpace(a[7:]); strings.HasPrefix(k, AgentKeyPrefix) || strings.HasPrefix(k, LegacyKeyPrefix) {
			return k
		}
	}
	return ""
}

// identify resolves the agent key of a request, if it carries one. ok false
// means the request was answered (an unknown, expired or revoked key).
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	ctx, status, err := s.IdentifyAgent(r)
	if err != nil {
		typ := "invalid_agent_key"
		if status == http.StatusServiceUnavailable {
			typ = "database_unavailable"
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="potluck access key"`)
		writeJSON(w, status, map[string]any{"error": map[string]any{"message": err.Error(), "type": typ}})
		return nil, false
	}
	if ctx != r.Context() {
		r = r.WithContext(ctx)
	}
	return r, true
}

// IdentifyAgent is identify for another transport on the same listener (the
// MCP endpoint): it returns the request's context, marked with the agent and
// its grant when the request carries a valid key, or the HTTP status and
// error to answer. A request that already acts for an agent (an in-process
// MCP tool call) and a request without a key pass unchanged.
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
	a, err := s.agents.resolve(ctx, key, s.opts.Now())
	if errors.Is(err, errUnknownKey) || errors.Is(err, errRevokedKey) || errors.Is(err, errExpiredKey) {
		return ctx, http.StatusUnauthorized, err
	}
	if err != nil {
		return ctx, http.StatusServiceUnavailable, err
	}
	ctx = audit.WithAgent(ctx, a.ID, a.Name)
	return access.WithGrant(ctx, access.Grant{AgentID: a.ID, Name: a.Name, Grants: grantsOf(a)}), 0, nil
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

// keyHint is the key as the page shows it: ptl_••••7c2e. For a key whose
// end is not known (made before hints, or rotated by an older image) the
// stored start: lls_kBFyEk••••.
func keyHint(a *cfgstore.Agent) string {
	if x := a.Access; x != nil && x.KeyLast4 != "" && x.KeyHash == a.KeyHash {
		return a.KeyPrefix[:min(len(a.KeyPrefix), len(AgentKeyPrefix))] + keyHintMask + x.KeyLast4
	}
	return a.KeyPrefix + keyHintMask
}

// mcpURL is the address of the MCP endpoint as the caller reached this
// server (its Host header; no forwarding header is read).
func mcpURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/mcp"
}

// mcpSettings is the "MCP settings for the agent" block: what to paste into
// an MCP client. key is the real key (only when it was just made) or a
// placeholder: the page never puts a masked key on the clipboard.
func mcpSettings(r *http.Request, key string) map[string]any {
	return map[string]any{"mcpServers": map[string]any{"potluck": map[string]any{
		"type": "http", "url": mcpURL(r), "headers": map[string]any{"Authorization": "Bearer " + key}}}}
}

func (s *Server) agentView(r *http.Request, a cfgstore.Agent, actions int) map[string]any {
	now := s.opts.Now()
	status, days := keyStatus(&a, now)
	g := grantsOf(&a)
	v := map[string]any{"id": a.ID, "name": a.Name, "description": a.Description, "key_prefix": a.KeyPrefix, "key_hint": keyHint(&a), "created_at": a.CreatedAt,
		"created_by": map[string]any{"source": a.CreatedSource, "actor": a.CreatedActor}, "rotated_at": nilIfZero(a.RotatedAt),
		"last_used_at": nilIfZero(a.LastUsedAt), "revoked_at": nilIfZero(a.RevokedAt), "revoked": a.RevokedAt > 0,
		"request_count": a.RequestCount, "actor": audit.AgentPrefix + a.Name, "audit_entries": actions,
		"expires_at": nilIfZero(expiresAt(&a)), "status": status, "permissions": g, "can": g.Can(), "asks": g.Asks(),
		// legacy: made before permissions existed: every permission allowed, no expiry.
		"legacy":       a.Access == nil,
		"mcp_settings": mcpSettings(r, pasteKeyHere)}
	if days > 0 {
		v["expires_in_days"] = days
	}
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
	active := 0
	for _, a := range list {
		v := s.agentView(r, a, actions[a.ID])
		if st := v["status"]; st == KeyActive || st == KeyExpiring {
			active++
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out, "active": active, "key_prefix": AgentKeyPrefix,
		"headers":      []string{"Authorization: Bearer <key>", "X-Agent-Key: <key>"},
		"mcp_url":      mcpURL(r),
		"mcp_settings": mcpSettings(r, pasteKeyHere),
		"permissions":  permissionList(),
		"note":         "A key limits and names an agent: a request with a key gets exactly its permissions and is recorded under its name; a request without a key is the default user with full access."})
}

// permissionList is the four permissions with their labels and the levels
// each takes, in the page's order.
func permissionList() []map[string]any {
	out := []map[string]any{}
	for _, p := range access.Permissions {
		levels := []access.Level{access.Allow, access.Ask, access.Off}
		if p == access.Observe {
			levels = []access.Level{access.Allow, access.Off}
		}
		out = append(out, map[string]any{"id": p, "label": access.Label(p), "levels": levels, "routes": routesOf(p)})
	}
	return out
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

// expiryArg reads expires_at from a body: unix ms, 0 or null for never. It
// must be in the future. has is false when the body leaves it out.
func (s *Server) expiryArg(obj map[string]any) (at int64, has bool, err error) {
	v, has := obj["expires_at"]
	if !has || v == nil {
		return 0, has, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != math.Trunc(f) {
		return 0, true, errors.New("expires_at must be a time in unix milliseconds, or 0 (or null) for never")
	}
	if f > 0 && int64(f) <= s.opts.Now().UnixMilli() {
		return 0, true, errors.New("expires_at must be in the future (or 0 for never)")
	}
	return int64(f), true, nil
}

// permissionsArg merges the body's permissions onto base.
func permissionsArg(obj map[string]any, base access.Grants) (access.Grants, bool, error) {
	v, has := obj["permissions"]
	if !has || v == nil {
		return base, false, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return base, true, errors.New(`permissions must be an object such as {"observe":"allow","zones":"ask","models":"off","settings":"off"}`)
	}
	g, err := base.Merge(m)
	return g, true, err
}

// accessAudit is what the audit log keeps of a key's access (never the key).
func accessAudit(a *cfgstore.Agent) map[string]any {
	return map[string]any{"name": a.Name, "description": a.Description, "key_hint": keyHint(a), "expires_at": expiresAt(a), "permissions": grantsOf(a)}
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
	now := s.opts.Now()
	exp, has, err := s.expiryArg(obj)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !has {
		exp = now.Add(DefaultKeyLife).UnixMilli()
	}
	g, _, err := permissionsArg(obj, access.Default())
	if err != nil {
		apiErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	key, hash, prefix, last4, err := newAgentKey()
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "no random key could be made: %v", err)
		return
	}
	var idb [6]byte
	rand.Read(idb[:])
	o := audit.FromRequest(r)
	a := cfgstore.Agent{ID: "ag_" + hex.EncodeToString(idb[:]), Name: name, Description: clipText(desc, 500), KeyHash: hash, KeyPrefix: prefix,
		CreatedAt: now.UnixMilli(), CreatedSource: o.Source, CreatedActor: o.Actor}
	x := cfgstore.AgentAccess{KeyHash: hash, KeyLast4: last4, ExpiresAt: exp, Grants: g.JSON(), UpdatedAt: now.UnixMilli()}
	if err := s.store.CreateAgentAccess(a, x); err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	a.Access = &x
	s.agents.reset()
	id := s.recordAudit(r, audit.ActionCreate, audit.SectionAgents, a.ID, nil, accessAudit(&a))
	s.announce(r, audit.SectionAgents, a.ID, id)
	writeJSON(w, http.StatusCreated, map[string]any{"agent": s.agentView(r, a, 0), "key": key, "mcp_settings": mcpSettings(r, key), "audit_id": id,
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
	if old.RevokedAt > 0 {
		apiErr(w, http.StatusConflict, "this key is revoked: a revoked key cannot be changed (create a new key)")
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
	exp, hasExp, err := s.expiryArg(obj)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !hasExp {
		exp = expiresAt(old)
	}
	g, hasPerms, err := permissionsArg(obj, grantsOf(old))
	if err != nil {
		apiErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	upd := *old
	upd.Name, upd.Description = name, desc
	x := cfgstore.AgentAccess{KeyHash: old.KeyHash, ExpiresAt: exp, Grants: g.JSON(), UpdatedAt: s.opts.Now().UnixMilli()}
	if old.Access != nil {
		x.KeyHash, x.KeyLast4, x.Warned = old.Access.KeyHash, old.Access.KeyLast4, old.Access.Warned
	}
	if exp != expiresAt(old) {
		x.Warned = "" // a new expiry: its warnings are due again
	}
	upd.Access = &x
	changedAccess := old.Access == nil && (hasExp || hasPerms) || old.Access != nil && (exp != old.Access.ExpiresAt || x.Grants != old.Access.Grants)
	if name == old.Name && desc == old.Description && !changedAccess {
		writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, *old, 0), "changed": false})
		return
	}
	if old.Access == nil && !changedAccess {
		err = s.store.UpdateAgent(id, name, desc) // an old-style key stays old-style until its access is set
		upd.Access = nil
	} else {
		err = s.store.UpdateAgentAccess(id, name, desc, x)
	}
	if err != nil {
		agentErr(w, err)
		return
	}
	s.agents.reset()
	aid := s.recordAudit(r, audit.ActionUpdate, audit.SectionAgents, id, accessAudit(old), accessAudit(&upd))
	s.announce(r, audit.SectionAgents, id, aid)
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, upd, 0), "changed": true, "audit_id": aid})
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
	key, hash, prefix, last4, err := newAgentKey()
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "no random key could be made: %v", err)
		return
	}
	now := s.opts.Now().UnixMilli()
	// The new key keeps the agent's permissions and expiry; an old-style
	// key's new key keeps full permissions and no expiry.
	x := cfgstore.AgentAccess{KeyHash: hash, KeyLast4: last4, Grants: grantsOf(old).JSON(), ExpiresAt: expiresAt(old), UpdatedAt: now}
	if old.Access != nil {
		x.Warned = old.Access.Warned
	}
	if err := s.store.RotateAgentAccess(id, hash, prefix, now, x); err != nil {
		agentErr(w, err)
		return
	}
	s.agents.reset()
	upd := *old
	upd.KeyHash, upd.KeyPrefix, upd.RotatedAt, upd.Access = hash, prefix, now, &x
	aid := s.recordAudit(r, "rotate", audit.SectionAgents, id, map[string]any{"name": old.Name, "key_hint": keyHint(old)}, map[string]any{"name": old.Name, "key_hint": keyHint(&upd)})
	s.announce(r, audit.SectionAgents, id, aid)
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, upd, 0), "key": key, "mcp_settings": mcpSettings(r, key), "audit_id": aid,
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
	now := s.opts.Now().UnixMilli()
	if err := s.store.RevokeAgent(id, now); err != nil {
		agentErr(w, err)
		return
	}
	s.agents.reset()
	aid := s.recordAudit(r, "revoke", audit.SectionAgents, id, map[string]any{"name": old.Name, "revoked": false}, map[string]any{"name": old.Name, "revoked": true})
	s.announce(r, audit.SectionAgents, id, aid)
	old.RevokedAt = now
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(r, *old, 0), "revoked": true, "audit_id": aid})
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

// keyCounts counts the keys that work now (not revoked, not expired) and,
// of those, the ones expiring within KeyExpiringSoon; false without a store
// or when the list cannot be read.
func (g *agentRegistry) keyCounts(ctx context.Context, now time.Time) (active, expiring int, ok bool) {
	if g.store == nil {
		return 0, 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.load(ctx)
	if err != nil {
		return 0, 0, false
	}
	for i := range list {
		switch st, _ := keyStatus(&list[i], now); st {
		case KeyActive:
			active++
		case KeyExpiring:
			active++
			expiring++
		}
	}
	return active, expiring, true
}

// activeKeys counts the keys that work now; false without a store or when
// the list cannot be read.
func (g *agentRegistry) activeKeys(ctx context.Context) (int, bool) {
	n, _, ok := g.keyCounts(ctx, time.Now())
	return n, ok
}
