package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/startups"
)

// StartupNotIncluded is the sentence shown with every start-up figure.
const StartupNotIncluded = "On a console the time before the Linux kernel starts cannot be seen from inside Linux and is not included; none of these is a time from power-on."

func (s *Server) startupRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/nodes/{id}/expect-restart", s.nodeExpectRestart)
	m.HandleFunc("GET /api/nodes/{id}/startups", s.nodeStartups)
	m.HandleFunc("GET /api/startups", s.allStartups)
}

// nodeExpectRestart is POST /api/nodes/{id}/expect-restart
// {"kind": "soft"|"full", "comment": "..."}: the operator is about to
// restart a paused node that holds no launch.
func (s *Server) nodeExpectRestart(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	id := r.PathValue("id")
	obj, c, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if c == "" {
		c = audit.FromRequest(r).Comment
	}
	if c == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	kind, _ := obj["kind"].(string)
	if kind != pool.ExpectSoft && kind != pool.ExpectFull {
		apiErr(w, http.StatusBadRequest, "kind must be \"soft\" or \"full\"")
		return
	}
	if err := s.opts.Pool.ExpectRestart(id, kind, c); err != nil {
		var conflict *pool.ConflictError
		if errors.As(err, &conflict) {
			apiErr(w, http.StatusConflict, "%s", conflict.Msg)
			return
		}
		nodeErr(w, id, err)
		return
	}
	s.log.Info("node restart expected", "node", id, "kind", kind, "comment", c)
	s.opts.Pool.Record(id, "Node "+id+" is expected to restart ("+kind+"): "+c, map[string]any{"action": "expect_restart", "kind": kind, "comment": c})
	aid := s.auditNode(r, "expect_restart", id, c, nil, map[string]any{"kind": kind, "valid_s": pool.ExpectRestartFor.Seconds()})
	writeJSON(w, http.StatusOK, map[string]any{"node": id, "kind": kind, "valid_s": pool.ExpectRestartFor.Seconds(), "audit_id": aid})
}

// nodeStartups is GET /api/nodes/{id}/startups?limit=N: the node's start-up
// records, newest first.
func (s *Server) nodeStartups(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > startups.KeepPerNode {
			apiErr(w, http.StatusBadRequest, "limit must be a number from 1 to %d", startups.KeepPerNode)
			return
		}
		limit = n
	}
	recs, err := s.opts.Pool.Startups().List(r.PathValue("id"), limit)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "start-up records: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": r.PathValue("id"), "startups": withOrder(recs), "not_included": StartupNotIncluded})
}

// allStartups is GET /api/startups: per node and kind, the count, last,
// median and worst figure, and the newest record with its marks. Kinds are
// never mixed.
func (s *Server) allStartups(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	recs, err := s.opts.Pool.Startups().All()
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "start-up records: %v", err)
		return
	}
	groups := startups.Aggregates(recs)
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		m := map[string]any{"node_id": g.NodeID, "node_name": g.NodeName, "kind": g.Kind, "count": g.Count,
			"with_figure": g.WithFigure, "requested": g.Requested, "last_s": g.LastS, "median_s": g.MedianS, "worst_s": g.WorstS}
		if g.Last != nil {
			m["last"] = recordOut(*g.Last)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "not_included": StartupNotIncluded})
}

// recordOut is a record with its marks in time order and its condition.
func recordOut(r startups.Record) map[string]any {
	return map[string]any{"record": r, "marks_in_order": r.MarksInOrder(), "condition": r.Condition()}
}

func withOrder(recs []startups.Record) []map[string]any {
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, recordOut(r))
	}
	return out
}
