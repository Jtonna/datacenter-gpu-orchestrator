package bench

// The context_fill phase (B8, slice F1): a conversation that fills the
// model's context to a share of its size with generated filler, one turn per
// mark, reading every machine's available memory after each turn, so the
// memory the context takes per token is measured rather than guessed. The
// last turn asks back the facts planted in the filler. A stop line keeps the
// machines safe: before a turn, a machine that would fall below it with the
// turn's expected growth stops the step; during a turn, a sample below it
// cuts the turn off. The step then fails, and the model is unloaded.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// FillResult is what a context_fill phase filled and read.
type FillResult struct {
	Ctx      int   `json:"ctx"`
	Parallel int   `json:"parallel,omitempty"`
	FillPct  int   `json:"fill_pct"`
	MarksPct []int `json:"marks_pct"`
	Seed     int64 `json:"seed"`
	// The filler as sent (its hash covers the paragraphs in order, joined by a blank line).
	FillerSHA256 string `json:"filler_sha256"`
	FillerChars  int    `json:"filler_chars"`
	// TemplateTokens is what the chat template added to the first turn
	// (its prompt tokens less the tokenizer's count of its text).
	TemplateTokens *int `json:"template_tokens,omitempty"`
	StopLineMiB    int  `json:"stop_available_mib"`
	// TargetTokens is fill_pct of the context; ReachedTokens the tokens held
	// after the last turn that finished.
	TargetTokens  int    `json:"target_tokens"`
	ReachedTokens int    `json:"reached_tokens"`
	Recall        int    `json:"recall"` // facts planted and asked back
	RecallCorrect *int   `json:"recall_correct,omitempty"`
	Answer        string `json:"answer,omitempty"` // the last turn's answer (first 2,000 characters)
	// StoppedBecause says why the phase stopped before the target (empty: it got there).
	StoppedBecause string     `json:"stopped_because,omitempty"`
	Marks          []FillMark `json:"marks"`
}

// FillMark is one turn: the tokens held after it and every machine's memory.
type FillMark struct {
	Pct    int `json:"pct"`
	Target int `json:"target_tokens"`
	// TokensHeld is the prompt (processed and reused) and the answer;
	// PromptTokens the prompt alone.
	TokensHeld   int        `json:"tokens_held"`
	PromptTokens int        `json:"prompt_tokens"`
	PromptN      int        `json:"prompt_n"`
	CacheN       int        `json:"cache_n"`
	PredictedN   int        `json:"predicted_n"`
	PrefillTPS   float64    `json:"prefill_tps,omitempty"`
	TTFTMS       *float64   `json:"ttft_ms,omitempty"`
	WallMS       float64    `json:"wall_ms"`
	Status       int        `json:"status"`
	Error        string     `json:"error,omitempty"`
	Nodes        []FillNode `json:"nodes"`
}

// FillNode is a machine's available memory at a mark: at the phase's start
// (Before), after the turn (After) and the lowest seen during it. Growth is
// Before less After for a machine the model uses, and KiBPerToken that
// growth over the tokens held.
type FillNode struct {
	ID          string   `json:"id"`
	Node        string   `json:"node"`
	InUse       bool     `json:"in_use"`
	Kind        string   `json:"kind,omitempty"`
	BeforeMiB   *int64   `json:"before_mib,omitempty"`
	AfterMiB    *int64   `json:"after_mib,omitempty"`
	LowestMiB   *int64   `json:"lowest_mib,omitempty"`
	GrowthMiB   *int64   `json:"growth_mib,omitempty"`
	KiBPerToken *float64 `json:"kib_per_token,omitempty"`
}

// fillMinKiBPerToken is the growth per token assumed before one is measured.
const fillMinKiBPerToken = 8.0

// fillShortAnswer is the answer budget of the turns before the last.
const fillShortAnswer = 8

// sharedShapeRefusal is why the step is refused before anything loads. A
// step with a context_fill phase never runs on a profile a person wrote: its
// memory peak would become that profile's stored peak. So it is refused,
// whatever refuse_shared_shape says, when its own profile is not generated
// or the plan does not say; and, with refuse_shared_shape (the default),
// when the plan names a written profile of the same shape, or does not say
// which profiles share it. Empty: go on.
func sharedShapeRefusal(b *benchspec.Benchmark, plan *PlanInfo) string {
	fills, wants := false, false
	for _, p := range b.Expand() {
		if p.Type == benchspec.PhaseContextFill {
			fills = true
			wants = wants || p.RefusesSharedShape()
		}
	}
	if !fills {
		return ""
	}
	switch {
	case plan.Generated == nil:
		return fmt.Sprintf("context_fill refuses to run: the scheduler's plan does not say whether profile %s was generated (an older scheduler), and a fill must never run on a profile a person wrote; use the recipe context-fill-recipe on a newer scheduler", b.Profile)
	case !*plan.Generated:
		return fmt.Sprintf("context_fill refuses to run: profile %s is a profile a person wrote, and the fill's memory peak would become its stored peak; use the recipe context-fill-recipe, which makes a generated profile, at a context no written profile has", b.Profile)
	case !wants:
		return ""
	}
	if plan.SharedWith == nil {
		return "context_fill refuses to run: the scheduler's plan does not say which profiles share this shape (an older scheduler), so the fill's memory peak could move another profile's plan; set refuse_shared_shape to false to run anyway"
	}
	var people []string
	for _, s := range *plan.SharedWith {
		if !s.Generated {
			people = append(people, s.ID)
		}
	}
	if len(people) == 0 {
		return ""
	}
	return fmt.Sprintf("context_fill refuses to run: profile %s has the same shape (model, context, parallel and options), and the fill's memory peak would become its stored peak; use a shape no written profile has, or set refuse_shared_shape to false", strings.Join(people, ", "))
}

// groupInt writes n with thousands separators (12,345).
func groupInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fillTrip watches the samples of one turn: the lowest figure of every
// machine, and the first one below the stop line, which cuts the turn off.
type fillTrip struct {
	line   int64
	inUse  map[string]bool
	cancel func()

	mu  sync.Mutex
	low map[string]int64
	why string
}

func (t *fillTrip) see(s *StatusInfo) {
	t.mu.Lock()
	defer supervise.Unlocked("bench", t.mu.Unlock)
	for _, w := range s.Workers {
		if w.AvailMiB == nil || (w.Local && !t.inUse[w.ID]) {
			continue
		}
		v := *w.AvailMiB
		if l, ok := t.low[w.ID]; !ok || v < l {
			t.low[w.ID] = v
		}
		if v < t.line && t.why == "" {
			t.why = fmt.Sprintf("stopped at the stop line: %s had %s MiB available during the turn, below the stop line of %s MiB", nodeName(w), groupInt(v), groupInt(t.line))
			if t.cancel != nil {
				t.cancel()
			}
		}
	}
}

func (t *fillTrip) tripped() string {
	t.mu.Lock()
	defer supervise.Unlocked("bench", t.mu.Unlock)
	return t.why
}

func nodeName(w WorkerInfo) string {
	if w.Hostname != "" {
		return w.Hostname
	}
	return w.ID
}

// fillInUse are the nodes the model is on: the plan's and the instance's.
func (br *benchRun) fillInUse() map[string]bool {
	br.mu.Lock()
	defer supervise.Unlocked("bench", br.mu.Unlock)
	out := map[string]bool{}
	if br.plan != nil {
		for _, id := range br.plan.Nodes {
			out[id] = true
		}
	}
	for _, n := range br.nodes {
		out[n.ID] = true
	}
	return out
}

// fillStatus reads the status once (fed to the memory watch too).
func (br *benchRun) fillStatus() (*StatusInfo, error) {
	ctx, cancel := context.WithTimeout(br.ctx, 5*time.Second)
	defer cancel()
	s, err := br.x.m.o.API.Status(ctx)
	if err != nil {
		return nil, err
	}
	br.observe(s)
	return s, nil
}

func (br *benchRun) phaseContextFill(i int, p *benchspec.Phase, pr *PhaseResult) {
	if !br.ensureLoaded() {
		return
	}
	tz, ok := br.x.m.o.API.(Tokenizer)
	if !ok {
		br.fail(OutcomeError, "context_fill: this scheduler API cannot count tokens")
		return
	}
	var plan *PlanInfo
	func() {
		br.mu.Lock()
		defer supervise.Unlocked("bench", br.mu.Unlock)
		plan = br.plan
	}()
	if plan == nil || plan.Ctx <= 0 {
		br.fail(OutcomeError, "context_fill: the scheduler's plan does not give the profile's context size (an older scheduler)")
		return
	}
	ctxSize, answer := plan.Ctx, p.FillAnswerTokens()
	ceiling := ctxSize - answer - 32
	marks := p.FillMarks()
	line := int64(p.StopLine())
	res := &FillResult{Ctx: ctxSize, Parallel: plan.Parallel, FillPct: p.FillTarget(), MarksPct: marks, Seed: p.FillSeed(),
		StopLineMiB: int(line), TargetTokens: ctxSize * p.FillTarget() / 100}
	pr.Fill = res
	fl := newFiller(p.FillSeed(), p.RecallCount(), res.TargetTokens)
	inUse := br.fillInUse()
	tok := func(s string) (int, error) {
		ctx, cancel := context.WithTimeout(br.ctx, 60*time.Second)
		defer cancel()
		return tz.Tokenize(ctx, br.b.Profile, s)
	}
	stop := func(why string) {
		res.StoppedBecause = why
		br.st.Reason = why
		br.fail(OutcomeFailed, why)
	}

	// The readings at the start: what every machine has before the fill.
	s0, err := br.fillStatus()
	if err != nil {
		br.fail(OutcomeError, "context_fill: status: "+err.Error())
		return
	}
	base := map[string]int64{}
	kinds := map[string]string{}
	for _, w := range s0.Workers {
		if w.AvailMiB != nil {
			base[w.ID], kinds[w.ID] = *w.AvailMiB, w.AvailKind
		}
	}
	kib := map[string]float64{} // measured growth per token so far, by node

	var hist []benchspec.Message
	var used []string // the filler paragraphs sent, in order
	next := 0         // the next paragraph of the filler
	heldPrev, overhead := 0, -1
	ratio := 4.0 // characters per token, measured as the turns go
	br.x.setUnits(br.track, len(marks), fmt.Sprintf("Context fill: 0 of %d marks", len(marks)))
	for j, pct := range marks {
		if br.x.a.cancelled() {
			br.fail(OutcomeCancelled, "cancelled")
			return
		}
		last := j == len(marks)-1
		target := ctxSize * pct / 100
		if target > ceiling {
			target = ceiling
		}
		instr, maxTok := "Reply with OK.", fillShortAnswer
		if last {
			maxTok = answer
			if fl.recall > 0 {
				instr = "That is the end of the text. List the harbour codes of all the records above, one per line, as 01: 1234."
			}
		}
		ovh := overhead
		if ovh < 0 {
			ovh = 16
		}
		room := target - heldPrev - ovh - fillShortAnswer
		// The paragraphs of this turn: as many as fit in room, counted with the model's tokenizer.
		var chunk []string
		content := func() string {
			if len(chunk) == 0 {
				return instr
			}
			return strings.Join(chunk, "\n\n") + "\n\n" + instr
		}
		n, err := tok(content())
		if err != nil {
			br.fail(OutcomeError, "context_fill: tokenize: "+err.Error())
			return
		}
		for capped := false; !capped && n < room; {
			added := 0
			est := n
			for est < room {
				para := fl.paragraph(next + len(chunk))
				pt := int(math.Ceil(float64(len(para)+2) / ratio))
				if est+pt > room && added > 0 {
					break
				}
				chunk = append(chunk, para)
				est += pt
				added++
			}
			before := n
			if n, err = tok(content()); err != nil {
				br.fail(OutcomeError, "context_fill: tokenize: "+err.Error())
				return
			}
			if grew := n - before; grew > 0 {
				chars := 0
				for _, c := range chunk[len(chunk)-added:] {
					chars += len(c) + 2
				}
				ratio = float64(chars) / float64(grew)
			}
			for n > room && len(chunk) > 0 {
				chunk = chunk[:len(chunk)-1]
				capped = true
				if n, err = tok(content()); err != nil {
					br.fail(OutcomeError, "context_fill: tokenize: "+err.Error())
					return
				}
			}
		}
		text := content()

		// Before the turn: every machine must keep the stop line with the turn's expected growth.
		sb, err := br.fillStatus()
		if err != nil {
			br.fail(OutcomeError, "context_fill: status: "+err.Error())
			return
		}
		turnTokens := n + ovh + maxTok
		for _, w := range sb.Workers {
			if w.AvailMiB == nil || (w.Local && !inUse[w.ID]) {
				continue
			}
			var need int64
			if inUse[w.ID] {
				need = int64(math.Ceil(float64(turnTokens) * math.Max(fillMinKiBPerToken, kib[w.ID]) / 1024))
			}
			if *w.AvailMiB < line+need {
				stop(fmt.Sprintf("stopped at the stop line before the turn to %d %%: %s has %s MiB available, and the turn (about %s tokens) would take about %s MiB more, below the stop line of %s MiB",
					pct, nodeName(w), groupInt(*w.AvailMiB), groupInt(int64(turnTokens)), groupInt(need), groupInt(line)))
				return
			}
		}

		// The turn, with the stop line watched while it runs.
		user := benchspec.Message{Role: "user", Content: text}
		msgs := append(append([]benchspec.Message(nil), hist...), user)
		temp := 0.0
		seed := p.FillSeed()
		cache, stream := true, true
		req := benchspec.Request{Name: fmt.Sprintf("fill %d%%", pct), Messages: msgs, MaxTokens: maxTok, Temperature: &temp, Seed: &seed, CachePrompt: &cache, Stream: &stream}
		paras := chunk
		func() {
			br.mu.Lock()
			defer supervise.Unlocked("bench", br.mu.Unlock)
			if br.redact == nil {
				br.redact = map[string]string{}
			}
			chars := 0
			for _, c := range paras {
				chars += len(c)
			}
			br.redact[text] = fmt.Sprintf("filler: seed %d, paragraphs %d to %d, %d chars, sha256 %s\n\n%s", seed, next+1, next+len(paras), chars, fillerDigest(paras), instr)
		}()
		br.x.setDetail(br.track, map[string]any{"turn": j + 1, "turns": len(marks), "text": fmt.Sprintf("Context fill: to %d %% (%s of %s tokens)", pct, groupInt(int64(target)), groupInt(int64(ctxSize)))})
		tctx, cancel := context.WithCancel(br.ctx)
		trip := &fillTrip{line: line, inUse: inUse, cancel: cancel, low: map[string]int64{}}
		hook := trip.see
		br.watch.hook.Store(&hook)
		rr, resp, err := br.send(tctx, req, PhaseFill, i, j+1)
		br.watch.hook.Store(nil)
		cancel()

		mark := FillMark{Pct: pct, Target: target}
		if rr != nil {
			mark.Status, mark.Error, mark.WallMS, mark.TTFTMS = rr.Status, rr.Error, rr.WallMS, rr.TTFTMS
			mark.PromptN, mark.CacheN, mark.PredictedN, mark.PrefillTPS = rr.PromptN, rr.CacheN, rr.PredictedN, rr.PromptTPS
			mark.PromptTokens = rr.PromptN + rr.CacheN
			mark.TokensHeld = mark.PromptTokens + rr.PredictedN
		}
		// The readings after the turn.
		if sa, err := br.fillStatus(); err == nil {
			trip.see(sa) // the after figure counts for the lowest (and the stop line) too
			mark.Nodes = fillNodes(sa, base, kinds, inUse, trip, mark.TokensHeld, kib)
		}
		res.Marks = append(res.Marks, mark)
		if why := trip.tripped(); why != "" {
			if mark.Status == 200 && mark.TokensHeld > 0 {
				res.ReachedTokens = mark.TokensHeld
			}
			stop(why)
			return
		}
		if !br.judge(resp, err) {
			res.StoppedBecause = br.st.Error
			return
		}
		res.ReachedTokens = mark.TokensHeld
		used = append(used, paras...)
		res.FillerSHA256, res.FillerChars = fillerDigest(used), 0
		for _, u := range used {
			res.FillerChars += len(u)
		}
		if j == 0 {
			t := mark.PromptTokens - n
			res.TemplateTokens = &t
			overhead = max(t, 0)
		} else if t := mark.PromptTokens - heldPrev - n; t > overhead {
			overhead = t
		}
		heldPrev = mark.TokensHeld
		next += len(paras)
		reply := resp.Text
		if reply == "" {
			reply = "(no answer)"
		}
		hist = append(msgs, benchspec.Message{Role: "assistant", Content: reply})
		if last {
			res.Recall = fl.planted
			res.Answer = Head(resp.Text, 2000)
			if fl.recall > 0 {
				c := fl.score(resp.Text)
				res.RecallCorrect = &c
			}
		}
		br.x.unitDone(br.track, fmt.Sprintf("Context fill: %d of %d marks", j+1, len(marks)))
	}
}

// fillNodes are the readings of every machine with a known figure after a turn.
func fillNodes(s *StatusInfo, base map[string]int64, kinds map[string]string, inUse map[string]bool, trip *fillTrip, held int, kib map[string]float64) []FillNode {
	var out []FillNode
	trip.mu.Lock()
	defer supervise.Unlocked("bench", trip.mu.Unlock)
	for _, w := range s.Workers {
		if w.AvailMiB == nil || (w.Local && !inUse[w.ID]) {
			continue
		}
		fn := FillNode{ID: w.ID, Node: nodeName(w), InUse: inUse[w.ID], Kind: kinds[w.ID]}
		after := *w.AvailMiB
		fn.AfterMiB = &after
		if l, ok := trip.low[w.ID]; ok {
			fn.LowestMiB = &l
		}
		if b, ok := base[w.ID]; ok {
			bv := b
			fn.BeforeMiB = &bv
			if fn.InUse {
				g := b - after
				fn.GrowthMiB = &g
				if held > 0 && g >= 0 {
					k := math.Round(float64(g)*1024/float64(held)*100) / 100
					fn.KiBPerToken = &k
					kib[w.ID] = k
				}
			}
		}
		out = append(out, fn)
	}
	return out
}

// fillRows are the measurement rows of a context_fill phase: per finished
// mark the tokens held, the turn's speed, and each used machine's growth.
func fillRows(p *PhaseResult, add func(*PhaseResult, string, float64, string, func(*Measurement)), afterLoad func(*Measurement), cond func(...any) func(*Measurement)) {
	f := p.Fill
	if f == nil {
		return
	}
	for k, mk := range f.Marks {
		if mk.Status != 200 {
			continue
		}
		turn := k + 1
		mod := func(m *Measurement) {
			m.Subject, m.Seq = fmt.Sprintf("fill %d%%", mk.Pct), turn
			afterLoad(m)
			cond("fill_pct", mk.Pct, "ctx", f.Ctx)(m)
		}
		add(p, "fill_tokens", float64(mk.TokensHeld), "tokens", mod)
		add(p, "prompt_tokens", float64(mk.PromptTokens), "tokens", mod)
		if mk.TTFTMS != nil {
			add(p, "ttft_ms", *mk.TTFTMS, "ms", mod)
		}
		if mk.PrefillTPS > 0 {
			add(p, "prefill_tps", mk.PrefillTPS, "tok/s", mod)
		}
		add(p, "wall_ms", mk.WallMS, "ms", mod)
		for _, n := range mk.Nodes {
			if !n.InUse {
				continue
			}
			node, kind := n.Node, n.Kind
			if kind == "" {
				kind = MemKindSystem
			}
			nmod := func(m *Measurement) { mod(m); m.Node = node; cond("memory", kind)(m) }
			if n.GrowthMiB != nil {
				add(p, "fill_growth_mib", float64(*n.GrowthMiB), "MiB", nmod)
			}
			if n.KiBPerToken != nil {
				add(p, "fill_kib_per_token", *n.KiBPerToken, "KiB", nmod)
			}
		}
	}
	if f.RecallCorrect != nil {
		of := float64(f.Recall)
		add(p, "recall_correct", float64(*f.RecallCorrect), "count", func(m *Measurement) {
			m.Of, m.Subject = &of, "recall"
			afterLoad(m)
			cond("fill_pct", f.FillPct, "ctx", f.Ctx)(m)
		})
	}
}

// fillMD writes a context_fill phase's readings: the settings, one row per
// mark and machine, and how it ended.
func fillMD(b *strings.Builder, f *FillResult) {
	fmt.Fprintf(b, "Context %s tokens, filled to %d %% (%s tokens): reached %s. Filler: seed %d, %s characters, sha256 `%s`. Stop line %s MiB.\n\n",
		groupInt(int64(f.Ctx)), f.FillPct, groupInt(int64(f.TargetTokens)), groupInt(int64(f.ReachedTokens)), f.Seed, groupInt(int64(f.FillerChars)), f.FillerSHA256, groupInt(int64(f.StopLineMiB)))
	b.WriteString("| Mark | Tokens held | Prefill tok/s | Machine | In use | Before MiB | After MiB | Lowest MiB | Growth MiB | KiB per token |\n|---|---|---|---|---|---|---|---|---|---|\n")
	opt := func(v *int64) string {
		if v == nil {
			return ""
		}
		return groupInt(*v)
	}
	for _, m := range f.Marks {
		held := groupInt(int64(m.TokensHeld))
		if m.Status != 200 {
			held = "cut off"
		}
		if len(m.Nodes) == 0 {
			fmt.Fprintf(b, "| %d %% | %s | %s | | | | | | | |\n", m.Pct, held, fmtNum(m.PrefillTPS))
		}
		for _, n := range m.Nodes {
			kib := ""
			if n.KiBPerToken != nil {
				kib = fmtNum(*n.KiBPerToken)
			}
			fmt.Fprintf(b, "| %d %% | %s | %s | %s | %v | %s | %s | %s | %s | %s |\n", m.Pct, held, fmtNum(m.PrefillTPS), mdCell(n.Node), n.InUse,
				opt(n.BeforeMiB), opt(n.AfterMiB), opt(n.LowestMiB), opt(n.GrowthMiB), kib)
		}
	}
	b.WriteString("\n")
	if f.RecallCorrect != nil {
		fmt.Fprintf(b, "Recall: %d of %d planted facts given back right.\n\n", *f.RecallCorrect, f.Recall)
	}
	if f.StoppedBecause != "" {
		fmt.Fprintf(b, "Stopped: %s.\n\n", mdCell(f.StoppedBecause))
	}
}
