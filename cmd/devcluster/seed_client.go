package main

// Clients for the startup seed: plain HTTP to the real server (as the UI and
// curl call it) and an MCP session (as an agent calls it). Every seeded write
// goes through the real handlers, so the audit log and the revisions record
// it with its real source and actor.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type seedClient struct {
	base string
	hc   *http.Client
}

func newSeedClient(base string) *seedClient {
	return &seedClient{base: base, hc: &http.Client{Timeout: 30 * time.Second}}
}

// who is the origin of a write: source "ui" (sent with X-Source: ui) or
// "rest" (no X-Source), the actor and the comment.
type who struct{ source, actor, comment string }

var (
	asJacob = who{source: "ui", actor: "jacob"}
	asCurl  = who{source: "rest", actor: "curl"}
)

func (w who) say(comment string) who { w.comment = comment; return w }

// do sends a request; a status of 300 or more is an error.
func (s *seedClient) do(method, path string, w who, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.source == "ui" {
		req.Header.Set("X-Source", "ui")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) devcluster seed")
	} else {
		req.Header.Set("User-Agent", "curl/8.9.1")
	}
	if w.actor != "" {
		req.Header.Set("X-Actor", w.actor)
	}
	if w.comment != "" {
		req.Header.Set("X-Comment", w.comment)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

// mcpClient is one MCP session on the real /mcp endpoint.
type mcpClient struct {
	url string
	sid string
	n   int
	hc  *http.Client
}

func (m *mcpClient) post(body string) (*http.Response, []byte, error) {
	req, _ := http.NewRequest("POST", m.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.sid != "" {
		req.Header.Set("Mcp-Session-Id", m.sid)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b, nil
}

// newMCP opens a session as the client name and version (the agent the audit names).
func newMCP(base, name, version string) (*mcpClient, error) {
	m := &mcpClient{url: base + "/mcp", hc: &http.Client{Timeout: 30 * time.Second}}
	init := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":%q,"version":%q}}}`, name, version)
	resp, b, err := m.post(init)
	if err != nil {
		return nil, err
	}
	if m.sid = resp.Header.Get("Mcp-Session-Id"); m.sid == "" {
		return nil, fmt.Errorf("mcp initialize: no session: %s", b)
	}
	if _, _, err := m.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		return nil, err
	}
	m.n = 1
	return m, nil
}

// call runs a tool and decodes its JSON text answer.
func (m *mcpClient) call(tool string, args map[string]any) (map[string]any, error) {
	m.n++
	a, _ := json.Marshal(args)
	_, b, err := m.post(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, m.n, tool, a))
	if err != nil {
		return nil, err
	}
	var o struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
		Error any
	}
	if err := json.Unmarshal(b, &o); err != nil || o.Error != nil || o.Result.IsError || len(o.Result.Content) == 0 {
		return nil, fmt.Errorf("mcp %s: %v %s", tool, err, b)
	}
	var out map[string]any
	if json.Unmarshal([]byte(o.Result.Content[0].Text), &out) != nil {
		out = map[string]any{"text": o.Result.Content[0].Text}
	}
	return out, nil
}
