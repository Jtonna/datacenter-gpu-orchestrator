package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The key goes in after the opening brace and nothing else changes; any
// body that is not a streamed JSON object without the key is untouched.
func TestAddTimingsPerToken(t *testing.T) {
	for _, c := range []struct {
		in, want string // want "" = untouched
	}{
		{`{"model":"m","stream":true,"messages":[]}`, `{"timings_per_token":true,"model":"m","stream":true,"messages":[]}`},
		{"  \n{ \"stream\" : true , \"n\": 1.50}", "  \n{\"timings_per_token\":true, \"stream\" : true , \"n\": 1.50}"},
		{`{"model":"m","stream":false}`, ""},
		{`{"model":"m"}`, ""},
		{`{"model":"m","stream":"true"}`, ""},
		{`{"model":"m","stream":true,"timings_per_token":false}`, ""},
		{`{"model":"m","stream":true,"timings_per_token":true}`, ""},
		{`{"model":"m","messages":[{"stream":true}]}`, ""},
		{`[{"stream":true}]`, ""},
		{`not json {"stream":true}`, ""},
		{`{"stream":true`, ""},
		{``, ""},
	} {
		out, ok := addTimingsPerToken([]byte(c.in))
		if c.want == "" {
			if ok {
				t.Errorf("%q rewritten to %q", c.in, out)
			}
			continue
		}
		if !ok || string(out) != c.want || !json.Valid(out) {
			t.Errorf("%q -> %q %v, want %q", c.in, out, ok, c.want)
		}
	}
	// Through Extracted: the body and its length follow; twice is once.
	e := &Extracted{data: []byte(`{"model":"m","stream":true}`)}
	if !e.AddTimingsPerToken() || e.AddTimingsPerToken() || !e.Rewritten() || e.BufferedBytes() != int64(len(e.data)) {
		t.Fatalf("%+v", e)
	}
	var b bytes.Buffer
	b.ReadFrom(e.Body)
	if b.String() != `{"timings_per_token":true,"model":"m","stream":true}` {
		t.Fatal(b.String())
	}
	if (&Extracted{}).AddTimingsPerToken() { // multipart: no whole body
		t.Fatal("a multipart body rewritten")
	}
}
