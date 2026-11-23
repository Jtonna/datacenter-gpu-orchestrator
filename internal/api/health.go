package api

// The judged health summary (08 A7): GET /api/health, the MCP tool
// get_system_health and the /ws topic "health" carry one document with the
// rows of the System health popup, each judged here (ok, attention, problem
// or unknown) with the sentence the popup shows, plus the warnings of the
// last two hours in short sentences and the heart's count and colour. The
// rules live in this file only; the browser shows what it is given.
//
// It answers while the core loop is stalled: the status is asked for with a
// one-second timeout and the last good one is used when the loop does not
// answer; loop health, disk figures and task counters are read from atomics
// and caches.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/availability"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/diskstat"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// Row states, from best to worst; unknown is outside the order (a row
// Potluck cannot judge: not counted, never the heart's colour unless every
// row is unknown).
const (
	HealthOK        = "ok"
	HealthAttention = "attention"
	HealthProblem   = "problem"
	HealthUnknown   = "unknown"
)

// TopicHealth carries the health summary on /ws: the whole document when a
// client subscribes, then again whenever it changes (checked every two
// seconds while a client wants it). Asked for by name.
const TopicHealth = "health"

// Thresholds of the rows (08 section 2.1, 4 and 5.7).
const (
	diskAttentionPct   = 90.0
	diskProblemPct     = 97.0
	diskAttentionFree  = 10_000_000_000 // bytes
	diskProblemFree    = 2_000_000_000
	budgetAttentionPct = 90.0
	loopBusyProblem    = 5 * time.Second
	recentWindow       = 2 * time.Hour
	recentMax          = 20
	runsWindow         = time.Hour
	runsProblemMin     = 5 // failures in the hour, at least, for problem
	healthStatusWait   = time.Second
)

// HealthRow is one row of the popup.
type HealthRow struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	State string `json:"state"`
	// Text is the sentence the row shows. Count is the number of issues the
	// row reports, 0 when it is ok or unknown (zones down, runs failed,
	// scheduler issues, storage folders or disks in trouble, requests waiting
	// past the threshold); the row's other figures are in Facts.
	Text  string         `json:"text"`
	Count int64          `json:"count"`
	Link  string         `json:"link"`
	Facts map[string]any `json:"facts"`

	// sigText is Text without the parts that move by themselves (the
	// Scheduler row's uptime), for the push's comparison; empty: Text.
	sigText string
}

// HealthEvent is one line of "Last 2 hours".
type HealthEvent struct {
	At       string `json:"at"`
	Seq      uint64 `json:"seq"`
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
	// Node is the zone the sentence starts with (shown in mono), when it
	// is about one; Until is when a set-aside ends.
	Node  string `json:"node,omitempty"`
	Until string `json:"until,omitempty"`
}

// HealthSummary is GET /api/health.
type HealthSummary struct {
	State string `json:"state"`
	// Color is the heart's dot: green, amber, red, or grey (unknown).
	Color string `json:"color"`
	// Issues counts the rows in attention or problem (the heart's label:
	// "System health, 2 issues").
	Issues     int           `json:"issues"`
	Sentence   string        `json:"sentence"`
	UpdatedAt  string        `json:"updated_at"`
	Rows       []HealthRow   `json:"rows"`
	Recent     []HealthEvent `json:"recent"`
	RecentLink string        `json:"recent_link"`
	AlertsLink string        `json:"alerts_link"`
}

// healthState is the server's part: the last good status and the push.
type healthState struct {
	last   atomic.Pointer[scheduler.Status]
	lastAt atomic.Int64 // unix ms of last
	runs   runsCache
	every  time.Duration
	// wake starts an evaluation now (a client subscribed); evals counts
	// the evaluations of the push.
	wake  chan struct{}
	evals atomic.Int64
	// mu guards what one evaluation of the push owns: the signature of
	// the last frame sent and the clients waiting for their first frame.
	mu      sync.Mutex
	lastSig string
	pending []*wsClient
}

type runsCache struct {
	mu   sync.Mutex
	at   time.Time
	in   runsIn
	have bool
}

// ---- gathering ------------------------------------------------------------

// startHealth starts the disk reader and the health topic's push.
func (s *Server) startHealth() {
	sys := s.opts.System
	s.disks = diskstat.New(diskstat.Options{Folders: []diskstat.Folder{
		{Name: "data", Path: sys.DataDir}, {Name: "history", Path: sys.HistoryDir},
		{Name: "logs", Path: sys.ChildLogDir}, {Name: "models", Path: sys.ModelsDir}},
		Stat: s.opts.diskStat, Timeout: s.opts.diskTimeout, Every: s.opts.diskEvery})
	s.bgWG.Add(1)
	s.disks.Start(s.bg, s.bgWG.Done)
	s.health.every = orDur(s.opts.healthEvery, 2*time.Second)
	s.health.wake = make(chan struct{}, 1)
	s.bgWG.Add(1)
	// While no client wants the topic the task waits for a subscribe and
	// does nothing else (no timer, no evaluation); while one does, it
	// evaluates every 2 s.
	supervise.Go(s.bg, supervise.Spec{Name: "api.health", Policy: supervise.Restart, Done: s.bgWG.Done,
		Rebuild: func(any) { s.health.reset() }}, func(ctx context.Context) {
		for {
			if s.ws.wantsAny(TopicHealth) {
				t := time.NewTimer(s.health.every)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				case <-s.health.wake:
					t.Stop()
				}
			} else {
				select {
				case <-ctx.Done():
					return
				case <-s.health.wake:
				}
			}
			s.healthTick(ctx)
		}
	})
}

func (h *healthState) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSig, h.pending = "", nil
}

// healthSubscribe is a client subscribing to the topic: it gets the summary
// from the next evaluation, which starts now.
func (s *Server) healthSubscribe(c *wsClient) {
	func() {
		s.health.mu.Lock()
		defer s.health.mu.Unlock()
		s.health.pending = append(s.health.pending, c)
	}()
	select {
	case s.health.wake <- struct{}{}:
	default:
	}
}

// healthTick is one evaluation of the push: every subscriber gets a frame
// when what a person reads changed (healthSig), a client that just
// subscribed gets one in any case, nobody else does.
func (s *Server) healthTick(ctx context.Context) {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	pending := s.health.pending
	s.health.pending = nil
	if len(pending) == 0 && !s.ws.wantsAny(TopicHealth) {
		return
	}
	s.health.evals.Add(1)
	sum := s.healthSummary(ctx)
	frame := wsFrame(TopicHealth, s.opts.Now(), sum)
	if sig := healthSig(sum); sig != s.health.lastSig {
		s.health.lastSig = sig
		s.ws.broadcast(TopicHealth, frame)
		return
	}
	for _, c := range pending {
		if c.wants(TopicHealth) {
			s.ws.enqueue(c, frame)
		}
	}
}

// healthSig is what a person reads in the summary: the heart, each row's
// state, sentence, count and link, and the recent lines. Facts that move by
// themselves (uptime_s, loop steps, checked_at, free bytes, updated_at) and
// the uptime at the end of the Scheduler sentence are left out; they reach a
// client with the next frame that is sent.
func healthSig(sum HealthSummary) string {
	type row struct {
		ID, State, Text, Link string
		Count                 int64
	}
	v := struct {
		State, Color, Sentence string
		Issues                 int
		Rows                   []row
		Recent                 []HealthEvent
	}{State: sum.State, Color: sum.Color, Sentence: sum.Sentence, Issues: sum.Issues, Recent: sum.Recent}
	for _, r := range sum.Rows {
		text := r.Text
		if r.sigText != "" {
			text = r.sigText
		}
		v.Rows = append(v.Rows, row{r.ID, r.State, text, r.Link, r.Count})
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// healthStatus is the scheduler's status, or the last good one (fresh
// false, nil before the first) when the loop does not answer in a second.
func (s *Server) healthStatus(ctx context.Context) (st *scheduler.Status, at time.Time, fresh bool) {
	if s.sched != nil {
		c, cancel := context.WithTimeout(ctx, healthStatusWait)
		v, err := s.sched.Status(c)
		cancel()
		if err == nil {
			now := s.opts.Now()
			s.health.last.Store(&v)
			s.health.lastAt.Store(now.UnixMilli())
			return &v, now, true
		}
	}
	return s.health.last.Load(), time.UnixMilli(s.health.lastAt.Load()), false
}

func (s *Server) handleHealthSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.healthSummary(r.Context()))
}

// healthSummary gathers the inputs and judges every row.
func (s *Server) healthSummary(ctx context.Context) HealthSummary {
	now := s.opts.Now()
	st, stAt, fresh := s.healthStatus(ctx)
	var facts []pool.NodeFacts
	if s.opts.Pool != nil {
		facts = s.opts.Pool.Facts()
	}
	var loop scheduler.LoopStats
	queueTimeout := time.Duration(0)
	if s.sched != nil {
		loop = s.sched.LoopStats()
		if c := s.sched.Config(); c != nil {
			queueTimeout = c.Defaults.QueueTimeout
		}
	}
	budget := int64(0)
	if c := s.conf(); c != nil {
		budget = int64(c.ModelsDiskBudgetGB) * 1_000_000_000
	}
	sch := schedIn{version: s.opts.Version, uptime: now.Sub(s.start), loop: loop, tasks: s.taskStats(), now: now, statusFresh: fresh}
	if s.sched != nil {
		sch.readPanics = s.sched.ReadPanics()
	}
	if sv := s.opts.Config; sv != nil {
		sch.writesNeedForce = len(sv.UnknownFields()) > 0
	}
	keys, keysKnown := s.agents.activeKeys(ctx)
	rows := []HealthRow{
		judgeZones(st, facts, fresh, stAt),
		judgeRuns(s.runsLastHour(now)),
		judgeScheduler(sch),
		judgeMCP(keys, keysKnown),
		judgeStorage(s.disks.Folders(), budget),
		judgeQueue(st, queueTimeout),
		judgeAlerts(),
	}
	return summarize(rows, s.recentEvents(now), now)
}

func (s *Server) taskStats() []supervise.TaskStats {
	if s.opts.taskStats != nil {
		return s.opts.taskStats()
	}
	return supervise.Stats()
}

// runsIn is what the Runs row judges.
type runsIn struct {
	logOn          bool
	failed, total  int64 // client requests in the hour: failed or refused, and all finished
	benchFailed    int64
	capped         bool // more failures than one page reads
	benchFailedIDs []string
}

// runsLastHour counts the failed and refused requests of the last hour in
// the request log, and the benchmark runs that failed (history), cached for
// ten seconds.
func (s *Server) runsLastHour(now time.Time) runsIn {
	c := &s.health.runs
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && now.Sub(c.at) < 10*time.Second && !now.Before(c.at) {
		return c.in
	}
	in := runsIn{logOn: s.reqs.Store().On()}
	from := now.Add(-runsWindow).UnixMilli()
	if in.logOn {
		if p, err := s.reqs.List(usage.Filter{Status: []string{usage.StatusFailed, usage.StatusRefused}, From: from, Limit: 500}); err == nil {
			in.failed, in.capped = int64(len(p.Requests)), p.Next != 0
		} else {
			in.logOn = false
		}
	}
	if in.logOn {
		if p, err := s.reqs.List(usage.Filter{Status: []string{usage.StatusDone, usage.StatusFailed, usage.StatusRefused, usage.StatusCancelled}, From: from, Limit: 500}); err == nil {
			in.total = int64(len(p.Requests))
		}
	}
	for _, e := range s.hist.Query(history.Query{Since: from, Types: []string{history.TBenchRunFinished}, Limit: history.MaxLimit}).Events {
		if st, _ := e.Data["state"].(string); st == "failed" {
			in.benchFailed++
			if id, _ := e.Data["bench_run"].(string); id != "" {
				in.benchFailedIDs = append(in.benchFailedIDs, id)
			}
		}
	}
	c.at, c.in, c.have = now, in, true
	return in
}

// recentEvents is "Last 2 hours": warnings and errors, newest first.
func (s *Server) recentEvents(now time.Time) []HealthEvent {
	out := []HealthEvent{}
	res := s.hist.Query(history.Query{Since: now.Add(-recentWindow).UnixMilli(), MinSeverity: history.Warn, Limit: recentMax})
	for _, e := range res.Events {
		out = append(out, shortEvent(e))
	}
	return out
}

// ---- judging --------------------------------------------------------------

func summarize(rows []HealthRow, recent []HealthEvent, now time.Time) HealthSummary {
	sum := HealthSummary{Rows: rows, Recent: recent, UpdatedAt: isoTime(now),
		RecentLink: "#/activity?range=2h&sev=warn", AlertsLink: "#/health/alerts"}
	worst, known := HealthOK, 0
	for _, r := range rows {
		switch r.State {
		case HealthAttention:
			sum.Issues++
			if worst == HealthOK {
				worst = HealthAttention
			}
		case HealthProblem:
			sum.Issues++
			worst = HealthProblem
		}
		if r.State != HealthUnknown {
			known++
		}
	}
	if known == 0 {
		worst = HealthUnknown
	}
	sum.State = worst
	sum.Color = map[string]string{HealthOK: "green", HealthAttention: "amber", HealthProblem: "red", HealthUnknown: "grey"}[worst]
	switch {
	case worst == HealthUnknown:
		sum.Sentence = "Health not known yet"
	case sum.Issues == 0:
		sum.Sentence = "All good"
	case sum.Issues == 1:
		sum.Sentence = "1 thing needs attention"
	default:
		sum.Sentence = fmt.Sprintf("%d things need attention", sum.Issues)
	}
	return sum
}

// judgeZones: down = lost or set aside (C2); switched off (paused, excluded)
// is neither down nor counted against reachable; removed zones are left out.
// Any zone down is a problem (the mockup's red dot at 11 of 232).
func judgeZones(st *scheduler.Status, facts []pool.NodeFacts, fresh bool, at time.Time) HealthRow {
	row := HealthRow{ID: "zones", Label: "Zones", Link: "#/zones?filter=down", Facts: map[string]any{}}
	if st == nil && len(facts) == 0 {
		row.State, row.Text = HealthUnknown, "Status not read yet"
		return row
	}
	removed := map[string]bool{}
	for _, f := range facts {
		if f.State == pool.StateRemoved {
			removed[f.ID] = true
		}
	}
	if st == nil {
		st = &scheduler.Status{}
	}
	var total, lost, setAside, off int64
	downNames := []string{}
	for _, smp := range availability.Classify(st, facts) {
		if smp.Removed || removed[smp.Node] {
			continue
		}
		total++
		switch smp.State {
		case availability.Lost:
			lost++
			downNames = append(downNames, smp.Name)
		case availability.SetAside:
			setAside++
			downNames = append(downNames, smp.Name)
		case availability.Off:
			off++
		}
	}
	down := lost + setAside
	row.Count = down
	row.Facts = map[string]any{"total": total, "reachable": total - down, "down": down, "lost": lost, "set_aside": setAside, "off": off,
		"down_names": downNames, "status_fresh": fresh}
	if !fresh && !at.IsZero() && at.Unix() > 0 {
		row.Facts["status_at"] = isoTime(at)
	}
	switch {
	case total == 0:
		row.State, row.Text = HealthUnknown, "No zones yet"
		row.Link = "#/zones"
	case down == 0:
		row.State, row.Text = HealthOK, fmt.Sprintf("%d of %d reachable", total, total)
		row.Link = "#/zones"
	default:
		row.State, row.Text = HealthProblem, fmt.Sprintf("%d of %d reachable · %d down", total-down, total, down)
	}
	return row
}

// judgeRuns: failed or refused client requests in the last hour, plus
// failed benchmark runs. Any is attention; at least five making more than
// half of the hour's requests is a problem.
func judgeRuns(in runsIn) HealthRow {
	row := HealthRow{ID: "runs", Label: "Runs", Link: "#/overview?runs=failed",
		Facts: map[string]any{"request_log": map[bool]string{true: "on", false: "off"}[in.logOn], "failed_requests": in.failed, "requests": in.total,
			"failed_bench_runs": in.benchFailed, "bench_runs": nonNil(in.benchFailedIDs), "more": in.capped}}
	if !in.logOn {
		row.State, row.Text = HealthUnknown, "Request log is off"
		return row
	}
	n := in.failed + in.benchFailed
	row.Count = n
	more := ""
	if in.capped {
		more = "+"
	}
	switch {
	case n == 0:
		row.State, row.Text = HealthOK, "None failed in the last hour"
		row.Link = "#/overview"
	case in.failed >= runsProblemMin && in.failed*2 > in.total:
		row.State, row.Text = HealthProblem, fmt.Sprintf("%d%s failed in the last hour", n, more)
	default:
		row.State, row.Text = HealthAttention, fmt.Sprintf("%d%s failed in the last hour", n, more)
	}
	return row
}

// schedIn is what the Scheduler row judges.
type schedIn struct {
	version         string
	uptime          time.Duration
	loop            scheduler.LoopStats
	tasks           []supervise.TaskStats
	readPanics      int64
	writesNeedForce bool
	statusFresh     bool
	now             time.Time
}

// judgeScheduler: problem when a task whose failure ends the process has
// failed, or the loop has been on one step for 5 s or more now (or does not
// answer); attention for a task failure in the last hour, a recovered read
// panic, a stored configuration from a newer version, or a step over
// loop_step_warn_ms in the last 10 minutes.
func judgeScheduler(in schedIn) HealthRow {
	row := HealthRow{ID: "scheduler", Label: "Scheduler", Link: "#/about"}
	hourAgo := in.now.Add(-time.Hour).UnixMilli()
	failedTasks, exitDown := []string{}, []string{}
	for _, t := range in.tasks {
		if t.LastFailureAt >= hourAgo && t.LastFailureAt > 0 {
			failedTasks = append(failedTasks, t.Name)
		}
		if t.Policy == "exit" && t.State == "failed" {
			exitDown = append(exitDown, t.Name)
		}
	}
	busy := time.Duration(in.loop.BusyNowMs) * time.Millisecond
	slow10m := in.loop.WarnMs > 0 && in.loop.Max10mMs > in.loop.WarnMs
	row.Facts = map[string]any{"release": in.version, "uptime_s": int64(in.uptime / time.Second), "tasks_failed_last_hour": failedTasks,
		"exit_tasks_failed": exitDown, "read_panics": in.readPanics, "writes_need_force": in.writesNeedForce, "status_answers": in.statusFresh, "loop": in.loop}
	var problems, warnings []string
	if len(exitDown) > 0 {
		problems = append(problems, plural(len(exitDown), "essential task", "essential tasks")+" stopped")
	}
	if busy >= loopBusyProblem {
		problems = append(problems, "Core loop busy "+spanText(busy)+" on one step")
	} else if !in.statusFresh {
		problems = append(problems, "Core loop not answering")
	}
	if len(failedTasks) > 0 {
		warnings = append(warnings, plural(len(failedTasks), "task", "tasks")+" failed in the last hour")
	}
	if slow10m {
		warnings = append(warnings, "Slowest step "+spanText(time.Duration(in.loop.Max10mMs)*time.Millisecond)+" in 10 min")
	}
	if in.readPanics > 0 {
		warnings = append(warnings, plural(int(in.readPanics), "read panic", "read panics"))
	}
	if in.writesNeedForce {
		warnings = append(warnings, "Stored settings from a newer release")
	}
	row.Count = int64(len(problems) + len(warnings))
	switch {
	case len(problems) > 0:
		row.State, row.sigText = HealthProblem, strings.Join(append(problems, warnings...), " · ")
	case len(warnings) > 0:
		row.State, row.sigText = HealthAttention, strings.Join(warnings, " · ")
	default:
		row.State, row.sigText = HealthOK, "Running · release "+orText(in.version, "unknown")
	}
	// The sentence ends with the uptime, as the mockup shows it; the push
	// compares the sentence without it (sigText), so the uptime alone never
	// sends a frame (it is also facts.uptime_s).
	row.Text = row.sigText + " · up " + upText(in.uptime)
	return row
}

// judgeMCP: the MCP server runs in the scheduler, so it runs while this
// answers; the row says how many agent keys are active (not revoked).
func judgeMCP(keys int, known bool) HealthRow {
	row := HealthRow{ID: "mcp", Label: "MCP server", Link: "#/mcp", State: HealthOK, Facts: map[string]any{"keys_active": keys, "keys_known": known}}
	if !known {
		row.Text = "Running"
		return row
	}
	row.Text = "Running · " + plural(keys, "key active", "keys active")
	return row
}

// judgeStorage: the fullest filesystem among the data, history, logs and
// models folders (08 5.7), on its real free space (Q18), and the model files
// against models_disk_budget_gb when that is set. Attention at 90 % used or
// under 10 GB free, or a folder whose disk does not answer or cannot be
// read; problem at 97 % or under 2 GB free; the budget: attention at 90 %,
// problem over it.
func judgeStorage(folders []diskstat.Entry, budget int64) HealthRow {
	row := HealthRow{ID: "storage", Label: "Storage", Link: "#/about", Facts: map[string]any{}}
	var fullest *diskstat.Entry
	state := HealthOK
	worse := func(s string) {
		if rank(s) > rank(state) {
			state = s
		}
	}
	var notes []string
	unsupported := 0
	var models *diskstat.Entry
	for i := range folders {
		f := &folders[i]
		if f.Name == "models" {
			models = f
		}
		switch f.State {
		case diskstat.StateTimeout:
			worse(HealthAttention)
			notes = append(notes, titleCase(f.Name)+" folder not answering")
			row.Count++
		case diskstat.StateError:
			worse(HealthAttention)
			notes = append(notes, titleCase(f.Name)+" folder unreadable")
			row.Count++
		case diskstat.StateUnknown:
			unsupported++
		}
		if !f.Known() || len(f.SameDiskAs) > 0 {
			continue
		}
		switch {
		case f.UsedPercent >= diskProblemPct || f.FreeBytes < diskProblemFree:
			worse(HealthProblem)
			row.Count++
		case f.UsedPercent >= diskAttentionPct || f.FreeBytes < diskAttentionFree:
			worse(HealthAttention)
			row.Count++
		}
		if fullest == nil || f.UsedPercent > fullest.UsedPercent {
			fullest = f
		}
	}
	row.Facts["folders"] = folders
	if fullest == nil {
		if state == HealthOK {
			state = HealthUnknown
		}
		row.State = state
		switch {
		case len(notes) > 0:
			row.Text = strings.Join(notes, " · ")
		case len(folders) > 0 && unsupported == len(folders):
			row.Text = "Disk figures not read on this platform"
		case len(folders) == 0:
			row.Text = "No folders to watch"
		default:
			row.Text = "Disk figures not read yet"
		}
		return row
	}
	row.Facts["fullest"], row.Facts["used_percent"] = fullest.Name, fullest.UsedPercent
	text := fmt.Sprintf("%s of %s GB used", gbText(fullest.UsedBytes), gbText(fullest.TotalBytes))
	if fullest.FreeBytes < diskAttentionFree {
		text += " · " + gbText(fullest.FreeBytes) + " GB free"
	}
	if budget > 0 {
		row.Facts["models_budget_bytes"] = budget
		if models != nil && models.State != diskstat.StateWaiting {
			pct := float64(models.PotluckBytes) / float64(budget) * 100
			row.Facts["models_budget_used_percent"] = math.Round(pct*10) / 10
			bt := fmt.Sprintf("models %s of %s GB budget", gbText(models.PotluckBytes), gbText(budget))
			switch {
			case pct > 100:
				worse(HealthProblem)
				text = bt + " · " + text
				row.Count++
			case pct >= budgetAttentionPct:
				worse(HealthAttention)
				text = bt + " · " + text
				row.Count++
			}
		}
	}
	if len(notes) > 0 {
		text = strings.Join(notes, " · ") + " · " + text
	}
	row.State, row.Text = state, text
	return row
}

// judgeQueue: attention when the oldest waiting request has waited three
// quarters of queue_timeout; problem at nine tenths of it. (The audit said
// half; the mockup shows 5 min 48 s of the default 10 min as OK.)
func judgeQueue(st *scheduler.Status, timeout time.Duration) HealthRow {
	row := HealthRow{ID: "queue", Label: "Queue", Link: "#/monitor/queue", Facts: map[string]any{"queue_timeout_s": int64(timeout / time.Second)}}
	if st == nil {
		row.State, row.Text = HealthUnknown, "Status not read yet"
		return row
	}
	var waiting int64
	var longest time.Duration
	for _, e := range st.Queue {
		waiting += int64(e.Waiters)
		if e.Age > longest {
			longest = e.Age
		}
	}
	row.Facts["waiting"], row.Facts["entries"], row.Facts["longest_s"] = waiting, len(st.Queue), int64(longest/time.Second)
	if waiting == 0 {
		row.State, row.Text = HealthOK, "Nothing waiting"
		return row
	}
	// "longest" grows by itself: the push compares the count and the state
	row.sigText = fmt.Sprintf("%d waiting", waiting)
	row.Text = row.sigText + " · longest " + spanText(longest)
	switch {
	case timeout > 0 && longest*10 >= timeout*9:
		row.State = HealthProblem
	case timeout > 0 && longest*4 >= timeout*3:
		row.State = HealthAttention
	default:
		row.State = HealthOK
	}
	if row.State != HealthOK {
		row.Count = waiting
	}
	return row
}

// judgeAlerts: webhooks are not built yet (08 A3 to A5).
func judgeAlerts() HealthRow {
	return HealthRow{ID: "alerts", Label: "Alerts", State: HealthUnknown, Text: "Not set up yet", Link: "#/health/alerts", Facts: map[string]any{"built": false}}
}

// ---- sentences ------------------------------------------------------------

var setAsideWhy = map[string]string{
	scheduler.SetAsidePreflight:        "failed a load",
	scheduler.SetAsideAgentUnreachable: "could not be reached for a load",
	scheduler.SetAsideWorkerHung:       "had a worker stop answering",
	scheduler.SetAsideNodeLost:         "was lost during a load",
}

// shortEvent is the popup's sentence for an event; the history summary is
// longer. Types without a sentence of their own use the summary's first
// sentence.
func shortEvent(e history.Event) HealthEvent {
	out := HealthEvent{At: isoTime(time.UnixMilli(e.Ts)), Seq: e.Seq, Type: e.Type, Severity: e.Severity}
	node := ""
	if len(e.Nodes) > 0 {
		node = e.Nodes[0]
	}
	str := func(k string) string { v, _ := e.Data[k].(string); return v }
	switch e.Type {
	case history.TNodeQuarantined:
		why := setAsideWhy[str("reason")]
		if why == "" {
			why = "failed a load"
		}
		out.Node, out.Text = node, node+" "+why+" and was set aside"
		if u, ok := e.Data["until"].(int64); ok && u > 0 {
			out.Until = isoTime(time.UnixMilli(u))
		} else if f, ok := e.Data["until"].(float64); ok && f > 0 {
			out.Until = isoTime(time.UnixMilli(int64(f)))
		}
	case history.TNodeLost:
		out.Node, out.Text = node, node+" went offline"
	case history.TNodeMembership:
		if str("state") == pool.StateLost {
			out.Node, out.Text = node, node+" went offline"
		}
	case history.TBenchRunFinished:
		if str("state") == "failed" {
			out.Text = "Benchmark run " + str("bench_run") + " failed"
		}
	case history.TRequestRefused:
		out.Text = "A request for " + e.Profile + " was refused"
	case history.TLoopSlowStep:
		if ms, ok := e.Data["ms"].(int64); ok {
			out.Text = "The core loop was busy " + spanText(time.Duration(ms)*time.Millisecond) + " on one step"
		} else if f, ok := e.Data["ms"].(float64); ok {
			out.Text = "The core loop was busy " + spanText(time.Duration(f)*time.Millisecond) + " on one step"
		}
	case history.TInternalFailure:
		out.Text = "A background task failed: " + str("task")
	}
	if out.Text == "" {
		out.Text = firstSentence(e.Summary)
		if node != "" && strings.HasPrefix(out.Text, "Node "+node) {
			out.Node = node
		}
	}
	return out
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	if len(s) > 140 {
		s = s[:137] + "..."
	}
	return s
}

func rank(s string) int {
	switch s {
	case HealthAttention:
		return 1
	case HealthProblem:
		return 2
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// gbText is decimal gigabytes as the mockup writes them: 50.3, 240.
func gbText(b int64) string {
	v := float64(b) / 1e9
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// spanText is a wait or a step: "48 s", "5 min 48 s", "2 h 5 min", "350 ms".
func spanText(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// upText is an uptime as the mockup writes it: "12 d", "5 h", "3 min", and
// "under 1 min" (no seconds, so the sentence changes once a minute at most).
func upText(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d d", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return "under 1 min"
}

// withModelsDisk puts "disk" (the models folder's disk from the one reader,
// C15, with models_disk_budget_gb) first in the catalog document, which is
// kept marshalled: the figures move every 30 s, the catalog does not.
func (s *Server) withModelsDisk(list []byte) []byte {
	if len(list) < 2 || list[0] != '{' {
		return list
	}
	d := map[string]any{"state": diskstat.StateWaiting}
	if e, ok := s.disks.Folder("models"); ok {
		d = map[string]any{"path": e.Path, "state": e.State, "total_bytes": e.TotalBytes, "free_bytes": e.FreeBytes, "used_bytes": e.UsedBytes,
			"used_percent": e.UsedPercent, "model_bytes": e.PotluckBytes, "files": e.FileCount, "checked_at": e.CheckedAt}
	}
	if c := s.conf(); c != nil && c.ModelsDiskBudgetGB > 0 {
		d["budget_bytes"] = int64(c.ModelsDiskBudgetGB) * 1_000_000_000
	}
	b, err := json.Marshal(d)
	if err != nil {
		return list
	}
	out := make([]byte, 0, len(list)+len(b)+10)
	out = append(append(append(out, `{"disk":`...), b...), ',')
	if len(list) > 2 && list[1] == '}' {
		out = out[:len(out)-1]
	}
	return append(out, list[1:]...)
}
