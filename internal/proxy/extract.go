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
}

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
	return &Extracted{Model: name, Body: io.NopCloser(bytes.NewReader(data))}, nil
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

func (s *spill) discard() {
	if s.file != nil {
		name := s.file.Name()
		s.file.Close()
		os.Remove(name)
		s.file = nil
	}
}

// replay returns the buffered prefix followed by rest.
func (s *spill) replay(rest io.Reader, restCloser io.Closer) io.ReadCloser {
	var prefix io.Reader = bytes.NewReader(s.mem.Bytes())
	rc := &replayBody{closer: restCloser}
	if s.file != nil {
		if _, err := s.file.Seek(0, io.SeekStart); err == nil {
			prefix = io.MultiReader(prefix, io.LimitReader(s.file, s.n-int64(s.mem.Len())))
		}
		rc.file = s.file
	}
	rc.r = io.MultiReader(prefix, rest)
	return rc
}

type replayBody struct {
	r      io.Reader
	file   *os.File
	closer io.Closer
}

func (b *replayBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *replayBody) Close() error {
	if b.file != nil {
		name := b.file.Name()
		b.file.Close()
		os.Remove(name)
		b.file = nil
	}
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
			return &Extracted{Model: name, Body: sp.replay(r.Body, r.Body)}, nil
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
