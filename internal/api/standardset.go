package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The standard set of a model file (profiles plan, section 6, with the
// reviewer's rulings A to G of 26 Nov):
//
//	GET  /api/models/{file}/standard-set[?short=<name>]   what it would write (observe)
//	POST /api/models/{file}/standard-set {comment, short?, ids?}   write the chosen rows (settings)
//
// THE NAME (A). Rows are `<short>` (the default context) and `<short>:<N>k`.
// <short> is an editable field of both calls. Its prefill is the base name
// of the file's existing PRODUCTION family where it has one (familyBase),
// else the file's own name with its dots kept (fileShort). The server
// checks it: the id rules, and no clash with another file's profiles or
// aliases (shortProblem). A second family beside an existing one is never
// proposed by default.
//
// CANDIDATES. 16k, 32k, 64k, 128k and 256k. One above the file's trained
// context is replaced by the trained context itself, once (a file trained
// at 163,840 gets 16k, 32k, 64k, 128k and 160k; one trained at 2,048 gets
// 2,048 alone); a file with no trained context gets 16k and 32k. A context
// EQUAL to the trained one is not above it (realctx.go runs it as written).
//
// FIT (C). Each is planned read-only (scheduler.PlanCandidates) on an idle
// fleet (nothing loaded): what happens to be loaded does not decide a
// model's standard set. The plan of now (start, evict, queue, reject) is
// shown beside it, never decides. The state is that of the need on the
// zones the fit was decided on (zones_need_*: the idle placement's count,
// else the most it may use), the figure the placer compares; need_mib is
// the plan's figure (/api/plan's), on the count of the latest measurement.
// When they differ, zones_need_note names both. A row's state:
//
//	fits                    measured need, fits: written by default
//	fits-derived            need derived from other contexts of the model, fits: written by default
//	fits-by-estimate        only the file estimate, fits: written by default, marked estimated
//	estimate-does-not-fit   only the file estimate, does not fit: "not measured: by the
//	                        file estimate it does not fit"; not written by default, may be ticked
//	does-not-fit            measured or derived need, does not fit: not kept, cannot be ticked
//	need-unknown            no need at all: not kept, cannot be ticked
//
// EXISTS (B). A row exists when its id (or an alias) is taken, or when the
// family already has that context. It is never written or edited, and the
// row says what the existing profile carries that a standard row would not
// (KV type, flash attention, thinking off, a hand-written need, forced
// zones, aliases, slots). The default `<short>` that exists at another
// context says so: "gemma-4-12b exists at 8k, kept as it is; the standard
// default would be 32k".
//
// THE NEED (F). need_mib is the plan's need, the figure /api/plan and
// /api/profiles show for the written profile; fit_need_mib is what the fit
// rules use (need_mib plus the 5% margin on an estimate), named in the row.
// No figure is carried across shapes (D): an estimate row of a file whose
// existing profiles were measured or written by hand in another shape (KV
// type, flash attention, extra arguments) names that profile's figure and
// shape beside its own, in shape_note.
//
// Nothing here runs at start, at scan, at a download or in a migration:
// only these two calls.

const standardSetSuffix = "/standard-set"

// standardSizes are the candidate contexts before the trained cap.
var standardSizes = []int{16 << 10, 32 << 10, 64 << 10, 128 << 10, 256 << 10}

// standardDefaultMax is the largest context the default `<short>` takes.
const standardDefaultMax = 32 << 10

// standardDescription is the description every standard profile gets.
const standardDescription = "standard set"

// maxShortLen bounds the name field (ids get ":<N>k" after it).
const maxShortLen = 80

// The row states (see the top of the file).
const (
	stdFits          = "fits"
	stdFitsDerived   = "fits-derived"
	stdFitsEstimate  = "fits-by-estimate"
	stdEstimateNoFit = "estimate-does-not-fit"
	stdNoFit         = "does-not-fit"
	stdNeedUnknown   = "need-unknown"
)

// standardPath is the file of a .../standard-set path.
func standardPath(p string) (file string, ok bool) {
	if strings.HasSuffix(p, standardSetSuffix) && len(p) > len(standardSetSuffix) {
		return strings.TrimSuffix(p, standardSetSuffix), true
	}
	return "", false
}

// standardCtxs are the candidate contexts for a trained context (0: not known).
func standardCtxs(trained int) []int {
	if trained <= 0 {
		return []int{16 << 10, 32 << 10}
	}
	var out []int
	for _, c := range standardSizes {
		c = min(c, trained)
		if len(out) == 0 || out[len(out)-1] != c {
			out = append(out, c)
		}
	}
	return out
}

// trainedNote says the candidates' rule for a trained context.
func trainedNote(trained int) string {
	top := standardSizes[len(standardSizes)-1]
	switch {
	case trained <= 0:
		return "the file names no trained context: 16k and 32k are tried"
	case trained < standardSizes[0]:
		return fmt.Sprintf("trained at %s tokens, below 16k: only that context is tried", commas(trained))
	case trained >= top:
		return fmt.Sprintf("trained at %s tokens: every standard context is at or below it", commas(trained))
	case slices.Contains(standardSizes, trained):
		return fmt.Sprintf("trained at %s tokens: the standard contexts above it are not tried; %s, equal to it, is", commas(trained), ctxTag(trained))
	}
	return fmt.Sprintf("trained at %s tokens: the standard contexts above it are replaced by the trained context itself (%s), tried once", commas(trained), ctxTag(trained))
}

// ctxTag is a context as a profile id writes it: k = 1024 tokens (as the
// benchmark runner's ctxKey), else the number.
func ctxTag(ctx int) string {
	if ctx > 0 && ctx%1024 == 0 {
		return strconv.Itoa(ctx/1024) + "k"
	}
	return strconv.Itoa(ctx)
}

// slugDots is lower case letters, digits and dots, any other run of
// characters one "-" ("Qwen2.5 72B Instruct" -> "qwen2.5-72b-instruct").
func slugDots(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-.")
}

// fileShort is the name a file without a family gets: its name (the GGUF
// metadata name when the catalog read it, else the file name) with its dots
// kept, the quant appended only when another file of the catalog has the
// same name.
func fileShort(mf ModelFile, files []ModelFile) string {
	base := func(f ModelFile) string {
		if s := slugDots(f.Name); s != "" {
			return s
		}
		return slugDots(strings.TrimSuffix(path.Base(f.File), path.Ext(f.File)))
	}
	short := base(mf)
	for _, o := range files {
		if o.File != mf.File && base(o) == short {
			if q := slugDots(mf.Quant); q != "" {
				return short + "-" + q
			}
			return slugDots(strings.TrimSuffix(path.Base(mf.File), path.Ext(mf.File)))
		}
	}
	return short
}

// familyBase is the base name of a family of profile ids (a file's
// production profiles): among the ids, and the part before ":" of the ids
// that have one, the name that the most ids extend as "<name>:..." ; on a
// tie the shorter, then the first in order. "" for no ids.
//
//	gemma-4-12b, gemma-4-12b-thinking, gemma-4-12b:48k, :64k, :96k -> gemma-4-12b (3 extend it)
//	qwen3.5-4b, qwen3.5-4b-thinking                                  -> qwen3.5-4b (none extend either: the shorter)
//	devstral-24b, devstral-24b:14k, devstral-24b:24k                -> devstral-24b
//	tinyllama                                                        -> tinyllama
func familyBase(ids []string) string {
	cands := map[string]bool{}
	for _, id := range ids {
		b, _, _ := strings.Cut(id, ":")
		cands[b] = true
	}
	best, bestN := "", -1
	for c := range cands {
		n := 0
		for _, id := range ids {
			if strings.HasPrefix(id, c+":") {
				n++
			}
		}
		if n > bestN || (n == bestN && (len(c) < len(best) || (len(c) == len(best) && c < best))) {
			best, bestN = c, n
		}
	}
	return best
}

// inFamily: id is the family's base or extends it.
func inFamily(id, short string) bool { return id == short || strings.HasPrefix(id, short+":") }

// shortProblem is why short cannot name file's standard set ("" when it
// can): the id rules (config section 4/5: no "@", not "TEST - ", one line,
// no spaces, no ":" in the name itself), and no clash with another file's
// profile or alias, as the name or as one of its rows.
func shortProblem(cfg *config.Config, file, short string) string {
	switch {
	case short == "":
		return "the name is empty"
	case len(short) > maxShortLen:
		return fmt.Sprintf("the name is %d characters; at most %d", len(short), maxShortLen)
	case strings.HasPrefix(strings.ToUpper(short), strings.ToUpper(config.TestPrefix)):
		return fmt.Sprintf("the name must not start with %q: a test profile is shown with it already", config.TestPrefix)
	case strings.Contains(short, "@"):
		return "the name must not contain \"@\" (kept for run-scoped names)"
	case strings.Contains(short, ":"):
		return "the name must not contain \":\": the rows add \":<N>k\" to it"
	case strings.ContainsFunc(short, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' }):
		return "the name must be one word: no spaces, line breaks or \"/\""
	}
	ids := make([]string, 0, len(cfg.Profiles))
	for id := range cfg.Profiles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		p := cfg.Profiles[id]
		if p.Model == file {
			continue
		}
		if inFamily(id, short) {
			return fmt.Sprintf("the name %s is taken: the profile %s is of another file (%s)", short, id, p.Model)
		}
		for _, a := range p.Aliases {
			if inFamily(a, short) {
				return fmt.Sprintf("the name %s is taken: %s is an alias of the profile %s, of another file (%s)", short, a, id, p.Model)
			}
		}
	}
	return ""
}

// carriesWords says what an existing profile carries that a standard row
// (automatic, default KV type, no flash attention, no extra arguments, one
// slot, the need from the store) would not.
func carriesWords(p *core.Profile) []string {
	var w []string
	if p.KVQuant != "" && p.KVQuant != "f16" {
		w = append(w, "KV type "+p.KVQuant)
	}
	if p.FlashAttn {
		w = append(w, "flash attention")
	}
	var other []string
	args := p.ExtraArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--chat-template-kwargs" && i+1 < len(args) && thinkingOff(args[i+1]) {
			w = append(w, "thinking off (the chat-template argument)")
			i++
			continue
		}
		if strings.HasPrefix(a, "--chat-template-kwargs=") && thinkingOff(a) {
			w = append(w, "thinking off (the chat-template argument)")
			continue
		}
		other = append(other, a)
	}
	if len(other) > 0 {
		w = append(w, "extra arguments "+strings.Join(other, " "))
	}
	if p.Parallel > 1 {
		w = append(w, fmt.Sprintf("%d slots", p.Parallel))
	}
	if p.NeedsMiB > 0 {
		w = append(w, fmt.Sprintf("a hand-written need of %s MiB", commas(p.NeedsMiB)))
	}
	if len(p.ForceNodes) > 0 {
		w = append(w, "forced zones ("+strings.Join(p.ForceNodes, ", ")+")")
	}
	if len(p.Aliases) > 0 {
		w = append(w, "aliases ("+strings.Join(p.Aliases, ", ")+")")
	}
	if p.Host == core.HostNode {
		w = append(w, "hosted on a node")
	}
	return w
}

// thinkingOff: a chat-template argument that turns thinking off.
func thinkingOff(a string) bool {
	a = strings.ReplaceAll(a, " ", "")
	return strings.Contains(a, "\"enable_thinking\":false")
}

// shapeWords is how p's shape differs from a standard row's, in words (""
// when it does not: the needs store would have given its figure).
func shapeWords(p *core.Profile) (theirs, ours string) {
	var t, o []string
	if p.KVQuant != "" && p.KVQuant != "f16" {
		t, o = append(t, "KV "+p.KVQuant), append(o, "the default KV type (f16)")
	}
	if p.FlashAttn {
		t, o = append(t, "flash attention"), append(o, "no flash attention")
	}
	if len(p.ExtraArgs) > 0 {
		t, o = append(t, "extra arguments "+strings.Join(p.ExtraArgs, " ")), append(o, "no extra arguments")
	}
	if p.Parallel > 1 {
		t, o = append(t, fmt.Sprintf("%d slots", p.Parallel)), append(o, "one slot")
	}
	if p.Host == core.HostNode {
		t, o = append(t, "hosted on a node"), append(o, "the scheduler's machine as host")
	}
	return strings.Join(t, ", "), strings.Join(o, ", ")
}

// standardRow is one candidate of the standard set.
type standardRow struct {
	ID  string `json:"id"`
	Ctx int    `json:"ctx"`
	// State is one of fits, fits-by-estimate, estimate-does-not-fit,
	// does-not-fit, need-unknown (top of the file).
	State string `json:"state"`
	// Kept: it fits on an idle fleet. Reason says why not when it is not.
	Kept   bool   `json:"kept"`
	Reason string `json:"reason,omitempty"`
	// Selected: written by a POST that names no ids (the default
	// selection); Tickable: a POST may name it.
	Selected bool `json:"selected"`
	Tickable bool `json:"tickable"`
	// Action is the plan on the fleet as it is now; NowNote says it in
	// words when it is not a plain start (it would unload, or wait).
	Action  string `json:"action"`
	NowNote string `json:"now_note,omitempty"`
	// Zones are the zones it would use on an idle fleet (when kept; else
	// the plan's); MayUse how many it may use now, ByPool when the setting
	// max_pool set that number.
	Zones  []string `json:"zones"`
	MayUse int      `json:"zones_may_use"`
	ByPool bool     `json:"zones_may_use_by_max_pool"`
	// NeedMiB is the plan's need (null when unknown): the figure /api/plan
	// shows for the written profile. FitNeedMiB is what the fit rules use
	// (with the estimate margin); NeedNote says so when they differ.
	// NeedSource: "measured for this shape", "derived", "estimate" or
	// "unknown"; NeedBasis the plan's own words.
	NeedMiB    *int   `json:"need_mib"`
	FitNeedMiB *int   `json:"fit_need_mib"`
	NeedNote   string `json:"need_note,omitempty"`
	NeedSource string `json:"need_source"`
	// ZonesNeed* is the need on the number of zones the fit was decided on
	// (ZonesNeedCount: the idle placement's, else the most it may use): the
	// figure the placer compares and the state is about. ZonesNeedNote names
	// both figures when it differs from NeedMiB.
	ZonesNeedCount  int    `json:"zones_need_count"`
	ZonesNeedMiB    *int   `json:"zones_need_mib"`
	ZonesNeedSource string `json:"zones_need_source"`
	ZonesNeedBasis  string `json:"zones_need_basis,omitempty"`
	ZonesNeedNote   string `json:"zones_need_note,omitempty"`
	NeedBasis       string `json:"need_basis,omitempty"`
	// ShapeNote: an estimate row of a file with an existing profile measured
	// or written by hand in another shape names that profile's figure.
	ShapeNote string `json:"shape_note,omitempty"`
	// UsesEveryZone: it fits only by using every zone it may use.
	UsesEveryZone bool   `json:"uses_every_zone"`
	EveryZoneNote string `json:"every_zone_note,omitempty"`
	// Host is where llama-server would run; HostMemoryNote the restore
	// points and prompt cache there, counted or not.
	Host              string `json:"host"`
	HostMemoryMiB     *int   `json:"host_memory_mib"`
	HostMemoryCounted bool   `json:"host_memory_counted"`
	HostMemoryNote    string `json:"host_memory_note,omitempty"`
	// Exists: the id (or an alias) is taken, or the family has this
	// context; ExistsAs names it, ExistsNote is the sentence, Carries what
	// the existing profile has that a standard row would not. Never edited.
	Exists     bool     `json:"exists"`
	ExistsAs   string   `json:"exists_as,omitempty"`
	ExistsNote string   `json:"exists_note,omitempty"`
	Carries    []string `json:"exists_carries,omitempty"`
	// Default: this row is `<short>`, the default context.
	Default bool `json:"default"`
}

// standardSet is the GET answer.
type standardSet struct {
	File string `json:"file"`
	// Short is the name the rows use; ShortSource "family" (the file's
	// production family), "file" (the file's name) or "asked" (?short=);
	// Family is the family's base name and FamilyIDs the file's production
	// ids (the family: one of them having a context makes that row exist);
	// ShortProblem why Short cannot be used (nothing can be written then).
	Short        string        `json:"short"`
	ShortSource  string        `json:"short_source"`
	Family       string        `json:"family,omitempty"`
	FamilyIDs    []string      `json:"family_ids,omitempty"`
	ShortProblem string        `json:"short_problem,omitempty"`
	TrainedCtx   int           `json:"trained_ctx"`
	TrainedSet   bool          `json:"trained_ctx_known"`
	Note         string        `json:"note"`
	DefaultID    string        `json:"default_id,omitempty"`
	DefaultCtx   int           `json:"default_ctx,omitempty"`
	MaxPool      int           `json:"max_pool"`
	Rows         []standardRow `json:"rows"`
	// ToWrite is the default selection: what a POST naming no ids writes.
	ToWrite []string `json:"to_write"`
}

// commas writes n with thousands separators ("10,240").
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// hostMemorySentence is the plan's host memory in plain words: "restore
// points and prompt cache on <machine>: up to N MiB, not counted (...)".
func hostMemorySentence(hm *scheduler.HostMemory) string {
	if hm == nil {
		return ""
	}
	where := hm.Machine
	if hm.OnNode || where == "" {
		where = "the host node"
	}
	bound := "up to"
	if hm.SizeUnknown {
		bound = "at least"
	}
	counted := "not counted in what it needs"
	if hm.Counted {
		counted = "counted in what it needs"
	}
	s := fmt.Sprintf("restore points and prompt cache on %s: %s %s MiB, %s", where, bound, commas(hm.TermMiB()), counted)
	if hm.SizeUnknown {
		return s + fmt.Sprintf(" (prompt cache %s MiB; restore points not included: their size is read at the first load)", commas(hm.PromptCacheMiB))
	}
	return s + fmt.Sprintf(" (restore points %s MiB, prompt cache %s MiB)", commas(hm.CheckpointsMiB), commas(hm.PromptCacheMiB))
}

// needSource names where a plan's need comes from. A derived need is
// placer.Measured too (needcount.go at), so derived is asked first: the
// own slope (DerivedSlope) or the formula step from another context
// (DerivedFromCtx).
func needSource(r scheduler.PlanReport) string {
	switch {
	case r.NeedMiB <= 0:
		return "unknown"
	case r.DerivedFromCtx > 0 || r.DerivedSlope != nil:
		return "derived"
	case r.Measured:
		return "measured for this shape"
	}
	return "estimate"
}

// zonesSource names where the need on the decided zone count comes from.
func zonesSource(cp scheduler.CandidatePlan) string {
	switch {
	case !cp.ZonesNeedKnown:
		return "unknown"
	case scheduler.DerivedSource(cp.ZonesNeedSource):
		return "derived"
	case scheduler.MeasuredSource(cp.ZonesNeedSource):
		return "measured for this shape"
	}
	return "estimate"
}

// sourceWords is a need source in the row's words.
var sourceWords = map[string]string{"measured for this shape": "measured need", "derived": "need derived from other contexts of this model", "estimate": "file estimate"}

// zonesNeedNote names the two figures of a row whose plan figure (on the
// count of the latest measurement) is not the one the fit was decided on.
func zonesNeedNote(r scheduler.PlanReport, cp scheduler.CandidatePlan, kept bool) string {
	plan := fmt.Sprintf("/api/plan shows %s MiB, the need on the count of devices its figure rests on (%s)", commas(r.NeedMiB), r.NeedBasis)
	src := map[string]string{"measured for this shape": "measured", "derived": "derived", "estimate": "the file estimate"}[zonesSource(cp)]
	if kept {
		return fmt.Sprintf("%s; on the %s it would use, its need is %s MiB (%s): the figure the fit was decided on", plan, plural(cp.ZonesCount, "zone", "zones"), commas(cp.ZonesNeedMiB), src)
	}
	most := "it may use at most " + plural(cp.ZonesCount, "zone", "zones")
	if cp.ByPool {
		most += " (the setting max_pool)"
	}
	out := fmt.Sprintf("%s; with nothing loaded %s, and on %d its need is %s MiB (%s): the figure the fit was decided on", plan, most, cp.ZonesCount, commas(cp.ZonesNeedMiB), src)
	// which of the two the plan's own message quotes: a capacity refusal
	// quotes the need on the zones it may use (placer.go Capacity, req.For),
	// a "does not fit" the need before a count is chosen (placer.go place)
	switch {
	case strings.Contains(r.Reason, fmt.Sprintf("need %d MiB", cp.ZonesNeedMiB)):
		out += "; the plan's message quotes this one"
	case strings.Contains(r.Reason, fmt.Sprintf("need %d MiB", r.NeedMiB)):
		out += fmt.Sprintf("; the plan's message quotes %s MiB, the need before a zone count is chosen", commas(r.NeedMiB))
	}
	return out
}

// everyZoneSentence: a row that uses every zone it may use.
func everyZoneSentence(used, mayUse int, byPool bool) string {
	if byPool {
		return fmt.Sprintf("this context fits only by using every zone it may use (%d: the setting max_pool): nothing else can be placed beside it", mayUse)
	}
	return fmt.Sprintf("this context fits only by using every zone it may use (%d of %d reachable): nothing else can be placed beside it", used, mayUse)
}

// standardSpec is what the POST writes for a row: automatic placement,
// stage production explicit, description "standard set"; no needs_mib, no
// force_nodes, no extra arguments, generated left out.
func standardSpec(file string, ctx int) *config.ProfileSpec {
	c := ctx
	return &config.ProfileSpec{Model: file, Ctx: &c, Stage: config.StageProduction, Description: standardDescription}
}

// standardProfile is the profile the read plans for a row: the spec as the
// configuration builds it (config ProfileSpec.toCore: KV f16, one slot).
func standardProfile(id, file string, ctx int) *core.Profile {
	return &core.Profile{Name: id, Kind: core.KindLlama, Model: file, Ctx: ctx, Parallel: 1, KVQuant: "f16",
		Description: standardDescription, Stage: config.StageProduction}
}

// errShort is a refused name (400).
type errShort struct{ msg string }

func (e errShort) Error() string { return e.msg }

// knownNeed is an existing profile of the file with a need not taken from
// the estimate: measured, or written by hand.
type knownNeed struct {
	p   *core.Profile
	mib int
	how string // "measured" or "written by hand"
}

// buildStandardSet reads the catalog file and plans every candidate under
// asked (the ?short= name; "" for the prefill). An asked name that cannot
// be used is an errShort; a prefill that cannot is ShortProblem.
func (s *Server) buildStandardSet(ctx context.Context, file, asked string) (*standardSet, int, error) {
	snap, err := s.catalogGet(ctx)
	if err != nil {
		return nil, 0, err
	}
	var mf *ModelFile
	for i := range snap.in.Files {
		if snap.in.Files[i].File == file {
			mf = &snap.in.Files[i]
			break
		}
	}
	if mf == nil {
		return nil, http.StatusNotFound, fmt.Errorf("no model file %s: see GET /api/models", file)
	}
	if s.sched == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the scheduler is not available")
	}
	cfg := s.conf()
	set := &standardSet{File: mf.File, TrainedCtx: mf.ContextLength, TrainedSet: mf.ContextLength > 0,
		Note: trainedNote(mf.ContextLength), MaxPool: max(cfg.Defaults.MaxPool, 1), Rows: []standardRow{}, ToWrite: []string{}}

	// the file's profiles; its production ones are the family
	var mine []*core.Profile
	var prod []string
	for _, id := range sortedKeys(cfg.Profiles) {
		p := cfg.Profiles[id]
		if p.Model != mf.File {
			continue
		}
		mine = append(mine, p)
		if st, _ := config.EffectiveStage(p); st == config.StageProduction {
			prod = append(prod, id)
		}
	}
	set.Family, set.FamilyIDs = familyBase(prod), prod
	switch {
	case asked != "":
		set.Short, set.ShortSource = asked, "asked"
		if why := shortProblem(cfg, mf.File, asked); why != "" {
			return nil, http.StatusBadRequest, errShort{why}
		}
	case set.Family != "":
		set.Short, set.ShortSource = set.Family, "family"
	default:
		set.Short, set.ShortSource = fileShort(*mf, snap.in.Files), "file"
	}
	if asked == "" {
		set.ShortProblem = shortProblem(cfg, mf.File, set.Short)
	}

	ctxs := standardCtxs(mf.ContextLength)
	// Plan under the `<short>:<N>k` ids; the default's id is set after.
	ps := make([]*core.Profile, len(ctxs))
	for i, c := range ctxs {
		ps[i] = standardProfile(set.Short+":"+ctxTag(c), mf.File, c)
	}
	plans, err := s.sched.PlanCandidates(ctx, ps)
	if err != nil {
		return nil, 0, err
	}
	// the file's profiles with a need that is not the estimate, for the
	// shape notes (D): never carried across shapes, only named
	var known []knownNeed
	for _, p := range mine {
		if p.NeedsMiB > 0 {
			known = append(known, knownNeed{p, p.NeedsMiB, "written by hand"})
			continue
		}
		if rep, err := s.sched.Plan(ctx, p.Name); err == nil && rep.Measured && rep.NeedMiB > 0 {
			known = append(known, knownNeed{p, rep.NeedMiB, "measured"})
		}
	}

	def := -1
	for i, cp := range plans {
		r := cp.Report
		row := standardRow{ID: ps[i].Name, Ctx: ctxs[i], Action: string(r.Action), Zones: cp.Zones, MayUse: cp.MayUse, ByPool: cp.ByPool,
			NeedSource: needSource(r), NeedBasis: r.NeedBasis, Host: r.Host, Kept: cp.IdleFits,
			ZonesNeedCount: cp.ZonesCount, ZonesNeedSource: zonesSource(cp), ZonesNeedBasis: cp.ZonesNeedBasis}
		if cp.ZonesNeedKnown {
			z := cp.ZonesNeedMiB
			row.ZonesNeedMiB = &z
		}
		on := fmt.Sprintf(" (%s MiB on %s)", commas(cp.ZonesNeedMiB), plural(cp.ZonesCount, "zone", "zones"))
		if row.Kept {
			row.Zones = cp.IdleZones
		}
		says := ""
		if r.Action == scheduler.PlanReject && r.Reason != "" {
			says = "; the plan says: " + r.Reason
		}
		switch zs := row.ZonesNeedSource; {
		case row.NeedSource == "unknown" || zs == "unknown":
			row.State, row.Kept, row.Reason = stdNeedUnknown, false, r.Reason // "memory need unknown: ...": nothing was placed
		case zs == "estimate" && row.Kept:
			row.State = stdFitsEstimate
		case zs == "estimate":
			row.State = stdEstimateNoFit
			row.Reason = "not measured: by the file estimate it does not fit even with nothing loaded" + on + says
		case row.Kept && zs == "derived":
			row.State = stdFitsDerived
		case row.Kept:
			row.State = stdFits
		default:
			row.State = stdNoFit
			row.Reason = "by the " + sourceWords[zs] + " it does not fit even with nothing loaded" + on + says
		}
		if row.ZonesNeedMiB != nil && r.NeedMiB != *row.ZonesNeedMiB {
			row.ZonesNeedNote = zonesNeedNote(r, cp, row.Kept)
		}
		switch r.Action {
		case scheduler.PlanEvict:
			vs := make([]string, len(r.Victims))
			for j, v := range r.Victims {
				vs[j] = v.Profile
			}
			row.NowNote = "fits only when other models are unloaded: now it would first unload " + strings.Join(vs, ", ")
		case scheduler.PlanQueue:
			row.NowNote = "now it would wait: " + r.Reason
		case scheduler.PlanReject:
			if row.Kept {
				row.NowNote = "now it is refused: " + r.Reason
			}
		}
		if row.Zones == nil {
			row.Zones = []string{}
		}
		if row.Host == "" {
			row.Host = core.HostServer
		}
		if r.NeedMiB > 0 {
			n, f := r.NeedMiB, r.FitNeedMiB
			row.NeedMiB, row.FitNeedMiB = &n, &f
			if f != n {
				row.NeedNote = fmt.Sprintf("the fit uses %s MiB: the estimate with its 5%% margin", commas(f))
			}
		}
		if row.NeedSource == "estimate" || row.ZonesNeedSource == "estimate" {
			row.ShapeNote = shapeNote(known, ctxs[i])
		}
		if row.Kept && len(row.Zones) > 0 && len(row.Zones) >= cp.MayUse {
			row.UsesEveryZone = true
			row.EveryZoneNote = everyZoneSentence(len(row.Zones), cp.MayUse, cp.ByPool)
		}
		if hm := r.HostMemory; hm != nil {
			t := hm.TermMiB()
			row.HostMemoryMiB, row.HostMemoryCounted = &t, hm.Counted
			row.HostMemoryNote = hostMemorySentence(hm)
		}
		if row.Kept && ctxs[i] <= standardDefaultMax {
			def = i
		}
		set.Rows = append(set.Rows, row)
	}
	if def >= 0 {
		set.Rows[def].ID, set.Rows[def].Default = set.Short, true
		set.DefaultID, set.DefaultCtx = set.Short, ctxs[def]
	}
	for i := range set.Rows {
		row := &set.Rows[i]
		s.standardExists(cfg, mf.File, mine, row)
		row.Tickable = !row.Exists && set.ShortProblem == "" && (row.Kept || row.State == stdEstimateNoFit)
		row.Selected = row.Tickable && row.Kept
		if row.Selected {
			set.ToWrite = append(set.ToWrite, row.ID)
		}
	}
	return set, 0, nil
}

// standardExists marks row as existing: its id or an alias is taken, or a
// production profile of the file (the family, whatever its id:
// gemma-4-12b-thinking at 32k counts) already has its context.
func (s *Server) standardExists(cfg *config.Config, file string, mine []*core.Profile, row *standardRow) {
	prodAt := func(skip string) *core.Profile {
		for _, q := range mine {
			if st, _ := config.EffectiveStage(q); st == config.StageProduction && q.Name != skip && q.Ctx == row.Ctx {
				return q
			}
		}
		return nil
	}
	if p, alias, ok := cfg.Resolve(row.ID); ok {
		row.Exists, row.ExistsAs, row.Carries = true, p.Name, carriesWords(p)
		switch {
		case alias:
			row.ExistsNote = fmt.Sprintf("%s is an alias of the profile %s, kept as it is", row.ID, p.Name)
		case p.Model == file && p.Ctx != row.Ctx && row.Default:
			row.ExistsNote = fmt.Sprintf("%s exists at %s, kept as it is; the standard default would be %s", p.Name, ctxTag(p.Ctx), ctxTag(row.Ctx))
		case p.Model == file && p.Ctx != row.Ctx:
			row.ExistsNote = fmt.Sprintf("%s exists at %s, kept as it is; this row would be %s", p.Name, ctxTag(p.Ctx), ctxTag(row.Ctx))
		case p.Model != file:
			row.ExistsNote = fmt.Sprintf("%s is a profile of another file (%s), kept as it is", p.Name, p.Model)
		default:
			row.ExistsNote = "exists, kept as it is"
		}
		if q := prodAt(p.Name); q != nil && p.Ctx != row.Ctx { // the family has this context under another id
			row.ExistsNote += fmt.Sprintf(" (the family has %s as %s)", ctxTag(row.Ctx), q.Name)
		}
		return
	}
	if q := prodAt(""); q != nil {
		row.Exists, row.ExistsAs, row.Carries = true, q.Name, carriesWords(q)
		row.ExistsNote = fmt.Sprintf("the family has %s as %s, kept as it is", ctxTag(row.Ctx), q.Name)
	}
}

// shapeNote names the existing profile of the file nearest to ctx whose
// need is measured or written by hand, when its shape differs from a
// standard row's: its figure is not this row's (D).
func shapeNote(known []knownNeed, ctx int) string {
	var best *knownNeed
	for i := range known {
		k := &known[i]
		if theirs, _ := shapeWords(k.p); theirs == "" {
			continue // same shape: the store's own figure would have been used
		}
		if best == nil || absInt(k.p.Ctx-ctx) < absInt(best.p.Ctx-ctx) || (absInt(k.p.Ctx-ctx) == absInt(best.p.Ctx-ctx) && k.p.Name < best.p.Name) {
			best = k
		}
	}
	if best == nil {
		return ""
	}
	theirs, ours := shapeWords(best.p)
	return fmt.Sprintf("the existing %s (%s) runs at %s MiB (%s) with %s; this row would use %s, so that figure is not its own",
		best.p.Name, ctxTag(best.p.Ctx), commas(best.mib), best.how, theirs, ours)
}

func absInt(n int) int { return max(n, -n) }

// sortedKeys is m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, cmp.Compare[string])
	return out
}

// standardErr answers a build error.
func (s *Server) standardErr(w http.ResponseWriter, code int, err error) {
	var es errShort
	switch {
	case errors.As(err, &es):
		apiErr(w, http.StatusBadRequest, "%s", es.msg)
	case code != 0:
		apiErr(w, code, "%s", err.Error())
	default:
		s.writeErr(w, err)
	}
}

// handleStandardSetGet is GET /api/models/{file}/standard-set[?short=].
func (s *Server) handleStandardSetGet(w http.ResponseWriter, r *http.Request, file string) {
	set, code, err := s.buildStandardSet(r.Context(), file, strings.TrimSpace(r.URL.Query().Get("short")))
	if err != nil {
		s.standardErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// handleModelPost is POST /api/models/{file}/standard-set (the only POST
// under /api/models/{key...}; the others have their own prefixes). Body:
// {comment (required, always), short (the name; the prefill when left
// out), ids (the rows to write; the read's default selection when left
// out)}. Every id must be a row of this call that may be ticked.
func (s *Server) handleModelPost(w http.ResponseWriter, r *http.Request) {
	file, ok := standardPath(r.PathValue("key"))
	if !ok {
		apiErr(w, http.StatusNotFound, "no such route: POST /api/models/%s (want {file}/standard-set)", r.PathValue("key"))
		return
	}
	sv := s.svc(w)
	if sv == nil {
		return
	}
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if comment == "" {
		comment = audit.FromRequest(r).Comment
	}
	// refused always, also when nothing is left to write (ruling E)
	if strings.TrimSpace(comment) == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	short, _ := obj["short"].(string)
	if v, has := obj["short"]; has && v != nil {
		if _, isStr := v.(string); !isStr {
			apiErr(w, http.StatusBadRequest, "short must be a string")
			return
		}
	}
	var ids []string
	idsGiven := false
	if v, has := obj["ids"]; has && v != nil {
		list, isList := v.([]any)
		if !isList {
			apiErr(w, http.StatusBadRequest, "ids must be a list of profile ids")
			return
		}
		idsGiven = true
		for _, x := range list {
			id, isStr := x.(string)
			if !isStr {
				apiErr(w, http.StatusBadRequest, "ids must be a list of profile ids")
				return
			}
			ids = append(ids, id)
		}
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	set, code, err := s.buildStandardSet(r.Context(), file, strings.TrimSpace(short))
	if err != nil {
		s.standardErr(w, code, err)
		return
	}
	if set.ShortProblem != "" {
		apiErr(w, http.StatusBadRequest, "%s", set.ShortProblem)
		return
	}
	if !idsGiven {
		ids = set.ToWrite
	}
	byID := map[string]*standardRow{}
	for i := range set.Rows {
		byID[set.Rows[i].ID] = &set.Rows[i]
	}
	want := map[string]bool{}
	for _, id := range ids {
		row := byID[id]
		switch {
		case row == nil:
			apiErr(w, http.StatusBadRequest, "%s is not one of this call's rows (%s)", id, strings.Join(rowIDs(set.Rows), ", "))
			return
		case row.Exists:
			apiErr(w, http.StatusBadRequest, "%s cannot be written: %s", id, row.ExistsNote)
			return
		case !row.Tickable:
			apiErr(w, http.StatusBadRequest, "%s cannot be written: %s", id, row.Reason)
			return
		}
		want[id] = true
	}
	var nps []cfgsvc.NewProfile
	for _, row := range set.Rows {
		if want[row.ID] {
			nps = append(nps, cfgsvc.NewProfile{ID: row.ID, Spec: standardSpec(set.File, row.Ctx)})
		}
	}
	type skip struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	skipped := []skip{}
	for _, row := range set.Rows {
		switch {
		case want[row.ID]:
		case row.Exists:
			skipped = append(skipped, skip{row.ID, row.ExistsNote})
		case row.Tickable:
			skipped = append(skipped, skip{row.ID, "not chosen"})
		default:
			skipped = append(skipped, skip{row.ID, "not kept: " + row.Reason})
		}
	}
	res, written, taken, err := sv.CreateMissing(r.Context(), o, nps)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	for _, id := range taken { // made between the read and the write
		skipped = append(skipped, skip{id, "exists, kept as it is"})
	}
	if written == nil {
		written = []string{}
	}
	s.respondWrite(w, http.StatusOK, res, map[string]any{"file": set.File, "short": set.Short, "written": written, "skipped": skipped, "rows": set.Rows})
}

func rowIDs(rows []standardRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
