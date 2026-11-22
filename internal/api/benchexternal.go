package api

import (
	"io"
	"net/http"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
)

// External result sets: benchmark results produced outside the bench runner,
// stored as documents and shown on the Benchmarks page. Changes are audited
// as section "bench", target "external:<id>"; the audit entry carries the set
// without its rows and attachment texts (they can be 2 MiB).

func (s *Server) externalRoutes(m *http.ServeMux) {
	g := s.benchGuard
	m.HandleFunc("GET /api/bench/external", g(s.listExternal))
	m.HandleFunc("POST /api/bench/external", g(s.createExternal))
	m.HandleFunc("GET /api/bench/external/{id}", g(s.getExternal))
	m.HandleFunc("PUT /api/bench/external/{id}", g(s.putExternal))
	m.HandleFunc("DELETE /api/bench/external/{id}", g(s.deleteExternal))
}

func (s *Server) listExternal(w http.ResponseWriter, r *http.Request) {
	all, err := s.opts.BenchStore.ListExternal()
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"external": all})
}

func (s *Server) getExternal(w http.ResponseWriter, r *http.Request) {
	x, err := s.opts.BenchStore.GetExternal(r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

// parseExternal reads (at most bench.MaxExternalBytes), decodes and validates
// a set; wantID, when not empty, is the id the path names.
func (s *Server) parseExternal(w http.ResponseWriter, r *http.Request, wantID string) (*bench.ExternalSet, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bench.MaxExternalBytes))
	if err != nil {
		apiErr(w, http.StatusRequestEntityTooLarge, "the body is larger than %d bytes or unreadable: %v", bench.MaxExternalBytes, err)
		return nil, false
	}
	x, err := bench.ParseExternalJSON(body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "the body is not a valid external result set: %v", err)
		return nil, false
	}
	if wantID != "" {
		if x.ID == "" {
			x.ID = wantID
		} else if x.ID != wantID {
			apiErr(w, http.StatusBadRequest, "the id in the body (%q) differs from the id in the path (%q)", x.ID, wantID)
			return nil, false
		}
	}
	if err := x.Validate(); err != nil {
		s.storeErr(w, err)
		return nil, false
	}
	return x, true
}

func (s *Server) createExternal(w http.ResponseWriter, r *http.Request) {
	x, ok := s.parseExternal(w, r, "")
	if !ok {
		return
	}
	if _, err := s.opts.BenchStore.GetExternal(x.ID); err == nil {
		apiErr(w, http.StatusConflict, "external result set %q already exists: use PUT /api/bench/external/%s to replace it", x.ID, x.ID)
		return
	}
	if err := s.opts.BenchStore.SaveExternal(x, s.opts.Now().UnixMilli()); err != nil {
		s.storeErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/bench/external/"+x.ID)
	s.audit(r, "create", "external:"+x.ID, nil, externalAudit(x))
	writeJSON(w, http.StatusCreated, x)
}

func (s *Server) putExternal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.opts.BenchStore.GetExternal(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	x, ok := s.parseExternal(w, r, id)
	if !ok {
		return
	}
	if err := s.opts.BenchStore.SaveExternal(x, s.opts.Now().UnixMilli()); err != nil {
		s.storeErr(w, err)
		return
	}
	s.audit(r, "update", "external:"+id, externalAudit(old), externalAudit(x))
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) deleteExternal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.opts.BenchStore.GetExternal(id)
	if err != nil {
		s.storeErr(w, err)
		return
	}
	if err := s.opts.BenchStore.DeleteExternal(id); err != nil {
		s.storeErr(w, err)
		return
	}
	s.audit(r, "delete", "external:"+id, externalAudit(old), nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// externalAudit is what an audit entry keeps of a set: its summary as a plain map.
func externalAudit(x *bench.ExternalSet) map[string]any {
	return flatten(x.Summary(), bench.SourceExternal, x.CreatedAt, x.UpdatedAt)
}
