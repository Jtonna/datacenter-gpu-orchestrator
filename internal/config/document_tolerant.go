package config

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ParseStoredDocument decodes a document read from the configuration store.
// Unlike ParseDocument (what clients send, which stays strict) it tolerates
// fields this version does not know: a newer version may have written them
// (an optional profile field, a setting, a tier field). Everything known is
// kept; the unknown fields are dropped from the result and named in
// unknown, sorted, with map keys as * and list positions as []
// ("profiles.*.description", "settings.new_key"). A setting, recovery or
// fleet key the schema does not know counts as unknown too: Build would
// refuse it. Broken JSON is still an error.
func ParseStoredDocument(b []byte) (d *Document, unknown []string, err error) {
	d = NewDocument()
	if err := json.Unmarshal(b, d); err != nil {
		return nil, nil, fmt.Errorf("configuration document: %w", err)
	}
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			unknown = append(unknown, p)
		}
	}
	for k := range RetiredSettings {
		delete(d.Settings, k) // a retired key is dropped quietly: it is not unknown
	}
	unknownIn(json.RawMessage(b), reflect.TypeOf(Document{}), "", add)
	for _, sc := range []struct {
		scope string
		m     map[string]any
	}{{ScopeSettings, d.Settings}, {ScopeRecovery, d.Recovery}, {ScopeFleet, d.Fleet}} {
		for k := range sc.m {
			if _, ok := findField(sc.scope, k); !ok {
				add(sc.scope + "." + k)
				delete(sc.m, k)
			}
		}
	}
	sort.Strings(unknown)
	d.Normalize()
	return d, unknown, nil
}

// RetiredSettings are settings an earlier release had and this one removed.
// A stored document that has one still opens: the key is dropped without
// counting as an unknown field (so writes need no force), and the next
// revision is stored without it. What a client sends stays strict: a PATCH
// with a retired key is refused as any key the schema does not know.
var RetiredSettings = map[string]string{
	"ui_model_actions": "v47 and v48: a page switch for Load, Unload and Force unload (default true); the page always has them now",
}

var (
	jsonUnmarshaler = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textUnmarshaler = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// unknownIn calls add with the path of every object key in raw that
// decoding into type t ignores (encoding/json's rules: the json tag name,
// else the field name, matched without regard to case; "-" is never set).
// A type that decodes itself, an interface or a map of interfaces takes
// anything.
func unknownIn(raw json.RawMessage, t reflect.Type, path string, add func(string)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler) {
		return
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return
	}
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return
		}
		fields := jsonFields(t)
		for k, v := range obj {
			f, ok := fields[k]
			if !ok {
				for name, ff := range fields {
					if strings.EqualFold(name, k) {
						f, ok = ff, true
						break
					}
				}
			}
			if !ok {
				add(join(k))
				continue
			}
			unknownIn(v, f.Type, join(k), add)
		}
	case reflect.Map:
		if t.Elem().Kind() == reflect.Interface {
			return
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return
		}
		for _, v := range obj {
			unknownIn(v, t.Elem(), join("*"), add)
		}
	case reflect.Slice, reflect.Array:
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			return
		}
		for _, v := range list {
			unknownIn(v, t.Elem(), path+"[]", add)
		}
	}
}

// jsonFields maps the JSON names of t's fields to the fields, with the
// fields of embedded structs promoted.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					if _, dup := out[k]; !dup {
						out[k] = v
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f
	}
	return out
}
