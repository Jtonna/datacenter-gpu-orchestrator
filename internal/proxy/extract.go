package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Error is a request-level failure with the HTTP status to answer.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

// Limits bound how much of a request body Extract may hold. Zero values
// select the defaults.
type Limits struct {
	// MaxJSON caps a JSON body, which is buffered whole (default 32 MiB).
	MaxJSON int64
	// MemBytes is how much of a multipart upload's prefix (everything read
	// while looking for the model field) stays in memory before spilling
	// to a temp file (default 1 MiB).
	MemBytes int64
	// MaxMultipartPrefix caps that prefix, so a client cannot make us
	// buffer an unbounded upload while the model field is still missing
	// (default 1 GiB).
	MaxMultipartPrefix int64
	// TempDir is where spill files go (default os.TempDir()).
	TempDir string
}

// Default limits.
const (
	DefaultMaxJSON            = 32 << 20
	DefaultMemBytes           = 1 << 20
	DefaultMaxMultipartPrefix = 1 << 30
)

func (l Limits) withDefaults() Limits {
	if l.MaxJSON <= 0 {
		l.MaxJSON = DefaultMaxJSON
	}
	if l.MemBytes <= 0 {
		l.MemBytes = DefaultMemBytes
	}
	if l.MaxMultipartPrefix <= 0 {
		l.MaxMultipartPrefix = DefaultMaxMultipartPrefix
	}
	return l
}

// Extracted is the result of Extract: the model named by the request and a
// body that replays the complete original request body.
type Extracted struct {
	Model string
	// Body yields every byte of the original body, in order. Close it
	// (it removes any spill file).
	Body io.ReadCloser

	rb       *replayBody // Body, when the replay can still be pre-filled
	buffered int64
}

// Prefetch keeps reading the unread remainder of a multipart body into the
// bounded spill (memory, then a temp file, same cap as the prefix) in the
// background, until the first Read of Body (the hand-over to the upstream)
// or Close. It exists so that a client that disconnects while its request
// is queued is noticed: Go's http.Server only cancels the request context
// on disconnect after the body has been read to the end.
//
// onErr is called at most once, from the background goroutine, if the read
// fails before EOF (the client went away, or the cap was exceeded: then the
// error is a *Error with status 413) while Body has not been handed over.
// interrupt, if not nil, is called by Close to unblock a read in progress
// (e.g. a read deadline in the past). It is a no-op for bodies with nothing
// left to read, such as JSON.
func (e *Extracted) Prefetch(onErr func(error), interrupt func()) {
	if e.rb != nil {
		e.rb.startPrefetch(onErr, interrupt)
	}
}

// BufferedBytes is how much of the body is held in memory for a JSON body
// (0 for multipart, whose memory use is bounded by Limits.MemBytes).
func (e *Extracted) BufferedBytes() int64 { return e.buffered }

// Extract reads the model name from a request without consuming the body
// for good.
//
// JSON bodies (and bodies with no recognised content type) are buffered in
// memory up to Limits.MaxJSON, then parsed for a string "model" field; a
// larger body is refused with 413. Large JSON bodies are rare (images as
// base64), so buffering them is simpler than a streaming JSON scanner.
//
// multipart/form-data bodies are scanned part by part until the "model" form
// field is found. Everything read on the way (the prefix) is teed into a
// buffer that keeps Limits.MemBytes in memory and spills the rest to a temp
// file, capped at Limits.MaxMultipartPrefix (413 beyond). The replay body is
// that prefix followed by the unread remainder of the request body, which is
// streamed through, never buffered. A client that sends "model" first (the
// OpenAI audio clients do) therefore costs a few hundred bytes of memory.
func Extract(r *http.Request, lim Limits) (*Extracted, error) {
	lim = lim.withDefaults()
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" {
		return extractMultipart(r, params["boundary"], lim)
	}
	return extractJSON(r, lim)
}

func tooLarge(what string, limit int64) error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("%s larger than %d bytes", what, limit)}
}

func missingModel() error {
	return &Error{Status: http.StatusBadRequest, Message: "missing or invalid \"model\" in the request body"}
}

func extractJSON(r *http.Request, lim Limits) (*Extracted, error) {
	if r.ContentLength > lim.MaxJSON {
		return nil, tooLarge("request body", lim.MaxJSON)
	}
	if r.Body == nil {
		return nil, missingModel()
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, lim.MaxJSON+1))
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "reading request body: " + err.Error()}
	}
	if int64(len(data)) > lim.MaxJSON {
		return nil, tooLarge("request body", lim.MaxJSON)
	}
	var probe struct {
		Model any `json:"model"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return nil, missingModel()
	}
	name, ok := probe.Model.(string)
	if !ok || name == "" {
		return nil, missingModel()
	}
	return &Extracted{Model: name, Body: io.NopCloser(bytes.NewReader(data)), buffered: int64(len(data))}, nil
}

// spill is a write-only buffer that keeps a prefix in memory and puts the
// rest in a temp file.
type spill struct {
	mem     bytes.Buffer
	memMax  int64
	max     int64
	n       int64
	dir     string
	file    *os.File
	tooBig  bool
	failure error
}

var errPrefixTooLarge = errors.New("proxy: multipart prefix too large")

func (s *spill) Write(p []byte) (int, error) {
	if s.n+int64(len(p)) > s.max {
		s.tooBig = true
		return 0, errPrefixTooLarge
	}
	s.n += int64(len(p))
	if s.file == nil {
		room := s.memMax - int64(s.mem.Len())
		if int64(len(p)) <= room {
			s.mem.Write(p)
			return len(p), nil
		}
		f, err := os.CreateTemp(s.dir, "llm-scheduler-upload-*")
		if err != nil {
			s.failure = err
			return 0, err
		}
		s.file = f
	}
	return s.file.Write(p)
}

// readAt reads buffered bytes at off (the memory part comes first, then the
// file).
func (s *spill) readAt(p []byte, off int64) (int, error) {
	if m := int64(s.mem.Len()); off < m {
		return copy(p, s.mem.Bytes()[off:]), nil
	} else if s.file != nil {
		return s.file.ReadAt(p, off-m)
	}
	return 0, io.EOF
}

func (s *spill) discard() {
	if s.file != nil {
		name := s.file.Name()
		s.file.Close()
		os.Remove(name)
		s.file = nil
	}
}

// replay returns the buffered prefix followed by rest.
func (s *spill) replay(rest io.Reader, restCloser io.Closer) *replayBody {
	b := &replayBody{sp: s, rest: rest, closer: restCloser}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// replayBody yields the spilled bytes, then whatever is still unread. While
// a prefetch is running it also follows the spill as it grows.
type replayBody struct {
	sp     *spill
	rest   io.Reader
	closer io.Closer

	mu        sync.Mutex
	cond      *sync.Cond
	off       int64 // replay position in the spill
	handed    bool  // the replay has started (or Close ran): prefetch must stop
	closed    bool
	pumping   bool  // the prefetch goroutine is alive
	err       error // sticky outcome of the prefetch: io.EOF, a read error, or 413
	interrupt func()
}

func (b *replayBody) startPrefetch(onErr func(error), interrupt func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handed || b.pumping || b.err != nil {
		return
	}
	b.pumping, b.interrupt = true, interrupt
	go b.pump(onErr)
}

func (b *replayBody) pump(onErr func(error)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := b.rest.Read(buf)
		b.mu.Lock()
		if n > 0 && !b.closed {
			if _, werr := b.sp.Write(buf[:n]); werr != nil {
				switch {
				case b.sp.tooBig:
					err = tooLarge("multipart upload", b.sp.max)
				case b.sp.failure != nil:
					err = &Error{Status: http.StatusInternalServerError, Message: "buffering upload: " + b.sp.failure.Error()}
				default:
					err = werr
				}
			}
		}
		stop := b.handed
		if err != nil {
			b.err = err
		}
		report := err != nil && !errors.Is(err, io.EOF) && !stop
		if err != nil || stop {
			b.pumping = false
		}
		b.cond.Broadcast()
		b.mu.Unlock()
		if report {
			onErr(err)
		}
		if err != nil || stop {
			return
		}
	}
}

func (b *replayBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.handed = true
	for {
		if b.off < b.sp.n {
			n, err := b.sp.readAt(p[:min(int64(len(p)), b.sp.n-b.off)], b.off)
			b.off += int64(n)
			b.mu.Unlock()
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b.pumping {
			b.cond.Wait() // the in-flight read of the prefetch finishes first
			continue
		}
		if b.err != nil {
			err := b.err
			b.mu.Unlock()
			return 0, err
		}
		break
	}
	b.mu.Unlock()
	return b.rest.Read(p)
}

func (b *replayBody) Close() error {
	b.mu.Lock()
	b.handed = true
	if b.pumping && b.interrupt != nil {
		b.interrupt()
		for b.pumping { // without an interrupt a stuck read cannot be waited for
			b.cond.Wait()
		}
	}
	b.closed = true
	b.sp.discard()
	b.mu.Unlock()
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

func extractMultipart(r *http.Request, boundary string, lim Limits) (*Extracted, error) {
	sp := &spill{memMax: lim.MemBytes, max: lim.MaxMultipartPrefix, dir: lim.TempDir}
	tee := io.TeeReader(r.Body, sp)
	mr := multipart.NewReader(tee, boundary)
	fail := func(err error) (*Extracted, error) {
		sp.discard()
		switch {
		case sp.tooBig:
			return nil, tooLarge("multipart data before the \"model\" field", lim.MaxMultipartPrefix)
		case sp.failure != nil:
			return nil, &Error{Status: http.StatusInternalServerError, Message: "buffering upload: " + sp.failure.Error()}
		}
		return nil, err
	}
	for {
		part, err := mr.NextPart()
		if err != nil { // io.EOF: no model field in the whole body
			if errors.Is(err, io.EOF) && !sp.tooBig && sp.failure == nil {
				sp.discard()
				return nil, missingModel()
			}
			if sp.tooBig || sp.failure != nil {
				return fail(err)
			}
			sp.discard()
			return nil, &Error{Status: http.StatusBadRequest, Message: "malformed multipart body: " + err.Error()}
		}
		if part.FormName() == "model" && part.FileName() == "" {
			val, err := io.ReadAll(io.LimitReader(part, 1025))
			if err != nil {
				return fail(&Error{Status: http.StatusBadRequest, Message: "reading model field: " + err.Error()})
			}
			name := strings.TrimSpace(string(val))
			if name == "" || len(val) > 1024 {
				sp.discard()
				return nil, missingModel()
			}
			rb := sp.replay(r.Body, r.Body)
			return &Extracted{Model: name, Body: rb, rb: rb}, nil
		}
		// Skip the part; the tee keeps its bytes for the replay.
		if _, err := io.Copy(io.Discard, part); err != nil {
			if sp.tooBig || sp.failure != nil {
				return fail(err)
			}
			sp.discard()
			return nil, &Error{Status: http.StatusBadRequest, Message: "malformed multipart body: " + err.Error()}
		}
	}
}
