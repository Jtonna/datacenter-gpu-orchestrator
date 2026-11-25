package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// The request log (UI audit 01, P1 and P6): every /v1 and /upstream request
// is a row, kept in usage.db by the usage package. The handlers only call
// the recorder, which updates memory and queues the change for its writer:
// nothing here touches the disk. The in-memory activity record
// (/api/metrics/activity) is kept as it was.

// trackRequest records a request as it arrives. p is nil for a model that
// is not known (the row is refused at once). Only inference is recorded:
// a request whose method and path (sent, as the model server sees it) do
// not run the model gets a nil tracker, whose methods do nothing, so it
// costs the recorder no row, no live entry and no write.
func (s *Server) trackRequest(r *http.Request, sent, kind, requested string, p *core.Profile, benchRun string) *usage.Tracker {
	if !usage.IsInference(r.Method, sent) {
		return nil
	}
	b := usage.Begin{Kind: kind, Path: r.URL.Path, Requested: requested, BenchRun: benchRun, SourceAddr: callerOf(r),
		Caller: s.defaultUser(), CallerKind: "default"}
	if p != nil {
		b.Profile, b.ModelFile, b.Ctx = p.Name, p.Model, p.Ctx
		// A run-scoped profile (scheduler/scoped.go) is logged under its
		// production id; the requested name keeps the scope.
		if base, _, ok := scheduler.SplitScoped(p.Name); ok && s.conf().Profiles[p.Name] == nil {
			b.Profile = base
		}
	}
	if _, name, ok := audit.AgentFrom(r.Context()); ok {
		b.Caller, b.CallerKind = name, "key"
	}
	return s.reqs.Queued(b)
}

// leasePlacement is where an admitted request runs.
func leasePlacement(l *scheduler.Lease) usage.Placement {
	pl := usage.Placement{SchedID: l.ID, Instance: l.Instance, Session: l.Session, Nodes: make([]usage.Node, len(l.Nodes))}
	for i, n := range l.Nodes {
		pl.Nodes[i] = usage.Node{ID: n.ID, Name: n.Name, Role: n.Role, Share: n.Share}
		if n.Role == scheduler.RoleHost {
			pl.HostNode = n.ID
		}
	}
	return pl
}

// upstreamPlacement is the request log's placement of a passthrough request:
// the instance that serves the model now, from the scheduler's snapshot (no
// lease is taken).
func upstreamPlacement(u scheduler.UpstreamPlacement) usage.Placement {
	pl := usage.Placement{Instance: u.Instance, Session: u.Session, Nodes: make([]usage.Node, len(u.Nodes))}
	for i, n := range u.Nodes {
		pl.Nodes[i] = usage.Node{ID: n.ID, Name: n.Name, Role: n.Role, Share: n.Share}
		if n.Role == scheduler.RoleHost {
			pl.HostNode = n.ID
		}
	}
	return pl
}

// resultEnd is how a forwarded request ended.
func resultEnd(res proxy.Result) usage.End {
	e := usage.End{Status: usage.StatusDone, HTTPStatus: res.Status, TokensIn: res.InputTokens, Reused: res.CachedTokens,
		TokensOut: res.OutputTokens, PrefillTPS: res.PromptPerSecond, DecodeTPS: res.TokensPerSecond, PrefillMS: res.PromptMS,
		DecodeMS: res.PredictedMS, Stream: res.Stream}
	switch reason := res.Reason(); {
	case res.Status == 0: // Forward never returned (it panicked)
		e.Status, e.Reason, e.Message = usage.StatusFailed, "internal", "the proxy failed"
	case reason == usage.ReasonClientLeft:
		e.Status, e.Reason, e.Message = usage.StatusCancelled, reason, "cancelled by the app that sent it"
	case reason != "":
		e.Status, e.Reason = usage.StatusFailed, reason
		e.Message = map[string]string{"stall": "the model stopped sending", "cut": "the zone dropped the connection",
			"upstream_error": "the model server could not be reached", "aborted": "the answer could not be delivered",
			"unloaded": "it was unloaded while it ran (a forced unload)"}[reason]
		if res.Err != nil && !res.Unloaded { // a cut request's reason is known: the transport's word (EOF) is noise there
			e.Message += ": " + res.Err.Error()
		}
	case res.Status >= 400:
		e.Status, e.Reason, e.Message = usage.StatusFailed, usage.ReasonUpstreamStatus, "the model server answered "+strconv.Itoa(res.Status)
	}
	return e
}

// refusedEnd is a request turned away before it ran; a client that left
// while queued is cancelled.
func refusedEnd(err error) usage.End {
	e := usage.End{Status: usage.StatusRefused, TokensIn: -1, Reused: -1, TokensOut: -1, PrefillTPS: -1, DecodeTPS: -1}
	var se *scheduler.Error
	if !errors.As(err, &se) {
		e.Reason, e.Message, e.HTTPStatus = "internal", err.Error(), http.StatusInternalServerError
		return e
	}
	e.Reason, e.Message, e.HTTPStatus = string(se.Reason), se.Message, se.Status
	if se.Status == scheduler.StatusClientClosed || se.Reason == scheduler.ReasonClientGone {
		e.Status, e.Reason, e.Message = usage.StatusCancelled, usage.ReasonClientLeftQueued, "the app that sent it left while it was queued"
	}
	return e
}

// ---- routes ----

func (s *Server) requestRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/requests", s.handleRequests)
	m.HandleFunc("GET /api/requests/summary", s.handleRequestSummary)
	m.HandleFunc("GET /api/requests/{id}", s.handleRequest)
}

// GET /api/requests: a page of the request log, newest first, the live
// requests (queued, then running) first.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	f := usage.Filter{Profile: v.Get("profile"), Node: v.Get("node"), Caller: v.Get("caller")}
	for _, st := range strings.Split(v.Get("status"), ",") {
		switch st = strings.TrimSpace(st); st {
		case "":
		case usage.StatusQueued, usage.StatusRunning, usage.StatusDone, usage.StatusFailed, usage.StatusRefused, usage.StatusCancelled:
			f.Status = append(f.Status, st)
		default:
			badParam(w, "status must be queued, running, done, failed, refused or cancelled (comma-separated), got %q", st)
			return
		}
	}
	var ok bool
	if f.From, ok = tsParam(w, r, "from"); !ok {
		return
	}
	if f.To, ok = tsParam(w, r, "to"); !ok {
		return
	}
	if f.Before, ok = tsParam(w, r, "before"); !ok {
		return
	}
	switch b := v.Get("bench"); b {
	case "":
	case "yes", "true":
		f.Bench = &[]bool{true}[0]
	case "no", "false":
		f.Bench = &[]bool{false}[0]
	default:
		badParam(w, "bench must be yes or no, got %q", b)
		return
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			badParam(w, "limit must be a positive integer, got %q", l)
			return
		}
		f.Limit = n
	}
	page, err := s.reqs.List(f)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err.Error(), "server_error", "usage store", nil)
		return
	}
	st := s.reqs.Store()
	writeJSON(w, http.StatusOK, map[string]any{"requests": page.Requests, "next": page.Next, "on": st.On(),
		"history_begins_at": beginsAt(st)})
}

// beginsAt is the store's history_begins_at in unix ms; nil (JSON null)
// when the store is off, never the zero time's -62135596800000.
func beginsAt(st *usage.Store) any {
	if !st.On() {
		return nil
	}
	return st.HistoryBeginsAt().UnixMilli()
}

// GET /api/requests/{id}: one request, live or stored.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badParam(w, "id must be a request id (an integer), got %q", r.PathValue("id"))
		return
	}
	row, err := s.reqs.Get(id)
	if errors.Is(err, usage.ErrNotFound) {
		proxy.WriteError(w, http.StatusNotFound, "no request with id "+r.PathValue("id"), "invalid_request_error", "not found", nil)
		return
	}
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err.Error(), "server_error", "usage store", nil)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// GET /api/requests/summary?from&to&step=hour|day&profile: totals and a
// series for the period (default the last 24 hours by hour).
func (s *Server) handleRequestSummary(w http.ResponseWriter, r *http.Request) {
	from, ok := tsParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := tsParam(w, r, "to")
	if !ok {
		return
	}
	if to == 0 {
		to = s.opts.Now().UnixMilli()
	}
	if from == 0 {
		from = to - int64(24*time.Hour/time.Millisecond)
	}
	sum, err := s.reqs.Summarize(from, to, r.URL.Query().Get("step"), r.URL.Query().Get("profile"))
	if err != nil && sum.Step == "" {
		badParam(w, "%v", err)
		return
	}
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err.Error(), "server_error", "usage store", nil)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
