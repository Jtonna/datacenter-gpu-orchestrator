package api

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Growth per token, collected only (B8 F2). For a llama cycle, each node's
// growth is its cycle peak less its use at settle (settle_use_mib, one
// free_average_window after ready), and the tokens are the most one request
// of the cycle's load session held, read from the request log. The reading
// is growth × 1024 / tokens KiB per token, listed in peak_cycles rows as
// growth_readings. Nothing reads it for a plan.

// growthKeep is how many of a row's latest cycles are kept for readings.
const growthKeep = 16

// growthSettleDelay: a cycle's request rows are read this long after its
// unload event at the earliest, so the log's writer has stored them.
const growthSettleDelay = 10 * time.Second

// growthMaxKiB caps a reading (KiB per token); a capped one says so.
const growthMaxKiB = 1024.0

// Why a cycle gives no reading.
const (
	growthNotLlama   = "not computed: not a llama profile"
	growthBeforeKind = "not computed: recorded before the profile's kind was stored"
	growthParallel   = "not computed: parallel > 1"
	growthNoLog      = "tokens unknown: no request log"
	growthNoSession  = "tokens unknown: the cycle has no load session"
	growthTrimmed    = "tokens unknown: the request log no longer holds the cycle"
	growthNoCounts   = "tokens unknown: no request of the cycle has token counts"
	growthLogError   = "tokens unknown: the request log could not be read"
	growthFewTokens  = "not computed: the cycle held fewer tokens than a quarter of ctx × parallel"
	growthPending    = "pending: the request log is read 10 s after the unload"
	growthNoFigures  = "not computed: no node has both the settle use and the peak"
)

type growthNode struct {
	id, name     string
	settle, peak *int
}

// growthCycle is one cycle's inputs and, once read, its tokens.
type growthCycle struct {
	at               int64
	profile, session string
	ready            int64 // when the instance was ready (unix ms)
	kind             string
	ctx, parallel    int
	nodes            []growthNode
	read             bool
	tokens           int
	note             string
}

// addGrowth keeps the inputs of one cycle (the latest growthKeep of a row).
func (r *peakCycleRow) addGrowth(e history.Event, d *cyclePeakData) {
	g := &growthCycle{at: e.Ts, profile: e.Profile, session: e.SessionID, kind: d.Kind, ctx: d.Ctx, parallel: max(d.Parallel, 1),
		ready: e.Ts - int64(d.LoadedForS*1000)}
	for _, n := range d.CyclePeaks {
		g.nodes = append(g.nodes, growthNode{id: n.NodeID, name: n.Node, settle: n.SettleMiB, peak: n.PeakMiB})
	}
	r.growth = append(r.growth, g)
	if len(r.growth) > growthKeep {
		r.growth = r.growth[len(r.growth)-growthKeep:]
	}
}

// growthView lists a row's cycles, newest first: each with its readings per
// node, or why it has none. The request log is read once per cycle.
func (pc *peakCycles) growthView(r *peakCycleRow) []map[string]any {
	now := time.Now()
	if pc.now != nil {
		now = pc.now()
	}
	out := []map[string]any{}
	for i := len(r.growth) - 1; i >= 0; i-- {
		g := r.growth[i]
		e := map[string]any{"at": isoTime(time.UnixMilli(g.at)), "profile": g.profile, "ctx": g.ctx, "parallel": g.parallel}
		note := pc.growthTokens(g, now)
		if note == "" && g.tokens*4 < g.ctx*g.parallel {
			note = growthFewTokens
		}
		if g.read && g.tokens > 0 {
			e["tokens"] = g.tokens
		}
		if note != "" {
			e["not_computed"] = note
			out = append(out, e)
			continue
		}
		share := math.Round(float64(g.tokens)/float64(g.ctx*g.parallel)*1000) / 1000
		e["ctx_share"] = share
		nodes := []map[string]any{}
		for _, n := range g.nodes {
			if n.settle == nil || n.peak == nil || *n.peak < *n.settle {
				continue
			}
			growth := *n.peak - *n.settle
			kib := float64(growth) * 1024 / float64(g.tokens)
			capped := kib > growthMaxKiB
			kib = math.Round(min(kib, growthMaxKiB)*100) / 100
			nodes = append(nodes, map[string]any{"node": n.id, "node_name": n.name, "growth_mib": growth, "kib_per_token": kib,
				"tokens": g.tokens, "ctx_share": share, "capped": capped})
		}
		if len(nodes) == 0 {
			e["not_computed"] = growthNoFigures
		} else {
			e["nodes"] = nodes
		}
		out = append(out, e)
	}
	return out
}

// growthTokens reads the cycle's tokens from the request log once (when it
// is old enough), and says why there are none ("" when there are).
func (pc *peakCycles) growthTokens(g *growthCycle, now time.Time) string {
	switch {
	case g.kind == "": // an unload event from before profile_kind was stored
		return growthBeforeKind
	case g.kind != "llama":
		return growthNotLlama
	case g.parallel > 1:
		return growthParallel
	case g.read:
		return g.note
	case pc.tokens == nil:
		return growthNoLog
	case g.session == "":
		return growthNoSession
	case now.Sub(time.UnixMilli(g.at)) < growthSettleDelay:
		return growthPending
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := pc.tokens(ctx, g.profile, g.session, g.ready-3600_000, g.ready, g.at)
	if err != nil {
		return growthLogError // read again next time
	}
	g.read = true
	switch {
	case st.Trimmed:
		g.note = growthTrimmed
	case st.Counted == 0:
		g.note = growthNoCounts
	default:
		g.tokens = st.MaxHeld
	}
	return g.note
}

// setTokens gives the tally the request log to read tokens from.
func (pc *peakCycles) setTokens(f func(ctx context.Context, profile, session string, from, need, to int64) (usage.SessionTokens, error)) {
	pc.mu.Lock()
	defer supervise.Unlocked("peakcycles", pc.mu.Unlock)
	pc.tokens = f
}

// growthShape is what makes stored growth readings count for a plan: the
// model file (as written), ctx (the planned one), parallel, kv_quant ("" is
// f16), flash_attn and host ("" or "server": the scheduler's machine).
// Extra args, the profile's name and the node set do not count.
type growthShape struct {
	Model     string
	Ctx       int
	Parallel  int
	KVQuant   string
	FlashAttn bool
	Host      string
}

// growthHigh is the plan's growth_history: the highest reading of a shape
// in KiB per token, how many readings count, and how many capped ones were
// left out (a capped reading is a bound, not a measurement).
type growthHigh struct {
	KiBPerToken   float64 `json:"kib_per_token"`
	Readings      int     `json:"readings"`
	CappedSkipped int     `json:"capped_skipped"`
}

// matches says whether a stored shape key is sh.
func (sh growthShape) matches(key string) bool {
	var k []any
	if json.Unmarshal([]byte(key), &k) != nil || len(k) < 7 {
		return false
	}
	kind, _ := k[0].(string)
	model, _ := k[1].(string)
	ctx, _ := k[2].(float64)
	par, _ := k[3].(float64)
	kv, _ := k[4].(string)
	fa, _ := k[5].(bool)
	host := ""
	if len(k) > 7 {
		h, _ := k[7].(string)
		host = strings.TrimPrefix(h, "host=")
	}
	norm := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	want := sh.Host
	if want == "server" {
		want = ""
	}
	return kind == "llama" && model == sh.Model && int(ctx) == sh.Ctx && int(par) == max(sh.Parallel, 1) &&
		norm(kv, "f16") == norm(sh.KVQuant, "f16") && fa == sh.FlashAttn && host == want
}

// highestGrowth is the highest growth reading of shape sh over every row of
// that shape (each row keeps its latest growthKeep cycles; every node of a
// cycle is a reading). A nil tally has none.
func (pc *peakCycles) highestGrowth(sh growthShape) growthHigh {
	var out growthHigh
	if pc == nil {
		return out
	}
	pc.mu.Lock()
	defer supervise.Unlocked("peakcycles", pc.mu.Unlock)
	for _, r := range pc.rows {
		if !sh.matches(r.shape) {
			continue
		}
		for _, g := range pc.growthView(r) {
			nodes, _ := g["nodes"].([]map[string]any)
			for _, n := range nodes {
				if c, _ := n["capped"].(bool); c {
					out.CappedSkipped++
					continue
				}
				k, _ := n["kib_per_token"].(float64)
				out.Readings++
				out.KiBPerToken = max(out.KiBPerToken, k)
			}
		}
	}
	return out
}

// addGrowthFacts adds to a plan answer growth_history (the highest stored
// growth per token of the planned shape: a context fill's stop check uses
// it) and the fill_reserve_kib_per_token setting. The shape is the
// profile's, with a scoped load's own settings, at the plan's ctx.
func (s *Server) addGrowthFacts(v map[string]any, model string, sc scheduler.ScopedLoad) {
	cfg := s.conf()
	v["fill_reserve_kib_per_token"] = cfg.Defaults.FillReserveKiBPerToken
	p, _, ok := cfg.Resolve(model)
	if !ok {
		if sp, held := s.sched.Scoped(model); held {
			p, ok = sp, true
		}
	}
	if !ok {
		return
	}
	sh := growthShape{Model: p.Model, Ctx: p.Ctx, Parallel: p.Parallel, KVQuant: p.KVQuant, FlashAttn: p.FlashAttn, Host: p.Host}
	if sc.Parallel != nil {
		sh.Parallel = *sc.Parallel
	}
	if sc.KVQuant != nil {
		sh.KVQuant = *sc.KVQuant
	}
	if sc.FlashAttn != nil {
		sh.FlashAttn = *sc.FlashAttn
	}
	if sc.Host != nil {
		sh.Host = *sc.Host
	}
	switch c := v["ctx"].(type) { // the planned ctx (the trained context when ctx is above it)
	case int:
		if c > 0 {
			sh.Ctx = c
		}
	case float64:
		if c > 0 {
			sh.Ctx = int(c)
		}
	}
	v["growth_history"] = s.peaks.highestGrowth(sh)
}
