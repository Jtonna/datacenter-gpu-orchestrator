package api

import (
	"errors"
	"net/http"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Drain then unload (Zones-Confirm "Wait for the runs, then unload"):
//
//	POST /api/models/drain/{model}     admits nothing new, unloads once its runs finish
//	POST /api/models/undrain/{model}   takes it back: it admits again
//
// A request arriving meanwhile queues and loads the model again after it
// has stopped. The status shows drain and drain_eta_s on the instance;
// for zones: POST /api/zone-actions/drain.

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	eta, err := s.sched.Drain(r.Context(), model)
	switch {
	case err == nil:
		s.auditRuntime(r, audit.ActionUnload, model, map[string]any{"drain": true})
		out := map[string]any{"msg": "draining", "model": model, "next_load": NextLoadMayMove}
		if eta != nil {
			out["eta_s"] = *eta
		}
		writeJSON(w, http.StatusOK, out)
	case errors.Is(err, scheduler.ErrNotLoaded):
		writeJSON(w, http.StatusOK, map[string]any{"msg": "not loaded", "model": model})
	case errors.Is(err, scheduler.ErrBusy):
		proxy.WriteError(w, http.StatusConflict, err.Error(), "invalid_request_error", "busy", nil)
	default:
		s.writeErr(w, err)
	}
}

func (s *Server) handleUndrain(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	err := s.sched.Undrain(r.Context(), model)
	switch {
	case err == nil:
		s.auditRuntime(r, "undrain", model, map[string]any{"drain": false})
		writeJSON(w, http.StatusOK, map[string]any{"msg": "ok", "model": model})
	case errors.Is(err, scheduler.ErrNotLoaded), errors.Is(err, scheduler.ErrNotDraining):
		proxy.WriteError(w, http.StatusConflict, model+" is not draining", "invalid_request_error", "not_draining", nil)
	default:
		s.writeErr(w, err)
	}
}

// checkViewNodes accepts node ids or hostnames the scheduler's view holds
// (a load on chosen nodes); anything else is a 400.
func (s *Server) checkViewNodes(w http.ResponseWriter, r *http.Request, nodes []string) bool {
	st, err := s.sched.Status(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return false
	}
	known := map[string]bool{}
	for _, n := range st.View.Nodes {
		known[string(n.ID)], known[n.Hostname] = true, true
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n == "" || !known[n] {
			apiErr(w, http.StatusBadRequest, "%q is not a node in the fleet view", n)
			return false
		}
		if seen[n] {
			apiErr(w, http.StatusBadRequest, "%q is named twice", n)
			return false
		}
		seen[n] = true
	}
	return true
}
