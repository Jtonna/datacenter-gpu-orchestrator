package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// canary is a credential of another provider, as a client sends by habit.
func canary(t *testing.T) string {
	var b [12]byte
	rand.Read(b[:])
	return "sk-CANARY-" + hex.EncodeToString(b[:])
}

// A credential that is not a Potluck key (another provider's real key, sent
// by habit) is never written anywhere: not to the log at any level, the
// audit log, a history event, the request log (usage.db), a response body or
// the model server. It goes through /v1 (streamed and not, a client that
// leaves, a model server that fails, an unknown model), /upstream, /api and
// MCP; then every sink the test can reach is searched for it, the database
// and history files byte for byte.
func TestAccessKeys_ForeignCredentialIsWrittenNowhere(t *testing.T) {
	tok := canary(t)
	auth := map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"}
	logs := &syncBuf{}
	debug := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug - 4}))
	var bodies strings.Builder
	seenUp := &syncBuf{} // every header the model server got

	// /v1 and /upstream: a real request log and history on disk
	usageDir, histDir := t.TempDir(), t.TempDir()
	st := usage.Open(usage.Options{Dir: usageDir, Logger: debug})
	t.Cleanup(func() { st.Close() })
	hist := history.Open(history.Options{Dir: histDir, Logger: debug})
	t.Cleanup(hist.Close)
	up := &endless{sent: make(chan int, 8)}
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.Header {
			seenUp.Write([]byte(k + ": " + strings.Join(v, ",") + "\n"))
		}
		up.ServeHTTP(w, r)
	})
	e := newEnv(t, spy, func(o *Options) {
		o.Requests, o.Usage, o.History, o.Logger, o.keyExpiryEvery = usage.NewRecorder(st), st, hist, debug, -1
	})
	send := func(path, body string, leave bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", e.front.URL+path, strings.NewReader(body))
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if leave {
			<-up.sent
			rd := bufio.NewReader(resp.Body)
			for i := 0; i < 6; i++ {
				line, _ := rd.ReadString('\n')
				bodies.WriteString(line)
			}
			cancel()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		bodies.Write(b)
	}
	send("/v1/chat/completions", chat("alpha"), false) // loads alpha
	eventually(t, "the first request ended", func() bool { return e.api.reqs.Ended() == 1 })
	send("/v1/chat/completions", `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)
	send("/v1/completions", `{"model":"alpha","prompt":"hi"}`, false)
	send("/upstream/alpha/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)
	send("/upstream/alpha/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, false)
	send("/v1/chat/completions", `{"model":"no-such-model","messages":[]}`, false) // refused
	send("/v1/chat/completions", `{"model":"alpha",`, false)                       // malformed
	up.dieAfter = 12                                                               // the model server fails mid-stream
	send("/v1/chat/completions", `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"hi"}]}`, false)
	eventually(t, "every request ended", func() bool { return e.api.reqs.Ended() >= 6 })
	for _, p := range []string{"/api/status", "/api/requests?limit=100", "/api/history?limit=500", "/api/system"} {
		st, b, _ := e.hdo("GET", p, nil, auth)
		if st != 200 {
			t.Errorf("GET %s with a foreign token: %d", p, st)
		}
		bodies.Write(b)
	}
	flushT(t, st)

	// /api and MCP: audited changes, tool calls, a failing tool call
	c, _, _ := cfgEnvWith(t, func(o *Options) { o.Logger, o.History, o.keyExpiryEvery = debug, hist, -1 })
	mcp := mcpserver.New(c.api, mcpserver.Options{Version: "test", Identify: c.api.IdentifyAgent, Scope: c.api.ScopeOf,
		Bridge: func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }})
	for _, r := range [][3]string{
		{"POST", "/api/config/profiles", `{"id":"c1","model":"c1.gguf","ctx":2048,"needs_mib":100}`},
		{"PATCH", "/api/config/settings", `{"queue_timeout":"901s"}`},
		{"PUT", "/api/config/tiers", `"not a list"`}, // refused
		{"POST", "/api/agents", `{"name":"made-with-a-foreign-token"}`},
	} {
		_, b, _ := c.hdo(r[0], r[1], []byte(r[2]), auth)
		bodies.Write(b)
	}
	for _, call := range [][2]string{{"get_status", `{}`}, {"create_profile", `{"profile":{"id":"c2","model":"c2.gguf","ctx":2048,"needs_mib":100}}`},
		{"create_profile", `{"profile":{"id":""}}`}, {"no_such_tool", `{}`}} {
		_, out := mcpCall(mcp, call[0], call[1], map[string]string{"Authorization": "Bearer " + tok})
		bodies.WriteString(out)
	}
	_, ab, _ := c.hdo("GET", "/api/audit?limit=500", nil, nil)
	evs, _ := json.Marshal(hist.Query(history.Query{Limit: history.MaxLimit}))
	if len(ab) < 100 || !strings.Contains(string(ab), "c1") {
		t.Fatalf("the audit log holds nothing to search: %s", ab)
	}

	for what, text := range map[string]string{"the log (every level)": logs.String(), "the response bodies": bodies.String(),
		"the audit log": string(ab), "the history events": string(evs), "the model server's headers": seenUp.String()} {
		if strings.Contains(text, tok) {
			t.Errorf("%s holds the foreign credential", what)
		}
	}
	if !strings.Contains(seenUp.String(), "Content-Type") {
		t.Fatal("the model server's headers were not seen")
	}
	if f := filesHolding(usageDir, tok); len(f) > 0 {
		t.Errorf("usage files hold the foreign credential: %v", f)
	}
	if f := filesHolding(histDir, tok); len(f) > 0 {
		t.Errorf("history files hold the foreign credential: %v", f)
	}
	// the test reached the sinks: rows, events and log lines exist
	if rows, rb, _ := e.hdo("GET", "/api/requests?limit=100", nil, nil); rows != 200 || !strings.Contains(string(rb), "alpha") || logs.String() == "" {
		t.Fatal("no request rows or log lines to search")
	}

}
