package api

import (
	"net/http"
)

// SystemInfo is what /api/system reports about the configuration.
type SystemInfo struct {
	Safety  map[string]any
	Storage map[string]string
}

func (s *Server) benchOn() bool { return s.opts.Bench != nil && s.opts.BenchStore != nil }

func (s *Server) benchRoutes(m *http.ServeMux) {}
