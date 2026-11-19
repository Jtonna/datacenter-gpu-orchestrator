package main

// The WebSocket front. The browser connects here; the relay dials the real
// server's /ws and copies frames both ways. On the way down it adds what the
// mock contract promises (hello.data.bench and config_revision, status
// config_revision) and it injects the mock's own frames: config after each
// config write and bench from the mock engine (a bench state frame follows
// hello). If the upstream connection drops, the client connection is closed.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type relayClient struct {
	conn   *websocket.Conn
	wmu    sync.Mutex
	mu     sync.Mutex
	topics map[string]bool // bench and config only; the rest is upstream's business
	ready  bool            // hello sent: injected frames may follow
}

func (c *relayClient) wants(t string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && c.topics[t]
}

func (c *relayClient) write(ctx context.Context, b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, b)
}

type relay struct {
	upstream string // ws://127.0.0.1:port/ws
	benchOn  bool
	revision func() int
	state    func() map[string]any // bench state document

	mu      sync.Mutex
	clients map[*relayClient]struct{}
}

func newRelay(upstreamHTTP string, benchOn bool, revision func() int, state func() map[string]any) *relay {
	return &relay{upstream: "ws" + strings.TrimPrefix(upstreamHTTP, "http") + "/ws", benchOn: benchOn, revision: revision, state: state,
		clients: map[*relayClient]struct{}{}}
}

func frame(typ string, data any) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "ts": time.Now().UnixMilli(), "data": data})
	return b
}

// inject sends a mock frame to every client that wants its topic.
func (rl *relay) inject(typ string, data any) {
	if typ == "bench" && !rl.benchOn {
		return
	}
	b := frame(typ, data)
	rl.mu.Lock()
	var cs []*relayClient
	for c := range rl.clients {
		cs = append(cs, c)
	}
	rl.mu.Unlock()
	for _, c := range cs {
		if c.wants(typ) {
			go c.write(context.Background(), b)
		}
	}
}

// augment adds the mock fields to a frame from the real server.
func (rl *relay) augment(b []byte) (out []byte, isHello bool) {
	var f map[string]json.RawMessage
	if json.Unmarshal(b, &f) != nil {
		return b, false
	}
	var typ string
	json.Unmarshal(f["type"], &typ)
	if typ != "hello" && typ != "status" {
		return b, false
	}
	var data map[string]any
	if json.Unmarshal(f["data"], &data) != nil || data == nil {
		return b, typ == "hello"
	}
	data["config_revision"] = rl.revision()
	if typ == "hello" {
		data["bench"] = map[bool]string{true: "on", false: "off"}[rl.benchOn]
		topics := []any{}
		if l, ok := data["topics"].([]any); ok {
			topics = l
		}
		data["topics"] = append(topics, "config")
	}
	f["data"], _ = json.Marshal(data)
	out, _ = json.Marshal(f)
	return out, typ == "hello"
}

func (rl *relay) serveWS(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	up, _, err := websocket.Dial(ctx, rl.upstream, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		writeErr(w, 502, "the real server's /ws did not answer: "+err.Error(), "bad gateway")
		return
	}
	up.SetReadLimit(64 << 20)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		up.Close(websocket.StatusGoingAway, "client handshake failed")
		return
	}
	conn.SetReadLimit(4096)
	c := &relayClient{conn: conn, topics: map[string]bool{"bench": true, "config": true}}
	rl.mu.Lock()
	rl.clients[c] = struct{}{}
	rl.mu.Unlock()
	defer func() {
		rl.mu.Lock()
		delete(rl.clients, c)
		rl.mu.Unlock()
	}()

	done := make(chan struct{})
	go func() { // upstream -> client
		defer close(done)
		for {
			_, b, err := up.Read(ctx)
			if err != nil {
				conn.Close(websocket.StatusGoingAway, "the scheduler connection closed")
				cancel()
				return
			}
			b, hello := rl.augment(b)
			if c.write(ctx, b) != nil {
				cancel()
				return
			}
			if hello {
				c.mu.Lock()
				c.ready = true
				c.mu.Unlock()
				if rl.benchOn && c.wants("bench") {
					c.write(ctx, frame("bench", map[string]any{"kind": "state", "state": rl.state()}))
				}
			}
		}
	}()
	// client -> upstream
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var m struct {
			Type   string   `json:"type"`
			Topics []string `json:"topics"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "subscribe" {
			next := map[string]bool{}
			for _, t := range m.Topics {
				next[t] = true
			}
			c.mu.Lock()
			prevBench := c.topics["bench"]
			c.topics = map[string]bool{"bench": next["bench"], "config": next["config"]}
			c.mu.Unlock()
			if next["bench"] && !prevBench && rl.benchOn && c.wants("bench") {
				c.write(ctx, frame("bench", map[string]any{"kind": "state", "state": rl.state()}))
			}
		}
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		err = up.Write(wctx, typ, b)
		wcancel()
		if err != nil {
			break
		}
	}
	up.Close(websocket.StatusNormalClosure, "")
	conn.Close(websocket.StatusNormalClosure, "")
	cancel()
	<-done
}

// closeAll ends every relayed connection (shutdown).
func (rl *relay) closeAll() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for c := range rl.clients {
		c.conn.Close(websocket.StatusGoingAway, "the dev harness is shutting down")
	}
}
