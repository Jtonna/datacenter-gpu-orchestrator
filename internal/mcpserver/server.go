package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/access"
)

// Protocol versions this server speaks, newest first. initialize answers with
// the client's version when it is in the list, else with the newest.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const (
	maxBody        = 4 << 20
	defaultSessMax = 256
	instructions   = "Operate Potluck, the LLM scheduler (llama.cpp across pooled machines). Start with get_status. " +
		"Before load_model call plan_load: it says whether the model fits and what it would evict. Loads take minutes (the call returns at once; poll get_status). " +
		"Never unload a model that has requests in flight unless the owner asked; unload_model force=true cuts generations off. " +
		"Configuration changes are audited (who, what, when) and can be rolled back with rollback_config; read get_config_schema before writing, " +
		"use dry_run to check a change, and pass a short comment. Benchmarks load real models: check get_bench_state first."
)

// Options configure a Server.
type Options struct {
	// Version is the scheduler's version string, reported as the server version.
	Version string
	// Guide is the benchmark guide text served as a resource (optional).
	Guide string
	// Bridge, when set, runs on the context of every in-process request after
	// the MCP marker is set: use it to hand the source and actor to the audit
	// package (see SourceFromContext).
	Bridge func(ctx context.Context, actor string) context.Context
	// MaxSessions bounds the remembered initialize handshakes (default 256).
	MaxSessions int
	// Identify, when set, runs on every request before it is handled: it
	// resolves the agent key the request may carry (Authorization: Bearer or
	// X-Agent-Key) and returns the context to continue with. A non-nil error
	// is answered with the given HTTP status (an unknown or revoked key: 401)
	// and nothing is run. A request without a key passes unchanged.
	Identify func(r *http.Request) (ctx context.Context, status int, err error)
	// Scope, when set, is what a request to method and path needs from an
	// access key (the API's route table). A tool needs what its route needs:
	// a key whose permission for it is Off does not see the tool in
	// tools/list, and calling it (or a tool set to Ask me) answers a tool
	// error that says why. A call without a key is not checked.
	Scope func(method, path string) access.Scope
	// Refused, when set, is told of every tool call refused by the key's
	// permissions (the API counts them per key: P6).
	Refused func(ctx context.Context, ref *access.Refusal)
}

// Server is the http.Handler for /mcp. inner is the scheduler's REST handler.
type Server struct {
	inner  http.Handler
	opts   Options
	tools  []*tool
	byName map[string]*tool

	mu    sync.Mutex
	sess  map[string]session
	order []string
}

type session struct{ client string }

// New builds the MCP server around the REST handler it dispatches to.
func New(inner http.Handler, o Options) *Server {
	if o.MaxSessions <= 0 {
		o.MaxSessions = defaultSessMax
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	s := &Server{inner: inner, opts: o, tools: allTools(), byName: map[string]*tool{}, sess: map[string]session{}}
	for _, t := range s.tools {
		s.byName[t.Name] = t
	}
	return s
}

// ---- JSON-RPC ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeMethod   = -32601
	codeParams   = -32602
	codeInternal = -32603
	codeAgentKey = -32001 // the request carried an agent key that is unknown or revoked
)

func rpcFail(id json.RawMessage, code int, msg string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{code, msg}}
}

// ServeHTTP implements the Streamable HTTP transport in its stateless form:
// every POST is one JSON-RPC message (or a batch) answered with one JSON body.
// There is no server-initiated stream, so GET answers 405 as the spec allows.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		http.Error(w, "this origin may not use the MCP endpoint: only the same origin and loopback or private-network origins are allowed", http.StatusForbidden)
		return
	}
	if s.opts.Identify != nil {
		ctx, status, err := s.opts.Identify(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="llm-scheduler agent identity"`)
			writeRPC(w, status, rpcFail(nil, codeAgentKey, err.Error()))
			return
		}
		if ctx != r.Context() {
			r = r.WithContext(ctx)
		}
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		s.dropSession(r.Header.Get("Mcp-Session-Id"))
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "this MCP endpoint answers POST only (no server-initiated stream)", http.StatusMethodNotAllowed)
		return
	}
	// A cross-site page can send a text/plain POST without a preflight; requiring
	// JSON forces one, and the origin check above refuses it.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supported(v) {
		http.Error(w, "unsupported MCP-Protocol-Version "+v, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeRPC(w, http.StatusBadRequest, rpcFail(nil, codeParse, "could not read the request body: "+err.Error()))
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		writeRPC(w, http.StatusBadRequest, rpcFail(nil, codeParse, "empty body"))
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			writeRPC(w, http.StatusBadRequest, rpcFail(nil, codeParse, "parse error: "+err.Error()))
			return
		}
		if len(batch) == 0 {
			writeRPC(w, http.StatusBadRequest, rpcFail(nil, codeInvalid, "empty batch"))
			return
		}
		var out []*rpcResponse
		for _, m := range batch {
			if resp, _ := s.handle(r, m, sid); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, out)
		return
	}
	var parsed rpcRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeRPC(w, http.StatusBadRequest, rpcFail(nil, codeParse, "parse error: "+err.Error()))
		return
	}
	resp, newSID := s.handle(r, body, sid)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if newSID != "" {
		w.Header().Set("Mcp-Session-Id", newSID)
	}
	status := http.StatusOK
	if resp.Error != nil && resp.Error.Code == codeInvalid {
		status = http.StatusBadRequest
	}
	writeRPC(w, status, resp)
}

func writeRPC(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func supported(v string) bool {
	for _, p := range protocolVersions {
		if p == v {
			return true
		}
	}
	return false
}

// handle runs one JSON-RPC message. A nil response means a notification (or a
// response from the client): nothing to send back.
func (s *Server) handle(r *http.Request, raw []byte, sid string) (*rpcResponse, string) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcFail(nil, codeInvalid, "invalid request: "+err.Error()), ""
	}
	if req.JSONRPC != "2.0" {
		return rpcFail(req.ID, codeInvalid, `invalid request: "jsonrpc" must be "2.0"`), ""
	}
	if req.Method == "" {
		if len(req.Result) > 0 || len(req.Error) > 0 {
			return nil, "" // a client's answer to a server request; we send none
		}
		return rpcFail(req.ID, codeInvalid, "invalid request: no method"), ""
	}
	isNote := len(req.ID) == 0
	ok := func(v any) (*rpcResponse, string) {
		if isNote {
			return nil, ""
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: v}, ""
	}
	switch req.Method {
	case "initialize":
		return s.initialize(req, isNote)
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		return ok(map[string]any{"tools": s.toolListFor(r.Context())})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return rpcFail(req.ID, codeParams, "tools/call needs params {name, arguments}"), ""
		}
		t := s.byName[p.Name]
		if t == nil {
			return rpcFail(req.ID, codeParams, "unknown tool: "+p.Name), ""
		}
		args := map[string]any{}
		if a := bytes.TrimSpace(p.Arguments); len(a) > 0 && string(a) != "null" {
			if err := json.Unmarshal(a, &args); err != nil {
				return rpcFail(req.ID, codeParams, "arguments must be a JSON object"), ""
			}
		}
		return ok(s.callTool(r.Context(), t, args, s.clientOf(sid)))
	case "resources/list":
		return ok(map[string]any{"resources": s.resourceList()})
	case "resources/templates/list":
		return ok(map[string]any{"resourceTemplates": []any{}})
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.URI == "" {
			return rpcFail(req.ID, codeParams, "resources/read needs params {uri}"), ""
		}
		res, err := s.readResource(r.Context(), p.URI, s.clientOf(sid))
		if err != nil {
			return rpcFail(req.ID, codeParams, err.Error()), ""
		}
		return ok(res)
	}
	if strings.HasPrefix(req.Method, "notifications/") || isNote {
		return nil, ""
	}
	return rpcFail(req.ID, codeMethod, "method not found: "+req.Method), ""
}

func (s *Server) initialize(req rpcRequest, isNote bool) (*rpcResponse, string) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return rpcFail(req.ID, codeParams, "initialize params: "+err.Error()), ""
		}
	}
	if isNote {
		return nil, ""
	}
	ver := protocolVersions[0]
	if supported(p.ProtocolVersion) {
		ver = p.ProtocolVersion
	}
	client := strings.TrimSpace(p.ClientInfo.Name + " " + p.ClientInfo.Version)
	if client == "" {
		client = "mcp-client"
	}
	sid := s.newSession(client)
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}, "resources": map[string]any{"subscribe": false, "listChanged": false}},
		"serverInfo":      map[string]any{"name": "llm-scheduler", "version": s.opts.Version},
		"instructions":    instructions,
	}}, sid
}

// ---- sessions: only to remember the client name for the audit actor ----

func (s *Server) newSession(client string) string {
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.order) >= s.opts.MaxSessions {
		delete(s.sess, s.order[0])
		s.order = s.order[1:]
	}
	s.sess[id] = session{client}
	s.order = append(s.order, id)
	return id
}

func (s *Server) dropSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sess[id]; !ok {
		return
	}
	delete(s.sess, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// clientOf is the client name from the handshake of this session, or a
// generic label when the client sent no (or an unknown) session id.
func (s *Server) clientOf(sid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.sess[sid]; ok {
		return c.client
	}
	return "mcp-client"
}

// ---- origin check (same rule as the live channel) ----

func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".lan") ||
		strings.HasSuffix(h, ".home.arpa") || strings.HasSuffix(h, ".internal") {
		return true
	}
	return !strings.Contains(h, ".")
}

// ---- tools ----

func (s *Server) toolList() []any { return s.toolListFor(context.Background()) }

// toolListFor is tools/list for a request: a request with an access key does
// not see the tools its key has Off.
func (s *Server) toolListFor(ctx context.Context) []any {
	g, keyed := access.GrantFrom(ctx)
	out := make([]any, 0, len(s.tools))
	for _, t := range s.tools {
		if keyed {
			if sc := s.scopeOf(t); isPermission(sc) && g.Grants.Of(sc) == access.Off {
				continue
			}
		}
		out = append(out, map[string]any{"name": t.Name, "description": t.Desc, "inputSchema": t.schema(), "annotations": t.annotations()})
	}
	return out
}

// scopeOf is what a tool needs: what its route needs. A route outside the
// four permissions (the model API's list of models), or no table: a
// read-only tool needs observe and any other settings.
func (s *Server) scopeOf(t *tool) access.Scope {
	if s.opts.Scope != nil && t.Path != "" {
		if sc := s.opts.Scope(t.Method, t.Path); isPermission(sc) {
			return sc
		}
	}
	if t.ReadOnly {
		return access.Observe
	}
	return access.Settings
}

func isPermission(sc access.Scope) bool {
	for _, p := range access.Permissions {
		if p == sc {
			return true
		}
	}
	return false
}

// Tool groups of ToolInfo.
const (
	GroupRead    = "read"
	GroupConfig  = "configuration"
	GroupBench   = "benchmarks"
	GroupRuntime = "runtime"
)

// group says what kind of thing a tool does, for a person reading the list.
func (t *tool) group() string {
	switch {
	case t.ReadOnly:
		return GroupRead
	case t.Name == "load_model" || t.Name == "unload_model" || strings.HasPrefix(t.Path, "/api/load") || strings.HasPrefix(t.Path, "/api/unload") || strings.HasPrefix(t.Path, "/api/models/unload"):
		return GroupRuntime
	case strings.HasPrefix(t.Path, "/api/bench"):
		return GroupBench
	}
	return GroupConfig
}

// ToolInfo describes the server for people: its name, version, protocol
// versions, instructions and every tool with a one-line description and its
// group (read, configuration, benchmarks, runtime). The web UI shows it.
func (s *Server) ToolInfo() any {
	tools := make([]any, 0, len(s.tools))
	for _, t := range s.tools {
		desc := t.Desc
		summary := desc
		if i := strings.Index(summary, ". "); i > 0 {
			summary = summary[:i+1]
		}
		tools = append(tools, map[string]any{"name": t.Name, "group": t.group(), "summary": summary, "description": desc,
			"read_only": t.ReadOnly, "destructive": !t.ReadOnly && t.Destructive, "input_schema": t.schema(), "scope": s.scopeOf(t)})
	}
	return map[string]any{"server": map[string]any{"name": "llm-scheduler", "version": s.opts.Version}, "path": "/mcp", "transport": "http",
		"protocol_versions": protocolVersions, "instructions": instructions, "tools": tools,
		"groups": []map[string]any{
			{"id": GroupRead, "title": "Read", "note": "Look at the cluster, the configuration, the history and benchmark results. Nothing changes."},
			{"id": GroupConfig, "title": "Configuration", "note": "Change settings, profiles, tiers and nodes. Every change is a revision that can be rolled back."},
			{"id": GroupBench, "title": "Benchmarks", "note": "Define benchmarks and suites, start and cancel runs. A run loads and unloads real models."},
			{"id": GroupRuntime, "title": "Load and unload", "note": "Load a model ahead of time or stop a loaded one. The web UI does this too (load, unload and force-unload, from Zones and Machine details); what it has not is an on/off switch for the scheduler."},
		}}
}

type toolResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

func textResult(text string, isErr bool) toolResult {
	return toolResult{Content: []map[string]any{{"type": "text", "text": text}}, IsError: isErr}
}

// recorder is a minimal http.ResponseWriter for in-process dispatch (httptest
// registers command-line flags, which a production binary should not).
type recorder struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) WriteHeader(c int) {
	if r.status == 0 {
		r.status = c
	}
}
func (r *recorder) Write(p []byte) (int, error) { r.WriteHeader(200); return r.buf.Write(p) }
func (r *recorder) Flush()                      {}

// dispatch runs one REST request through the inner handler.
func (s *Server) dispatch(ctx context.Context, rq restReq, actor string) (int, []byte, error) {
	ctx = withSource(ctx, actor)
	if s.opts.Bridge != nil {
		ctx = s.opts.Bridge(ctx, actor)
	}
	target := rq.Path
	if len(rq.Query) > 0 {
		target += "?" + rq.Query.Encode()
	}
	var body io.Reader = http.NoBody
	if rq.Body != nil {
		body = bytes.NewReader(rq.Body)
	}
	req, err := http.NewRequestWithContext(ctx, rq.Method, "http://mcp.internal"+target, body)
	if err != nil {
		return 0, nil, err
	}
	req.RemoteAddr = "mcp-in-process"
	for k, v := range rq.Header {
		req.Header[k] = v
	}
	if rq.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{h: http.Header{}}
	s.inner.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = 200
	}
	return rec.status, rec.buf.Bytes(), nil
}

// notMerged reports that the inner mux has no such route (its catch-all
// answers 404 "no such path"), as opposed to a handler's own 404.
func notMerged(status int, body []byte) bool {
	return status == http.StatusNotFound && bytes.Contains(body, []byte("no such path:"))
}

func compact(b []byte) string {
	var buf bytes.Buffer
	if json.Valid(b) && json.Compact(&buf, bytes.TrimSpace(b)) == nil {
		return buf.String()
	}
	return string(b)
}

func actorFor(client string, args map[string]any) string {
	if a, ok := args["actor"].(string); ok && strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a) + " (via " + client + ")"
	}
	return client
}

func (s *Server) callTool(ctx context.Context, t *tool, args map[string]any, client string) toolResult {
	if g, keyed := access.GrantFrom(ctx); keyed {
		if ref := access.Check(g, s.scopeOf(t)); ref != nil {
			if s.opts.Refused != nil {
				s.opts.Refused(ctx, ref)
			}
			return textResult(t.Name+" was refused: "+ref.Message+".", true)
		}
	}
	if err := t.validate(args); err != nil {
		return textResult("invalid arguments for "+t.Name+": "+err.Error(), true)
	}
	rq, err := t.build(args)
	if err != nil {
		return textResult("invalid arguments for "+t.Name+": "+err.Error(), true)
	}
	return s.run(ctx, t, rq, args, actorFor(client, args))
}

func (s *Server) run(ctx context.Context, t *tool, rq restReq, args map[string]any, actor string) toolResult {
	status, body, err := s.dispatch(ctx, rq, actor)
	if err != nil {
		return textResult("internal error: "+err.Error(), true)
	}
	if notMerged(status, body) {
		if t.Fallback != nil {
			return s.runFallback(ctx, t, args, actor)
		}
		return textResult(fmt.Sprintf("this server build does not have that endpoint (%s %s); it is probably an older scheduler version", rq.Method, rq.Path), true)
	}
	if status < 200 || status > 299 {
		return textResult(fmt.Sprintf("HTTP %d from %s %s: %s", status, rq.Method, rq.Path, compact(body)), true)
	}
	if t.Derive != nil {
		var doc map[string]any
		if json.Unmarshal(body, &doc) != nil {
			return textResult("unexpected non-JSON answer from "+rq.Path, true)
		}
		out, _ := json.Marshal(t.Derive(doc, args))
		return textResult(string(out), false)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return textResult(fmt.Sprintf("OK (HTTP %d, no content)", status), false)
	}
	return textResult(compact(body), false)
}

func (s *Server) runFallback(ctx context.Context, t *tool, args map[string]any, actor string) toolResult {
	fb := t.Fallback
	status, body, err := s.dispatch(ctx, restReq{Method: fb.Method, Path: fb.Path, Query: url.Values{}, Header: http.Header{}}, actor)
	if err != nil || status != 200 {
		return textResult(fmt.Sprintf("this server build does not have that endpoint (%s)", t.Path), true)
	}
	var doc map[string]any
	json.Unmarshal(body, &doc)
	if v, ok := fb.Reshape(doc, args); ok {
		out, _ := json.Marshal(v)
		return textResult(string(out), false)
	}
	return textResult(fmt.Sprintf("no such %s: %v", "item", args["id"]), true)
}

// ---- resources ----

const (
	uriStatus = "llm-scheduler://status"
	uriConfig = "llm-scheduler://config"
	uriGuide  = "llm-scheduler://docs/benchmark-guide"
)

func (s *Server) resourceList() []any {
	r := []any{
		map[string]any{"uri": uriStatus, "name": "status", "description": "The full /api/status object: instances, nodes, queue.", "mimeType": "application/json"},
		map[string]any{"uri": uriConfig, "name": "config", "description": "The current configuration.", "mimeType": "application/json"},
	}
	if s.opts.Guide != "" {
		r = append(r, map[string]any{"uri": uriGuide, "name": "benchmark-guide", "description": "How to design, run and read benchmarks.", "mimeType": "text/markdown"})
	}
	return r
}

func (s *Server) readResource(ctx context.Context, uri, client string) (any, error) {
	item := func(mime, text string) any {
		return map[string]any{"contents": []any{map[string]any{"uri": uri, "mimeType": mime, "text": text}}}
	}
	var path string
	switch uri {
	case uriGuide:
		if s.opts.Guide == "" {
			return nil, fmt.Errorf("unknown resource: %s", uri)
		}
		return item("text/markdown", s.opts.Guide), nil
	case uriStatus:
		path = "/api/status"
	case uriConfig:
		path = "/api/config"
	default:
		return nil, fmt.Errorf("unknown resource: %s", uri)
	}
	status, body, err := s.dispatch(ctx, restReq{Method: "GET", Path: path, Query: url.Values{}, Header: http.Header{}}, client)
	if err != nil {
		return nil, err
	}
	if notMerged(status, body) {
		return nil, fmt.Errorf("this server build does not have that endpoint (GET %s)", path)
	}
	if status != 200 {
		return nil, fmt.Errorf("HTTP %d from GET %s: %s", status, path, compact(body))
	}
	return item("application/json", compact(body)), nil
}
