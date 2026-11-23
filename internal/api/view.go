package api

import (
	"reflect"
	"strings"
	"time"
	"unicode"
)

// jsonView converts the scheduler's status structs (which carry no JSON
// tags) into plain JSON values: snake_case keys, durations as seconds
// (float), times as RFC 3339 (zero time as null), slices never null. A nil
// pointer field is left out of its object (the optional load_* fields), and
// so is a field tagged view:"omitempty" while it holds its zero value (the
// additive facts that apply to some nodes and instances only).
func jsonView(v any) any { return view(reflect.ValueOf(v)) }

var (
	timeType     = reflect.TypeOf(time.Time{})
	durationType = reflect.TypeOf(time.Duration(0))
)

func snake(name string) string {
	name = strings.ReplaceAll(name, "MiB", "Mib")
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if unicode.IsUpper(r) {
			prevLower := i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]))
			acronymEnd := i > 0 && unicode.IsUpper(rs[i-1]) && i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if prevLower || acronymEnd {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func view(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Invalid:
		return nil
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return view(v.Elem())
	case reflect.Struct:
		if v.Type() == timeType {
			t := v.Interface().(time.Time)
			if t.IsZero() {
				return nil
			}
			return isoTime(t)
		}
		out := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				if fv := v.Field(i); fv.Kind() == reflect.Pointer && fv.IsNil() {
					continue
				} else if f.Tag.Get("view") == "omitempty" && fv.IsZero() {
					continue
				}
				out[snake(f.Name)] = view(v.Field(i))
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			out = append(out, view(v.Index(i)))
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		for _, k := range v.MapKeys() {
			out[k.String()] = view(v.MapIndex(k))
		}
		return out
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Type() == durationType {
			return time.Duration(v.Int()).Seconds()
		}
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		return v.Float()
	case reflect.Bool:
		return v.Bool()
	case reflect.String:
		return v.String()
	}
	return nil
}
