package scheduler

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// The model server caps every slot at the model's trained context
// (llama.cpp tools/server/server-context.cpp n_ctx_slot: min(n_ctx_seq,
// n_ctx_train)), and says so only in its log. A profile whose ctx is above
// the trained context is therefore planned and launched AT the trained
// context: what the server can use, and the memory it then takes.
// core.Profile.CtxAsked keeps the configured ctx and the plan says so. A
// profile with ctx_above_trained keeps its ctx and is launched with
// --override-kv <arch>.context_length=int:<ctx>, which lifts the cap
// (llama-model-loader get_key honours the override).

// trainedCtxReader is a need provider that reads model headers.
type trainedCtxReader interface {
	// TrainedCtx is the model file's trained context and architecture;
	// false when the header cannot be read or names none.
	TrainedCtx(model string) (ctx int, arch string, ok bool)
}

// TrainedCtx implements trainedCtxReader (the header is cached: metaOf).
func (c *ConfigNeeds) TrainedCtx(model string) (int, string, bool) {
	c.mu.Lock()
	defer supervise.Unlocked("scheduler", c.mu.Unlock)
	path, err := modelpath.Join(c.Cfg.ModelsDir, model)
	if err != nil {
		return 0, "", false
	}
	meta, err := c.metaOf(path)
	if err != nil || meta.ContextLength == 0 {
		return 0, "", false
	}
	return int(meta.ContextLength), meta.Architecture, true
}

var _ trainedCtxReader = (*ConfigNeeds)(nil)

// realCtx is cfg with every llama profile whose ctx is above its model's
// trained context planned at the trained context (or, with
// ctx_above_trained, given the override). cfg itself is never changed: a
// changed configuration is a copy.
func realCtx(cfg *config.Config, tr trainedCtxReader) *config.Config {
	if cfg == nil || tr == nil {
		return cfg
	}
	type tc struct {
		ctx  int
		arch string
		ok   bool
	}
	seen := map[string]tc{} // one header look-up per model file, not per profile
	var out *config.Config
	for _, name := range slices.Sorted(maps.Keys(cfg.Profiles)) {
		p := cfg.Profiles[name]
		if p == nil || p.Kind != core.KindLlama || p.Ctx <= 0 || p.Model == "" {
			continue
		}
		t, hit := seen[p.Model]
		if !hit {
			t.ctx, t.arch, t.ok = tr.TrainedCtx(p.Model)
			seen[p.Model] = t
		}
		q := capCtx(p, t.ctx, t.arch, t.ok)
		if q == nil {
			continue
		}
		if out == nil {
			cp := *cfg
			cp.Profiles = maps.Clone(cfg.Profiles)
			out = &cp
		}
		out.Profiles[name] = q
	}
	if out == nil {
		return cfg
	}
	return out
}

// hasCtxOverride says whether extra args already override the trained context.
func hasCtxOverride(args []string) bool {
	for i, a := range args {
		v := ""
		switch {
		case a == "--override-kv" && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "--override-kv="):
			v = strings.TrimPrefix(a, "--override-kv=")
		}
		if strings.Contains(v, ".context_length=") {
			return true
		}
	}
	return false
}

// ctxNote is what a plan says about the context it plans: the trained-
// context cap, or a running instance whose server runs another context.
func ctxNote(p *core.Profile) string {
	if p == nil || p.CtxAsked <= 0 {
		return ""
	}
	return fmt.Sprintf("ctx %s is above this model's trained context of %s; the server runs it at %s", groupThousands(p.CtxAsked), groupThousands(p.Ctx), groupThousands(p.Ctx))
}

// groupThousands writes n with thousands separators.
func groupThousands(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + groupThousands(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ctxPad is the slot padding of the model server (llama.cpp pads the
// context per sequence to 256 tokens): a served context up to this much
// above the planned one is the same context.
const ctxPad = 256

// servedCtx records the context per slot the instance's server reports
// after the load. A read that fails is not a load failure: the instance
// says "context not confirmed". A context other than the planned one is a
// warn event, and the instance and the plan say "runs at 2,048, profile
// says 8,192".
func (c *schedCore) servedCtx(iid uint64, n int, err error) {
	in := c.instByIID(iid)
	if in == nil || in.prof.Kind != core.KindLlama {
		return
	}
	c.changed = true
	if err != nil {
		in.servedCtx, in.ctxErr = 0, "context not confirmed: "+err.Error()
		c.log.Info("the model server's context could not be read; context not confirmed", "profile", in.prof.Name, "err", err)
		return
	}
	in.servedCtx, in.ctxErr = n, ""
	if !ctxDiffers(n, in.prof.Ctx) {
		return
	}
	text := servedNote(in.prof, n)
	c.log.Warn("the model server runs another context than the plan: "+text, "profile", in.prof.Name, "ctx", in.prof.Ctx, "served_ctx", n)
	data := map[string]any{"ctx": in.prof.Ctx, "served_ctx": n, "parallel": max(in.prof.Parallel, 1)}
	if in.prof.CtxAsked > 0 {
		data["ctx_asked"] = in.prof.CtxAsked
	}
	c.recordInst(in, history.TContextMismatch, history.Warn, in.prof.Name+" "+text, data)
}

// ctxDiffers says whether a served context per slot is not the planned one
// (beyond the server's padding).
func ctxDiffers(served, planned int) bool {
	return served > 0 && planned > 0 && (served < planned || served >= planned+ctxPad)
}

// servedNote is "runs at 2,048, profile says 8,192".
func servedNote(p *core.Profile, served int) string {
	return fmt.Sprintf("runs at %s, profile says %s", groupThousands(served), groupThousands(p.Ctx))
}

// instCtx is the context per slot requests to in get: the server's own
// figure once read, else the planned one.
func instCtx(in *inst) int {
	if in.servedCtx > 0 {
		return in.servedCtx
	}
	return in.prof.Ctx
}

// instCtxStatus fills an instance's context fields: the planned ctx, the
// server's own figure, and the note (the trained-context cap, a mismatch,
// or "context not confirmed").
func instCtxStatus(s *InstanceStatus, in *inst) {
	if in.prof.Kind != core.KindLlama {
		return
	}
	s.Ctx = in.prof.Ctx
	if in.servedCtx > 0 {
		v := in.servedCtx
		s.CtxServed = &v
	}
	switch {
	case in.ctxErr != "":
		s.CtxNote = in.ctxErr
	case ctxDiffers(in.servedCtx, in.prof.Ctx):
		s.CtxNote = servedNote(in.prof, in.servedCtx)
	default:
		s.CtxNote = ctxNote(in.prof)
	}
}

// capCtx is p planned at its model's trained context (CtxAsked keeps the
// configured ctx), or with ctx_above_trained given the --override-kv that
// lifts the cap; nil when p needs neither (ctx within the trained context,
// the trained context unknown, or an override already written).
func capCtx(p *core.Profile, trained int, arch string, ok bool) *core.Profile {
	if !ok || trained <= 0 || p.Ctx <= trained {
		return nil
	}
	q := *p
	if p.CtxAboveTrained {
		if hasCtxOverride(p.ExtraArgs) || arch == "" {
			return nil
		}
		q.ExtraArgs = append(slices.Clone(p.ExtraArgs), "--override-kv", fmt.Sprintf("%s.context_length=int:%d", arch, p.Ctx))
	} else {
		q.CtxAsked, q.Ctx = p.Ctx, trained
	}
	return &q
}

// realScoped is a run-scoped profile with the same rule as a configured
// one: it is built from the written production profile with the load's own
// ctx, so a ctx above the trained context is planned, fit-checked on its
// zones and launched at the trained context, with the note.
func (c *schedCore) realScoped(sp *core.Profile) *core.Profile {
	tr, ok := c.needs.(trainedCtxReader)
	if !ok || sp == nil || sp.Kind != core.KindLlama || sp.Ctx <= 0 || sp.Model == "" {
		return sp
	}
	trained, arch, known := tr.TrainedCtx(sp.Model)
	if q := capCtx(sp, trained, arch, known); q != nil {
		return q
	}
	return sp
}
