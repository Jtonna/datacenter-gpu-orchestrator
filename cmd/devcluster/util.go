package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		proxy.WriteError(w, 500, err.Error(), "server_error", "internal error", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// writeErr is the real server's error shape (proxy.WriteError): code is the
// HTTP status, reason a short word.
func writeErr(w http.ResponseWriter, status int, msg, reason string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	proxy.WriteError(w, status, msg, typ, reason, nil)
}

// mockErr is the error shape of the mock contract: code is a string.
// extra keys are added at the top level (such as "revision").
func mockErr(w http.ResponseWriter, status int, msg, code string, extra map[string]any) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	doc := map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}}
	for k, v := range extra {
		doc[k] = v
	}
	writeJSON(w, status, doc)
}

func readJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		mockErr(w, 413, "the body is larger than 4 MiB or unreadable", "body_too_large", nil)
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		mockErr(w, 400, "the body is not valid JSON: "+err.Error(), "bad_json", nil)
		return false
	}
	return true
}

func qInt(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func qInt64(r *http.Request, name string) int64 {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// clone deep-copies a JSON-shaped value.
func clone(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

func cloneMap(v map[string]any) map[string]any {
	m, _ := clone(v).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func jsonRaw(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}
