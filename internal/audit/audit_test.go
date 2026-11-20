package audit

import (
	"net/http/httptest"
	"testing"
)

func TestFromRequestSourceAndActor(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r.RemoteAddr = "192.0.2.5:4321"
	r.Header.Set("User-Agent", "curl/8")
	o := FromRequest(r)
	if o.Source != SourceREST || o.Actor != DefaultActor || o.RemoteAddr != "192.0.2.5" || o.UserAgent != "curl/8" {
		t.Fatalf("%+v", o)
	}
	r.Header.Set("X-Source", "ui")
	r.Header.Set("X-Actor", "web page")
	r.Header.Set("X-Comment", "why")
	if o = FromRequest(r); o.Source != SourceUI || o.Actor != "web page" || o.Comment != "why" {
		t.Fatalf("%+v", o)
	}
	// An outside client cannot claim mcp, import or system.
	for _, claim := range []string{"mcp", "import", "system", "MCP"} {
		r.Header.Set("X-Source", claim)
		if o = FromRequest(r); o.Source != SourceREST {
			t.Fatalf("X-Source %s accepted as %s", claim, o.Source)
		}
	}
	// Only an in-process caller can set it.
	r2 := r.WithContext(WithSource(r.Context(), SourceMCP, "claude-code"))
	if o = FromRequest(r2); o.Source != SourceMCP || o.Actor != "claude-code" {
		t.Fatalf("%+v", o)
	}
}
