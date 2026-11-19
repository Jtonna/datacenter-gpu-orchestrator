package main

// The WebSocket front. The browser connects here; the relay dials the real
// server's /ws and copies frames both ways unchanged (the real server sends
// hello with config_revision, status, event, config and bench frames itself).
//
// DEV-ONLY, and only while a frozen bench scenario is selected
// (bench_frozen.go): the real server's bench frames are dropped and the
// frozen world's are sent instead, starting with its state frame after hello,
// on a bench subscription and when the scenario is selected.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type relayClient struct {
	conn  *websocket.Conn
	wmu   sync.Mutex
	mu    sync.Mutex
	bench bool // subscribed to the bench topic
	ready bool // hello sent: injected frames may follow
}

func (c *relayClient) wantsBench() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && c.bench
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

	// frozen, when set, is the state document of the frozen bench world: its
	// frames replace the real server's bench frames.
	frozen atomic.Pointer[func() any]

	mu      sync.Mutex
	clients map[*relayClient]struct{}
}

func newRelay(upstreamHTTP string) *relay {
	return &relay{upstream: "ws" + strings.TrimPrefix(upstreamHTTP, "http") + "/ws", clients: map[*relayClient]struct{}{}}
}

func frame(typ string, data any) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "ts": time.Now().UnixMilli(), "data": data})
	return b
}

// setFrozen switches the bench frames to a frozen world (state != nil) or
// back to the real server (state nil, real is its state document), and
// sends every client the state frame of the world now shown.
func (rl *relay) setFrozen(state func() any, real func() any) {
	if state == nil {
		rl.frozen.Store(nil)
		rl.broadcastBench(map[string]any{"kind": "state", "state": real()})
		return
	}
	rl.frozen.Store(&state)
	rl.broadcastBench(map[string]any{"kind": "state", "state": state()})
}

// injectFrozen sends a frame of the frozen world, if it is the one shown.
func (rl *relay) injectFrozen(ev map[string]any) {
	if rl.frozen.Load() != nil {
		rl.broadcastBench(ev)
	}
}

func (rl *relay) broadcastBench(ev map[string]any) {
	b := frame("bench", ev)
	rl.mu.Lock()
	var cs []*relayClient
	for c := range rl.clients {
		cs = append(cs, c)
	}
	rl.mu.Unlock()
	for _, c := range cs {
		if c.wantsBench() {
			go c.write(context.Background(), b)
		}
	}
}

func (rl *relay) frozenState(c *relayClient, ctx context.Context) {
	if f := rl.frozen.Load(); f != nil && c.wantsBench() {
		c.write(ctx, frame("bench", map[string]any{"kind": "state", "state": (*f)()}))
	}
}

func frameType(b []byte) string {
	var f struct {
		Type string `json:"type"`
	}
	json.Unmarshal(b, &f)
	return f.Type
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
	c := &relayClient{conn: conn, bench: true}
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
			typ := frameType(b)
			if typ == "bench" && rl.frozen.Load() != nil {
				continue // DEV-ONLY: a frozen bench scenario shows its own world
			}
			if c.write(ctx, b) != nil {
				cancel()
				return
			}
			if typ == "hello" {
				c.mu.Lock()
				c.ready = true
				c.mu.Unlock()
				rl.frozenState(c, ctx)
			}
		}
	}()
	// client -> upstream
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			break
		}
		sendState := false
		var m struct {
			Type   string   `json:"type"`
			Topics []string `json:"topics"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "subscribe" {
			on := false
			for _, t := range m.Topics {
				on = on || t == "bench"
			}
			c.mu.Lock()
			was := c.bench
			c.bench = on
			c.mu.Unlock()
			sendState = on && !was
		}
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		err = up.Write(wctx, typ, b)
		wcancel()
		if err != nil {
			break
		}
		if sendState {
			rl.frozenState(c, ctx)
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
