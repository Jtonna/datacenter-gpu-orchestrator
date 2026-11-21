package logrelay

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// The trusted fields (design section 8). Potluck sets them for a whole
// request; a sender cannot set them.
const (
	FieldNodeID     = "node_id"
	FieldNodeName   = "node_name"
	FieldPlatform   = "platform"
	FieldRelease    = "release"
	FieldSource     = "source"
	FieldLaunchID   = "launch_id"
	FieldProfile    = "profile"
	FieldActor      = "actor"
	FieldClientAddr = "client_addr"
	// FieldInstanceID is set on Potluck's own host output (source host): the
	// instance of the program that wrote the line. Not a trusted field.
	FieldInstanceID = "instance_id"
)

// trustedFields is every trusted field name, in a fixed order.
var trustedFields = []string{FieldNodeID, FieldNodeName, FieldPlatform, FieldRelease, FieldSource,
	FieldLaunchID, FieldProfile, FieldActor, FieldClientAddr}

// streamFields is the store's _stream_fields argument: the fields that name
// a log stream.
const streamFields = "source,node_id,unit"

// maxFieldValue bounds one trusted value, in bytes.
const maxFieldValue = 256

// Field is one trusted field of a request.
type Field struct{ Name, Value string }

// cleanValue makes a value safe for the store's extra_fields argument.
//
// The store (VictoriaLogs v1.54.0, app/vlinsert/insertutil/common_params.go
// and lib/httputil/array.go) reads the argument with FormValue, splits it at
// every comma, trims each part and splits the part at its first "=". It has
// no escape for a comma, and a part with an empty name or an empty value
// fails the whole request. So:
//
//   - a comma is written %2C and a percent sign %25. The value is stored
//     that way; it can be read back without loss, and it cannot start a second
//     field;
//   - a control character becomes a space;
//   - space at both ends is dropped, as the store would drop it;
//   - bytes that are not UTF-8 are dropped;
//   - the value is cut at about maxFieldValue bytes.
//
// "=" and ":" need nothing: only the first "=" of a part separates, and it
// is the one after the field name.
func cleanValue(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r == utf8.RuneError:
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		case r == ',':
			b.WriteString("%2C")
		case r == '%':
			b.WriteString("%25")
		default:
			b.WriteRune(r)
		}
		if b.Len() >= maxFieldValue {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// storeQuery is the query string of the forward to the store:
//
//	extra_fields=<name>=<value>,<name>=<value>...   the trusted fields
//	ignore_fields=<name>,<name>...                   the trusted fields this request does not set
//	_stream_fields=source,node_id,unit
//
// A field with an empty value is left out of extra_fields (the store refuses
// an empty value) and is in ignore_fields instead, so a line cannot set it
// either. No name is in both lists.
func storeQuery(fields []Field) string {
	var extra []string
	set := map[string]bool{}
	for _, f := range fields {
		v := cleanValue(f.Value)
		if v == "" || set[f.Name] {
			continue
		}
		set[f.Name] = true
		extra = append(extra, f.Name+"="+v)
	}
	var ignore []string
	for _, n := range trustedFields {
		if !set[n] {
			ignore = append(ignore, n)
		}
	}
	q := url.Values{}
	if len(extra) > 0 {
		q.Set("extra_fields", strings.Join(extra, ","))
	}
	if len(ignore) > 0 {
		q.Set("ignore_fields", strings.Join(ignore, ","))
	}
	q.Set("_stream_fields", streamFields)
	return q.Encode()
}
