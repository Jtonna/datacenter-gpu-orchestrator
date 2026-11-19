package cfgsvc

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/audit"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
)

func toAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// Diff lists the changed leaf paths between two JSON-like values.
func Diff(prefix string, before, after any) []audit.DiffItem {
	var out []audit.DiffItem
	diffInto(&out, prefix, toAny(before), toAny(after))
	return out
}

func diffInto(out *[]audit.DiffItem, path string, a, b any) {
	if reflect.DeepEqual(a, b) {
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			p := k
			if path != "" {
				p = path + "." + k
			}
			diffInto(out, p, am[k], bm[k])
		}
		return
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok && len(al) == len(bl) {
		for i := range al {
			diffInto(out, fmt.Sprintf("%s[%d]", path, i), al[i], bl[i])
		}
		return
	}
	*out = append(*out, audit.DiffItem{Path: path, Old: a, New: b})
}

type sectionChange struct {
	section, target string
	before, after   any
	perKey          bool // settings-like: an unset key is a reset, not a delete
}

// sectionChanges compares two documents section by section (profiles, settings
// keys, node overrides per id; the rest as a whole).
func sectionChanges(before, after *config.Document) []sectionChange {
	a, b := toAny(before).(map[string]any), toAny(after).(map[string]any)
	var out []sectionChange
	for _, sec := range []string{"settings", "tiers", "profiles", "exclude_nodes", "node_overrides", "recovery", "fleet"} {
		av, bv := a[sec], b[sec]
		am, aok := av.(map[string]any)
		bm, bok := bv.(map[string]any)
		if !aok || !bok {
			if !reflect.DeepEqual(av, bv) {
				out = append(out, sectionChange{section: sec, before: av, after: bv})
			}
			continue
		}
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if !reflect.DeepEqual(am[k], bm[k]) {
				out = append(out, sectionChange{section: sec, target: k, before: am[k], after: bm[k], perKey: sec == "settings" || sec == "recovery" || sec == "fleet"})
			}
		}
	}
	return out
}

func raw(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(v)
	return b
}

// entries turns section changes into audit entries. action overrides the
// derived create/update/delete when not empty (rollback, import).
func entries(chs []sectionChange, action string) []audit.Entry {
	out := make([]audit.Entry, 0, len(chs))
	for _, c := range chs {
		act := action
		if act == "" {
			switch {
			case c.perKey:
				act = audit.ActionUpdate
			case c.before == nil && c.after != nil:
				act = audit.ActionCreate
			case c.before != nil && c.after == nil:
				act = audit.ActionDelete
			default:
				act = audit.ActionUpdate
			}
		}
		prefix := c.section
		if c.target != "" {
			prefix += "." + c.target
		}
		out = append(out, audit.Entry{Action: act, Section: c.section, Target: c.target,
			Before: raw(c.before), After: raw(c.after), Diff: Diff(prefix, c.before, c.after)})
	}
	return out
}
