package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
)

// Agent membership (agent contract). Two groups of endpoints:
//
// potluck-agent to Potluck (contract 4 and 5), on the contract's paths:
//
//	POST /api/pool/register
//	POST /api/pool/nodes/{id}/heartbeat
//	POST /api/pool/nodes/{id}/deregister
//	GET  /api/pool/release
//	GET  /api/pool/programs/{release}/{platform}/{file}
//
// They have their own /api/pool prefix; /api/agents belongs to the AI-agent
// identity routes (agents.go).
//
// Operator actions on a node (contract 7.2 and 10), each with a required
// comment (body "comment" or the X-Comment header) and an audit entry:
//
//	POST   /api/nodes/{id}/pause
//	POST   /api/nodes/{id}/unpause
//	POST   /api/nodes/{id}/force-unload
//	DELETE /api/nodes/{id}
//	POST   /api/nodes/{id}/readmit

// SectionNodes is the audit section of the node actions.
const SectionNodes = "nodes"

func (s *Server) nodeRoutes(m *http.ServeMux) {
	m.HandleFunc("POST "+agentproto.PathRegister, s.agentRegister)
	m.HandleFunc("POST /api/pool/nodes/{id}/heartbeat", s.agentHeartbeat)
	m.HandleFunc("POST /api/pool/nodes/{id}/deregister", s.agentDeregister)
	m.HandleFunc("GET "+agentproto.PathRelease, s.agentRelease)
	m.HandleFunc("GET /api/pool/programs/{release}/{platform}/{file}", s.agentProgram)

	m.HandleFunc("POST /api/nodes/{id}/pause", func(w http.ResponseWriter, r *http.Request) { s.nodePause(w, r, true) })
	m.HandleFunc("POST /api/nodes/{id}/unpause", func(w http.ResponseWriter, r *http.Request) { s.nodePause(w, r, false) })
	m.HandleFunc("POST /api/nodes/{id}/force-unload", s.nodeForceUnload)
	m.HandleFunc("DELETE /api/nodes/{id}", s.nodeRemove)
	m.HandleFunc("POST /api/nodes/{id}/readmit", s.nodeReadmit)
}

// agentsOn answers 503 when membership is not agents.
func (s *Server) agentsOn(w http.ResponseWriter) bool {
	if s.opts.Pool == nil {
		apiErr(w, http.StatusServiceUnavailable, "this scheduler takes its nodes from the fleet feed (defaults.membership is fleet); agents are not used")
		return false
	}
	return true
}

func (s *Server) agentRegister(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	var req agentproto.RegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, "registration: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Pool.Register(req))
}

func (s *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		apiErr(w, http.StatusBadRequest, "heartbeat: %v", err)
		return
	}
	res := s.opts.Pool.Heartbeat(r.PathValue("id"), b)
	if res.Status != http.StatusOK {
		apiErr(w, res.Status, "%s", res.Err)
		return
	}
	writeJSON(w, http.StatusOK, res.Answer)
}

func (s *Server) agentDeregister(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	if !s.opts.Pool.Deregister(r.Context(), r.PathValue("id")) {
		apiErr(w, http.StatusNotFound, "no registered agent %s", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"left": true})
}

func (s *Server) agentRelease(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	rel := s.opts.Pool.Programs()
	if rel == nil {
		apiErr(w, http.StatusServiceUnavailable, "Potluck serves no verified program release: %s", s.opts.Pool.ReleaseStatus()["error"])
		return
	}
	writeJSON(w, http.StatusOK, agentproto.ReleaseInfo{Release: rel.Release, Agents: rel.Agents()})
}

// agentProgram serves one verified file (5.3). Only files named in a
// folder's SHA256SUMS are served; the file is checked against its size again
// so a file changed on disk since the start is not handed out.
func (s *Server) agentProgram(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	rel := s.opts.Pool.Programs()
	f, ok := rel.File(r.PathValue("release"), r.PathValue("platform"), r.PathValue("file"))
	if !ok {
		apiErr(w, http.StatusNotFound, "no verified file %s/%s/%s", r.PathValue("release"), r.PathValue("platform"), r.PathValue("file"))
		return
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer fh.Close()
	if st, err := fh.Stat(); err != nil || st.Size() != f.Bytes {
		apiErr(w, http.StatusConflict, "%s changed on disk since it was verified; restart Potluck to verify it again", f.Name)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Checksum-Sha256", f.SHA256)
	http.ServeContent(w, r, f.Name, time.Time{}, fh)
}

// nodeComment reads the required comment of a node action.
func nodeComment(w http.ResponseWriter, r *http.Request) (string, bool) {
	_, c, ok := bodyObject(w, r)
	if !ok {
		return "", false
	}
	if c == "" {
		c = audit.FromRequest(r).Comment
	}
	if c == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return "", false
	}
	return c, true
}

func (s *Server) auditNode(r *http.Request, action, id, comment string, before, after any) int64 {
	if s.opts.Audit == nil {
		return 0
	}
	e := audit.Entry{Action: action, Section: SectionNodes, Target: "node:" + id, Before: raw(before), After: raw(after),
		TS: time.Now().UnixMilli(), Comment: comment}
	audit.FromRequest(r).Apply(&e)
	id64, err := s.opts.Audit.Record(r.Context(), e)
	if err != nil {
		s.log.Warn("audit entry not recorded (the action itself was taken)", "action", action, "node", id, "err", err)
		return 0
	}
	s.announce(r, SectionNodes, "node:"+id, id64)
	return id64
}

func nodeErr(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, pool.ErrUnknownNode) {
		apiErr(w, http.StatusNotFound, "no agent node %s", id)
		return
	}
	apiErr(w, http.StatusConflict, "%v", err)
}

func (s *Server) nodePause(w http.ResponseWriter, r *http.Request, pause bool) {
	if !s.agentsOn(w) {
		return
	}
	id := r.PathValue("id")
	c, ok := nodeComment(w, r)
	if !ok {
		return
	}
	prev, err := s.opts.Pool.SetPaused(id, pause)
	if err != nil {
		nodeErr(w, id, err)
		return
	}
	action := "pause"
	if !pause {
		action = "unpause"
	}
	s.log.Info("node "+action+"d", "node", id, "comment", c)
	s.opts.Pool.Record(id, "Node "+id+" was "+action+"d: "+c, map[string]any{"action": action, "comment": c})
	aid := s.auditNode(r, action, id, c, map[string]any{"paused": prev}, map[string]any{"paused": pause})
	writeJSON(w, http.StatusOK, map[string]any{"node": id, "paused": pause, "audit_id": aid})
}

func (s *Server) nodeForceUnload(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	id := r.PathValue("id")
	c, ok := nodeComment(w, r)
	if !ok {
		return
	}
	prof, err := s.sched.ForceUnloadNode(r.Context(), core.NodeID(id))
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	s.log.Warn("node force unloaded", "node", id, "profile", prof, "comment", c)
	s.opts.Pool.Record(id, "Node "+id+" was force unloaded: "+c, map[string]any{"action": "force_unload", "profile": prof, "comment": c})
	aid := s.auditNode(r, "force_unload", id, c, nil, map[string]any{"stopped_profile": prof})
	writeJSON(w, http.StatusOK, map[string]any{"node": id, "stopped_profile": prof, "audit_id": aid})
}

func (s *Server) nodeRemove(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	id := r.PathValue("id")
	c, ok := nodeComment(w, r)
	if !ok {
		return
	}
	if err := s.opts.Pool.Remove(r.Context(), id); err != nil {
		nodeErr(w, id, err)
		return
	}
	s.log.Warn("node removed", "node", id, "comment", c)
	s.opts.Pool.Record(id, "Node "+id+" was removed from the pool: "+c, map[string]any{"action": "remove", "comment": c})
	aid := s.auditNode(r, audit.ActionDelete, id, c, nil, map[string]any{"removed": true})
	writeJSON(w, http.StatusOK, map[string]any{"node": id, "removed": true, "audit_id": aid})
}

func (s *Server) nodeReadmit(w http.ResponseWriter, r *http.Request) {
	if !s.agentsOn(w) {
		return
	}
	id := r.PathValue("id")
	c, ok := nodeComment(w, r)
	if !ok {
		return
	}
	if err := s.opts.Pool.Readmit(id); err != nil {
		apiErr(w, http.StatusNotFound, "node %s is not removed", id)
		return
	}
	s.log.Info("node re-admitted", "node", id, "comment", c)
	s.opts.Pool.Record(id, "Node "+id+" was re-admitted: "+c, map[string]any{"action": "readmit", "comment": c})
	aid := s.auditNode(r, "readmit", id, c, map[string]any{"removed": true}, map[string]any{"removed": false})
	writeJSON(w, http.StatusOK, map[string]any{"node": id, "removed": false, "audit_id": aid})
}

// agentStatus adds the agent facts to a status view (agents membership only):
// each node of view.nodes gets "agent", and "agents" lists every agent node
// Potluck knows (lost, left and removed ones too).
func (s *Server) agentStatus(v map[string]any) {
	if s.opts.Pool == nil {
		return
	}
	facts := s.opts.Pool.Facts()
	s.logFacts(facts) // CR-10: what the log route counted per node
	byID := map[string]pool.NodeFacts{}
	for _, f := range facts {
		byID[f.ID] = f
	}
	if view, ok := v["view"].(map[string]any); ok {
		if nodes, ok := view["nodes"].([]any); ok {
			for _, n := range nodes {
				nm, ok := n.(map[string]any)
				if !ok {
					continue
				}
				if f, ok := byID[toString(nm["id"])]; ok {
					nm["agent"] = f
				}
			}
		}
	}
	v["agents"] = facts
	v["membership"] = "agents"
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}
