package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/filecache"
)

// A cold load (the load phase's cold option): right before the load the
// model file is dropped from the page cache of every node the load will use
// and of the scheduler's machine, through POST /api/cache/evict, so the load
// reads the file from disk or the share. The phase result keeps a cold block
// that says whether that was verified.

// CacheEvictRequest is the body of POST /api/cache/evict.
type CacheEvictRequest struct {
	Model string   `json:"model"`
	Nodes []string `json:"nodes"`           // agent node ids
	Local bool     `json:"local,omitempty"` // also the scheduler's own machine
}

// EvictFigures are the bytes of the file and how many were cached before and
// after the drop.
type EvictFigures struct {
	FileBytes    int64   `json:"file_bytes"`
	CachedBefore int64   `json:"cached_before"`
	CachedAfter  int64   `json:"cached_after"`
	DurationS    float64 `json:"duration_s,omitempty"`
}

// NodeEvict is one node's part of the answer of POST /api/cache/evict.
// Status is the agent's HTTP status (0: not asked); Reason and Error say why
// it did not drop the file.
type NodeEvict struct {
	ID      string `json:"id"`
	Node    string `json:"node,omitempty"`
	Capable bool   `json:"capable"`
	Status  int    `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Error   string `json:"error,omitempty"`
	*EvictFigures
}

// LocalEvict is the scheduler machine's part of the answer.
type LocalEvict struct {
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	*EvictFigures
}

// CacheEvictAnswer is the 200 answer of POST /api/cache/evict.
type CacheEvictAnswer struct {
	Model string      `json:"model"`
	File  string      `json:"file"`
	Local *LocalEvict `json:"local,omitempty"`
	Nodes []NodeEvict `json:"nodes"`
}

// CacheEvicter is the scheduler API's file cache eviction. HTTPAPI has it; a
// scripted API without it makes every cold load "not guaranteed".
type CacheEvicter interface {
	EvictCache(ctx context.Context, req CacheEvictRequest) (*CacheEvictAnswer, error)
}

// EvictCache implements CacheEvicter: POST /api/cache/evict.
func (h *HTTPAPI) EvictCache(ctx context.Context, req CacheEvictRequest) (*CacheEvictAnswer, error) {
	b, _ := json.Marshal(req)
	resp, err := h.do(ctx, "POST", "/api/cache/evict", b, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		var eb errBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Message != "" {
			return nil, fmt.Errorf("/api/cache/evict answered %d: %s", resp.StatusCode, eb.Error.Message)
		}
		return nil, fmt.Errorf("/api/cache/evict answered %d: %s", resp.StatusCode, Head(strings.TrimSpace(string(raw)), 300))
	}
	var ans CacheEvictAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, fmt.Errorf("/api/cache/evict: %w", err)
	}
	return &ans, nil
}

// ColdFigures are the file and cache figures of one machine in a cold block.
type ColdFigures struct {
	FileBytes    int64  `json:"file_bytes"`
	CachedBefore int64  `json:"cached_before"`
	CachedAfter  int64  `json:"cached_after"`
	Error        string `json:"error,omitempty"`
}

// ColdNode is one node of a cold block.
type ColdNode struct {
	Node string `json:"node"`
	ID   string `json:"id"`
	ColdFigures
}

// ColdResult is the cold block of a load phase: whether the load could be
// made cold. Verified is true only when the scheduler's machine and every
// node the load used ended with under 1 % of the model file cached; Reason
// says why not.
type ColdResult struct {
	Requested bool         `json:"requested"`
	Verified  bool         `json:"verified"`
	Reason    string       `json:"reason,omitempty"`
	Local     *ColdFigures `json:"local,omitempty"`
	Nodes     []ColdNode   `json:"nodes"`
}

// Default waits of a cold load (ManagerOptions.ColdRetry, ColdSettle).
const (
	DefaultColdRetry  = 20 * time.Second
	DefaultColdSettle = 2500 * time.Millisecond
	// DefaultUnloadGrace is ManagerOptions.UnloadGrace's default.
	DefaultUnloadGrace = 3 * time.Second
)

func (r *ColdResult) notGuaranteed(why string) {
	r.Verified = false
	if r.Reason == "" {
		r.Reason = "not guaranteed: " + why
		return
	}
	r.Reason += "; " + why
}

func pctText(f ColdFigures) string {
	return fmt.Sprintf("%.1f%%", filecache.CachedPct(f.FileBytes, f.CachedAfter))
}

// retryable: a node that is still stopping its launch, or evicted a moment ago.
func retryable(n NodeEvict) bool {
	return n.EvictFigures == nil && (n.Status == 409 && n.Reason == "busy" || n.Status == 429)
}

// makeCold drops the profile's model file from the page cache of the nodes
// the plan names and of the scheduler's machine, retrying nodes that are
// still busy (a worker launch being stopped) or rate limited, then reads the
// plan again so the stored reload figures match the emptied caches.
func (br *benchRun) makeCold() *ColdResult {
	x, b := br.x, br.b
	res := &ColdResult{Requested: true, Verified: true, Nodes: []ColdNode{}}
	ev, ok := x.m.o.API.(CacheEvicter)
	if !ok {
		res.notGuaranteed("this scheduler's API cannot drop files from the file cache")
		return res
	}
	x.setDetail(br.track, map[string]any{"text": "Dropping the model file from the file caches"})
	br.mu.Lock()
	localID := br.localID
	var nodes []string
	if br.plan != nil {
		for _, id := range br.plan.Nodes {
			if id != localID && !slices.Contains(nodes, id) {
				nodes = append(nodes, id)
			}
		}
	}
	br.mu.Unlock()
	// the step's own context: a step that has run out of time does not evict
	ctx, cancel := context.WithTimeout(WithRun(br.ctx, br.st.RunID), 2*time.Minute)
	defer cancel()
	ans, err := ev.EvictCache(ctx, CacheEvictRequest{Model: b.Profile, Nodes: nodes, Local: true})
	if err != nil {
		res.notGuaranteed("the eviction request failed: " + err.Error())
		return res
	}
	byID := map[string]NodeEvict{}
	for _, n := range ans.Nodes {
		byID[n.ID] = n
	}
	retry := x.m.o.ColdRetry
	deadline := time.Now().Add(retry)
	for {
		var again []string
		for _, id := range nodes {
			if n, ok := byID[id]; ok && retryable(n) {
				again = append(again, id)
			}
		}
		if len(again) == 0 || retry <= 0 || !time.Now().Before(deadline) || !x.sleep(x.m.o.IdlePoll) {
			break
		}
		more, err := ev.EvictCache(ctx, CacheEvictRequest{Model: b.Profile, Nodes: again})
		if err != nil {
			break
		}
		for _, n := range more.Nodes {
			byID[n.ID] = n
		}
	}

	x.a.mu.Lock()
	names := mergeHosts(nil, x.a.hosts)
	x.a.mu.Unlock()
	name := func(id, given string) string {
		if n := names[id]; n != "" {
			return n
		}
		if given != "" {
			return given
		}
		return id
	}
	switch l := ans.Local; {
	case l == nil:
		res.notGuaranteed("the scheduler's machine did not drop the file")
	case l.EvictFigures == nil:
		res.Local = &ColdFigures{Error: orWord(l.Error, l.Reason)}
		res.notGuaranteed("the scheduler's machine could not drop the file: " + res.Local.Error)
	default:
		res.Local = &ColdFigures{FileBytes: l.FileBytes, CachedBefore: l.CachedBefore, CachedAfter: l.CachedAfter}
		if !filecache.Cold(l.FileBytes, l.CachedAfter) {
			res.notGuaranteed("the scheduler's machine still holds " + pctText(*res.Local) + " of the file in its cache")
		}
	}
	for _, id := range nodes {
		n, ok := byID[id]
		cn := ColdNode{ID: id, Node: name(id, n.Node)}
		switch {
		case !ok:
			cn.Error = "not in the answer"
			res.notGuaranteed("node " + cn.Node + " was not asked")
		case !n.Capable && n.EvictFigures == nil:
			cn.Error = orWord(n.Error, "no file cache eviction")
			res.notGuaranteed("node " + cn.Node + " has an agent without file cache eviction")
		case n.EvictFigures == nil:
			cn.Error = strings.TrimSpace(orWord(n.Reason, "refused") + ": " + n.Error)
			res.notGuaranteed("node " + cn.Node + " refused the eviction (" + cn.Error + ")")
		default:
			cn.FileBytes, cn.CachedBefore, cn.CachedAfter = n.FileBytes, n.CachedBefore, n.CachedAfter
			if !filecache.Cold(n.FileBytes, n.CachedAfter) {
				res.notGuaranteed("node " + cn.Node + " still holds " + pctText(cn.ColdFigures) + " of the file in its cache")
			}
		}
		res.Nodes = append(res.Nodes, cn)
	}

	// The plan's reload figures come from the nodes' file cache figures,
	// which their next heartbeat brings: wait for it, then read the plan again.
	if settle := x.m.o.ColdSettle; settle > 0 {
		x.sleep(settle)
	}
	if plan, err := x.m.o.API.Plan(br.ctx, b.Profile); err == nil {
		br.st.Plan = plan.Raw
		br.mu.Lock()
		br.plan = plan
		br.mu.Unlock()
	}
	return res
}

// checkColdPlacement compares the nodes the load used with the nodes whose
// cache was dropped: a node the plan did not name was not made cold.
func (br *benchRun) checkColdPlacement(res *ColdResult) {
	if res == nil {
		return
	}
	done := map[string]bool{}
	for _, n := range res.Nodes {
		done[n.ID] = true
	}
	br.mu.Lock()
	used, localID := append([]InstanceNodeInfo(nil), br.nodes...), br.localID
	br.mu.Unlock()
	var missed []string
	for _, n := range used {
		if n.Local || n.ID == localID || done[n.ID] {
			continue
		}
		missed = append(missed, br.x.hostnames([]string{n.ID})[0])
	}
	if len(missed) == 0 {
		return
	}
	sort.Strings(missed)
	res.notGuaranteed("the load was placed on " + strings.Join(missed, ", ") + ", which the plan did not name, so its cache was not dropped")
}

func orWord(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
