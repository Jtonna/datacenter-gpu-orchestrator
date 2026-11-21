package logrelay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// storeParse reads extra_fields and ignore_fields the way the store does.
// It is the logic of VictoriaLogs v1.54.0 (getExtraFields and getArray in
// app/vlinsert/insertutil/common_params.go, GetArray in lib/httputil/array.go):
// FormValue, split at every comma, trim, drop empty parts, split at the first
// "=", and an empty name or value is an error.
func storeParse(r *http.Request) (extra map[string]string, order []string, ignore []string, err error) {
	array := func(key string) []string {
		v := r.FormValue(key)
		if v == "" {
			return nil
		}
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	extra = map[string]string{}
	for _, ef := range array("extra_fields") {
		n := strings.Index(ef, "=")
		if n <= 0 || n == len(ef)-1 {
			return nil, nil, nil, fmt.Errorf("invalid extra_field format: %q", ef)
		}
		if _, dup := extra[ef[:n]]; dup {
			return nil, nil, nil, fmt.Errorf("field %q twice", ef[:n])
		}
		extra[ef[:n]] = ef[n+1:]
		order = append(order, ef[:n])
	}
	return extra, order, array("ignore_fields"), nil
}

func parseQuery(t *testing.T, fields []Field) (map[string]string, []string, []string) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://store.example:9428/insert/jsonline?"+storeQuery(fields), nil)
	extra, order, ignore, err := storeParse(r)
	if err != nil {
		t.Fatalf("the store would refuse the request: %v (query %s)", err, r.URL.RawQuery)
	}
	if got := r.FormValue("_stream_fields"); got != "source,node_id,unit" {
		t.Fatalf("_stream_fields = %q", got)
	}
	return extra, order, ignore
}

func TestExtraFieldsPlainValues(t *testing.T) {
	extra, order, ignore := parseQuery(t, []Field{
		{FieldNodeID, "02:00:00:00:00:01"}, {FieldNodeName, "console-1"}, {FieldPlatform, "linux-amd64-vulkan"},
		{FieldRelease, "0.1.0"}, {FieldSource, "launch"}, {FieldLaunchID, "ln-1763719200000-7-12"}, {FieldProfile, "gemma-4-12b"}})
	want := map[string]string{"node_id": "02:00:00:00:00:01", "node_name": "console-1", "platform": "linux-amd64-vulkan",
		"release": "0.1.0", "source": "launch", "launch_id": "ln-1763719200000-7-12", "profile": "gemma-4-12b"}
	if fmt.Sprint(extra) != fmt.Sprint(want) {
		t.Fatalf("extra fields\n got %v\nwant %v", extra, want)
	}
	if strings.Join(order, ",") != "node_id,node_name,platform,release,source,launch_id,profile" {
		t.Fatalf("order %v", order)
	}
	if strings.Join(ignore, ",") != "actor,client_addr" {
		t.Fatalf("ignore_fields %v", ignore)
	}
}

// Values with the characters that matter to the store's parser: a comma would
// start a new field, an equals sign separates name and value, node ids hold
// colons. No value may add, rename or empty a field.
func TestExtraFieldsHostileValues(t *testing.T) {
	cases := []struct{ in, want string }{
		{"02:00:00:00:00:01", "02:00:00:00:00:01"},
		{"a,node_id=02:00:00:00:00:99", "a%2Cnode_id=02:00:00:00:00:99"},
		{"k=v=w", "k=v=w"},
		{"=", "="},
		{"==", "=="},
		{"a=", "a="},
		{"50% done", "50%25 done"},
		{"%2C", "%252C"},
		{",", "%2C"},
		{",,,source=agent,,", "%2C%2C%2Csource=agent%2C%2C"},
		{"  padded name  ", "padded name"},
		{"tab\there\nnewline", "tab here newline"},
		{"a&extra_fields=node_id=x&b", "a&extra_fields=node_id=x&b"},
		{"a#b?c/d+e f", "a#b?c/d+e f"},
		{"knoten-ä-節点", "knoten-ä-節点"},
		{"bad\xffbyte", "badbyte"},
		{"fe80::1%eth0", "fe80::1%25eth0"},
		{"[2001:db8::1]:8080", "[2001:db8::1]:8080"},
	}
	for _, c := range cases {
		extra, _, ignore := parseQuery(t, []Field{{FieldNodeID, "02:00:00:00:00:01"}, {FieldNodeName, c.in}, {FieldSource, "agent"}})
		if len(extra) != 3 || extra["node_id"] != "02:00:00:00:00:01" || extra["source"] != "agent" {
			t.Errorf("%q: the value changed another field: %v", c.in, extra)
		}
		if extra["node_name"] != c.want {
			t.Errorf("%q: stored as %q, want %q", c.in, extra["node_name"], c.want)
		}
		for _, n := range ignore {
			if _, both := extra[n]; both {
				t.Errorf("%q: %s is in extra_fields and in ignore_fields", c.in, n)
			}
		}
	}
}

// An empty value would make the store refuse the whole request, so the field
// is left out and ignored instead: a line cannot set it either.
func TestExtraFieldsEmptyValuesAreIgnoredFields(t *testing.T) {
	for _, empty := range []string{"", "   ", "\t\n", "\xff"} {
		extra, _, ignore := parseQuery(t, []Field{{FieldSource, "client"}, {FieldClientAddr, "192.0.2.50"}, {FieldActor, empty}})
		if len(extra) != 2 || extra["source"] != "client" || extra["client_addr"] != "192.0.2.50" {
			t.Fatalf("%q: %v", empty, extra)
		}
		if strings.Join(ignore, ",") != "node_id,node_name,platform,release,launch_id,profile,actor" {
			t.Fatalf("%q: ignore_fields %v", empty, ignore)
		}
	}
}

func TestExtraFieldsLongValueIsCut(t *testing.T) {
	extra, _, _ := parseQuery(t, []Field{{FieldActor, strings.Repeat("x,", 4000)}, {FieldSource, "client"}})
	if n := len(extra["actor"]); n < maxFieldValue || n > maxFieldValue+8 {
		t.Fatalf("actor is %d bytes", n)
	}
	if extra["source"] != "client" || len(extra) != 2 {
		t.Fatalf("%v", extra)
	}
}
