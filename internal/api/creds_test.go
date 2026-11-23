package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/mcpserver"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/proxy"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/usage"
)

// filesHolding lists the files under dir whose bytes contain s.
func filesHolding(dir, s string) []string {
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), s) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// A client's credentials are not Potluck's to keep or pass on. A marked
// value sent in each client credential header (proxy.ClientCredentialHeaders)
// through /v1 (streamed and not, a client that leaves, a model server that
// fails, an unknown model), /upstream (streamed and not), /api and MCP
// reaches neither the model server nor any sink the test can read: the log
// at every level, response bodies, the audit log, history events, and the
// request log (usage.db) and history files byte for byte.
func TestClientCredentials_ReachNoModelServerAndNoSink(t *testing.T) {
	const mark = "sk-test-CANARY-7d1e"
	creds := map[string]string{} // every credential header but X-Agent-Key (Potluck reads that one as its own key)
	for _, h := range proxy.ClientCredentialHeaders {
		if h != "X-Agent-Key" {
			creds[h] = "Bearer " + mark + "-" + h
		}
	}
	hdr := func(extra map[string]string) map[string]string {
		m := map[string]string{"Content-Type": "application/json"}
		for k, v := range creds {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	logs := &syncBuf{}
	debug := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug - 4}))
	var bodies strings.Builder
	seenUp := &syncBuf{}

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
	e := newEnv(t, spy, func(o *Options) { o.Requests, o.Usage, o.History, o.Logger = usage.NewRecorder(st), st, hist, debug })
	send := func(path, body string, leave bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", e.front.URL+path, strings.NewReader(body))
		for k, v := range hdr(nil) {
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
	send("/v1/chat/completions", `{"model":"no-such-model","messages":[]}`, false)
	send("/v1/chat/completions", `{"model":"alpha",`, false)
	up.dieAfter = 12 // the model server fails mid-stream
	send("/v1/chat/completions", `{"model":"alpha","stream":true,"messages":[{"role":"user","content":"hi"}]}`, false)
	eventually(t, "every request ended", func() bool { return e.api.reqs.Ended() >= 6 })
	// X-Agent-Key is Potluck's own: an unknown value is refused (401) and not echoed
	_, b, _ := e.hdo("POST", "/v1/chat/completions", []byte(chat("alpha")), map[string]string{"X-Agent-Key": mark + "-X-Agent-Key", "Content-Type": "application/json"})
	bodies.Write(b)
	for _, p := range []string{"/api/status", "/api/requests?limit=100", "/api/history?limit=500", "/api/system"} {
		code, b, _ := e.hdo("GET", p, nil, hdr(nil))
		if code != 200 {
			t.Errorf("GET %s with client credentials: %d", p, code)
		}
		bodies.Write(b)
	}
	flushT(t, st)

	// /api and MCP: audited changes, tool calls, failures
	c, _, _ := cfgEnvWith(t, func(o *Options) { o.Logger, o.History = debug, hist })
	mcp := mcpserver.New(c.api, mcpserver.Options{Version: "test", Identify: c.api.IdentifyAgent,
		Bridge: func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }})
	for _, r := range [][3]string{
		{"POST", "/api/config/profiles", `{"id":"c1","model":"c1.gguf","ctx":2048,"needs_mib":100}`},
		{"PATCH", "/api/config/settings", `{"queue_timeout":"901s"}`},
		{"PUT", "/api/config/tiers", `"not a list"`},
		{"POST", "/api/agents", `{"name":"made-with-client-credentials"}`},
	} {
		_, b, _ := c.hdo(r[0], r[1], []byte(r[2]), hdr(nil))
		bodies.Write(b)
	}
	for _, call := range [][2]string{{"get_status", `{}`}, {"create_profile", `{"profile":{"id":"c2","model":"c2.gguf","ctx":2048,"needs_mib":100}}`},
		{"create_profile", `{"profile":{"id":""}}`}, {"no_such_tool", `{}`}} {
		_, out := mcpCall(mcp, call[0], call[1], creds)
		bodies.WriteString(out)
	}
	_, ab, _ := c.hdo("GET", "/api/audit?limit=500", nil, nil)
	evs, _ := json.Marshal(hist.Query(history.Query{Limit: history.MaxLimit}))
	if !strings.Contains(string(ab), "c1") || !strings.Contains(string(ab), "c2") {
		t.Fatalf("the audit log holds nothing to search: %s", ab)
	}

	for what, text := range map[string]string{"the log (every level)": logs.String(), "the response bodies": bodies.String(),
		"the audit log": string(ab), "the history events": string(evs), "the model server's headers": seenUp.String()} {
		if strings.Contains(text, mark) {
			i := strings.Index(text, mark)
			t.Errorf("%s holds a client credential: …%s…", what, text[max(0, i-80):min(len(text), i+60)])
		}
	}
	if !strings.Contains(seenUp.String(), "Content-Type") {
		t.Fatal("the model server's headers were not seen")
	}
	for _, dir := range []string{usageDir, histDir} {
		if f := filesHolding(dir, mark); len(f) > 0 {
			t.Errorf("files hold a client credential: %v", f)
		}
	}
	if code, rb, _ := e.hdo("GET", "/api/requests?limit=100", nil, nil); code != 200 || !strings.Contains(string(rb), "alpha") || logs.String() == "" {
		t.Fatal("no request rows or log lines to search")
	}
}
