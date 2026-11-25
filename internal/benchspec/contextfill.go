package benchspec

// The context_fill phase (B8): a conversation that fills the model's
// context to a chosen share with generated filler, taking a memory reading
// at each mark, and asking back the facts planted in the filler at the end.
// The runner is internal/bench/contextfill.go.

// Limits and defaults of a context_fill phase.
const (
	DefaultFillPct       = 90
	MaxFillPct           = 95
	DefaultRecall        = 8
	MaxRecall            = 32
	DefaultStopAvailMiB  = 2000
	MinStopAvailMiB      = 500
	MaxStopAvailMiB      = 1 << 20
	DefaultFillAnswerTok = 128
	MaxFillAnswerTok     = 1024
	DefaultFillSeed      = 1
)

// defaultMarks are the marks when marks_pct is not given (those below the target, then the target).
var defaultMarks = []int{25, 50, 75}

// FillTarget is fill_pct, else the last of marks_pct, else 90.
func (p *Phase) FillTarget() int {
	switch {
	case p.FillPct != nil:
		return *p.FillPct
	case len(p.MarksPct) > 0:
		return p.MarksPct[len(p.MarksPct)-1]
	}
	return DefaultFillPct
}

// FillMarks are marks_pct, else 25, 50, 75 below the target and the target.
func (p *Phase) FillMarks() []int {
	if len(p.MarksPct) > 0 {
		return append([]int(nil), p.MarksPct...)
	}
	t := p.FillTarget()
	var out []int
	for _, m := range defaultMarks {
		if m < t {
			out = append(out, m)
		}
	}
	return append(out, t)
}

// RecallCount is recall (default 8).
func (p *Phase) RecallCount() int {
	if p.Recall == nil {
		return DefaultRecall
	}
	return *p.Recall
}

// StopLine is stop_available_mib (default 2000).
func (p *Phase) StopLine() int {
	if p.StopAvailableMiB == nil {
		return DefaultStopAvailMiB
	}
	return *p.StopAvailableMiB
}

// RefusesSharedShape is refuse_shared_shape (default true).
func (p *Phase) RefusesSharedShape() bool { return p.RefuseSharedShape == nil || *p.RefuseSharedShape }

// FillAnswerTokens is max_tokens of the last turn (default 128).
func (p *Phase) FillAnswerTokens() int {
	if p.MaxTokens <= 0 {
		return DefaultFillAnswerTok
	}
	return p.MaxTokens
}

// FillSeed is seed (default 1).
func (p *Phase) FillSeed() int64 {
	if p.Seed == nil {
		return DefaultFillSeed
	}
	return *p.Seed
}

func validateContextFill(p *Phase, w func(string, ...any)) {
	if p.FillPct != nil && (*p.FillPct < 1 || *p.FillPct > MaxFillPct) {
		w("fill_pct must be 1 to %d", MaxFillPct)
	}
	for i, m := range p.MarksPct {
		if m < 1 || m > MaxFillPct {
			w("marks_pct values must be 1 to %d", MaxFillPct)
			break
		}
		if i > 0 && m <= p.MarksPct[i-1] {
			w("marks_pct must rise")
			break
		}
	}
	if len(p.MarksPct) > MaxTurns {
		w("marks_pct: at most %d", MaxTurns)
	}
	if p.FillPct != nil && len(p.MarksPct) > 0 && p.MarksPct[len(p.MarksPct)-1] != *p.FillPct {
		w("the last of marks_pct must be fill_pct (%d)", *p.FillPct)
	}
	if p.Recall != nil && (*p.Recall < 0 || *p.Recall > MaxRecall) {
		w("recall must be 0 to %d", MaxRecall)
	}
	if p.StopAvailableMiB != nil && (*p.StopAvailableMiB < MinStopAvailMiB || *p.StopAvailableMiB > MaxStopAvailMiB) {
		w("stop_available_mib must be %d to %d", MinStopAvailMiB, MaxStopAvailMiB)
	}
	if p.MaxTokens < 0 || p.MaxTokens > MaxFillAnswerTok {
		w("max_tokens must be 1 to %d (the last turn's answer)", MaxFillAnswerTok)
	}
}
