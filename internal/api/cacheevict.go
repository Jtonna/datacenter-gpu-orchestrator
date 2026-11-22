package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/filecache"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// localCacheMu makes the scheduler machine's evictions one at a time.
var localCacheMu sync.Mutex

// handleCacheEvict is POST /api/cache/evict: drop a profile's model file
// from the page cache of the named agent nodes (POST /cache/evict on each,
// agent contract 6.13) and, with "local": true, of the scheduler's own
// machine, which serves the file to the nodes. Every machine's figures or
// refusal is in the answer; the call itself fails only on a bad body or an
// unknown profile. Used by the benchmark's cold load.
func (s *Server) handleCacheEvict(w http.ResponseWriter, r *http.Request) {
	var req bench.CacheEvictRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Model == "" {
		proxy.WriteError(w, http.StatusBadRequest, "body must be JSON with a \"model\" field, and optionally \"nodes\" and \"local\"", "invalid_request_error", "bad request", nil)
		return
	}
	if len(req.Nodes) > 64 {
		proxy.WriteError(w, http.StatusBadRequest, "at most 64 nodes", "invalid_request_error", "bad request", nil)
		return
	}
	cfg := s.conf()
	p, _, ok := cfg.Resolve(req.Model)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, "unknown model "+req.Model, "invalid_request_error", "model_not_found", nil)
		return
	}
	if p.Model == "" {
		proxy.WriteError(w, http.StatusBadRequest, "profile "+p.Name+" has no model file", "invalid_request_error", "bad request", nil)
		return
	}
	ans := bench.CacheEvictAnswer{Model: p.Name, File: p.Model, Nodes: make([]bench.NodeEvict, len(req.Nodes))}
	var wg sync.WaitGroup
	for i, id := range req.Nodes {
		wg.Add(1)
		go supervise.Once("api.cache_evict", func() {
			n := bench.NodeEvict{ID: id}
			answered := false
			defer func() { // a failure is this node's error, not the request's
				if !answered {
					n.Reason, n.Error = filecache.ReasonFailed, "internal error"
					ans.Nodes[i] = n
				}
				wg.Done()
			}()
			if s.opts.Pool == nil {
				n.Reason, n.Error = "no_agents", "the scheduler has no agent membership"
				ans.Nodes[i], answered = n, true
				return
			}
			res := s.opts.Pool.EvictFileCache(r.Context(), id, p.Model)
			n.Node, n.Capable, n.Status, n.Reason, n.Error = res.Name, res.Capable, res.Status, res.Reason, res.Message
			if a := res.Answer; a != nil {
				n.EvictFigures = &bench.EvictFigures{FileBytes: a.FileBytes, CachedBefore: a.CachedBefore, CachedAfter: a.CachedAfter, DurationS: a.DurationS}
			}
			ans.Nodes[i], answered = n, true
		}, nil)
	}
	if req.Local {
		ans.Local = s.evictLocal(cfg.ModelsDir, p.Model)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, ans)
}

// evictLocal drops the file from the scheduler machine's own page cache.
func (s *Server) evictLocal(root, file string) *bench.LocalEvict {
	sys := s.opts.fileCache
	if sys == nil {
		sys = filecache.Default()
	}
	localCacheMu.Lock()
	defer localCacheMu.Unlock()
	e := &filecache.Evictor{Root: root, Sys: sys, MinGap: -1, Logger: s.log}
	res, err := e.Evict(file)
	if err != nil {
		var fe *filecache.Error
		if errors.As(err, &fe) {
			return &bench.LocalEvict{Reason: fe.Reason, Error: fe.Message}
		}
		return &bench.LocalEvict{Reason: filecache.ReasonFailed, Error: err.Error()}
	}
	return &bench.LocalEvict{EvictFigures: &bench.EvictFigures{FileBytes: res.FileBytes, CachedBefore: res.CachedBefore, CachedAfter: res.CachedAfter, DurationS: res.DurationS}}
}
