// Package api is the HTTP surface (design section 9): OpenAI paths routed
// by model, the llama-swap compatible paths the hub's llama panel and push
// loop consume (design Appendix A), and the admin paths.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/filecache"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/diskstat"
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

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/groups"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelfiles"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
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
	WSRequestsTick   time.Duration // running rows on the requests topic (default 1 s)
	WSQueue          int           // per-client send queue (default 64 frames)
	WSWriteTimeout   time.Duration // one frame write (default 10 s)

	// Bench and BenchStore are the benchmark feature; nil means it is off (its
	// endpoints answer 503).
	Bench      *bench.Manager
	BenchStore *bench.Store
	// BenchOffReason: the database's bench tables are newer than this build
	// (bench.OpenShared): the bench routes answer 503 with this reason and
	// enabled false, and the health summary has a Benchmarks row.
	BenchOffReason string
	// System is what /api/system reports about the configuration and storage.
	System SystemInfo
	// Config is the configuration service (REST /api/config); Audit receives the audit entries of runtime actions. Nil turns the endpoints off (503).
	Config *cfgsvc.Service
	Audit  AuditStore
	// Pool is the agent membership (defaults.membership agents); nil in fleet
	// membership, where the agent and node endpoints answer 503 (nodes.go).
	Pool *pool.Pool
	// Logs is the pass-through in front of the log store (CR-10, logs.go);
	// nil means central logging is not there and its routes answer 404.
	Logs *logrelay.Relay
	// Requests is the request log (usage.db, requests.go); nil keeps only
	// the live requests, in memory.
	Requests *usage.Recorder
	// Usage is the usage history (usage.db); nil means it is not kept. The
	// availability sampler runs when it is set (availability.go).
	Usage *usage.Store
	// Groups is the zone groups (scheduler.db); nil keeps them in memory
	// for this process.
	Groups *groups.Store

	statusSource func(ctx context.Context) (any, error) // tests replace the status the hub sends
	modelFiles   func() []ModelFile                     // the dev harness replaces the models directory scan
	// trashOps replaces the writes of the models trash (tests act out a
	// read-only folder); trashStop acts out a kill between its steps.
	trashOps  *modelfiles.Ops
	trashStop func(step string) bool
	// proxyOnResponse is passed to the proxy as Options.OnResponse (tests cut
	// a request once the model server's headers arrived).
	proxyOnResponse func()
	// trashUnloadWait replaces the 5 s a delete with interrupt waits for the
	// file's instances to be gone (tests).
	trashUnloadWait time.Duration
	// trashClearEvery replaces the hour between two passes of the models
	// trash clearing; < 0: the clearing task does not start (tests run
	// clearTrash themselves).
	trashClearEvery time.Duration
	// modelsWritable replaces modelfiles.Writable (tests act out a read-only
	// folder, or one where it cannot be told); it is then asked every time.
	modelsWritable func(dir string) (writable, ok bool)
	fileCache      filecache.Sys // tests replace the page cache calls of POST /api/cache/evict
	// catalogSource, when set and answering true, is what the model catalog is
	// joined from instead of this server's own status, profiles, events,
	// aggregates and files (the dev harness shows a scripted or recorded cluster).
	// It is called with the signature of the catalog the server has; when its
	// own signature is the same it need not gather the input again.
	catalogSource func(ctx context.Context, have string) (in CatalogInput, sig string, ok bool)
	// catalogClock, catalogMinGap and catalogDriftGap replace time.Now and
	// the gaps of the live channel's catalog rebuilds (catalogplans.go; tests).
	catalogClock    func() time.Time
	catalogMinGap   time.Duration
	catalogDriftGap time.Duration

	// The health summary (health.go): tests replace the disk read, its
	// timeout and period, the push period and the task counters.
	diskStat    func(path string) (diskstat.FS, error)
	diskTimeout time.Duration
	diskEvery   time.Duration
	healthEvery time.Duration
	taskStats   func() []supervise.TaskStats
	// keyExpiryEvery: how often access key expiry is checked; < 0 off (tests).
	keyExpiryEvery time.Duration
}

// Server is the http.Handler for the whole API.
type Server struct {
	occ   *occupiedTotals // occupied.go: /api/system occupied_only
	peaks *peakCycles     // peakcycles.go: /api/needs peak_cycles
	// bg is cancelled by Close, which then waits for bgWG: the background
	// work started outside a request (Warm) that reads the store.
	bg       context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup

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
	reqs     *usage.Recorder // the request log (requests.go)
	hist     *history.Store
	files    modelFileCache // metadata of the files in models_dir (models.go)
	catalog  catalogCache   // the model catalog, kept in memory (catalog.go)
	// store is the database behind the audit log when it is the scheduler's
	// own: it also keeps the agents, the model icons and the file metadata.
	store       *cfgstore.Store
	trash       trashState // the models trash (trash.go)
	agents      agentRegistry
	mcpToolList atomic.Pointer[func() any]

	auditQ    chan audit.Entry
	auditQuit chan struct{}
	auditDone chan struct{}
	auditOnce sync.Once
	aliasMu   sync.Mutex
	aliasSeen map[string]time.Time
	feed      feedSeq // the node feed's change counter (feed.go)
	// avail is the availability sampler (availability.go); nil without a usage store.
	avail *availability.Sampler
	// disks is the one reader of disk capacity and free space; health the
	// summary's state (health.go).
	disks  *diskstat.Reader
	health healthState
	// groups is the zone groups (groups.go); in memory when Options.Groups is nil.
	groups *groups.Store
	links  *loadLinks // linkspeed.go
	// pending is the zones made before their machine joined (pending.go).
	pending *pendingZones
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
		activity: newActivityLog(200), aliasSeen: map[string]time.Time{}, reqs: o.Requests}
	if s.reqs == nil {
		s.reqs = usage.NewRecorder(nil) // live rows only
	}
	s.reqs.SetNamer(s.modelName) // short_name: the same rule as /api/models
	s.bg, s.bgCancel = context.WithCancel(context.Background())
	s.occ = startOccupied(o.History)
	s.peaks = startPeakCycles(o.History)
	if o.Usage.On() {
		s.peaks.setTokens(o.Usage.SessionTokens)
	}
	s.groups = o.Groups
	s.pending = newPendingZones()
	if s.groups == nil {
		s.groups = groups.Memory()
	}
	s.store, _ = o.Audit.(*cfgstore.Store)
	s.agents.store = s.store
	s.hub = newHub(sched, o.Now)
	s.ws = s.newWSHub()
	s.startCatalog()
	s.mux = s.routes()
	s.startAudit()
	s.startAvailability() // availability.go
	s.startHealth()       // health.go
	s.startLoadLinks()    // linkspeed.go
	s.startKeyExpiry()    // keyexpiry.go
	if o.Audit != nil {
		s.SetAuditHook(s.recorderHook(o.Audit)) // benchmark changes and runs are audited too (section bench)
	}
	return s
}

// ServeHTTP implements http.Handler.
// A request that carries an access key is attributed to that agent first
// (agents.go) and may do only what its key allows (scopes.go); a request
// without one is the default user.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(audit.WithDefaultUser(r.Context(), s.defaultUser()))
	if r, ok := s.identify(w, r); ok && s.authorize(w, r) {
		s.mux.ServeHTTP(w, r)
	}
}

// defaultUser is the name of the default user (setting default_user_name).
func (s *Server) defaultUser() string {
	if c := s.conf(); c != nil && c.DefaultUserName != "" {
		return c.DefaultUserName
	}
	return audit.DefaultActor
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

// Close stops the feed goroutine and closes every event stream, cancels and
// waits for the background catalog build, and writes what the audit queue
// and the agents table hold. Nothing of the server uses the store after it
// returns, so the caller may close the database then.
func (s *Server) Close() {
	s.occ.cancel()
	s.peaks.cancel()
	s.stopLoadLinks()
	s.bgCancel()
	s.bgWG.Wait()
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
	m.HandleFunc("GET /unload", s.handleUnloadAllTextGet) // deprecated: a GET that changes state
	m.HandleFunc("POST /unload", s.handleUnloadAllText)
	m.HandleFunc("POST /api/models/unload/{model...}", s.handleUnloadByName)
	m.HandleFunc("POST /api/models/drain/{model...}", s.handleDrain)     // drain.go
	m.HandleFunc("POST /api/models/undrain/{model...}", s.handleUndrain) // drain.go
	m.HandleFunc("/upstream/{model}/{rest...}", s.handleUpstream)
	m.HandleFunc("/upstream/{model}", s.handleUpstream)
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("GET /ws", s.handleWS)
	s.benchRoutes(m)
	s.configRoutes(m) // /api/config, /api/audit (config.go)
	m.HandleFunc("GET /api/profiles", s.handleProfiles)
	m.HandleFunc("GET /api/profiles/test", s.handleTestProfiles) // testprofiles.go
	m.HandleFunc("GET /api/models", s.handleModelCatalog)
	m.HandleFunc("GET /api/models/{key...}", s.handleModelDetail)             // and GET /api/models/{key}/icon
	m.HandleFunc("PUT /api/models/pin/{key...}", s.handlePinPut)              // pins.go
	m.HandleFunc("POST /api/models/delete/{key...}", s.handleModelDelete)     // trash.go
	m.HandleFunc("GET /api/models/trash", s.handleTrashList)                  // trash.go
	m.HandleFunc("POST /api/models/trash/{id}/restore", s.handleModelRestore) // trash.go
	m.HandleFunc("POST /api/models/trash/{id}/remove", s.handleTrashRemove)   // trashremove.go
	m.HandleFunc("DELETE /api/models/pin/{key...}", s.handlePinDelete)        // pins.go
	m.HandleFunc("PUT /api/models/{key...}", s.handleIconPut)                 // only /api/models/{key}/icon
	m.HandleFunc("DELETE /api/models/{key...}", s.handleIconDelete)
	m.HandleFunc("POST /api/models/{key...}", s.handleModelPost) // only /api/models/{file}/standard-set (standardset.go)
	// GET /api/nodes/availability (availability.go)
	s.availabilityRoutes(m)
	m.HandleFunc("GET /api/health", s.handleHealthSummary)
	s.agentRoutes(m) // /api/agents, /api/mcp/tools (agents.go)
	s.groupRoutes(m) // zone groups (groups.go)
	s.nodeRoutes(m)  // potluck-agent's /api/pool/..., /api/nodes/{id}/... (nodes.go)
	s.logRoutes(m)   // central logging: /api/pool/nodes/{id}/logs, /api/logs (logs.go)
	m.HandleFunc("GET /api/metrics/activity", s.handleActivity)
	s.requestRoutes(m) // the request log: /api/requests... (requests.go)
	s.uiRoutes(m)      // the web UI at /ui/ (uifiles.go)
	m.HandleFunc("GET /api/status", s.handleStatus)
	m.HandleFunc("GET /api/history", s.handleHistory)
	m.HandleFunc("GET /api/history/summary", s.handleHistorySummary)
	m.HandleFunc("GET /api/history/export", s.handleHistoryExport)
	m.HandleFunc("GET /api/about", s.handleAbout)
	m.HandleFunc("POST /api/plan", s.handlePlan)
	m.HandleFunc("POST /api/load", s.handleLoad)
	m.HandleFunc("POST /api/unload", s.handleUnload)
	m.HandleFunc("POST /api/forget-need", s.handleForgetNeed)
	m.HandleFunc("POST /api/cache/evict", s.handleCacheEvict) // cacheevict.go
	m.HandleFunc("GET /api/needs", s.handleNeeds)             // needs.go
	m.HandleFunc("POST /api/needs/forget", s.handleForgetShape)
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

// aliasDue notes a use of key; false when it was logged within
// AliasLogEvery.
func (s *Server) aliasDue(key string, now time.Time) bool {
	s.aliasMu.Lock()
	defer s.aliasMu.Unlock()
	if last, ok := s.aliasSeen[key]; ok && now.Sub(last) < s.opts.AliasLogEvery {
		return false
	}
	if len(s.aliasSeen) > 1024 {
		for k, t := range s.aliasSeen {
			if now.Sub(t) >= s.opts.AliasLogEvery {
				delete(s.aliasSeen, k)
			}
		}
	}
	s.aliasSeen[key] = now
	return true
}

// logAlias logs one use of a deprecated alias with the caller's address
// (design 8.2 step 1), at most once per alias+caller per AliasLogEvery.
func (s *Server) logAlias(r *http.Request, alias, profile string) {
	caller := callerOf(r)
	key := alias + "\x00" + caller
	now := s.opts.Now()
	if !s.aliasDue(key, now) {
		return
	}
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

	benchRun := benchRunHeader(r.Header.Get("X-Bench-Run"))
	// Live prompt figures (requests_live_timings): a benchmark's requests
	// are left as sent, so it measures the server as a client sees it.
	if s.conf().RequestsLiveTimings && benchRun == "" && liveTimingsPath(r.URL.Path) {
		ex.AddTimingsPerToken()
	}
	p, alias, ok := s.resolveModel(ex.Model)
	if !ok {
		e := &scheduler.Error{Status: http.StatusNotFound, Reason: scheduler.ReasonUnknownModel,
			Message: fmt.Sprintf("model %q not found", ex.Model)}
		s.trackRequest(r, r.URL.Path, usage.KindV1, ex.Model, nil, benchRun).Ended(refusedEnd(e))
		writeSchedError(w, e)
		return
	}
	if alias {
		s.logAlias(r, ex.Model, p.Name)
	}
	track := s.trackRequest(r, r.URL.Path, usage.KindV1, ex.Model, p, benchRun)
	if n := ex.BufferedBytes(); n > 0 {
		if !s.reserveBody(n) {
			e := &scheduler.Error{Status: http.StatusServiceUnavailable, Reason: scheduler.ReasonClusterBusy,
				Message: "too many request bodies are waiting in the queue", RetryAfter: 30 * time.Second}
			track.Ended(refusedEnd(e))
			writeSchedError(w, e)
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
	lease, err := s.sched.AcquireFor(actx, ex.Model, benchRun)
	releaseWait() // admitted or answered: the body no longer waits in the queue
	if err != nil {
		track.Ended(refusedEnd(err))
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
	live := &proxy.Live{}
	track.Started(leasePlacement(lease), live)
	var res proxy.Result
	defer func() {
		lease.Release()
		if res.Unloaded && res.Err != nil { // the request log says why; the transport's word stays here
			s.log.Debug("a request was cut by a forced unload", "profile", lease.Profile, "transport", res.Err)
		}
		track.Ended(resultEnd(res))
		s.activity.add(activityRecord{
			Timestamp: s.opts.Now(), Model: lease.Profile, ReqPath: r.URL.Path, Status: res.Status,
			DurationMS: s.opts.Now().Sub(start).Milliseconds(),
			In:         res.InputTokens, Out: res.OutputTokens, TPS: res.TokensPerSecond,
		})
		s.hub.activityDone()
	}()
	bodyLen := int64(0)
	if ex.Rewritten() {
		bodyLen = ex.BufferedBytes()
	}
	res = proxy.Forward(w, r, proxy.Options{Addr: lease.Addr, Body: ex.Body, BodyLen: bodyLen, StallTimeout: lease.StallTimeout,
		APIKey: lease.APIKey, Gone: lease.Gone, Live: live, CutBy: cutByOf(lease),
		OnResponse: s.opts.proxyOnResponse})
	if res.Aborted {
		panic(http.ErrAbortHandler)
	}
}

// cutByOf is what the proxy tells a request whose instance was stopped on
// purpose while it ran (scheduler Lease.CutBy): the 503 unloaded answer a
// loading or queued caller gets, written the same way (writeSchedError).
func cutByOf(l *scheduler.Lease) func() *proxy.CutAnswer {
	if l == nil || l.CutBy == nil {
		return nil
	}
	return func() *proxy.CutAnswer {
		e := l.CutBy()
		if e == nil {
			return nil
		}
		msg := string(e.Reason)
		if e.Message != "" {
			msg += ": " + e.Message
		}
		return &proxy.CutAnswer{Status: e.Status, Message: msg, Type: errType(e.Status), Reason: string(e.Reason), RetryAfter: e.RetryAfter}
	}
}

// liveTimingsPath is whether requests_live_timings applies to a /v1 path:
// chat and text completions, the requests that stream tokens.
func liveTimingsPath(p string) bool {
	return p == "/v1/chat/completions" || p == "/v1/completions"
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
	func() {
		s.bodyMu.Lock()
		defer s.bodyMu.Unlock()
		s.bodyHeld -= n
	}()
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

// unloadGetWarning is the Warning header of GET /unload.
const unloadGetWarning = `299 - "GET /unload is deprecated: it changes state; use POST /unload or POST /api/models/unload"`

// handleUnloadAllTextGet is GET /unload, kept for llama-swap clients: it
// unloads everything idle like POST /unload, marks its answer deprecated and
// logs one WARN per call naming the caller.
func (s *Server) handleUnloadAllTextGet(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("GET /unload is deprecated: it changes state; use POST /unload or POST /api/models/unload", "caller", callerOf(r))
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Warning", unloadGetWarning)
	s.handleUnloadAllText(w, r)
}

// handleUnloadAllText is POST /unload: everything idle is unloaded; the
// answer is "OK" in plain text (llama-swap's shape).
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
	p, alias, ok := s.resolveModel(model)
	if !ok {
		writeSchedError(w, &scheduler.Error{Status: http.StatusNotFound, Reason: scheduler.ReasonUnknownModel,
			Message: fmt.Sprintf("model %q not found", model)})
		return
	}
	if alias {
		s.logAlias(r, model, p.Name)
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
	// Only inference is a row; a monitoring call (/slots, /props, /health)
	// gets a nil tracker, no live counter and no placement look-up.
	track := s.trackRequest(r, plain, usage.KindUpstream, model, p, benchRunHeader(r.Header.Get("X-Bench-Run")))
	addr, ready := s.sched.Upstream(model)
	if !ready {
		e := &scheduler.Error{Status: http.StatusServiceUnavailable, Reason: "not ready",
			Message: fmt.Sprintf("%s is not loaded or is not ready; /upstream never starts a model", p.Name), RetryAfter: time.Second}
		track.Ended(refusedEnd(e))
		writeSchedError(w, e)
		return
	}
	var live *proxy.Live
	if track != nil {
		live = &proxy.Live{}
		up, _ := s.sched.UpstreamPlacement(model) // a passthrough holds no lease: the machines of the instance serving it now
		track.Started(upstreamPlacement(up), live)
	}
	var res proxy.Result
	defer func() { track.Ended(resultEnd(res)) }() // also when Forward panics
	res = proxy.Forward(w, r, proxy.Options{Addr: addr, Path: plain, RawPath: tail, StallTimeout: s.conf().EffectiveStallTimeout(p),
		APIKey: s.sched.UpstreamKey(model), Live: live})
	if res.Aborted {
		panic(http.ErrAbortHandler)
	}
}

// resolveModel is config Resolve, else a run-scoped profile the scheduler
// holds now (scheduler/scoped.go).
func (s *Server) resolveModel(model string) (*core.Profile, bool, bool) {
	if p, alias, ok := s.conf().Resolve(model); ok {
		return p, alias, true
	}
	if p, ok := s.sched.Scoped(model); ok {
		return p, false, true
	}
	return nil, false, false
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.activity.snapshot()})
}

// ---- admin ----

// modelRequest is the body of /api/plan, /api/load and /api/unload.
type modelRequest struct {
	Model string `json:"model"`
	Force bool   `json:"force"`
	// Nodes (load and plan): place this load on these nodes only, as
	// force_nodes would, without changing the profile (pin.go).
	Nodes []string `json:"nodes"`
	// Scope and the run's settings (load and plan): a run-scoped profile,
	// "<model>@<scope>", the production profile with these settings, held
	// in memory only (scheduler/scoped.go).
	Scope     string  `json:"scope"`
	Ctx       *int    `json:"ctx"`
	Parallel  *int    `json:"parallel"`
	KVQuant   *string `json:"kv_quant"`
	FlashAttn *bool   `json:"flash_attn"`
	Host      *string `json:"host"`
	Thinking  *bool   `json:"thinking"`
}

func (m modelRequest) scoped() scheduler.ScopedLoad {
	return scheduler.ScopedLoad{Model: m.Model, Scope: m.Scope, Nodes: m.Nodes, Ctx: m.Ctx, Parallel: m.Parallel,
		KVQuant: m.KVQuant, FlashAttn: m.FlashAttn, Host: m.Host, Thinking: m.Thinking}
}

// scopedAudit is what the audit keeps of a scoped load: the scope, the
// zones and the settings the body gave.
func (m modelRequest) scopedAudit() map[string]any {
	out := map[string]any{"scope": m.Scope}
	if len(m.Nodes) > 0 {
		out["nodes"] = m.Nodes
	}
	if m.Ctx != nil {
		out["ctx"] = *m.Ctx
	}
	if m.Parallel != nil {
		out["parallel"] = *m.Parallel
	}
	if m.KVQuant != nil {
		out["kv_quant"] = *m.KVQuant
	}
	if m.FlashAttn != nil {
		out["flash_attn"] = *m.FlashAttn
	}
	if m.Host != nil {
		out["host"] = *m.Host
	}
	if m.Thinking != nil {
		out["thinking"] = *m.Thinking
	}
	return out
}

func readModelRequest(w http.ResponseWriter, r *http.Request) (modelRequest, bool) {
	var req modelRequest
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err == nil {
		err = json.Unmarshal(b, &req)
	}
	if err != nil || req.Model == "" {
		proxy.WriteError(w, http.StatusBadRequest, "body must be JSON with a \"model\" field", "invalid_request_error", "bad request", nil)
		return req, false
	}
	// A scoped load's body is read strictly: a field this version does not
	// know (say, a model file of its own) is refused, never ignored. The
	// model file is always the production profile's.
	if req.scoped().Set() {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var strict modelRequest
		if err := dec.Decode(&strict); err != nil {
			proxy.WriteError(w, http.StatusBadRequest, "scoped load: "+err.Error()+" (the model file is the production profile's)", "invalid_request_error", "bad request", nil)
			return req, false
		}
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
	var rep scheduler.PlanReport
	var err error
	if req.scoped().Set() {
		if len(req.Nodes) > 0 && !s.checkViewNodes(w, r, req.Nodes) {
			return
		}
		rep, err = s.sched.PlanScoped(r.Context(), req.scoped())
	} else if len(req.Nodes) > 0 {
		if !s.checkViewNodes(w, r, req.Nodes) {
			return
		}
		rep, err = s.sched.PlanOn(r.Context(), req.Model, req.Nodes)
	} else {
		rep, err = s.sched.Plan(r.Context(), req.Model)
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	v := jsonView(rep).(map[string]any)
	v["model"] = v["profile"]
	delete(v, "profile")
	addShapeFacts(v, s.conf(), req.Model)
	s.addGrowthFacts(v, req.Model, req.scoped())
	writeJSON(w, http.StatusOK, v)
}

// addShapeFacts adds to a plan answer the profile's ctx and parallel and
// shape_shared_with: the other profiles with the same shape (model, ctx,
// parallel and options), each with whether it was generated. A benchmark's
// context_fill phase refuses to run on a shape a written profile shares,
// because its memory peak becomes that shape's stored peak (B8).
func addShapeFacts(v map[string]any, cfg *config.Config, model string) {
	p, _, ok := cfg.Resolve(model)
	if !ok {
		return
	}
	if _, has := v["ctx"]; !has {
		v["ctx"] = p.Ctx
	}
	if _, has := v["parallel"]; !has {
		v["parallel"] = max(p.Parallel, 1)
	}
	if _, has := v["generated"]; !has {
		v["generated"] = IsGeneratedProfile(p.Name, p.Generated)
	}
	shared := []map[string]any{}
	for _, n := range scheduler.ProfilesOfShape(cfg, p.Name) {
		if n == p.Name {
			continue
		}
		shared = append(shared, map[string]any{"id": n, "generated": IsGeneratedProfile(n, cfg.Profiles[n].Generated)})
	}
	v["shape_shared_with"] = shared
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown(w) {
		return
	}
	req, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	if req.scoped().Set() {
		s.loadScoped(w, r, req)
		return
	}
	var err error
	if len(req.Nodes) > 0 {
		if !s.checkViewNodes(w, r, req.Nodes) {
			return
		}
		err = s.sched.LoadOn(r.Context(), req.Model, req.Nodes)
	} else {
		err = s.sched.LoadFor(r.Context(), req.Model, benchRunHeader(r.Header.Get("X-Bench-Run")))
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	name := req.Model
	if p, _, ok := s.conf().Resolve(req.Model); ok {
		name = p.Name
	}
	var after map[string]any
	out := map[string]any{"msg": "loading", "model": name}
	if len(req.Nodes) > 0 {
		after = map[string]any{"nodes": req.Nodes}
		out["nodes"] = req.Nodes
	}
	s.auditRuntime(r, audit.ActionLoad, name, after)
	writeJSON(w, http.StatusAccepted, out)
}

// loadScoped is POST /api/load with a scope: the answer names the scoped
// profile, its production profile and the scope.
func (s *Server) loadScoped(w http.ResponseWriter, r *http.Request, req modelRequest) {
	if len(req.Nodes) > 0 && !s.checkViewNodes(w, r, req.Nodes) {
		return
	}
	name, err := s.sched.LoadScoped(r.Context(), req.scoped(), benchRunHeader(r.Header.Get("X-Bench-Run")))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	base, scope, _ := scheduler.SplitScoped(name)
	after := req.scopedAudit()
	after["production"] = base
	s.auditRuntime(r, audit.ActionLoad, name, after)
	out := map[string]any{"msg": "loading", "model": name, "production": base, "scope": scope}
	if len(req.Nodes) > 0 {
		out["nodes"] = req.Nodes
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	req, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	s.unload(w, r, req.Model, req.Force)
}

// handleForgetNeed drops a profile's learned memory need, in memory and in
// the database, so fit decisions use the estimate or needs_mib again (for
// when the hardware changed). Audited as a runtime action.
func (s *Server) handleForgetNeed(w http.ResponseWriter, r *http.Request) {
	req, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	had, err := s.sched.ForgetNeed(r.Context(), req.Model)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	name := req.Model
	if p, _, ok := s.conf().Resolve(req.Model); ok {
		name = p.Name
	}
	s.auditRuntime(r, audit.ActionForgetNeed, name, map[string]any{"forgotten": had})
	// A learned need belongs to the shape of the load, so forgetting it
	// applies to every profile of that shape.
	shared := scheduler.ProfilesOfShape(s.conf(), name)
	if shared == nil {
		shared = []string{}
	}
	msg := "forgotten for every profile of this shape"
	if !had {
		msg = "nothing learned"
	}
	writeJSON(w, http.StatusOK, map[string]any{"msg": msg, "model": name, "forgotten": had, "profiles_of_shape": shared})
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
