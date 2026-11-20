// Package api is the HTTP surface (design section 9): OpenAI paths routed
// by model, the llama-swap compatible paths the hub's llama panel and push
// loop consume (design Appendix A), and the admin paths.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// DefaultMaxQueuedBodyBytes is the default Options.MaxQueuedBodyBytes.
const DefaultMaxQueuedBodyBytes = 256 << 20

// Options tune a Server. The zero value is production behaviour.
type Options struct {
	Logger *slog.Logger
	// Limits bound request-body buffering for model extraction.
	Limits proxy.Limits
	// MaxQueuedBodyBytes caps the sum of buffered JSON request bodies held
	// by requests that wait for an instance (default 256 MiB); a request
	// that would exceed it gets 503 with Retry-After.
	MaxQueuedBodyBytes int64
	// PingInterval is the /api/events keep-alive comment period (default 15 s).
	PingInterval time.Duration
	// Now is the clock for activity records, inflight timestamps and alias
	// log rate limiting (default time.Now).
	Now func() time.Time
	// AliasLogEvery is the minimum gap between log lines for one
	// alias+caller pair (default 1 minute).
	AliasLogEvery time.Duration
	// History is the event history behind /api/history; nil means history is off.
	History *history.Store
	// Version is the scheduler's version string, shown by /api/about.
	Version string

	// Live channel (design 9.2). Zero values are the defaults.
	WSMaxClients     int           // concurrent /ws clients (default 32)
	WSStatusInterval time.Duration // status look period: at most 4 messages a second (default 250 ms)
	WSLoadInterval   time.Duration // least status rate while a load runs (default 1 s)
	WSIdleInterval   time.Duration // least status rate at all (default 5 s)
	WSPingInterval   time.Duration // ping message period (default 15 s)
	WSQueue          int           // per-client send queue (default 64 frames)
	WSWriteTimeout   time.Duration // one frame write (default 10 s)

	// Bench and BenchStore are the benchmark feature; nil means it is off (its
	// endpoints answer 503).
	Bench      *bench.Manager
	BenchStore *bench.Store
	// System is what /api/system reports about the configuration and storage.
	System SystemInfo
	// Config is the configuration service (REST /api/config); Audit receives the audit entries of runtime actions. Nil turns the endpoints off (503).
	Config *cfgsvc.Service
	Audit  AuditStore
	// Pool is the agent membership (defaults.membership agents); nil in fleet
	// membership, where the agent and node endpoints answer 503 (nodes.go).
	Pool *pool.Pool

	statusSource func(ctx context.Context) (any, error) // tests replace the status the hub sends
	modelFiles   func() []ModelFile                     // the dev harness replaces the models directory scan
	// catalogSource, when set and answering true, is what the model catalog is
	// joined from instead of this server's own status, profiles, events,
	// aggregates and files (the dev harness shows a scripted or recorded cluster).
	// It is called with the signature of the catalog the server has; when its
	// own signature is the same it need not gather the input again.
	catalogSource func(ctx context.Context, have string) (in CatalogInput, sig string, ok bool)
}

// Server is the http.Handler for the whole API.
type Server struct {
	cfg   *config.Config
	sched *scheduler.Scheduler
	opts  Options
	log   *slog.Logger
	mux   *http.ServeMux
	start time.Time

	shutting atomic.Bool
	bodyMu   sync.Mutex
	bodyHeld int64 // buffered JSON bytes of requests that are waiting
	hub      *hub
	ws       *wsHub
	activity *activityLog
	hist     *history.Store
	files    modelFileCache // metadata of the files in models_dir (models.go)
	catalog  catalogCache   // the model catalog, kept in memory (catalog.go)
	// store is the database behind the audit log when it is the scheduler's
	// own: it also keeps the agents, the model icons and the file metadata.
	store       *cfgstore.Store
	agents      agentRegistry
	mcpToolList atomic.Pointer[func() any]

	auditQ    chan audit.Entry
	auditQuit chan struct{}
	auditDone chan struct{}
	auditOnce sync.Once
	aliasMu   sync.Mutex
	aliasSeen map[string]time.Time
}

// New builds the API for a scheduler (whose Run is started by the caller)
// and subscribes to its feed. Call Close to stop the feed goroutine.
func New(cfg *config.Config, sched *scheduler.Scheduler, o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 15 * time.Second
	}
	if o.MaxQueuedBodyBytes <= 0 {
		o.MaxQueuedBodyBytes = DefaultMaxQueuedBodyBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AliasLogEvery <= 0 {
		o.AliasLogEvery = time.Minute
	}
	if o.History == nil {
		o.History = history.Open(history.Options{}) // off
	}
	s := &Server{cfg: cfg, sched: sched, opts: o, log: o.Logger, start: o.Now(), hist: o.History,
		activity: newActivityLog(200), aliasSeen: map[string]time.Time{}}
	s.store, _ = o.Audit.(*cfgstore.Store)
	s.agents.store = s.store
	s.hub = newHub(sched, o.Now)
	s.ws = s.newWSHub()
	s.startCatalog()
	s.mux = s.routes()
	s.startAudit()
	if o.Audit != nil {
		s.SetAuditHook(s.recorderHook(o.Audit)) // benchmark changes and runs are audited too (section bench)
	}
	return s
}

// ServeHTTP implements http.Handler.
// A request that carries an agent key is attributed to that agent first (agents.go).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r, ok := s.identify(w, r); ok {
		s.mux.ServeHTTP(w, r)
	}
}

// BeginShutdown makes every new request that would start work answer 503
// (design 8.6, "stop accepting"). The caller closes its listener too.
func (s *Server) BeginShutdown() { s.shutting.Store(true) }

// Shutdown is the graceful hook: stop accepting, then run the scheduler's
// shutdown (answer waiters 503, wait for in-flight requests up to
// shutdown_grace, stop the children). When it returns the event streams
// are closed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.BeginShutdown()
	err := s.sched.Shutdown(ctx)
	s.hub.close()
	s.ws.close()
	return err
}

// Close stops the feed goroutine and closes every event stream.
func (s *Server) Close() {
	s.stopCatalog()
	s.hub.close()
	s.ws.close()
	s.stopAudit()
	if s.store != nil {
		s.agents.flush()
	}
}

// conf is the configuration in force now (it changes when a revision is applied).
func (s *Server) conf() *config.Config {
	if s.sched != nil {
		return s.sched.Config()
	}
	return s.cfg
}

func (s *Server) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.WriteError(w, http.StatusNotFound, "no such path: "+r.URL.Path, "invalid_request_error", "not found", nil)
	})
	m.HandleFunc("/v1/", s.handleV1)
	m.HandleFunc("GET /v1/models", s.handleModels)
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "OK\n")
	})
	m.HandleFunc("GET /running", s.handleRunning)
	m.HandleFunc("POST /api/models/unload", s.handleUnloadAll)
	m.HandleFunc("GET /unload", s.handleUnloadAllText)
	m.HandleFunc("POST /api/models/unload/{model...}", s.handleUnloadByName)
	m.HandleFunc("/upstream/{model}/{rest...}", s.handleUpstream)
	m.HandleFunc("/upstream/{model}", s.handleUpstream)
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("GET /ws", s.handleWS)
	s.benchRoutes(m)
	s.configRoutes(m) // /api/config, /api/audit (config.go)
	m.HandleFunc("GET /api/profiles", s.handleProfiles)
	m.HandleFunc("GET /api/models", s.handleModelCatalog)
	m.HandleFunc("GET /api/models/{key...}", s.handleModelDetail) // and GET /api/models/{key}/icon
	m.HandleFunc("PUT /api/models/{key...}", s.handleIconPut)     // only /api/models/{key}/icon
	m.HandleFunc("DELETE /api/models/{key...}", s.handleIconDelete)
	s.agentRoutes(m) // /api/agents, /api/mcp/tools (agents.go)
	s.nodeRoutes(m)  // potluck-agent's /api/pool/..., /api/nodes/{id}/... (nodes.go)
	m.HandleFunc("GET /api/metrics/activity", s.handleActivity)
	m.HandleFunc("GET /ui/{$}", s.handleUI)
	m.HandleFunc("GET /ui/", s.handleUIFile)
	m.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusMovedPermanently) })
	m.HandleFunc("GET /api/status", s.handleStatus)
	m.HandleFunc("GET /api/history", s.handleHistory)
	m.HandleFunc("GET /api/history/summary", s.handleHistorySummary)
	m.HandleFunc("GET /api/history/export", s.handleHistoryExport)
	m.HandleFunc("GET /api/about", s.handleAbout)
	m.HandleFunc("POST /api/plan", s.handlePlan)
	m.HandleFunc("POST /api/load", s.handleLoad)
	m.HandleFunc("POST /api/unload", s.handleUnload)
	return m
}

// ---- errors ----

func errType(status int) string {
	if status >= 500 {
		return "server_error"
	}
	return "invalid_request_error"
}

// writeSchedError answers a scheduler error in OpenAI format: the status,
// Retry-After for every 503, and the reason, plus the child's output tail
// for a failed load. A client that left (499) is not answered.
func writeSchedError(w http.ResponseWriter, e *scheduler.Error) {
	if e.Status == scheduler.StatusClientClosed {
		return
	}
	msg := string(e.Reason)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if len(e.Tail) > 0 {
		msg += "\n" + strings.Join(e.Tail, "\n")
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	code := ""
	if e.Reason == scheduler.ReasonUnknownModel {
		code = "model_not_found"
	}
	if code == "" {
		proxy.WriteError(w, e.Status, msg, errType(e.Status), string(e.Reason), e.Tail)
		return
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": errType(e.Status), "param": nil, "code": code, "reason": string(e.Reason)}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	w.Write(b)
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var se *scheduler.Error
	if errors.As(err, &se) {
		writeSchedError(w, se)
		return
	}
	proxy.WriteError(w, http.StatusInternalServerError, err.Error(), "server_error", "internal error", nil)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err.Error(), "server_error", "internal error", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func (s *Server) shuttingDown(w http.ResponseWriter) bool {
	if !s.shutting.Load() {
		return false
	}
	writeSchedError(w, &scheduler.Error{Status: http.StatusServiceUnavailable, Reason: scheduler.ReasonShuttingDown,
		Message: "the scheduler is shutting down", RetryAfter: 30 * time.Second})
	return true
}

// ---- alias logging ----

func callerOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// logAlias logs one use of a deprecated alias with the caller's address
// (design 8.2 step 1), at most once per alias+caller per AliasLogEvery.
func (s *Server) logAlias(r *http.Request, alias, profile string) {
	caller := callerOf(r)
	key := alias + "\x00" + caller
	now := s.opts.Now()
	s.aliasMu.Lock()
	if last, ok := s.aliasSeen[key]; ok && now.Sub(last) < s.opts.AliasLogEvery {
		s.aliasMu.Unlock()
		return
	}
	if len(s.aliasSeen) > 1024 {
		for k, t := range s.aliasSeen {
			if now.Sub(t) >= s.opts.AliasLogEvery {
				delete(s.aliasSeen, k)
			}
		}
	}
	s.aliasSeen[key] = now
	s.aliasMu.Unlock()
	attrs := []any{"alias", alias, "profile", profile, "caller", caller}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		attrs = append(attrs, "forwarded_for", xff)
	}
	s.log.Warn("deprecated model alias used", attrs...)
}

// ---- /v1 ----

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		proxy.WriteError(w, http.StatusMethodNotAllowed, "method not allowed: "+r.Method+" "+r.URL.Path, "invalid_request_error", "method not allowed", nil)
		return
	}
	if s.shuttingDown(w) {
		return
	}
	releaseWait := func() {}
	ex, err := proxy.Extract(r, s.opts.Limits)
	if err != nil {
		var pe *proxy.Error
		if errors.As(err, &pe) {
			proxy.WriteError(w, pe.Status, pe.Message, errType(pe.Status), "bad request", nil)
		} else {
			proxy.WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "bad request", nil)
		}
		return
	}
	defer ex.Body.Close()

	p, alias, ok := s.conf().Resolve(ex.Model)
	if !ok {
		writeSchedError(w, &scheduler.Error{Status: http.StatusNotFound, Reason: scheduler.ReasonUnknownModel,
			Message: fmt.Sprintf("model %q not found", ex.Model)})
		return
	}
	if alias {
		s.logAlias(r, ex.Model, p.Name)
	}
	if n := ex.BufferedBytes(); n > 0 {
		if !s.reserveBody(n) {
			writeSchedError(w, &scheduler.Error{Status: http.StatusServiceUnavailable, Reason: scheduler.ReasonClusterBusy,
				Message: "too many request bodies are waiting in the queue", RetryAfter: 30 * time.Second})
			return
		}
		var once sync.Once
		release := func() { once.Do(func() { s.releaseBody(n) }) }
		defer release()
		releaseWait = release
	}
	// While queued, keep reading the rest of an upload into the bounded spill:
	// that makes a client that disconnects mid-upload visible (design 8.2
	// step 5), because net/http only cancels the context once the body has
	// been read to the end.
	actx, cancelWait := context.WithCancelCause(r.Context())
	defer cancelWait(nil)
	ex.Prefetch(func(err error) { cancelWait(err) }, func() {
		http.NewResponseController(w).SetReadDeadline(time.Now())
	})
	lease, err := s.sched.AcquireFor(actx, ex.Model, benchRunHeader(r.Header.Get("X-Bench-Run")))
	releaseWait() // admitted or answered: the body no longer waits in the queue
	if err != nil {
		var se *scheduler.Error
		if cause := context.Cause(actx); r.Context().Err() == nil && cause != nil && errors.As(err, &se) && se.Status == scheduler.StatusClientClosed {
			var pe *proxy.Error
			if errors.As(cause, &pe) {
				proxy.WriteError(w, pe.Status, pe.Message, errType(pe.Status), "bad request", nil)
			} else {
				s.log.Debug("client left mid-upload while queued", "model", ex.Model, "err", cause)
			}
			return
		}
		s.writeErr(w, err)
		return
	}
	start := s.opts.Now()
	var res proxy.Result
	defer func() {
		lease.Release()
		s.activity.add(activityRecord{
			Timestamp: s.opts.Now(), Model: lease.Profile, ReqPath: r.URL.Path, Status: res.Status,
			DurationMS: s.opts.Now().Sub(start).Milliseconds(),
			In:         res.InputTokens, Out: res.OutputTokens, TPS: res.TokensPerSecond,
		})
		s.hub.activityDone()
	}()
	res = proxy.Forward(w, r, proxy.Options{Addr: lease.Addr, Body: ex.Body, StallTimeout: lease.StallTimeout})
	if res.Aborted {
		panic(http.ErrAbortHandler)
	}
}

// reserveBody counts n buffered body bytes against the global cap.
func (s *Server) reserveBody(n int64) bool {
	s.bodyMu.Lock()
	defer s.bodyMu.Unlock()
	if s.bodyHeld+n > s.opts.MaxQueuedBodyBytes {
		return false
	}
	s.bodyHeld += n
	return true
}

func (s *Server) releaseBody(n int64) {
	s.bodyMu.Lock()
	s.bodyHeld -= n
	s.bodyMu.Unlock()
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.conf().Profiles))
	for n := range s.conf().Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	data := make([]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{"id": n, "object": "model", "created": s.start.Unix(), "owned_by": "llm-scheduler"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---- llama-swap compatible ----

func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	list := s.sched.Running()
	out := make([]any, 0, len(list))
	for _, ri := range list {
		e := map[string]any{"model": ri.Profile, "state": string(ri.State), "name": ri.Profile, "cmd": ri.Cmd}
		if ri.LoadProgress != nil {
			e["load_progress"] = *ri.LoadProgress // an addition; the llama-swap fields above are unchanged
		}
		if ri.Load != nil {
			e["load"] = jsonView(ri.Load) // the structured report (design 7.6a)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": out, "recent_loads": jsonView(s.sched.RecentLoads())})
}

func (s *Server) handleUnloadAll(w http.ResponseWriter, r *http.Request) {
	got, err := s.sched.UnloadAllIdle(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if got == nil {
		got = []string{}
	}
	s.auditRuntime(r, audit.ActionUnload, "*", map[string]any{"unloaded": got})
	writeJSON(w, http.StatusOK, map[string]any{"msg": "ok", "unloaded": got})
}

func (s *Server) handleUnloadAllText(w http.ResponseWriter, r *http.Request) {
	got, err := s.sched.UnloadAllIdle(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.auditRuntime(r, audit.ActionUnload, "*", map[string]any{"unloaded": got})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "OK\n")
}

func (s *Server) unload(w http.ResponseWriter, r *http.Request, model string, force bool) {
	err := s.sched.Unload(r.Context(), model, force)
	switch {
	case err == nil:
		s.auditRuntime(r, audit.ActionUnload, model, map[string]any{"force": force})
		writeJSON(w, http.StatusOK, map[string]any{"msg": "ok", "model": model})
	case errors.Is(err, scheduler.ErrNotLoaded):
		writeJSON(w, http.StatusOK, map[string]any{"msg": "not loaded", "model": model})
	case errors.Is(err, scheduler.ErrBusy):
		proxy.WriteError(w, http.StatusConflict, err.Error()+" (use force to stop it anyway)", "invalid_request_error", "busy", nil)
	default:
		s.writeErr(w, err)
	}
}

func (s *Server) handleUnloadByName(w http.ResponseWriter, r *http.Request) {
	s.unload(w, r, r.PathValue("model"), false)
}

// handleUpstream is the llama-swap passthrough. It never starts a load and
// never waits: the instance must be ready or draining right now.
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	p, alias, ok := s.conf().Resolve(model)
	if !ok {
		writeSchedError(w, &scheduler.Error{Status: http.StatusNotFound, Reason: scheduler.ReasonUnknownModel,
			Message: fmt.Sprintf("model %q not found", model)})
		return
	}
	if alias {
		s.logAlias(r, model, p.Name)
	}
	addr, ready := s.sched.Upstream(model)
	if !ready {
		writeSchedError(w, &scheduler.Error{Status: http.StatusServiceUnavailable, Reason: "not ready",
			Message: fmt.Sprintf("%s is not loaded or is not ready; /upstream never starts a model", p.Name), RetryAfter: time.Second})
		return
	}
	// Strip "/upstream/{model}" from the escaped path, keeping the rest as written.
	esc := strings.TrimPrefix(r.URL.EscapedPath(), "/upstream/")
	tail := "/"
	if i := strings.IndexByte(esc, '/'); i >= 0 {
		tail = esc[i:]
	}
	plain, err := url.PathUnescape(tail)
	if err != nil {
		plain = tail
	}
	res := proxy.Forward(w, r, proxy.Options{Addr: addr, Path: plain, RawPath: tail, StallTimeout: s.conf().EffectiveStallTimeout(p)})
	if res.Aborted {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.activity.snapshot()})
}

// ---- admin ----

// modelRequest is the body of /api/plan, /api/load and /api/unload.
type modelRequest struct {
	Model string `json:"model"`
	Force bool   `json:"force"`
}

func readModelRequest(w http.ResponseWriter, r *http.Request) (modelRequest, bool) {
	var req modelRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil || req.Model == "" {
		proxy.WriteError(w, http.StatusBadRequest, "body must be JSON with a \"model\" field", "invalid_request_error", "bad request", nil)
		return req, false
	}
	return req, true
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.statusView(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	req, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	rep, err := s.sched.Plan(r.Context(), req.Model)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	v := jsonView(rep).(map[string]any)
	v["model"] = v["profile"]
	delete(v, "profile")
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown(w) {
		return
	}
	req, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	if err := s.sched.LoadFor(r.Context(), req.Model, benchRunHeader(r.Header.Get("X-Bench-Run"))); err != nil {
		s.writeErr(w, err)
		return
	}
	name := req.Model
	if p, _, ok := s.conf().Resolve(req.Model); ok {
		name = p.Name
	}
	s.auditRuntime(r, audit.ActionLoad, name, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"msg": "loading", "model": name})
}

func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	req, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	s.unload(w, r, req.Model, req.Force)
}

// benchRunHeader validates the X-Bench-Run header (the id of the benchmark run
// that sent the request): 1 to 64 characters of letters, digits, dot, dash or
// underscore. Anything else is ignored (the request is served untagged).
func benchRunHeader(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return ""
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return ""
		}
	}
	return v
}
