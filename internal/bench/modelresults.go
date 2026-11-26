package bench

// Results per model (the Overview's "Latest result for each model" and the
// Runs page's result views Load times, Generation speed, First token and
// Memory): read from the stored steps and the measurement record, nothing
// stored. Each FIGURE (cold load, reload, tok/s, first token, memory, lowest
// available) comes from the newest finished step of the model file that
// measured it (reviewer's ruling, 26 Nov: a newer step that measured
// something else, such as a context fill, does not blank the column); per
// zone count the same among the steps on that many zones. Each figure says
// which step it came from (From), and Mixed says the cell's figures come from
// more than one run. The smallest fit is the fewest zones on which a load of
// the file completed, in any run. A figure no step measured is left out (the
// page draws a dash).

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ResultCell is the figures of one finished step.
type ResultCell struct {
	RunID     string `json:"run_id"`
	Step      int    `json:"step"`
	Benchmark string `json:"benchmark_id"`
	NodeCount int    `json:"node_count"`
	Ctx       int    `json:"ctx,omitempty"`
	At        int64  `json:"at"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	// ColdLoadS is the step's first load; Cold says whether it was asked
	// cold (null: the step's load had no cold option) and ColdVerified
	// whether the caches were seen dropped.
	ColdLoadS    *float64 `json:"cold_load_s,omitempty"`
	Cold         *bool    `json:"cold,omitempty"`
	ColdVerified *bool    `json:"cold_verified,omitempty"`
	// ReloadS is a later load of the same step (a recipe's Reload).
	ReloadS *float64 `json:"reload_s,omitempty"`
	// DecodeTPS and TTFTMs are means over the step's request phases
	// (warm-up left out); steady generation only when the step marks it.
	DecodeTPS *float64 `json:"decode_tps,omitempty"`
	TTFTMs    *float64 `json:"ttft_ms,omitempty"`
	// MemoryMiB is the device memory the model took (all zones);
	// LowestAvailableMiB the lowest available memory of one zone over the step.
	MemoryMiB          *float64 `json:"memory_mib,omitempty"`
	LowestAvailableMiB *float64 `json:"lowest_available_mib,omitempty"`
	// LowestNode is the machine of that figure: only the machines the step
	// used (its roles: the host and the workers), system memory only (a
	// GPU's own free memory is another figure).
	LowestNode string `json:"lowest_available_node,omitempty"`
	// From names, per figure key (cold_load_s, reload_s, decode_tps,
	// ttft_ms, memory_mib, lowest_available_mib), the step it was read from.
	From map[string]FigFrom `json:"from,omitempty"`
	// Mixed: the figures come from more than one run.
	Mixed bool `json:"mixed,omitempty"`
}

// FigFrom is the step one figure was read from.
type FigFrom struct {
	RunID     string `json:"run_id"`
	RunNumber int    `json:"run_number,omitempty"`
	Step      int    `json:"step"`
	At        int64  `json:"at"`
	NodeCount int    `json:"node_count"`
}

// ModelResults is one model file's row.
type ModelResults struct {
	// FileGone: the model file is no longer in the models folder (the API
	// sets it from the catalog's scan; such rows are listed last).
	FileGone bool `json:"file_gone,omitempty"`
	// TrashID: the file is in the trash (Deleted models) under this id.
	TrashID     string       `json:"trash_id,omitempty"`
	Model       string       `json:"model"`
	SmallestFit int          `json:"smallest_fit,omitempty"` // 0: no load of the file completed
	Latest      *ResultCell  `json:"latest"`
	ByNodes     []ResultCell `json:"by_nodes"` // newest step per zone count, fewest zones first
}

// ResultsFilter narrows the steps read.
type ResultsFilter struct {
	Ctx   int    // only steps measured at this context (a recipe's ctx input)
	RunID string // only the steps of this run (board Bench-Runs-Load "From: run N"); "": the latest of each
}

// ResultsByModel reads the rows, model files in name order.
func (s *Store) ResultsByModel(f ResultsFilter) ([]ModelResults, error) {
	where, args := `s.model_file<>'' AND s.outcome<>'' AND s.outcome<>'skipped' AND s.state<>'running'`, []any{}
	if f.Ctx > 0 {
		where += ` AND EXISTS (SELECT 1 FROM measurements c WHERE c.step_id=s.id AND c.ctx=?)`
		args = append(args, f.Ctx)
	}
	if f.RunID != "" {
		where += ` AND s.run_id=?`
		args = append(args, f.RunID)
	}
	rows, err := s.db.Query(`SELECT s.id, s.run_id, s.idx, s.benchmark_id, s.model_file, s.node_count, s.outcome, s.reason, s.error,
	    CASE WHEN s.started_at>0 THEN s.started_at ELSE s.finished_at END
	  FROM run_steps s WHERE `+where+` ORDER BY 10 DESC, s.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	type stepRow struct {
		id    int64
		model string
		cell  ResultCell
	}
	var steps []stepRow
	for rows.Next() {
		var r stepRow
		var errText string
		if err := rows.Scan(&r.id, &r.cell.RunID, &r.cell.Step, &r.cell.Benchmark, &r.model, &r.cell.NodeCount, &r.cell.Outcome, &r.cell.Reason, &errText, &r.cell.At); err != nil {
			rows.Close()
			return nil, err
		}
		if r.cell.Reason == "" {
			r.cell.Reason = errText
		}
		steps = append(steps, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// the figures of every step, then per model (and per model and zone
	// count) each figure from the newest step that measured it
	ids := map[int64]bool{}
	for _, r := range steps {
		ids[r.id] = true
	}
	figs, err := s.stepFigures(ids)
	if err != nil {
		return nil, err
	}
	nums, err := s.RunNumbers()
	if err != nil {
		return nil, err
	}
	byModel := map[string]*ModelResults{}
	zone := map[string]int{} // model\x00node_count -> index in ByNodes
	for i := range steps {
		r := &steps[i]
		own := r.cell
		if fg := figs[r.id]; fg != nil {
			fg.into(&own)
		}
		from := FigFrom{RunID: r.cell.RunID, RunNumber: nums[r.cell.RunID], Step: r.cell.Step, At: r.cell.At, NodeCount: r.cell.NodeCount}
		m := byModel[r.model]
		if m == nil {
			m = &ModelResults{Model: r.model, ByNodes: []ResultCell{}}
			byModel[r.model] = m
			c := r.cell // the newest step: its run, outcome and reason
			m.Latest = &c
		}
		m.Latest.take(own, from)
		if r.cell.NodeCount > 0 {
			key := r.model + "\x00" + itoa(r.cell.NodeCount)
			j, ok := zone[key]
			if !ok {
				j = len(m.ByNodes)
				zone[key] = j
				m.ByNodes = append(m.ByNodes, r.cell)
			}
			m.ByNodes[j].take(own, from)
		}
	}
	// the smallest fit: the fewest zones a load of the file completed on
	fits := map[string]int{}
	frows, err := s.db.Query(`SELECT s.model_file, MIN(s.node_count) FROM run_steps s
	  WHERE s.model_file<>'' AND s.node_count>0 AND EXISTS (SELECT 1 FROM measurements m WHERE m.step_id=s.id AND m.metric='load_s')
	  GROUP BY s.model_file`)
	if err != nil {
		return nil, err
	}
	for frows.Next() {
		var file string
		var n int
		if err := frows.Scan(&file, &n); err != nil {
			frows.Close()
			return nil, err
		}
		fits[file] = n
	}
	frows.Close()
	out := make([]ModelResults, 0, len(byModel))
	for _, m := range byModel {
		sort.Slice(m.ByNodes, func(i, j int) bool { return m.ByNodes[i].NodeCount < m.ByNodes[j].NodeCount })
		m.SmallestFit = fits[m.Model]
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Model) < strings.ToLower(out[j].Model) })
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// ResultContexts are the contexts the record holds figures at (the
// "Context" filter of the result views), smallest first.
func (s *Store) ResultContexts() ([]int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT ctx FROM measurements WHERE ctx>0 AND metric IN ('load_s','decode_tps','ttft_ms','memory_mib') ORDER BY ctx`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// take fills each figure c has not got yet from src, a step older than (or
// the same as) the ones already taken, and notes where it came from.
func (c *ResultCell) take(src ResultCell, from FigFrom) {
	got := false
	set := func(key string, dst **float64, v *float64) {
		if *dst == nil && v != nil {
			*dst = v
			if c.From == nil {
				c.From = map[string]FigFrom{}
			}
			c.From[key] = from
			got = true
		}
	}
	if c.ColdLoadS == nil && src.ColdLoadS != nil {
		c.Cold, c.ColdVerified = src.Cold, src.ColdVerified
	}
	set("cold_load_s", &c.ColdLoadS, src.ColdLoadS)
	set("reload_s", &c.ReloadS, src.ReloadS)
	set("decode_tps", &c.DecodeTPS, src.DecodeTPS)
	set("ttft_ms", &c.TTFTMs, src.TTFTMs)
	set("memory_mib", &c.MemoryMiB, src.MemoryMiB)
	if c.LowestAvailableMiB == nil && src.LowestAvailableMiB != nil {
		c.LowestNode = src.LowestNode
	}
	set("lowest_available_mib", &c.LowestAvailableMiB, src.LowestAvailableMiB)
	if got && c.Ctx == 0 {
		c.Ctx = src.Ctx
	}
	runs := map[string]bool{}
	for _, f := range c.From {
		runs[f.RunID] = true
	}
	c.Mixed = len(runs) > 1
}

// stepFig gathers one step's figures from its measurement rows.
type stepFig struct {
	loads       []loadFig
	tps, ttft   []float64
	steadyTPS   []float64
	mem, lowest *float64
	lowestNode  string
	used        map[string]bool // the step's machines (roles); nil: not recorded
	ctx         int
}

type loadFig struct {
	phase              int
	s                  float64
	cold, coldVerified *bool
}

func (f *stepFig) into(c *ResultCell) {
	sort.Slice(f.loads, func(i, j int) bool { return f.loads[i].phase < f.loads[j].phase })
	if len(f.loads) > 0 {
		v := f.loads[0].s
		c.ColdLoadS, c.Cold, c.ColdVerified = &v, f.loads[0].cold, f.loads[0].coldVerified
	}
	if len(f.loads) > 1 {
		v := f.loads[1].s
		c.ReloadS = &v
	}
	if len(f.steadyTPS) > 0 {
		c.DecodeTPS = mean(f.steadyTPS)
	} else if len(f.tps) > 0 {
		c.DecodeTPS = mean(f.tps)
	}
	if len(f.ttft) > 0 {
		c.TTFTMs = mean(f.ttft)
	}
	c.MemoryMiB, c.LowestAvailableMiB, c.LowestNode = f.mem, f.lowest, f.lowestNode
	if f.ctx > 0 {
		c.Ctx = f.ctx
	}
}

func mean(v []float64) *float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	m := t / float64(len(v))
	return &m
}

// requestPhases are the phase types whose speed and first token count (the
// warm-up is the first request after a load: left out).
var requestPhases = map[string]bool{"requests": true, "conversation": true, "hold": true, "concurrent_requests": true}

func (s *Store) stepFigures(ids map[int64]bool) (map[int64]*stepFig, error) {
	out := map[int64]*stepFig{}
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(ids))
	args := []any{}
	for id := range ids {
		list = append(list, "?")
		args = append(args, id)
	}
	// the machines each step used (its roles), for the lowest available memory
	used := map[int64]map[string]bool{}
	rrows, err := s.db.Query(`SELECT id, roles FROM run_steps WHERE roles IS NOT NULL AND id IN (`+strings.Join(list, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var id int64
		var doc string
		if err := rrows.Scan(&id, &doc); err != nil {
			rrows.Close()
			return nil, err
		}
		var roles []NodeRole
		if json.Unmarshal([]byte(doc), &roles) == nil && len(roles) > 0 {
			used[id] = map[string]bool{}
			for _, r := range roles {
				used[id][r.Node] = true
			}
		}
	}
	rrows.Close()
	rows, err := s.db.Query(`SELECT step_id, COALESCE(phase_idx, -1), phase_type, node, metric, value, conditions, ctx FROM measurements
	  WHERE metric IN ('load_s','decode_tps','ttft_ms','memory_mib','lowest_available_mib') AND step_id IN (`+strings.Join(list, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var phase, ctx int
		var ptype, node, metric string
		var v float64
		var cond *string
		if err := rows.Scan(&id, &phase, &ptype, &node, &metric, &v, &cond, &ctx); err != nil {
			return nil, err
		}
		f := out[id]
		if f == nil {
			f = &stepFig{used: used[id]}
			out[id] = f
		}
		if ctx > 0 {
			f.ctx = ctx
		}
		c := map[string]any{}
		if cond != nil {
			_ = json.Unmarshal([]byte(*cond), &c)
		}
		switch metric {
		case "load_s":
			if node != "" {
				continue
			}
			l := loadFig{phase: phase, s: v}
			if b, ok := c["cold"].(bool); ok {
				l.cold = &b
			}
			if b, ok := c["cold_verified"].(bool); ok {
				l.coldVerified = &b
			}
			f.loads = append(f.loads, l)
		case "decode_tps":
			if !requestPhases[ptype] {
				continue
			}
			f.tps = append(f.tps, v)
			if c["generation"] == "steady" {
				f.steadyTPS = append(f.steadyTPS, v)
			}
		case "ttft_ms":
			if requestPhases[ptype] {
				f.ttft = append(f.ttft, v)
			}
		case "memory_mib":
			if node == "" && (f.mem == nil || v > *f.mem) {
				x := v
				f.mem = &x
			}
		case "lowest_available_mib":
			// system memory of a machine the step used; a GPU's own free
			// memory (memory: gpu) and machines outside the step are left out
			if k, _ := c["memory"].(string); k != "" && k != "system" {
				continue
			}
			if f.used == nil || !f.used[node] {
				continue
			}
			if f.lowest == nil || v < *f.lowest {
				x := v
				f.lowest, f.lowestNode = &x, node
			}
		}
	}
	return out, rows.Err()
}
