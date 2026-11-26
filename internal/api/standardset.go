package api

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// The standard set of a model file (profiles plan, section 6):
//
//	GET  /api/models/{file}/standard-set   what it would write (observe)
//	POST /api/models/{file}/standard-set   write the kept rows that do not exist (settings)
//
// Candidates are 16k, 32k, 64k, 128k and 256k, each capped at the file's
// trained context (a file trained at 2,048 gets 2,048 alone; a file with no
// trained context gets 16k and 32k). Each is planned read-only
// (scheduler.PlanCandidates) and KEPT when it fits on an idle fleet (nothing
// loaded): what happens to be loaded at that moment does not decide a
// model's standard set (team-lead ruling, 26 Nov). The plan on the fleet as
// it is (start, evict, queue, reject) is shown beside it, in now and
// now_note. The default `<short>` is the largest kept context up to 32k;
// the others are `<short>:<N>k`. Nothing here runs at start, at scan, at a
// download or in a migration: only these two calls.

const standardSetSuffix = "/standard-set"

// standardSizes are the candidate contexts before the trained cap.
var standardSizes = []int{16 << 10, 32 << 10, 64 << 10, 128 << 10, 256 << 10}

// standardDefaultMax is the largest context the default `<short>` takes.
const standardDefaultMax = 32 << 10

// standardDescription is the description every standard profile gets.
const standardDescription = "standard set"

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

// ctxTag is a context as a profile id writes it: k = 1024 tokens (as the
// benchmark runner's ctxKey), else the number.
func ctxTag(ctx int) string {
	if ctx > 0 && ctx%1024 == 0 {
		return strconv.Itoa(ctx/1024) + "k"
	}
	return strconv.Itoa(ctx)
}

// slug is lower case letters and digits, any other run of characters one "-".
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// standardShort is the `<short>` of mf among files: the slug of its name
// (the GGUF metadata name when the catalog read it, else the file name),
// with the quant appended only when another file of the catalog has the
// same short name.
func standardShort(mf ModelFile, files []ModelFile) string {
	base := func(f ModelFile) string {
		if s := slug(f.Name); s != "" {
			return s
		}
		return slug(strings.TrimSuffix(path.Base(f.File), path.Ext(f.File)))
	}
	short := base(mf)
	for _, o := range files {
		if o.File != mf.File && base(o) == short {
			if q := slug(mf.Quant); q != "" {
				return short + "-" + q
			}
			return slug(strings.TrimSuffix(path.Base(mf.File), path.Ext(mf.File)))
		}
	}
	return short
}

// standardRow is one candidate of the standard set.
type standardRow struct {
	ID  string `json:"id"`
	Ctx int    `json:"ctx"`
	// Kept: it fits on an idle fleet. Reason says why not when it is not.
	Kept   bool   `json:"kept"`
	Reason string `json:"reason,omitempty"`
	// Action is the plan on the fleet as it is now; NowNote says it in
	// words when it is not a plain start (it would unload, or wait).
	Action  string `json:"action"`
	NowNote string `json:"now_note,omitempty"`
	// Zones are the zones it would use on an idle fleet (when kept; else
	// the plan's); MayUse how many it may use now.
	Zones  []string `json:"zones"`
	MayUse int      `json:"zones_may_use"`
	// NeedMiB is the need the plan used (null when unknown); NeedSource
	// "measured for this shape", "derived", "estimate" or "unknown";
	// NeedBasis the plan's own words.
	NeedMiB    *int   `json:"need_mib"`
	NeedSource string `json:"need_source"`
	NeedBasis  string `json:"need_basis,omitempty"`
	// UsesEveryZone: it fits only by using every zone it may use.
	UsesEveryZone bool   `json:"uses_every_zone"`
	EveryZoneNote string `json:"every_zone_note,omitempty"`
	// Host is where llama-server would run; HostMemoryNote the restore
	// points and prompt cache there, counted or not.
	Host              string `json:"host"`
	HostMemoryMiB     *int   `json:"host_memory_mib"`
	HostMemoryCounted bool   `json:"host_memory_counted"`
	HostMemoryNote    string `json:"host_memory_note,omitempty"`
	// Exists: a profile with this id or alias exists (ExistsAs says which);
	// it is never edited.
	Exists   bool   `json:"exists"`
	ExistsAs string `json:"exists_as,omitempty"`
	// Default: this row is `<short>`, the default context.
	Default bool `json:"default"`
}

// standardSet is the GET answer.
type standardSet struct {
	File       string        `json:"file"`
	Short      string        `json:"short"`
	TrainedCtx int           `json:"trained_ctx"`
	TrainedSet bool          `json:"trained_ctx_known"`
	Note       string        `json:"note"`
	DefaultID  string        `json:"default_id,omitempty"`
	DefaultCtx int           `json:"default_ctx,omitempty"`
	Rows       []standardRow `json:"rows"`
	// ToWrite are the ids a POST would write now (kept, not existing).
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

// needSource names where a plan's need comes from.
func needSource(r scheduler.PlanReport) string {
	switch {
	case r.NeedMiB <= 0:
		return "unknown"
	case r.Measured:
		return "measured for this shape"
	case r.DerivedFromCtx > 0 || r.DerivedSlope != nil:
		return "derived"
	}
	return "estimate"
}

// standardSpec is what the POST writes for a row: automatic placement,
// stage production explicit, description "standard set"; no needs_mib, no
// force_nodes, no extra arguments, generated left out.
func standardSpec(file string, ctx int) *config.ProfileSpec {
	c := ctx
	return &config.ProfileSpec{Model: file, Ctx: &c, Stage: config.StageProduction, Description: standardDescription}
}

// standardProfile is the profile the read plans for a row: the spec as the
// configuration would build it.
func standardProfile(id, file string, ctx int) *core.Profile {
	return &core.Profile{Name: id, Kind: core.KindLlama, Model: file, Ctx: ctx, Parallel: 1, KVQuant: "f16",
		Description: standardDescription, Stage: config.StageProduction}
}

// buildStandardSet reads the catalog file and plans every candidate.
func (s *Server) buildStandardSet(ctx context.Context, file string) (*standardSet, int, error) {
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
	set := &standardSet{File: mf.File, Short: standardShort(*mf, snap.in.Files), TrainedCtx: mf.ContextLength, TrainedSet: mf.ContextLength > 0,
		Rows: []standardRow{}, ToWrite: []string{}}
	ctxs := standardCtxs(mf.ContextLength)
	switch {
	case !set.TrainedSet:
		set.Note = "the file names no trained context: 16k and 32k are tried"
	case mf.ContextLength < standardSizes[0]:
		set.Note = fmt.Sprintf("trained at %s tokens, below 16k: only that context is tried", commas(mf.ContextLength))
	default:
		set.Note = fmt.Sprintf("trained at %s tokens: no candidate above it", commas(mf.ContextLength))
	}
	// Plan under the `<short>:<N>k` ids; the default's id is set after.
	ps := make([]*core.Profile, len(ctxs))
	for i, c := range ctxs {
		ps[i] = standardProfile(set.Short+":"+ctxTag(c), mf.File, c)
	}
	plans, err := s.sched.PlanCandidates(ctx, ps)
	if err != nil {
		return nil, 0, err
	}
	def := -1
	for i, cp := range plans {
		r := cp.Report
		row := standardRow{ID: ps[i].Name, Ctx: ctxs[i], Action: string(r.Action), Zones: cp.Zones, MayUse: cp.MayUse,
			NeedSource: needSource(r), NeedBasis: r.NeedBasis, Host: r.Host, Kept: cp.IdleFits}
		if row.Kept {
			row.Zones = cp.IdleZones
		}
		switch {
		case !row.Kept && r.Action == scheduler.PlanReject:
			row.Reason = r.Reason
		case !row.Kept:
			row.Reason = "it does not fit even with nothing loaded"
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
			n := r.FitNeedMiB
			row.NeedMiB = &n
		}
		if row.Kept && len(row.Zones) > 0 && len(row.Zones) >= cp.MayUse {
			row.UsesEveryZone = true
			row.EveryZoneNote = fmt.Sprintf("this context fits only by using every zone it may use (%d of %d): nothing else can be placed beside it", len(row.Zones), cp.MayUse)
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
	cfg := s.conf()
	for i := range set.Rows {
		row := &set.Rows[i]
		if p, alias, ok := cfg.Resolve(row.ID); ok {
			row.Exists, row.ExistsAs = true, "the profile "+row.ID
			if alias {
				row.ExistsAs = "an alias of the profile " + p.Name
			}
		}
		if row.Kept && !row.Exists {
			set.ToWrite = append(set.ToWrite, row.ID)
		}
	}
	return set, 0, nil
}

// handleStandardSetGet is GET /api/models/{file}/standard-set.
func (s *Server) handleStandardSetGet(w http.ResponseWriter, r *http.Request, file string) {
	set, code, err := s.buildStandardSet(r.Context(), file)
	if err != nil {
		if code != 0 {
			apiErr(w, code, "%s", err.Error())
			return
		}
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// handleModelPost is POST /api/models/{file}/standard-set (the only POST
// under /api/models/{key...}; the others have their own prefixes).
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
	_, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if comment == "" {
		comment = audit.FromRequest(r).Comment
	}
	if strings.TrimSpace(comment) == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	set, code, err := s.buildStandardSet(r.Context(), file)
	if err != nil {
		if code != 0 {
			apiErr(w, code, "%s", err.Error())
			return
		}
		s.writeErr(w, err)
		return
	}
	var nps []cfgsvc.NewProfile
	for _, row := range set.Rows {
		if row.Kept && !row.Exists {
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
		case row.Exists:
			skipped = append(skipped, skip{row.ID, "exists, kept as it is"})
		case !row.Kept:
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
	s.respondWrite(w, http.StatusOK, res, map[string]any{"file": set.File, "written": written, "skipped": skipped, "rows": set.Rows})
}
