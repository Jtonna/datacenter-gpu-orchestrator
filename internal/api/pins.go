package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
)

// Pins (models slice 2): a pinned model is listed under "Pinned" on the
// models page. A pin changes nothing in scheduling. Pins are global (one
// owner), stored in model_pins (cfgstore, ledger model_migrations), keyed by
// the catalog key: the file name, or command:<profile id>. Both routes take
// the permission models, a comment (body {"comment": "..."} or X-Comment),
// and write an audit entry in section models.
//
//	PUT    /api/models/pin/{key...}   pin (pinning a pinned model changes nothing)
//	DELETE /api/models/pin/{key...}   unpin (404 when it was not pinned)

func (s *Server) handlePinPut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !s.pinStore(w) {
		return
	}
	comment, ok := nodeComment(w, r)
	if !ok {
		return
	}
	snap, err := s.catalogGet(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if _, found := snap.raw[key]; !found {
		apiErr(w, http.StatusNotFound, "no model %s: see GET /api/models", key)
		return
	}
	since, already, err := s.store.PinModel(key, time.Now().UnixMilli())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var id int64
	if !already {
		id = s.recordAuditWith(r, comment, audit.ActionCreate, audit.SectionModels, "pin:"+key, nil, map[string]any{"pinned_at": since})
		s.catalogDirty(false)
		s.announce(r, audit.SectionModels, key, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "pinned": true, "pinned_at": since, "changed": !already, "audit_id": id})
}

func (s *Server) handlePinDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !s.pinStore(w) {
		return
	}
	comment, ok := nodeComment(w, r)
	if !ok {
		return
	}
	pins, err := s.store.ModelPins(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	since, was := pins[key]
	if err := s.store.UnpinModel(key); errors.Is(err, cfgstore.ErrNotFound) || (err == nil && !was) {
		apiErr(w, http.StatusNotFound, "%s is not pinned", key)
		return
	} else if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	id := s.recordAuditWith(r, comment, audit.ActionDelete, audit.SectionModels, "pin:"+key, map[string]any{"pinned_at": since}, nil)
	s.catalogDirty(false)
	s.announce(r, audit.SectionModels, key, id)
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "pinned": false, "changed": true, "audit_id": id})
}

func (s *Server) pinStore(w http.ResponseWriter) bool {
	if s.store == nil {
		apiErr(w, http.StatusServiceUnavailable, "pins need the database, which is not available")
		return false
	}
	return true
}

// modelPins reads the pins for the catalog; nil without a database.
func (s *Server) modelPins(ctx context.Context) map[string]int64 {
	if s.store == nil {
		return nil
	}
	p, err := s.store.ModelPins(ctx)
	if err != nil {
		s.log.Warn("model pins not read; the catalog shows none", "err", err)
		return nil
	}
	return p
}
