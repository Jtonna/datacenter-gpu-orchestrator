package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// queuedEnv is an env whose upstream holds "alpha" chat requests until
// release is closed (keeping alpha busy so a "beta:64k" request must queue)
// and records the multipart bodies it receives.
type queuedEnv struct {
	*env
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	bodies  [][32]byte
	lens    []int
}

func newQueuedEnv(t *testing.T, mod ...func(*Options)) *queuedEnv {
	q := &queuedEnv{release: make(chan struct{})}
	q.env = newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			<-q.release
			io.WriteString(w, `{"ok":true}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		q.mu.Lock()
		q.bodies = append(q.bodies, sha256.Sum256(b))
		q.lens = append(q.lens, len(b))
		q.mu.Unlock()
		io.WriteString(w, `{"text":"ok"}`)
	}), mod...)
	// registered after newEnv's cleanup, so it runs first: the held alpha
	// request must finish before the front server can close
	t.Cleanup(q.unblock)
	return q
}

func (q *queuedEnv) unblock() { q.once.Do(func() { close(q.release) }) }

func (q *queuedEnv) busyAlpha() {
	q.t.Helper()
	go func() {
		resp, err := http.Post(q.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"alpha"}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	eventually(q.t, "alpha in flight", func() bool {
		st, err := q.sched.Status(context.Background())
		return err == nil && len(st.Instances) == 1 && st.Instances[0].InFlight == 1
	})
}

func (q *queuedEnv) waiters() int {
	st, err := q.sched.Status(context.Background())
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range st.Queue {
		n += e.Waiters
	}
	return n
}

// multipartBody is a multipart upload with the model field first and a
// large file part.
func multipartBody(fileBytes int) (ctype string, body []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "beta:64k")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	chunk := make([]byte, 1000)
	for i := 0; i < fileBytes/len(chunk); i++ {
		for j := range chunk {
			chunk[j] = byte(i*7 + j)
		}
		fw.Write(chunk)
	}
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

func dialRaw(t *testing.T, e *env) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(e.front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func writeHead(t *testing.T, c net.Conn, ctype string, n int) {
	t.Helper()
	fmt.Fprintf(c, "POST /v1/audio/transcriptions HTTP/1.1\r\nHost: x\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", ctype, n)
}

// Design 8.2 step 5: a multipart client that goes away while queued is
// removed from the queue even though most of its body was never read.
func TestQueuedMultipartClientDisconnectLeavesTheQueue(t *testing.T) {
	q := newQueuedEnv(t)
	q.busyAlpha()
	ctype, body := multipartBody(4 << 20)
	c := dialRaw(t, q.env)
	writeHead(t, c, ctype, len(body))
	c.Write(body[:300<<10])
	eventually(t, "beta waiting in the queue", func() bool { return q.waiters() == 1 })
	c.Close()
	eventually(t, "waiter to leave after the client closed mid-upload", func() bool { return q.waiters() == 0 })
}

// A slow but complete upload that is still arriving when the model loads
// reaches the upstream byte for byte.
func TestQueuedMultipartSlowUploadArrivesIntact(t *testing.T) {
	q := newQueuedEnv(t)
	q.busyAlpha()
	ctype, body := multipartBody(3 << 20)
	c := dialRaw(t, q.env)
	writeHead(t, c, ctype, len(body))
	c.Write(body[:300<<10])
	eventually(t, "beta waiting in the queue", func() bool { return q.waiters() == 1 })
	// keep uploading slowly while queued; alpha finishes part-way through
	// the upload, so beta loads while bytes are still arriving
	time.AfterFunc(400*time.Millisecond, q.unblock)
	c.SetWriteDeadline(time.Now().Add(20 * time.Second))
	for off := 300 << 10; off < len(body); {
		end := min(off+64<<10, len(body))
		if _, err := c.Write(body[off:end]); err != nil {
			t.Fatalf("upload stalled at %d of %d: %v", off, len(body), err)
		}
		off = end
		time.Sleep(15 * time.Millisecond)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.bodies) != 1 || q.lens[0] != len(body) || q.bodies[0] != sha256.Sum256(body) {
		t.Fatalf("upstream got %v bodies, lens %v, want one of %d identical bytes", len(q.bodies), q.lens, len(body))
	}
}

// The read-ahead while queued is capped like the prefix: past the cap the
// request is answered 413 and leaves the queue.
func TestQueuedMultipartUploadOverCapIs413AndLeavesTheQueue(t *testing.T) {
	q := newQueuedEnv(t, func(o *Options) { o.Limits.MaxMultipartPrefix = 400 << 10 })
	q.busyAlpha()
	ctype, body := multipartBody(2 << 20)
	c := dialRaw(t, q.env)
	writeHead(t, c, ctype, len(body))
	go func() { // the server may stop reading once it has answered
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		c.Write(body)
	}()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	eventually(t, "waiter to leave", func() bool { return q.waiters() == 0 })
}

// HEAD on a GET route answers with the GET headers and no body; on the
// event stream it must not open a stream (nginx forwards HEAD there).
func TestHeadOnGetRoutesAnswersHeadersOnly(t *testing.T) {
	e := newEnv(t, chatOK)
	cl := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/api/events", "/health", "/running", "/v1/models", "/api/metrics/activity", "/api/status", "/ui/"} {
		req, _ := http.NewRequest(http.MethodHead, e.front.URL+path, nil)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("HEAD %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(b) != 0 {
			t.Errorf("HEAD %s: %d with %d body bytes", path, resp.StatusCode, len(b))
		}
		if path == "/api/events" && resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("HEAD /api/events content-type %q", resp.Header.Get("Content-Type"))
		}
	}
	if n := e.api.hub.clientCount(); n != 0 {
		t.Fatalf("HEAD opened %d event streams", n)
	}
}

// The sum of JSON bodies buffered for requests that are waiting is capped; a
// request that would exceed the cap is answered 503 with Retry-After.
func TestQueuedJSONBodiesAreCappedInTotal(t *testing.T) {
	q := newQueuedEnv(t, func(o *Options) { o.MaxQueuedBodyBytes = 5000 })
	q.busyAlpha()
	req := func() string {
		return `{"model":"beta:64k","pad":"` + strings.Repeat("x", 3000) + `"}`
	}
	first := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(q.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(req()))
		if err != nil {
			t.Error(err)
			close(first)
			return
		}
		first <- resp
	}()
	eventually(t, "first beta request queued", func() bool { return q.waiters() == 1 })
	resp, err := http.Post(q.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(req()))
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second request: %d retry-after %q: %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}
	if q.waiters() != 1 {
		t.Fatalf("waiters = %d", q.waiters())
	}
	// small requests still fit under the cap
	q.unblock()
	r1 := <-first
	if r1 != nil {
		body(t, r1)
	}
	resp = q.post("/v1/chat/completions", req())
	if b := body(t, resp); resp.StatusCode != 200 {
		t.Fatalf("after the queue drained: %d %s", resp.StatusCode, b)
	}
}
