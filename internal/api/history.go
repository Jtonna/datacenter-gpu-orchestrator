package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// The event history endpoints and the dashboard page (design 9.1). All of
// them are read-only.

func badParam(w http.ResponseWriter, format string, args ...any) {
	proxy.WriteError(w, http.StatusBadRequest, fmt.Sprintf(format, args...), "invalid_request_error", "bad request", nil)
}

// tsParam reads an optional unix-milliseconds parameter.
func tsParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		badParam(w, "%s must be a unix time in milliseconds, got %q", name, v)
		return 0, false
	}
	return n, true
}

// historyQuery reads the filters of GET /api/history.
func historyQuery(w http.ResponseWriter, r *http.Request) (history.Query, bool) {
	var q history.Query
	var ok bool
	if q.Since, ok = tsParam(w, r, "since"); !ok {
		return q, false
	}
	if q.Until, ok = tsParam(w, r, "until"); !ok {
		return q, false
	}
	v := r.URL.Query()
	for _, t := range strings.Split(v.Get("type"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			q.Types = append(q.Types, t)
		}
	}
	if sev := v.Get("severity"); sev != "" {
		switch sev {
		case history.Info, history.Warn, history.Error:
			q.MinSeverity = sev
		default:
			badParam(w, "severity must be info, warn or error, got %q", sev)
			return q, false
		}
	}
	q.Node, q.Profile, q.SessionID, q.Text = v.Get("node"), v.Get("profile"), v.Get("session_id"), v.Get("q")
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			badParam(w, "limit must be a positive integer, got %q", l)
			return q, false
		}
		q.Limit = n // the store caps it at history.MaxLimit
	}
	switch o := v.Get("order"); o {
	case "", "desc":
	case "asc":
		q.Asc = true
	default:
		badParam(w, "order must be asc or desc, got %q", o)
		return q, false
	}
	return q, true
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q, ok := historyQuery(w, r)
	if !ok {
		return
	}
	if !s.historyNodes(w, r, &q) {
		return
	}
	writeJSON(w, http.StatusOK, s.hist.Query(q))
}

// noNode matches no event: the node filter of an empty selection.
const noNode = "\x00none"

// historyNodes turns node= (one name or id, or a comma-separated list) and
// group= (a zone group's members) into the query's node filter: an event
// naming any of them, by id or by name, matches. An unknown group is 404;
// a group with no members matches nothing.
func (s *Server) historyNodes(w http.ResponseWriter, r *http.Request, q *history.Query) bool {
	var names []string
	for _, n := range strings.Split(q.Node, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	group := r.URL.Query().Get("group")
	if group != "" {
		g, ok := s.groups.Get(group)
		if !ok {
			apiErr(w, http.StatusNotFound, "no zone group %q", group)
			return false
		}
		names = append(names, g.Members...)
	}
	if len(names) == 0 {
		if group != "" {
			q.Node = noNode
		}
		return true
	}
	q.Node, q.NodeAliases = names[0], nil
	for _, n := range names {
		for _, a := range append([]string{n}, s.nodeAliases(r.Context(), n)...) {
			if a != q.Node && !slices.Contains(q.NodeAliases, a) {
				q.NodeAliases = append(q.NodeAliases, a)
			}
		}
	}
	return true
}

// nodeAliases gives the other names of the node called name (by id or by
// name): events name a node by its hostname, callers such as the UI's zone
// panel often hold its id. A name no node has gives none.
func (s *Server) nodeAliases(ctx context.Context, name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	add := func(id, host string) {
		if id == name || strings.EqualFold(host, name) {
			for _, a := range []string{id, host} {
				if a != "" && a != name && !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
		}
	}
	if st, err := s.sched.Status(ctx); err == nil {
		for _, n := range st.View.Nodes {
			add(string(n.ID), n.Hostname)
		}
	}
	if s.opts.Pool != nil {
		for _, f := range s.opts.Pool.Facts() { // also the lost and removed agents
			add(f.ID, f.Name)
		}
	}
	return out
}

func (s *Server) handleHistorySummary(w http.ResponseWriter, r *http.Request) {
	since, ok := tsParam(w, r, "since")
	if !ok {
		return
	}
	until, ok := tsParam(w, r, "until")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.hist.Summarize(since, until))
}

// handleHistoryExport streams the matching events as JSON Lines, oldest first.
func (s *Server) handleHistoryExport(w http.ResponseWriter, r *http.Request) {
	since, ok := tsParam(w, r, "since")
	if !ok {
		return
	}
	until, ok := tsParam(w, r, "until")
	if !ok {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="llm-scheduler-history-%d-%d.jsonl"`, since, until))
	w.WriteHeader(http.StatusOK)
	s.hist.Export(w, since, until)
}

// handleAbout is what the dashboard header shows.
func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	state := "off"
	if s.hist.On() {
		state = "on"
	}
	now := s.opts.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.opts.Version, "started_at": s.start.UnixMilli(), "now": now.UnixMilli(),
		"uptime_s": int64(now.Sub(s.start) / time.Second), "history": state, "oldest_ts": s.hist.OldestTs(),
	})
}
