// DEV-ONLY MOCK of a server contract that is not built on this branch: the
// configuration API (/api/config*). It follows the real implementation on the
// bench branch (internal/api/config.go, internal/cfgsvc, design 6.0): one
// JSON document with the sections settings, tiers, profiles, exclude_nodes,
// node_overrides, recovery and fleet; settings, recovery and fleet store only
// the keys that were set; every committed change is a whole new revision and
// one audit entry per touched (section, target). The schema is a verbatim dump
// of config.Schema() from that branch (mock_config_schema.json). Nothing here
// changes the real scheduler underneath; it all lives in memory.

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed mock_config_schema.json
var schemaJSON []byte

type schemaField struct {
	Key             string   `json:"key"`
	Scope           string   `json:"scope"`
	Section         string   `json:"section"`
	Type            string   `json:"type"`
	Enum            []string `json:"enum,omitempty"`
	Default         any      `json:"default"`
	Min             *float64 `json:"min,omitempty"`
	Max             *float64 `json:"max,omitempty"`
	Unit            string   `json:"unit,omitempty"`
	Description     string   `json:"description"`
	RestartRequired bool     `json:"restart_required"`
	Required        bool     `json:"required,omitempty"`
	AppliesTo       string   `json:"applies_to,omitempty"`
	Nullable        bool     `json:"nullable,omitempty"`
}

var schemaFields = func() []schemaField {
	var d struct {
		Fields []schemaField `json:"fields"`
	}
	if err := json.Unmarshal(schemaJSON, &d); err != nil {
		panic(err)
	}
	return d.Fields
}()

func fieldOf(scope, key string) (schemaField, bool) {
	for _, f := range schemaFields {
		if f.Scope == scope && f.Key == key {
			return f, true
		}
	}
	return schemaField{}, false
}

func fieldsOf(scope string) []schemaField {
	var out []schemaField
	for _, f := range schemaFields {
		if f.Scope == scope {
			out = append(out, f)
		}
	}
	return out
}

// ---- small JSON helpers (also used by the bench mock) ----

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

type invalid struct{ msg string }

func (e *invalid) Error() string { return e.msg }

func bad(format string, a ...any) error { return &invalid{fmt.Sprintf(format, a...)} }

func show(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- errors (the real cfgErr shapes) ----

type validationError struct{ messages []string }

func (e *validationError) Error() string { return strings.Join(e.messages, "; ") }

type staleError struct{ current int }

func (e *staleError) Error() string {
	return fmt.Sprintf("the configuration changed: revision %d is current", e.current)
}

type refError struct {
	id       string
	warnings []string
}

func (e *refError) Error() string {
	return fmt.Sprintf("profile %q is referenced by %d benchmark(s); delete with ?force=1 to proceed", e.id, len(e.warnings))
}

var (
	errNotFound = errors.New("not found")
	errExists   = errors.New("already exists")
	errNotEmpty = errors.New("the database already holds a configuration; import with ?replace=1")
)

// cfgErr writes {"error":{"message","type",...}} like the real server.
func cfgErr(w http.ResponseWriter, err error) {
	body := func(status int, typ string, extra map[string]any) {
		e := map[string]any{"message": err.Error(), "type": typ}
		for k, v := range extra {
			e[k] = v
		}
		writeJSON(w, status, map[string]any{"error": e})
	}
	var ve *validationError
	var st *staleError
	var re *refError
	switch {
	case errors.As(err, &ve):
		body(400, "invalid_config", map[string]any{"messages": ve.messages})
	case errors.As(err, &st):
		body(409, "stale_revision", map[string]any{"current_revision": st.current})
	case errors.As(err, &re):
		body(409, "profile_referenced", map[string]any{"warnings": re.warnings})
	case errors.Is(err, errExists), errors.Is(err, errNotEmpty):
		body(409, "conflict", nil)
	case errors.Is(err, errNotFound):
		body(404, "not_found", nil)
	default:
		body(500, "server_error", nil)
	}
}

// apiBad is the plain 400 of the real apiErr for unreadable requests.
func apiBad(w http.ResponseWriter, format string, a ...any) {
	writeJSON(w, 400, map[string]any{"error": map[string]any{"message": fmt.Sprintf(format, a...), "type": "invalid_request_error"}})
}

// ---- the document ----

// emptyDoc is a configuration with nothing in it (first start).
func emptyDoc() map[string]any {
	return map[string]any{"settings": map[string]any{}, "tiers": []any{}, "profiles": map[string]any{}, "exclude_nodes": []any{},
		"node_overrides": map[string]any{}, "recovery": map[string]any{}, "fleet": map[string]any{}}
}

func normalizeDoc(d map[string]any) {
	for _, k := range []string{"settings", "profiles", "node_overrides", "recovery", "fleet"} {
		if _, ok := d[k].(map[string]any); !ok {
			d[k] = map[string]any{}
		}
	}
	for _, k := range []string{"tiers", "exclude_nodes"} {
		if _, ok := d[k].([]any); !ok {
			d[k] = []any{}
		}
	}
}

func fmtDuration(d time.Duration) string { return d.String() }

// parseDurationValue accepts "300s" or a bare 0 (config.ParseDurationValue).
func parseDurationValue(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		x, err := time.ParseDuration(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (write it like 300s, or 0)", t)
		}
		return x, nil
	case float64:
		if t == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("invalid duration %v (write it like 300s, or 0)", v)
}

// coerceSetting is config.CoerceSetting: the stored JSON form of a value.
func coerceSetting(scope, key string, v any) (any, error) {
	f, ok := fieldOf(scope, key)
	if !ok {
		return nil, fmt.Errorf("unknown %s setting %q", scope, key)
	}
	switch f.Type {
	case "duration":
		d, err := parseDurationValue(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", key, err)
		}
		return fmtDuration(d), nil
	case "int":
		x, ok := v.(float64)
		if !ok || x != math.Trunc(x) || math.Abs(x) > 1e12 {
			return nil, fmt.Errorf("%s: must be a whole number", key)
		}
		return x, nil
	case "float":
		x, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("%s: must be a number", key)
		}
		return x, nil
	case "bool":
		x, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s: must be true or false", key)
		}
		return x, nil
	case "string", "enum":
		x, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: must be a string", key)
		}
		return strings.TrimSpace(x), nil
	case "list":
		raw, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a list of strings", key)
		}
		out := make([]any, len(raw))
		for i, e := range raw {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s: entry %d must be a string", key, i)
			}
			out[i] = strings.TrimSpace(s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: unsupported type %s", key, f.Type)
}

// effective is the value of a key with the default filled in.
func effective(doc map[string]any, scope, key string) any {
	if v, ok := asMap(doc[scope])[key]; ok {
		return v
	}
	f, _ := fieldOf(scope, key)
	return f.Default
}

func numberOf(f schemaField, v any) (float64, bool) {
	switch f.Type {
	case "duration":
		d, err := parseDurationValue(v)
		return d.Seconds(), err == nil
	case "int", "float":
		x, ok := v.(float64)
		return x, ok
	}
	return 0, false
}

// ---- validation (a subset of config.Build, with the real wording) ----

var knownModelFiles = func() map[string]bool {
	m := map[string]bool{}
	for f := range modelBytes {
		m[f] = true
	}
	for _, p := range profiles {
		m[p.Model] = true
	}
	return m
}()

var managedFlags = []string{"--rpc", "--device", "-dev", "--tensor-split", "-ts", "--split-mode", "-sm", "--host", "--port", "--model", "-m",
	"--ctx-size", "-c", "--n-gpu-layers", "--gpu-layers", "-ngl", "--parallel", "-np", "--cache-type-k", "-ctk", "--cache-type-v", "-ctv",
	"--fit", "-fit", "--cache-ram", "-cram", "--flash-attn", "-fa", "--main-gpu", "-mg"}

var profileKeys = func() map[string]schemaField {
	m := map[string]schemaField{}
	for _, f := range fieldsOf("profile") {
		m[f.Key] = f
	}
	return m
}()

// parseProfile is cfgsvc.ParseProfile: a strict decode into the stored form.
func parseProfile(obj map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var msgs []string
	for _, k := range sortedMapKeys(obj) {
		v := obj[k]
		f, ok := profileKeys[k]
		if !ok {
			msgs = append(msgs, fmt.Sprintf("profile: json: unknown field %q", k))
			continue
		}
		if v == nil {
			continue
		}
		switch f.Type {
		case "duration":
			d, err := parseDurationValue(v)
			if err != nil {
				msgs = append(msgs, fmt.Sprintf("profile: %s: %v", k, err))
				continue
			}
			if d == 0 && k == "load_timeout" {
				continue // omitempty in the real ProfileSpec
			}
			out[k] = fmtDuration(d)
		case "int":
			x, ok := v.(float64)
			if !ok || x != math.Trunc(x) {
				msgs = append(msgs, fmt.Sprintf("profile: json: cannot unmarshal %s into Go struct field ProfileSpec.%s of type int", jsonKind(v), k))
				continue
			}
			if x != 0 || k == "ctx" || k == "parallel" {
				out[k] = x
			}
		case "bool":
			x, ok := v.(bool)
			if !ok {
				msgs = append(msgs, fmt.Sprintf("profile: json: cannot unmarshal %s into Go struct field ProfileSpec.%s of type bool", jsonKind(v), k))
				continue
			}
			out[k] = x
		case "string", "enum":
			x, ok := v.(string)
			if !ok {
				msgs = append(msgs, fmt.Sprintf("profile: json: cannot unmarshal %s into Go struct field ProfileSpec.%s of type string", jsonKind(v), k))
				continue
			}
			if x != "" {
				out[k] = x
			}
		case "list":
			l, ok := v.([]any)
			if !ok {
				msgs = append(msgs, fmt.Sprintf("profile: json: cannot unmarshal %s into Go struct field ProfileSpec.%s of type []", jsonKind(v), k))
				continue
			}
			wantNum := k == "allow_tiers" || k == "tensor_split"
			okAll := true
			for _, e := range l {
				if _, isNum := e.(float64); wantNum != isNum {
					okAll = false
				}
				if _, isStr := e.(string); !wantNum && !isStr {
					okAll = false
				}
			}
			if !okAll {
				msgs = append(msgs, fmt.Sprintf("profile: json: cannot unmarshal into Go struct field ProfileSpec.%s: wrong element type", k))
				continue
			}
			if len(l) > 0 {
				out[k] = l
			}
		}
	}
	if len(msgs) > 0 {
		return nil, &validationError{messages: msgs}
	}
	return out, nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "null"
}

var tierKeys = map[string]bool{"match": true, "priority": true, "headroom_mib": true, "shared_memory": true, "link_mb_s": true, "settle": true}
var matchKeys = map[string]bool{"hostname": true, "role": true, "source": true, "local": true}

// normTier is the stored form of a tier (TierSpec with omitempty).
func normTier(i int, v any) (map[string]any, []string) {
	t, ok := v.(map[string]any)
	if !ok {
		return nil, []string{fmt.Sprintf("invalid tiers: json: cannot unmarshal %s into a tier", jsonKind(v))}
	}
	var msgs []string
	out := map[string]any{}
	for _, k := range sortedMapKeys(t) {
		if !tierKeys[k] {
			msgs = append(msgs, fmt.Sprintf("invalid tiers: json: unknown field %q", k))
		}
	}
	m := map[string]any{}
	for _, k := range sortedMapKeys(asMap(t["match"])) {
		v := asMap(t["match"])[k]
		if !matchKeys[k] {
			msgs = append(msgs, fmt.Sprintf("invalid tiers: json: unknown field %q", k))
			continue
		}
		if k == "local" {
			if b, ok := v.(bool); ok {
				m[k] = b
			} else if v != nil {
				msgs = append(msgs, fmt.Sprintf("tier %d: match.local must be true or false", i+1))
			}
			continue
		}
		if s, _ := v.(string); s != "" {
			m[k] = s
		}
	}
	if len(m) > 0 {
		out["match"] = m
	}
	for _, k := range []string{"priority", "headroom_mib", "link_mb_s"} {
		if v, ok := t[k]; ok && v != nil {
			x, ok := v.(float64)
			if !ok || (k != "link_mb_s" && x != math.Trunc(x)) {
				msgs = append(msgs, fmt.Sprintf("tier %d: %s must be a number", i+1, k))
			} else if x != 0 {
				out[k] = x
			}
		}
	}
	if v, ok := t["shared_memory"]; ok && v != nil {
		if b, ok := v.(bool); !ok {
			msgs = append(msgs, fmt.Sprintf("tier %d: shared_memory must be true or false", i+1))
		} else if b {
			out["shared_memory"] = true
		}
	}
	if v, ok := t["settle"]; ok && v != nil {
		d, err := parseDurationValue(v)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("tier %d: settle: %v", i+1, err))
		} else if d != 0 {
			out["settle"] = fmtDuration(d)
		}
	}
	return out, msgs
}

func globMatch(pattern, s string) bool {
	ok, err := filepath.Match(strings.ToLower(pattern), strings.ToLower(s))
	return err == nil && ok
}

// validateDoc is the whole-document validation (config.Build).
func validateDoc(d map[string]any) []string {
	var msgs []string
	for _, scope := range []string{"settings", "recovery", "fleet"} {
		vals := asMap(d[scope])
		for _, k := range sortedMapKeys(vals) {
			if _, err := coerceSetting(scope, k, vals[k]); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s: %v", scope, err))
				continue
			}
			f, _ := fieldOf(scope, k)
			if x, ok := numberOf(f, vals[k]); ok {
				if f.Min != nil && x < *f.Min {
					msgs = append(msgs, fmt.Sprintf("%s: %s must be at least %v%s, got %s", scope, k, *f.Min, unitSuffix(f), show(vals[k])))
				}
				if f.Max != nil && x > *f.Max {
					msgs = append(msgs, fmt.Sprintf("%s: %s must be at most %v%s, got %s", scope, k, *f.Max, unitSuffix(f), show(vals[k])))
				}
			}
			if f.Type == "enum" && !contains(f.Enum, fmt.Sprint(vals[k])) {
				msgs = append(msgs, fmt.Sprintf("%s: %s must be one of %s, got %s", scope, k, strings.Join(f.Enum, ", "), show(vals[k])))
			}
			if f.Type == "list" && len(f.Enum) > 0 {
				seen := map[string]bool{}
				for _, e := range asList(vals[k]) {
					s := fmt.Sprint(e)
					if !contains(f.Enum, s) {
						msgs = append(msgs, fmt.Sprintf("%s: %s: unknown action %q (one of %s)", scope, k, s, strings.Join(f.Enum, ", ")))
					}
					if seen[s] {
						msgs = append(msgs, fmt.Sprintf("%s: %s: %q appears twice", scope, k, s))
					}
					seen[s] = true
				}
			}
		}
	}
	minHold, _ := parseDurationValue(effective(d, "settings", "min_hold"))
	maxHold, _ := parseDurationValue(effective(d, "settings", "max_hold"))
	if maxHold < minHold {
		msgs = append(msgs, fmt.Sprintf("defaults: max_hold (%s) must be at least min_hold (%s)", maxHold, minHold))
	}

	for i, t := range asList(d["tiers"]) {
		tm := asMap(t)
		if p, _ := tm["priority"].(float64); p < 1 {
			msgs = append(msgs, fmt.Sprintf("tier %d: priority must be at least 1", i+1))
		}
		if h, _ := asMap(tm["match"])["hostname"].(string); h != "" {
			if _, err := filepath.Match(h, "x"); err != nil {
				msgs = append(msgs, fmt.Sprintf("tier %d: match.hostname %q is not a valid glob", i+1, h))
			}
		}
	}
	excl := []string{}
	for i, e := range asList(d["exclude_nodes"]) {
		s, _ := e.(string)
		if strings.TrimSpace(s) == "" {
			msgs = append(msgs, fmt.Sprintf("exclude_nodes[%d]: must not be empty", i))
			continue
		}
		if _, err := filepath.Match(s, "x"); err != nil {
			msgs = append(msgs, fmt.Sprintf("exclude_nodes[%d]: %q is not a valid glob", i, s))
		}
		excl = append(excl, s)
	}
	for _, id := range sortedMapKeys(asMap(d["node_overrides"])) {
		o := asMap(asMap(d["node_overrides"])[id])
		for k := range o {
			if k != "rpc" {
				msgs = append(msgs, fmt.Sprintf("node_overrides %q: unknown field %q", id, k))
			}
		}
		if rpc, _ := o["rpc"].(string); rpc != "" {
			if _, port, err := splitHostPort(rpc); err != nil || port == "" {
				msgs = append(msgs, fmt.Sprintf("node_overrides %q: rpc %q must be host:port", id, rpc))
			}
		}
	}

	names := map[string]string{}
	ps := asMap(d["profiles"])
	for _, id := range sortedMapKeys(ps) {
		names[id] = id
	}
	for _, id := range sortedMapKeys(ps) {
		p := asMap(ps[id])
		pre := fmt.Sprintf("profile %q: ", id)
		kind, _ := p["kind"].(string)
		if kind == "" {
			kind = "llama"
		}
		if kind != "llama" && kind != "command" {
			msgs = append(msgs, pre+fmt.Sprintf("kind must be llama or command, got %q", kind))
		}
		for _, a := range asList(p["aliases"]) {
			s := fmt.Sprint(a)
			if other, dup := names[s]; dup {
				msgs = append(msgs, pre+fmt.Sprintf("alias %q is already the name or an alias of profile %q", s, other))
			}
			names[s] = id
		}
		if kind == "command" {
			if c, _ := p["cmd"].(string); !strings.Contains(c, "${PORT}") {
				msgs = append(msgs, pre+"cmd is required and must contain ${PORT}")
			}
			if n, _ := p["needs_mib"].(float64); n <= 0 {
				msgs = append(msgs, pre+"needs_mib is required for kind command")
			}
			continue
		}
		model, _ := p["model"].(string)
		switch {
		case model == "":
			msgs = append(msgs, pre+"model is required")
		case !knownModelFiles[model]:
			msgs = append(msgs, pre+fmt.Sprintf("model: file %q not found under models_dir %q", model, "/models"))
		}
		if c, _ := p["ctx"].(float64); c < 1 {
			msgs = append(msgs, pre+"ctx is required and must be at least 1")
		}
		if v, ok := p["parallel"].(float64); ok && v < 1 {
			msgs = append(msgs, pre+"parallel must be at least 1")
		}
		kv, _ := p["kv_quant"].(string)
		if kv != "" && !contains([]string{"f16", "q8_0", "q4_0"}, kv) {
			msgs = append(msgs, pre+fmt.Sprintf("kv_quant must be f16, q8_0 or q4_0, got %q", kv))
		}
		if fa, _ := p["flash_attn"].(bool); kv != "" && kv != "f16" && !fa {
			msgs = append(msgs, pre+fmt.Sprintf("kv_quant %s needs flash_attn: true", kv))
		}
		force, split := asList(p["force_nodes"]), asList(p["tensor_split"])
		if len(force) > 0 && len(asList(p["allow_tiers"])) > 0 {
			msgs = append(msgs, pre+"force_nodes and allow_tiers cannot be used together")
		}
		if len(split) > 0 && len(force) == 0 {
			msgs = append(msgs, pre+"tensor_split needs force_nodes")
		}
		if len(split) > 0 && len(force) > 0 && len(split) != len(force) {
			msgs = append(msgs, pre+fmt.Sprintf("tensor_split has %d entries but force_nodes has %d", len(split), len(force)))
		}
		for _, n := range force {
			for _, e := range excl {
				if strings.EqualFold(fmt.Sprint(n), e) || globMatch(e, fmt.Sprint(n)) {
					msgs = append(msgs, pre+fmt.Sprintf("force_nodes names %s, which exclude_nodes excludes (%s)", n, e))
				}
			}
		}
		for _, a := range asList(p["extra_args"]) {
			s := strings.SplitN(fmt.Sprint(a), "=", 2)[0]
			if contains(managedFlags, s) {
				msgs = append(msgs, pre+fmt.Sprintf("extra_args: %s is managed by the scheduler", s))
			}
		}
	}
	return msgs
}

func unitSuffix(f schemaField) string {
	if f.Type == "duration" {
		return "s"
	}
	if f.Unit != "" {
		return " " + f.Unit
	}
	return ""
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", errors.New("missing port")
	}
	if _, err := strconv.Atoi(s[i+1:]); err != nil {
		return "", "", err
	}
	return s[:i], s[i+1:], nil
}

// ---- diffs and audit entries (cfgsvc/diff.go) ----

type diffEntry struct {
	Path string `json:"path"`
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

func diffValues(prefix string, a, b any) []diffEntry {
	var out []diffEntry
	diffInto(&out, prefix, clone(a), clone(b))
	return out
}

func diffInto(out *[]diffEntry, path string, a, b any) {
	if reflect.DeepEqual(a, b) {
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]any{}
		for k := range am {
			keys[k] = nil
		}
		for k := range bm {
			keys[k] = nil
		}
		for _, k := range sortedMapKeys(keys) {
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
	*out = append(*out, diffEntry{Path: path, Old: a, New: b})
}

type sectionChange struct {
	section, target string
	before, after   any
	perKey          bool
}

func sectionChanges(before, after map[string]any) []sectionChange {
	a, b := cloneMap(before), cloneMap(after)
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
		keys := map[string]any{}
		for k := range am {
			keys[k] = nil
		}
		for k := range bm {
			keys[k] = nil
		}
		for _, k := range sortedMapKeys(keys) {
			if !reflect.DeepEqual(am[k], bm[k]) {
				out = append(out, sectionChange{section: sec, target: k, before: am[k], after: bm[k],
					perKey: sec == "settings" || sec == "recovery" || sec == "fleet"})
			}
		}
	}
	return out
}

func entriesOf(chs []sectionChange, action string) []auditEntry {
	out := make([]auditEntry, 0, len(chs))
	for _, c := range chs {
		act := action
		if act == "" {
			switch {
			case c.perKey:
				act = "update"
			case c.before == nil && c.after != nil:
				act = "create"
			case c.before != nil && c.after == nil:
				act = "delete"
			default:
				act = "update"
			}
		}
		prefix := c.section
		if c.target != "" {
			prefix += "." + c.target
		}
		out = append(out, auditEntry{Action: act, Section: c.section, Target: c.target, Before: c.before, After: c.after,
			Diff: diffValues(prefix, c.before, c.after)})
	}
	return out
}

func restartKeys(diff []diffEntry) []string {
	need := map[string]any{}
	for _, d := range diff {
		parts := strings.Split(d.Path, ".")
		if len(parts) < 2 {
			continue
		}
		scope := parts[0]
		if scope != "settings" && scope != "fleet" && scope != "recovery" {
			continue
		}
		key := strings.SplitN(parts[1], "[", 2)[0]
		if f, ok := fieldOf(scope, key); ok && f.RestartRequired {
			need[scope+"."+key] = nil
		}
	}
	return sortedMapKeys(need)
}

// ---- the store ----

type revisionRec struct {
	Revision int    `json:"revision"`
	Ts       int64  `json:"ts"`
	Source   string `json:"source"`
	Actor    string `json:"actor"`
	Comment  string `json:"comment"`
	doc      map[string]any
}

type configStore struct {
	mu        sync.Mutex
	rev       int
	updatedAt int64
	doc       map[string]any
	revs      []*revisionRec
	audit     *auditLog
	notify    func(data map[string]any)
	// profileRefs lists the benchmarks that use a profile (delete check).
	profileRefs func(id string) []string
}

func newConfigStore(audit *auditLog) *configStore {
	return &configStore{audit: audit, doc: emptyDoc()}
}

func (s *configStore) current() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

// view is GET /api/config (cfgsvc.View).
func viewOf(rev int, upd int64, doc map[string]any) map[string]any {
	settings, recovery, fleet := map[string]any{}, map[string]any{}, map[string]any{}
	for _, f := range fieldsOf("settings") {
		settings[f.Key] = clone(effective(doc, "settings", f.Key))
	}
	for _, f := range fieldsOf("recovery") {
		recovery[f.Key] = clone(effective(doc, "recovery", f.Key))
	}
	for _, f := range fieldsOf("fleet") {
		fleet[f.Key] = clone(effective(doc, "fleet", f.Key))
	}
	set := sortedMapKeys(asMap(doc["settings"]))
	return map[string]any{"revision": rev, "updated_at": upd, "configured": rev > 0, "settings": settings, "settings_set": set,
		"tiers": clone(doc["tiers"]), "profiles": clone(doc["profiles"]), "exclude_nodes": clone(doc["exclude_nodes"]),
		"node_overrides": clone(doc["node_overrides"]), "recovery": recovery, "fleet": fleet}
}

func (s *configStore) view() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return viewOf(s.rev, s.updatedAt, s.doc)
}

// origin is who made a change (audit.FromRequest).
type origin struct {
	Source, Actor, Comment, Remote, UserAgent string
	Ts                                        int64
}

func originOf(r *http.Request, bodyComment string) origin {
	o := origin{Source: "rest", Actor: "anonymous", Comment: clip(strings.TrimSpace(r.Header.Get("X-Comment")), 2000),
		UserAgent: clip(r.UserAgent(), 300), Ts: time.Now().UnixMilli()}
	if bodyComment != "" {
		o.Comment = bodyComment
	}
	if host, _, err := splitHostPort(r.RemoteAddr); err == nil {
		o.Remote = strings.Trim(host, "[]")
	} else {
		o.Remote = r.RemoteAddr
	}
	if a := clip(strings.TrimSpace(r.Header.Get("X-Actor")), 200); a != "" {
		o.Actor = a
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Source")), "ui") {
		o.Source = "ui"
	}
	return o
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type writeOpts struct {
	o       origin
	dryRun  bool
	force   bool
	ifMatch *int
}

type writeResult struct {
	Revision        int         `json:"revision"`
	AuditEntryIDs   []int64     `json:"audit_entry_ids"`
	Changed         bool        `json:"changed"`
	DryRun          bool        `json:"dry_run"`
	Diff            []diffEntry `json:"diff"`
	Warnings        []string    `json:"warnings"`
	RestartRequired []string    `json:"restart_required"`
}

type mutation struct {
	action       string // rollback, import; "" derives create/update/delete
	requireEmpty bool
	fn           func(d map[string]any) error
	refuse       func(force, dry bool) ([]string, error)
}

// mutate is cfgsvc.Service.mutate: change, validate, diff, check If-Match,
// commit one revision with one audit entry per touched (section, target).
func (s *configStore) mutate(o writeOpts, m mutation) (*writeResult, error) {
	s.mu.Lock()
	before := cloneMap(s.doc)
	after := cloneMap(s.doc)
	if err := m.fn(after); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	after = cloneMap(after)
	normalizeDoc(after)
	var warnings []string
	if m.refuse != nil {
		w, err := m.refuse(o.force, o.dryRun)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		warnings = w
	}
	if msgs := validateDoc(after); len(msgs) > 0 {
		s.mu.Unlock()
		return nil, &validationError{messages: msgs}
	}
	chs := sectionChanges(before, after)
	res := &writeResult{Revision: s.rev, Changed: len(chs) > 0, DryRun: o.dryRun, AuditEntryIDs: []int64{}, Warnings: append([]string{}, warnings...),
		Diff: []diffEntry{}}
	es := entriesOf(chs, m.action)
	for _, e := range es {
		res.Diff = append(res.Diff, e.Diff...)
	}
	res.RestartRequired = restartKeys(res.Diff)
	if o.ifMatch != nil && *o.ifMatch != s.rev {
		cur := s.rev
		s.mu.Unlock()
		return nil, &staleError{current: cur}
	}
	if m.requireEmpty && s.rev > 0 {
		s.mu.Unlock()
		return nil, errNotEmpty
	}
	if o.dryRun || (!res.Changed && !m.requireEmpty) {
		s.mu.Unlock()
		return res, nil
	}
	s.rev++
	s.doc, s.updatedAt = after, o.o.Ts
	s.revs = append(s.revs, &revisionRec{Revision: s.rev, Ts: o.o.Ts, Source: o.o.Source, Actor: o.o.Actor, Comment: o.o.Comment, doc: cloneMap(after)})
	rev := s.rev
	s.mu.Unlock()
	if s.audit != nil {
		for _, e := range es {
			e.Ts, e.Source, e.Actor, e.RemoteAddr, e.UserAgent, e.Comment, e.Revision = o.o.Ts, o.o.Source, o.o.Actor, o.o.Remote, o.o.UserAgent, o.o.Comment, rev
			res.AuditEntryIDs = append(res.AuditEntryIDs, s.audit.add(e).ID)
		}
	}
	res.Revision = rev
	if s.notify != nil {
		ev := map[string]any{"revision": rev, "audit_entry_ids": res.AuditEntryIDs, "source": o.o.Source, "actor": o.o.Actor, "section": "multiple", "target": ""}
		if len(es) == 1 {
			ev["section"], ev["target"] = es[0].Section, es[0].Target
		}
		s.notify(ev)
	}
	return res, nil
}

// ---- HTTP ----

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func optsOf(w http.ResponseWriter, r *http.Request, bodyComment string) (writeOpts, bool) {
	o := writeOpts{o: originOf(r, bodyComment), dryRun: truthy(r.URL.Query().Get("dry_run")), force: truthy(r.URL.Query().Get("force"))}
	if v := strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("If-Match")), "W/"), `"`); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			apiBad(w, "If-Match must be a revision number, got %q", v)
			return o, false
		}
		o.ifMatch = &n
	}
	return o, true
}

func readBodyBytes(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, 4<<20)); err != nil {
		writeJSON(w, 413, map[string]any{"error": map[string]any{"message": "the body is larger than 4 MiB or unreadable", "type": "invalid_request_error"}})
		return nil, false
	}
	return buf.Bytes(), true
}

// bodyObject reads a JSON object and takes its "comment" field out.
func bodyObject(w http.ResponseWriter, r *http.Request) (map[string]any, string, bool) {
	b, ok := readBodyBytes(w, r)
	if !ok {
		return nil, "", false
	}
	obj := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
			apiBad(w, "the body must be a JSON object: %v", err)
			return nil, "", false
		}
	}
	c, _ := obj["comment"].(string)
	delete(obj, "comment")
	return obj, strings.TrimSpace(c), true
}

// listBody reads a bare value or {"<key>": value, "comment": "..."}.
func listBody(w http.ResponseWriter, r *http.Request, key string) (any, string, bool) {
	b, ok := readBodyBytes(w, r)
	if !ok {
		return nil, "", false
	}
	b = bytes.TrimSpace(b)
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		apiBad(w, "invalid JSON: %v", err)
		return nil, "", false
	}
	comment := ""
	if m, isObj := v.(map[string]any); isObj {
		comment, _ = m["comment"].(string)
		inner, has := m[key]
		if !has && key == "node_overrides" {
			delete(m, "comment")
			inner, has = m, true
		}
		if !has {
			apiBad(w, "the object must have a %q field", key)
			return nil, "", false
		}
		v = inner
	}
	return v, strings.TrimSpace(comment), true
}

func (s *configStore) respond(w http.ResponseWriter, status int, res *writeResult, err error, extra map[string]any) {
	if err != nil {
		cfgErr(w, err)
		return
	}
	out := map[string]any{}
	b, _ := json.Marshal(res)
	json.Unmarshal(b, &out)
	out["audit_ids"] = res.AuditEntryIDs
	for k, v := range extra {
		out[k] = v
	}
	if res.DryRun {
		status = 200
	}
	w.Header().Set("X-Config-Revision", strconv.Itoa(res.Revision))
	writeJSON(w, status, out)
}

func (s *configStore) routes(m *http.ServeMux) {
	m.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.view()) })
	m.HandleFunc("GET /api/config/schema", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(schemaJSON)
	})
	m.HandleFunc("GET /api/config/profiles", func(w http.ResponseWriter, r *http.Request) {
		v := s.view()
		writeJSON(w, 200, map[string]any{"revision": v["revision"], "profiles": v["profiles"]})
	})
	m.HandleFunc("GET /api/config/profiles/{id...}", func(w http.ResponseWriter, r *http.Request) {
		v := s.view()
		id := r.PathValue("id")
		p, ok := asMap(v["profiles"])[id]
		if !ok {
			cfgErr(w, errNotFound)
			return
		}
		writeJSON(w, 200, map[string]any{"revision": v["revision"], "id": id, "profile": p})
	})
	m.HandleFunc("POST /api/config/profiles", s.profileCreate)
	m.HandleFunc("PUT /api/config/profiles/{id...}", s.profilePut)
	m.HandleFunc("DELETE /api/config/profiles/{id...}", s.profileDelete)
	for _, sec := range []struct{ path, key string }{{"tiers", "tiers"}, {"settings", "settings"}, {"exclude-nodes", "exclude_nodes"},
		{"node-overrides", "node_overrides"}, {"recovery", "recovery"}, {"fleet", "fleet"}} {
		sec := sec
		m.HandleFunc("GET /api/config/"+sec.path, func(w http.ResponseWriter, r *http.Request) {
			v := s.view()
			out := map[string]any{"revision": v["revision"], sec.key: v[sec.key]}
			if sec.key == "settings" {
				out["settings_set"] = v["settings_set"]
			}
			writeJSON(w, 200, out)
		})
	}
	m.HandleFunc("PATCH /api/config/settings", s.patchScope("settings", false))
	m.HandleFunc("PUT /api/config/recovery", s.patchScope("recovery", true))
	m.HandleFunc("PATCH /api/config/recovery", s.patchScope("recovery", false))
	m.HandleFunc("PUT /api/config/fleet", s.patchScope("fleet", true))
	m.HandleFunc("PATCH /api/config/fleet", s.patchScope("fleet", false))
	m.HandleFunc("PUT /api/config/tiers", s.tiersPut)
	m.HandleFunc("PUT /api/config/exclude-nodes", s.excludePut)
	m.HandleFunc("PUT /api/config/node-overrides", s.overridesPut)
	m.HandleFunc("GET /api/config/revisions", s.revisions)
	m.HandleFunc("GET /api/config/revisions/{n}", s.revisionGet)
	m.HandleFunc("POST /api/config/revisions/{n}/rollback", s.rollback)
	m.HandleFunc("GET /api/config/export", s.export)
	m.HandleFunc("POST /api/config/import", s.importDoc)
	m.HandleFunc("/api/config/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "no such configuration endpoint: " + r.Method + " " + r.URL.Path, "type": "not_found"}})
	})
}

// profileBody: {"id", "profile": {...}} or the bare profile fields (+ id).
func profileBody(w http.ResponseWriter, r *http.Request) (string, map[string]any, string, bool) {
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return "", nil, "", false
	}
	id := ""
	if v, has := obj["id"]; has {
		id, _ = v.(string)
		delete(obj, "id")
	}
	if inner, has := obj["profile"].(map[string]any); has && len(obj) == 1 {
		obj = inner
	}
	p, err := parseProfile(obj)
	if err != nil {
		cfgErr(w, err)
		return "", nil, "", false
	}
	return id, p, comment, true
}

func (s *configStore) profileCreate(w http.ResponseWriter, r *http.Request) {
	id, p, comment, ok := profileBody(w, r)
	if !ok {
		return
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	if strings.TrimSpace(id) == "" {
		cfgErr(w, &validationError{messages: []string{"profile id must not be empty"}})
		return
	}
	res, err := s.mutate(o, mutation{fn: func(d map[string]any) error {
		ps := asMap(d["profiles"])
		if _, ok := ps[id]; ok {
			return fmt.Errorf("profile %q: %w", id, errExists)
		}
		ps[id] = p
		d["profiles"] = ps
		return nil
	}})
	s.respond(w, 201, res, err, map[string]any{"id": id})
}

func (s *configStore) profilePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, p, comment, ok := profileBody(w, r)
	if !ok {
		return
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	res, err := s.mutate(o, mutation{fn: func(d map[string]any) error {
		ps := asMap(d["profiles"])
		if _, ok := ps[id]; !ok {
			return fmt.Errorf("profile %q: %w", id, errNotFound)
		}
		ps[id] = p
		return nil
	}})
	s.respond(w, 200, res, err, map[string]any{"id": id})
}

func (s *configStore) profileDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o, ok := optsOf(w, r, "")
	if !ok {
		return
	}
	res, err := s.mutate(o, mutation{
		fn: func(d map[string]any) error {
			ps := asMap(d["profiles"])
			if _, ok := ps[id]; !ok {
				return fmt.Errorf("profile %q: %w", id, errNotFound)
			}
			delete(ps, id)
			return nil
		},
		refuse: func(force, dry bool) ([]string, error) {
			if s.profileRefs == nil {
				return nil, nil
			}
			var warn []string
			for _, b := range s.profileRefs(id) {
				warn = append(warn, fmt.Sprintf("benchmark %s refers to profile %s", b, id))
			}
			if len(warn) > 0 && !force && !dry {
				return nil, &refError{id: id, warnings: warn}
			}
			return warn, nil
		},
	})
	s.respond(w, 200, res, err, map[string]any{"id": id})
}

func (s *configStore) patchScope(scope string, replace bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		obj, comment, ok := bodyObject(w, r)
		if !ok {
			return
		}
		o, ok := optsOf(w, r, comment)
		if !ok {
			return
		}
		res, err := s.mutate(o, mutation{fn: func(d map[string]any) error {
			m := asMap(d[scope])
			if replace {
				m = map[string]any{}
			}
			var msgs []string
			for _, k := range sortedMapKeys(obj) {
				if obj[k] == nil {
					if _, ok := fieldOf(scope, k); !ok && !replace {
						msgs = append(msgs, fmt.Sprintf("unknown %s key %q", scope, k))
					}
					delete(m, k)
					continue
				}
				v, err := coerceSetting(scope, k, obj[k])
				if err != nil {
					msgs = append(msgs, err.Error())
					continue
				}
				m[k] = v
			}
			if len(msgs) > 0 {
				sort.Strings(msgs)
				return &validationError{messages: msgs}
			}
			d[scope] = m
			return nil
		}})
		s.respond(w, 200, res, err, nil)
	}
}

func (s *configStore) tiersPut(w http.ResponseWriter, r *http.Request) {
	v, comment, ok := listBody(w, r, "tiers")
	if !ok {
		return
	}
	list, isList := v.([]any)
	if !isList && v != nil {
		apiBad(w, "invalid tiers: json: cannot unmarshal %s into Go value of type []config.TierSpec", jsonKind(v))
		return
	}
	tiers := []any{}
	for i, t := range list {
		nt, msgs := normTier(i, t)
		if len(msgs) > 0 {
			if strings.HasPrefix(msgs[0], "invalid tiers") {
				apiBad(w, "%s", msgs[0])
			} else {
				cfgErr(w, &validationError{messages: msgs})
			}
			return
		}
		tiers = append(tiers, nt)
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	res, err := s.mutate(o, mutation{fn: func(d map[string]any) error { d["tiers"] = tiers; return nil }})
	s.respond(w, 200, res, err, nil)
}

func (s *configStore) excludePut(w http.ResponseWriter, r *http.Request) {
	v, comment, ok := listBody(w, r, "exclude_nodes")
	if !ok {
		return
	}
	list := []any{}
	for _, e := range asList(v) {
		str, isStr := e.(string)
		if !isStr {
			apiBad(w, "invalid exclude_nodes: json: cannot unmarshal %s into Go value of type string", jsonKind(e))
			return
		}
		list = append(list, strings.TrimSpace(str))
	}
	if _, isList := v.([]any); !isList && v != nil {
		apiBad(w, "invalid exclude_nodes: json: cannot unmarshal %s into Go value of type []string", jsonKind(v))
		return
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	res, err := s.mutate(o, mutation{fn: func(d map[string]any) error { d["exclude_nodes"] = list; return nil }})
	s.respond(w, 200, res, err, nil)
}

func (s *configStore) overridesPut(w http.ResponseWriter, r *http.Request) {
	v, comment, ok := listBody(w, r, "node_overrides")
	if !ok {
		return
	}
	in, isMap := v.(map[string]any)
	if !isMap && v != nil {
		apiBad(w, "invalid node_overrides: json: cannot unmarshal %s into Go value of type map[string]config.NodeOverrideSpec", jsonKind(v))
		return
	}
	out := map[string]any{}
	for id, ov := range in {
		om, ok := ov.(map[string]any)
		if !ok {
			apiBad(w, "invalid node_overrides: json: cannot unmarshal %s into Go value of type config.NodeOverrideSpec", jsonKind(ov))
			return
		}
		e := map[string]any{}
		for k, x := range om {
			if k != "rpc" {
				apiBad(w, "invalid node_overrides: json: unknown field %q", k)
				return
			}
			if s, _ := x.(string); s != "" {
				e["rpc"] = s
			}
		}
		out[id] = e
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	res, err := s.mutate(o, mutation{fn: func(d map[string]any) error { d["node_overrides"] = out; return nil }})
	s.respond(w, 200, res, err, nil)
}

func (s *configStore) revisions(w http.ResponseWriter, r *http.Request) {
	limit := qInt(r, "limit", 100)
	if limit < 1 {
		limit = 100
	}
	s.mu.Lock()
	out := []any{}
	for i := len(s.revs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *s.revs[i])
	}
	cur := s.rev
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"revisions": out, "current": cur})
}

func revNumber(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		apiBad(w, "the revision must be a positive number")
		return 0, false
	}
	return n, true
}

func (s *configStore) revision(n int) *revisionRec {
	for _, rv := range s.revs {
		if rv.Revision == n {
			return rv
		}
	}
	return nil
}

func (s *configStore) revisionGet(w http.ResponseWriter, r *http.Request) {
	n, ok := revNumber(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	rv := s.revision(n)
	var out map[string]any
	if rv != nil {
		out = map[string]any{"revision": rv.Revision, "ts": rv.Ts, "source": rv.Source, "actor": rv.Actor, "comment": rv.Comment,
			"config": viewOf(rv.Revision, rv.Ts, rv.doc), "document": cloneMap(rv.doc)}
	}
	s.mu.Unlock()
	if rv == nil {
		cfgErr(w, errNotFound)
		return
	}
	writeJSON(w, 200, out)
}

func (s *configStore) rollback(w http.ResponseWriter, r *http.Request) {
	n, ok := revNumber(w, r)
	if !ok {
		return
	}
	obj, comment, ok := bodyObject(w, r)
	_ = obj
	if !ok {
		return
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	s.mu.Lock()
	rv := s.revision(n)
	s.mu.Unlock()
	if rv == nil {
		cfgErr(w, errNotFound)
		return
	}
	target := cloneMap(rv.doc)
	res, err := s.mutate(o, mutation{action: "rollback", fn: func(d map[string]any) error {
		for k := range d {
			delete(d, k)
		}
		for k, v := range target {
			d[k] = v
		}
		return nil
	}})
	s.respond(w, 200, res, err, map[string]any{"rolled_back_to": n})
}

const exportFormat = "llm-scheduler-config/1"

func (s *configStore) export(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := cloneMap(s.doc)
	out["format"], out["revision"], out["exported_at"] = exportFormat, s.rev, time.Now().UnixMilli()
	s.mu.Unlock()
	w.Header().Set("Content-Disposition", `attachment; filename="llm-scheduler-config.json"`)
	writeJSON(w, 200, out)
}

func (s *configStore) importDoc(w http.ResponseWriter, r *http.Request) {
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if f, has := obj["format"].(string); has && f != exportFormat {
		apiBad(w, "unknown export format %q (this scheduler reads %s)", f, exportFormat)
		return
	}
	for _, k := range []string{"format", "revision", "exported_at"} {
		delete(obj, k)
	}
	for k := range obj {
		if !contains([]string{"settings", "tiers", "profiles", "exclude_nodes", "node_overrides", "recovery", "fleet"}, k) {
			apiBad(w, "json: unknown field %q", k)
			return
		}
	}
	o, ok := optsOf(w, r, comment)
	if !ok {
		return
	}
	res, err := s.mutate(o, mutation{action: "import", requireEmpty: !truthy(r.URL.Query().Get("replace")), fn: func(d map[string]any) error {
		for k := range d {
			delete(d, k)
		}
		for k, v := range obj {
			d[k] = clone(v)
		}
		return nil
	}})
	s.respond(w, 200, res, err, nil)
}
