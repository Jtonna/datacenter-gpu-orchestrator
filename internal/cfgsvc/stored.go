package cfgsvc

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/llamaflags"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelpath"
)

// A profile put back exactly as it was stored (a model restored from the
// trash, a delete undone, a rollback to a revision) is checked as a stored
// profile, never as a new write. Every exemption a stored profile has:
//
//   - the model path rule (internal/modelpath): KeepModels for the profiles
//     of the configuration being changed; KeepPaths for one stored elsewhere
//     (its file must be there);
//   - --ctx-checkpoints / --cache-ram in extra_args in a form a write now
//     refuses (llamaflags.Parse: the "=" form, a flag given twice, a bad
//     value): KeepArgs, unchanged args; the launch uses the cluster value;
//   - a ctx above the model's trained context: KeepCtx, unchanged model
//     and ctx.
//
// Each holds only while the profile keeps the stored value; an ordinary
// write (create, update, import) gets none of them for a new profile.

// storedKeep adds the stored profiles to the kept values of parse; a stored
// profile's own value wins over the configuration's for the same id (it is
// what the new document holds).
func storedKeep(parse *config.Options, stored map[string]*config.ProfileSpec) {
	if len(stored) == 0 {
		return
	}
	paths := map[string]string{}
	args := map[string][]string{}
	ctxs := map[string]config.KeptCtx{}
	for k, v := range parse.KeepArgs {
		args[k] = v
	}
	for k, v := range parse.KeepCtx {
		ctxs[k] = v
	}
	for id, p := range stored {
		if p == nil {
			continue
		}
		if p.Model != "" {
			paths[id] = p.Model
		}
		if len(p.ExtraArgs) > 0 {
			args[id] = p.ExtraArgs
		}
		if p.Ctx != nil {
			ctxs[id] = config.KeptCtx{Model: p.Model, Ctx: *p.Ctx}
		}
	}
	parse.KeepPaths, parse.KeepArgs, parse.KeepCtx = paths, args, ctxs
}

// storedWarnings says which stored profiles carry something a new write
// would refuse; the profiles are written all the same. A profile the
// configuration before already held unchanged is not named again.
func (s *Service) storedWarnings(before, after *config.Document, m mutation, cfg *config.Config) []string {
	if len(m.stored) == 0 {
		return nil
	}
	ps := map[string]*config.ProfileSpec{}
	for id := range m.stored {
		ps[id] = after.Profiles[id]
	}
	return s.storedNotes(ps, before, m.storedWhat, cfg.ModelsDir)
}

// StoredNotes says, for profiles as they are stored now (the delete's dry
// run: the profiles a later restore puts back), what a new write would
// refuse in them; nil when nothing.
func (s *Service) StoredNotes(ps map[string]*config.ProfileSpec, what string) []string {
	return s.storedNotes(ps, nil, what, s.Config().ModelsDir)
}

func (s *Service) storedNotes(ps map[string]*config.ProfileSpec, before *config.Document, what, modelsDir string) []string {
	if what == "" {
		what = "profiles put back"
	}
	type agg struct {
		ids     []string
		total   int
		trained int
	}
	above := map[string]*agg{}
	total := map[string]int{}
	var paths, late []string
	for _, id := range sortedKeys(keysOf(ps)) {
		p := ps[id]
		if p == nil || p.Kind == "command" {
			continue
		}
		total[p.Model]++
		if before != nil {
			if b := before.Profiles[id]; b != nil && reflect.DeepEqual(b, p) {
				continue
			}
		}
		if p.Model != "" && modelpath.Check(p.Model) != nil {
			paths = append(paths, fmt.Sprintf("profile %q names the model path %q, which a new profile may not (%v): it is kept and its load is refused", id, p.Model, modelpath.Check(p.Model)))
		}
		if _, problems := llamaflags.Parse(p.ExtraArgs); len(problems) > 0 {
			late = append(late, fmt.Sprintf("profile %q: extra_args %s; the launch uses the cluster value for it; a new profile may not set it this way", id, strings.Join(problems, "; ")))
		}
		if p.Ctx != nil && (p.CtxAboveTrained == nil || !*p.CtxAboveTrained) && s.o.Parse.TrainedCtx != nil {
			if tr, ok := s.o.Parse.TrainedCtx(filepath.Join(modelsDir, p.Model)); ok && tr > 0 && *p.Ctx > tr {
				a := above[p.Model]
				if a == nil {
					a = &agg{trained: tr}
					above[p.Model] = a
				}
				a.ids = append(a.ids, id)
			}
		}
	}
	var out []string
	models := make([]string, 0, len(above))
	for k := range above {
		models = append(models, k)
	}
	sort.Strings(models)
	for _, model := range models {
		a := above[model]
		out = append(out, fmt.Sprintf("%d of the %d %s of %s have a context above the model's trained %s and run at %s (a new profile would be refused for it): %s",
			len(a.ids), total[model], what, model, thousands(a.trained), thousands(a.trained), strings.Join(a.ids, ", ")))
	}
	return append(append(out, paths...), late...)
}

// thousands is n with commas (2,048).
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
