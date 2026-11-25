package bench

// Results per model (the Overview's "Latest result for each model" and the
// Runs page's result views Load times, Generation speed, First token and
// Memory): read from the stored steps and the measurement record, nothing
// stored. A model file's figures come from its newest finished step, and per
// zone count from the newest finished step on that many zones; the smallest
// fit is the fewest zones on which a load of the file completed, in any run.
// A figure the step did not measure is left out (the page draws a dash).

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
}

// ModelResults is one model file's row.
type ModelResults struct {
	// FileGone: the model file is no longer in the models folder (the API
	// sets it from the catalog's scan; such rows are listed last).
	FileGone    bool         `json:"file_gone,omitempty"`
	Model       string       `json:"model"`
	SmallestFit int          `json:"smallest_fit,omitempty"` // 0: no load of the file completed
	Latest      *ResultCell  `json:"latest"`
	ByNodes     []ResultCell `json:"by_nodes"` // newest step per zone count, fewest zones first
}

// ResultsFilter narrows the steps read.
type ResultsFilter struct {
	Ctx int // only steps measured at this context (a recipe's ctx input)
}

// ResultsByModel reads the rows, model files in name order.
func (s *Store) ResultsByModel(f ResultsFilter) ([]ModelResults, error) {
	where, args := `s.model_file<>'' AND s.outcome<>'' AND s.outcome<>'skipped' AND s.state<>'running'`, []any{}
	if f.Ctx > 0 {
		where += ` AND EXISTS (SELECT 1 FROM measurements c WHERE c.step_id=s.id AND c.ctx=?)`
		args = append(args, f.Ctx)
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
	// the newest step per model, and per model and zone count
	byModel := map[string]*ModelResults{}
	picked := map[int64]bool{}
	seen := map[string]bool{}
	for i := range steps {
		r := &steps[i]
		m := byModel[r.model]
		if m == nil {
			m = &ModelResults{Model: r.model, ByNodes: []ResultCell{}}
			byModel[r.model] = m
			c := r.cell
			m.Latest = &c
			picked[r.id] = true
		}
		key := r.model + "\x00" + itoa(r.cell.NodeCount)
		if r.cell.NodeCount > 0 && !seen[key] {
			seen[key] = true
			m.ByNodes = append(m.ByNodes, r.cell)
		}
	}
	// the figures of the picked steps (Latest and every ByNodes cell)
	ids := map[int64]bool{}
	for id := range picked {
		ids[id] = true
	}
	stepOf := map[string]int64{} // run\x00idx -> step id
	for _, r := range steps {
		stepOf[r.cell.RunID+"\x00"+itoa(r.cell.Step)] = r.id
	}
	for _, m := range byModel {
		for _, c := range m.ByNodes {
			ids[stepOf[c.RunID+"\x00"+itoa(c.Step)]] = true
		}
	}
	figs, err := s.stepFigures(ids)
	if err != nil {
		return nil, err
	}
	fill := func(c *ResultCell) {
		if fg, ok := figs[stepOf[c.RunID+"\x00"+itoa(c.Step)]]; ok {
			sid := c
			fg.into(sid)
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
		fill(m.Latest)
		for i := range m.ByNodes {
			fill(&m.ByNodes[i])
		}
		sort.Slice(m.ByNodes, func(i, j int) bool { return m.ByNodes[i].NodeCount < m.ByNodes[j].NodeCount })
		m.SmallestFit = fits[m.Model]
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Model) < strings.ToLower(out[j].Model) })
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

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
