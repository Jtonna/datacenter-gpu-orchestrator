package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// CacheEvictTimeout bounds one POST /cache/evict: counting the pages of a
// large file maps it in chunks and reads no data, so it is quick.
const CacheEvictTimeout = 60 * time.Second

// CacheEvictResult is one node's answer to a file cache eviction (agent
// contract 6.13). Answer is set on a 200; otherwise Status, Reason and
// Message say why not (Status 0: the agent could not be asked).
type CacheEvictResult struct {
	ID      string
	Name    string
	Capable bool // the agent advertised file_cache_evict
	Status  int
	Reason  string
	Message string
	Answer  *agentproto.CacheEvictAnswer
}

// EvictFileCache asks the agent of node id to drop file (relative to its
// model source) from its page cache. An unknown, lost or departed node, or an
// agent that does not advertise file_cache_evict, is not asked.
func (p *Pool) EvictFileCache(ctx context.Context, id, file string) CacheEvictResult {
	res := CacheEvictResult{ID: id}
	var a *agent
	var base, gone string
	func() {
		p.mu.Lock()
		defer supervise.Unlocked("pool", p.mu.Unlock)
		a = p.agents[id]
		if a != nil {
			res.Name, res.Capable = a.reg.Name, a.reg.HasCapability(agentproto.CapFileCacheEvict)
			base = p.agentBase(a)
			switch {
			case a.left:
				gone = "has left the pool"
			case a.lost:
				gone = "is lost (no heartbeat)"
			}
		}
	}()
	switch {
	case a == nil:
		res.Reason, res.Message = "unknown_node", "no agent is registered with this id"
		return res
	case gone != "":
		res.Reason, res.Message = "node_unavailable", "the node "+gone
		return res
	case !res.Capable:
		res.Reason, res.Message = agentproto.ReasonUnsupported, "the agent does not advertise "+agentproto.CapFileCacheEvict+" (an older release, not Linux, or no model files on the node)"
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, CacheEvictTimeout)
	defer cancel()
	body, _ := json.Marshal(agentproto.CacheEvictRequest{File: file})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+agentproto.PathCacheEvict, bytes.NewReader(body))
	if err != nil {
		res.Reason, res.Message = "request_failed", err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.o.HTTP.Do(req)
	if err != nil {
		res.Reason, res.Message = "request_failed", err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	res.Status = resp.StatusCode
	if resp.StatusCode == http.StatusOK {
		var ans agentproto.CacheEvictAnswer
		if err := json.Unmarshal(raw, &ans); err != nil {
			res.Reason, res.Message = "bad_answer", "the answer is not the contract's: "+err.Error()
			return res
		}
		res.Answer = &ans
		p.log.Info("model file dropped from a node's file cache", "node", res.Name, "id", id, "file", file,
			"file_bytes", ans.FileBytes, "cached_before", ans.CachedBefore, "cached_after", ans.CachedAfter)
		return res
	}
	var ref agentproto.LaunchRefusal
	if json.Unmarshal(raw, &ref) == nil && ref.Reason != "" {
		res.Reason, res.Message = ref.Reason, ref.Message
	} else {
		res.Reason, res.Message = "refused", fmt.Sprintf("answered %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	p.log.Info("a node refused the file cache eviction", "node", res.Name, "id", id, "file", file, "status", res.Status, "reason", res.Reason, "msg", res.Message)
	return res
}
