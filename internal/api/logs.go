package api

import (
	"net/http"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/logrelay"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

// Central logging (CR-10, docs/cr-10-central-logging.md). Potluck is the
// pass-through in front of the log store:
//
//	GET  /api/pool/nodes/{id}/logs     the node's logs object
//	POST /api/pool/nodes/{id}/logs     a node's log lines (?source=agent|launch|journal[&launch_id=...])
//	GET  /api/logs/config              the clients' logs object
//	POST /api/logs                     a client's log lines
//
// The work is in internal/logrelay. These handlers take no lock of the pool
// or of the scheduler: the relay reads the pool's published snapshot and the
// configuration snapshot, and a log flood is stopped by the relay's cap, not
// by anything the scheduler waits for. With no relay, or while logging is
// off, every route answers 404.

func (s *Server) logRoutes(m *http.ServeMux) {
	off := func(w http.ResponseWriter, r *http.Request) bool {
		if s.opts.Logs != nil {
			return false
		}
		proxy.WriteError(w, http.StatusNotFound, "no such path: "+r.URL.Path, "invalid_request_error", "not found", nil)
		return true
	}
	m.HandleFunc("GET /api/pool/nodes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		if !off(w, r) {
			s.opts.Logs.ServeNodeObject(w, r, r.PathValue("id"))
		}
	})
	m.HandleFunc("POST /api/pool/nodes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		if !off(w, r) {
			s.opts.Logs.ServeNode(w, r, r.PathValue("id"))
		}
	})
	m.HandleFunc("GET "+agentproto.PathClientLogsConfig, func(w http.ResponseWriter, r *http.Request) {
		if !off(w, r) {
			s.opts.Logs.ServeClientObject(w, r)
		}
	})
	m.HandleFunc("POST "+agentproto.PathClientLogs, func(w http.ResponseWriter, r *http.Request) {
		if !off(w, r) {
			s.opts.Logs.ServeClient(w, r)
		}
	})
}

// LogLookups are the relay's two lookups, read from the pool's published
// snapshot. Neither takes the pool's lock. get may return nil (fleet
// membership: no node is known).
func LogLookups(get func() *pool.Pool) (node func(id string) (logrelay.Node, bool), launch func(launchID, nodeID string) (string, bool)) {
	node = func(id string) (logrelay.Node, bool) {
		p := get()
		if p == nil {
			return logrelay.Node{}, false
		}
		n, ok := p.LogSnapshot().Node(id)
		return logrelay.Node{StartID: n.StartID, Name: n.Name, Platform: n.Platform, Release: n.Release}, ok
	}
	launch = func(launchID, nodeID string) (string, bool) {
		p := get()
		if p == nil {
			return "", false
		}
		return p.LogSnapshot().Profile(launchID, nodeID)
	}
	return node, launch
}

// logFacts adds what the log route counted for each node to its facts.
func (s *Server) logFacts(facts []pool.NodeFacts) {
	rl := s.opts.Logs
	if rl == nil {
		return
	}
	on := rl.On()
	for i := range facts {
		c, sent := rl.NodeCounters(facts[i].ID)
		if !sent && !on && facts[i].Logs == nil {
			continue
		}
		if facts[i].Logs == nil {
			facts[i].Logs = &pool.NodeLogs{}
		}
		if c.Refused == nil {
			c.Refused = map[string]int64{}
		}
		facts[i].Logs.AcceptedBytes, facts[i].Logs.AcceptedRequests, facts[i].Logs.Refused = c.AcceptedBytes, c.AcceptedRequests, c.Refused
	}
}

// logStatus adds the relay's own figures to a status view, when logging is
// on or Potluck has a log store address.
func (s *Server) logStatus(v map[string]any) {
	if rl := s.opts.Logs; rl != nil && (rl.On() || rl.HasStore()) {
		v["logs"] = rl.Status()
	}
}
