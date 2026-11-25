package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
)

// Run-scoped profiles (profiles plan, slice A). A run-scoped profile is a
// production profile with a run's settings and zones, held in memory under
// a name of its own, "<production id>@<scope>". Its own name gives it its
// own instance, queue entry, back-off and refusals, so a benchmark at the
// production shape runs beside production traffic and neither is admitted
// to the other's instance. It is never written to the configuration.
//
// It is made by POST /api/load with a scope (ScopedLoad); POST /api/plan
// with the same body plans it and changes nothing. It lives while its queue
// entry or instance does: once neither holds it (refused, unloaded, stopped)
// it is forgotten, as a pinned copy is (pin.go), and the name answers 404
// again. History events and request log rows name the production id and
// carry the scope (data.scope).

// ScopeSep separates the production id from the scope in a scoped name.
const ScopeSep = "@"

// ReasonScopedConflict: a scoped name is in use with other settings.
const ReasonScopedConflict Reason = "scoped name in use" // 409

// ReasonBadScopedLoad: a scoped load's body is not valid.
const ReasonBadScopedLoad Reason = "invalid scoped load" // 400

// MaxScope is the longest scope.
const MaxScope = 64

// ScopedLoad is the body of a scoped /api/load or /api/plan: the production
// profile (an id or alias), the scope, and the run's settings. A nil
// setting keeps the production profile's.
type ScopedLoad struct {
	Model     string
	Scope     string
	Nodes     []string // the zones, in device order (empty: placed as the profile says)
	Ctx       *int
	Parallel  *int
	KVQuant   *string
	FlashAttn *bool
	Host      *string // "server" or "node"
	Thinking  *bool   // false adds --reasoning-budget 0; true removes it
}

// Set reports whether the body asks for a scoped load (a scope or any of
// the run's settings); without it /api/load is the plain or pinned load.
func (r ScopedLoad) Set() bool {
	return r.Scope != "" || r.Ctx != nil || r.Parallel != nil || r.KVQuant != nil || r.FlashAttn != nil || r.Host != nil || r.Thinking != nil
}

// ScopedName is "<production id>@<scope>".
func ScopedName(production, scope string) string { return production + ScopeSep + scope }

// SplitScoped splits a scoped name into the production id and the scope.
func SplitScoped(name string) (production, scope string, ok bool) {
	i := strings.LastIndex(name, ScopeSep)
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	return name[:i], name[i+1:], true
}

func badScoped(format string, a ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Reason: ReasonBadScopedLoad, Message: fmt.Sprintf(format, a...)}
}

// validScope: 1 to 64 letters, digits, dot, dash or underscore (as a bench
// run id).
func validScope(s string) bool {
	if s == "" || len(s) > MaxScope {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

var thinkingOff = []string{"--reasoning-budget", "0"}

// withoutThinkingOff drops every "--reasoning-budget 0" pair.
func withoutThinkingOff(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if i+1 < len(args) && args[i] == thinkingOff[0] && args[i+1] == thinkingOff[1] {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// scopedProfile builds the scoped profile of production p for r, or the
// 400 that says what is wrong. cfg is the configuration it is checked
// against (a name that is a configured id or alias is refused).
func scopedProfile(cfg *config.Config, p *core.Profile, r ScopedLoad) (*core.Profile, *Error) {
	if !validScope(r.Scope) {
		return nil, badScoped("scope must be 1 to %d letters, digits, dots, dashes or underscores, got %q", MaxScope, r.Scope)
	}
	if p.Kind != core.KindLlama {
		return nil, badScoped("%s is a command profile: a scoped load is for llama profiles", p.Name)
	}
	if err := modelpath.Check(p.Model); err != nil { // the shared rule, as for any profile
		return nil, badScoped("%s: model %s", p.Name, err)
	}
	name := ScopedName(p.Name, r.Scope)
	if _, _, taken := cfg.Resolve(name); taken {
		return nil, badScoped("%s is a configured profile id or alias", name)
	}
	cp := *p
	if len(r.Nodes) > 0 {
		cp = *pinCopy(p, r.Nodes)
	} else {
		cp.ForceNodes, cp.TensorSplit = slices.Clone(p.ForceNodes), slices.Clone(p.TensorSplit)
	}
	cp.Name = name
	cp.Aliases = nil
	cp.AllowTiers = slices.Clone(p.AllowTiers)
	cp.ExtraArgs = slices.Clone(p.ExtraArgs)
	if r.Ctx != nil {
		if *r.Ctx <= 0 {
			return nil, badScoped("ctx must be positive, got %d", *r.Ctx)
		}
		cp.Ctx = *r.Ctx
	}
	if r.Parallel != nil {
		if *r.Parallel <= 0 {
			return nil, badScoped("parallel must be positive, got %d", *r.Parallel)
		}
		cp.Parallel = *r.Parallel
	}
	if r.FlashAttn != nil {
		cp.FlashAttn = *r.FlashAttn
	}
	if r.KVQuant != nil {
		cp.KVQuant = *r.KVQuant
	}
	if r.KVQuant != nil || r.FlashAttn != nil { // the production profile's own pair is valid already
		switch cp.KVQuant {
		case "", "f16":
		case "q8_0", "q4_0":
			if !cp.FlashAttn {
				return nil, badScoped("kv_quant %s requires flash_attn: true", cp.KVQuant)
			}
		default:
			return nil, badScoped("kv_quant: unknown value %q (want f16, q8_0 or q4_0)", cp.KVQuant)
		}
	}
	if r.Host != nil {
		switch *r.Host {
		case "", core.HostServer:
			cp.Host = ""
		case core.HostNode:
			cp.Host = core.HostNode
		default:
			return nil, badScoped("host: unknown value %q (want server or node)", *r.Host)
		}
	}
	if cp.Host == core.HostNode {
		if len(cp.ForceNodes) == 0 {
			return nil, badScoped("host node requires zones (nodes); the first one hosts")
		}
		if !cfg.AgentsMode() {
			return nil, badScoped("host node requires membership: agents")
		}
	}
	if r.Thinking != nil {
		cp.ExtraArgs = withoutThinkingOff(cp.ExtraArgs)
		if !*r.Thinking {
			cp.ExtraArgs = append(cp.ExtraArgs, thinkingOff...)
		}
	}
	// A written need (needs_mib) belongs to the production shape: a copy of
	// another shape plans from the estimate or a measurement of its own
	// shape (writtenNeedDropped says so in its plan).
	if shapeOf(&cp) != shapeOf(p) {
		cp.NeedsMiB = 0
	}
	return &cp, nil
}

// writtenNeedDropped is the plan's note for a scoped profile that did not
// take its production profile's written need ("" when it did, or there was
// none).
func (c *schedCore) writtenNeedDropped(p *core.Profile) string {
	base, _, ok := c.scopeOf(p.Name)
	if !ok {
		return ""
	}
	if bp := c.cfg.Profiles[base]; bp != nil && bp.NeedsMiB > 0 && p.NeedsMiB == 0 && shapeOf(bp) != shapeOf(p) {
		return "; written need not used: shape differs from " + base
	}
	return ""
}

// sameScoped reports whether two scoped profiles load the same way.
func sameScoped(a, b *core.Profile) bool {
	x, y := *a, *b
	x.Description, y.Description = "", ""
	return reflect.DeepEqual(x, y)
}

// profile is the profile named name: a configured one, else a run-scoped one.
func (c *schedCore) profile(name string) *core.Profile {
	if p := c.cfg.Profiles[name]; p != nil {
		return p
	}
	return c.scoped[name]
}

// scopeOf is the production id and scope of a scoped name (not a
// configured profile).
func (c *schedCore) scopeOf(name string) (production, scope string, ok bool) {
	if c.cfg.Profiles[name] != nil {
		return "", "", false
	}
	return SplitScoped(name)
}

// addScoped makes sp current under its name, or refuses: 409 when the name
// is held with other settings, the pin refusal when a chosen zone may not
// take a model now. It returns the profile to use (the one held already
// when it is the same).
func (c *schedCore) addScoped(sp *core.Profile) (*core.Profile, *Error) {
	if old := c.scoped[sp.Name]; old != nil {
		if !sameScoped(old, sp) {
			return nil, &Error{Status: http.StatusConflict, Reason: ReasonScopedConflict,
				Message: fmt.Sprintf("%s is loaded or queued with other settings: unload it first, or use another scope", sp.Name)}
		}
		return old, nil
	}
	if c.insts[sp.Name] != nil || c.entries[sp.Name] != nil { // not held, yet present: never expected
		return nil, &Error{Status: http.StatusConflict, Reason: ReasonScopedConflict, Message: sp.Name + " is in use"}
	}
	if len(sp.ForceNodes) > 0 {
		if err := c.pinUnusable(sp, sp.ForceNodes); err != nil {
			return nil, err
		}
	}
	c.forgetScopedNeed(sp.Name) // a name used before may have planned another shape
	c.scoped[sp.Name] = sp
	return sp, nil
}

// fitRule is the forced fit check that applies to p, and why, in the words
// the refusal and the plan carry. A scoped profile is always held to reject on
// the zones it names, whatever defaults.forced_fit_check says (reviewer's
// ruling: its shape is new, so nothing vouches for it); a production profile
// follows the setting as before.
func (c *schedCore) fitRule(p *core.Profile) (mode, rule string) {
	if _, _, ok := c.scopeOf(p.Name); ok {
		return config.ForcedFitReject, "a scoped load is always fit-checked on the zones it names"
	}
	mode = c.cfg.Defaults.ForcedFitCheck
	return mode, "defaults.forced_fit_check is " + mode
}

// shownName is a profile name as a sentence says it: "run-scoped load
// <scope> of <production id>" for a scoped name, else the name itself.
func (c *schedCore) shownName(name string) string {
	if base, scope, ok := c.scopeOf(name); ok {
		return "run-scoped load " + scope + " of " + base
	}
	return name
}

// scopedOrigin: measurement l was made by a run-scoped load (its profile is
// a scoped name, this run or restored from the store; configured ids never
// hold ScopeSep). Such a row is kept under its own shape's key, and it is
// never a source of a derived need or a model's own line (deriveIndex): a
// run's shape must not feed another shape's plan, production's above all.
func scopedOrigin(l *learnedNeed) bool {
	_, _, ok := SplitScoped(l.profile)
	return ok
}

// pruneScoped forgets the scoped profiles no entry or instance holds.
func (c *schedCore) pruneScoped() {
	for name, p := range c.scoped {
		if e := c.entries[name]; e != nil && e.prof == p {
			continue
		}
		if in := c.insts[name]; in != nil && in.prof == p {
			continue
		}
		delete(c.scoped, name)
		c.forgetScopedNeed(name)
	}
}

// NeedForgetter is implemented by a NeedProvider that caches by profile
// name: a scoped name may come back with other settings.
type NeedForgetter interface{ Forget(name string) }

func (c *schedCore) forgetScopedNeed(name string) {
	delete(c.needs_, name)
	delete(c.warnedLow, name)
	if f, ok := c.needs.(NeedForgetter); ok {
		f.Forget(name)
	}
}

// scopedPlan is the plan of a scoped load; nothing changes.
func (c *schedCore) scopedPlan(sp *core.Profile) PlanReport {
	if old := c.scoped[sp.Name]; old != nil {
		if !sameScoped(old, sp) {
			return PlanReport{Profile: sp.Name, Action: PlanReject,
				Reason: fmt.Sprintf("%s is loaded or queued with other settings", sp.Name)}
		}
		return c.dryPlanOf(sp.Name, old)
	}
	if len(sp.ForceNodes) > 0 {
		if e := c.pinUnusable(sp, sp.ForceNodes); e != nil {
			return PlanReport{Profile: sp.Name, Action: PlanReject, Reason: e.Message}
		}
	}
	return c.dryPlanOf(sp.Name, sp)
}

// resolve is Config().Resolve, else a run-scoped profile held now (from the
// snapshot of the last step).
func (s *Scheduler) resolve(model string) (*core.Profile, bool, bool) {
	if p, alias, ok := s.Config().Resolve(model); ok {
		return p, alias, true
	}
	if p := s.snap.Load().scoped[model]; p != nil {
		return p, false, true
	}
	return nil, false, false
}

// Scoped is the run-scoped profile named name, held now; false when there
// is none (a configured name is never one).
func (s *Scheduler) Scoped(name string) (*core.Profile, bool) {
	if _, _, ok := s.Config().Resolve(name); ok {
		return nil, false
	}
	p := s.snap.Load().scoped[name]
	return p, p != nil
}

// buildScoped resolves r's production profile and builds the scoped one.
func (s *Scheduler) buildScoped(r ScopedLoad) (*core.Profile, error) {
	cfg := s.Config()
	p, _, ok := cfg.Resolve(r.Model)
	if !ok {
		return nil, errUnknown(r.Model)
	}
	sp, err := scopedProfile(cfg, p, r)
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// LoadScoped is POST /api/load with a scope: load the production profile
// with the run's settings under its scoped name, which it returns. As Load
// it returns at once; benchRun tags the history records (X-Bench-Run).
func (s *Scheduler) LoadScoped(ctx context.Context, r ScopedLoad, benchRun string) (string, error) {
	sp, err := s.buildScoped(r)
	if err != nil {
		return "", err
	}
	w := &waiter{profile: sp.Name, synthetic: true, reply: make(chan answer, 1), benchRun: benchRun, scoped: sp}
	if !s.post(ctx, evAcquire{w: w}) {
		return "", errShutting()
	}
	if err := s.query(ctx, func(*schedCore) {}); err != nil {
		return "", err
	}
	select {
	case a := <-w.reply:
		if a.err != nil {
			return "", a.err
		}
	default:
	}
	return sp.Name, nil
}

// PlanScoped is POST /api/plan with a scope: the plan of LoadScoped(r);
// nothing changes.
func (s *Scheduler) PlanScoped(ctx context.Context, r ScopedLoad) (PlanReport, error) {
	sp, err := s.buildScoped(r)
	if err != nil {
		return PlanReport{}, err
	}
	return askRead(s, ctx, "plan of "+sp.Name, func(c *schedCore) PlanReport { return c.scopedPlan(sp) })
}
