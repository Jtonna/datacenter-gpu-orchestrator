package proxy

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExtractJSONModelAndReplay(t *testing.T) {
	body := `{"model":"gemma-4-12b:64k","messages":[{"role":"user","content":"hi"}],"stream":true}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ex, err := Extract(r, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Body.Close()
	if ex.Model != "gemma-4-12b:64k" {
		t.Fatalf("model = %q", ex.Model)
	}
	if got := string(readAll(t, ex.Body)); got != body {
		t.Fatalf("replayed body differs: %q", got)
	}
}

func TestExtractJSONWithoutContentTypeIsStillJSON(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"model":"a"}`))
	r.Header.Del("Content-Type")
	ex, err := Extract(r, Limits{})
	if err != nil || ex.Model != "a" {
		t.Fatalf("got %+v, %v", ex, err)
	}
}

func TestExtractJSONOverCapIs413(t *testing.T) {
	big := `{"model":"a","pad":"` + strings.Repeat("x", 2000) + `"}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
	r.Header.Set("Content-Type", "application/json")
	_, err := Extract(r, Limits{MaxJSON: 1000})
	var he *Error
	if !errors.As(err, &he) || he.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want 413", err)
	}
}

func TestExtractJSONOverCapByContentLengthIs413WithoutReading(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", io.LimitReader(zeroReader{}, 5000))
	r.ContentLength = 5000
	r.Header.Set("Content-Type", "application/json")
	_, err := Extract(r, Limits{MaxJSON: 1000})
	var he *Error
	if !errors.As(err, &he) || he.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want 413", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func TestExtractMissingModelIs400(t *testing.T) {
	for _, body := range []string{`{}`, `{"model":""}`, `{"model":3}`, `not json`, ``} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		_, err := Extract(r, Limits{})
		var he *Error
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
			t.Errorf("body %q: err = %v, want 400", body, err)
		}
	}
}

func multipartBody(t *testing.T, fields func(w *multipart.Writer)) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields(w)
	w.Close()
	return w.FormDataContentType(), buf.Bytes()
}

func TestExtractMultipartModelFirstStreamsTheRest(t *testing.T) {
	file := bytes.Repeat([]byte("A"), 3<<20)
	ct, body := multipartBody(t, func(w *multipart.Writer) {
		w.WriteField("model", "large-v3-turbo")
		fw, _ := w.CreateFormFile("file", "a.wav")
		fw.Write(file)
	})
	pr, pw := io.Pipe()
	go func() {
		// Send only the first 4 KB, then hold: Extract must return without
		// waiting for the rest of the upload.
		pw.Write(body[:4096])
	}()
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", pr)
	r.Header.Set("Content-Type", ct)
	done := make(chan *Extracted, 1)
	go func() {
		ex, err := Extract(r, Limits{})
		if err != nil {
			t.Error(err)
		}
		done <- ex
	}()
	ex := <-done
	if ex == nil || ex.Model != "large-v3-turbo" {
		t.Fatalf("got %+v", ex)
	}
	go func() { pw.Write(body[4096:]); pw.Close() }()
	if got := readAll(t, ex.Body); !bytes.Equal(got, body) {
		t.Fatalf("replayed body differs (%d vs %d bytes)", len(got), len(body))
	}
	ex.Body.Close()
}

func TestExtractMultipartModelAfterFileSpillsToDiskAndReplays(t *testing.T) {
	file := bytes.Repeat([]byte("B"), 3<<20)
	ct, body := multipartBody(t, func(w *multipart.Writer) {
		fw, _ := w.CreateFormFile("file", "a.wav")
		fw.Write(file)
		w.WriteField("model", "large-v3-turbo")
		w.WriteField("language", "en")
	})
	tmp := t.TempDir()
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	ex, err := Extract(r, Limits{TempDir: tmp, MemBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Model != "large-v3-turbo" {
		t.Fatalf("model = %q", ex.Model)
	}
	if ents, _ := os.ReadDir(tmp); len(ents) == 0 {
		t.Error("expected a spill file in the temp dir for a 3 MiB prefix with a 64 KiB memory budget")
	}
	if got := readAll(t, ex.Body); !bytes.Equal(got, body) {
		t.Fatalf("replayed body differs (%d vs %d bytes)", len(got), len(body))
	}
	ex.Body.Close()
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("spill file left behind after Close: %v", ents)
	}
}

func TestExtractMultipartPrefixOverCapIs413(t *testing.T) {
	ct, body := multipartBody(t, func(w *multipart.Writer) {
		fw, _ := w.CreateFormFile("file", "a.wav")
		fw.Write(bytes.Repeat([]byte("C"), 1<<20))
		w.WriteField("model", "m")
	})
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	_, err := Extract(r, Limits{TempDir: t.TempDir(), MaxMultipartPrefix: 100 << 10})
	var he *Error
	if !errors.As(err, &he) || he.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want 413", err)
	}
}

func TestExtractMultipartWithoutModelIs400(t *testing.T) {
	ct, body := multipartBody(t, func(w *multipart.Writer) { w.WriteField("language", "en") })
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	_, err := Extract(r, Limits{})
	var he *Error
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400", err)
	}
}

func prefetchRequest(t *testing.T, fileBytes int) (*http.Request, *io.PipeWriter, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "m")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write(bytes.Repeat([]byte("abcdefghij"), fileBytes/10))
	mw.Close()
	pr, pw := io.Pipe()
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", pr)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r, pw, buf.Bytes()
}

func TestPrefetchReplaysSpilledThenLiveBytesAndRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	r, pw, full := prefetchRequest(t, 300<<10)
	go pw.Write(full[:1000]) // enough for the model field
	ex, err := Extract(r, Limits{MemBytes: 10 << 10, TempDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	ex.Prefetch(func(err error) { errs <- err }, func() { pw.CloseWithError(errors.New("interrupted")) })
	go func() {
		pw.Write(full[1000:200000])
		pw.Write(full[200000:])
		pw.Close()
	}()
	if got := readAll(t, ex.Body); !bytes.Equal(got, full[:len(got)]) || len(got) != len(full) {
		t.Fatalf("replayed %d bytes, want %d identical", len(got), len(full))
	}
	ex.Body.Close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
	select {
	case err := <-errs:
		t.Fatalf("unexpected onErr: %v", err)
	default:
	}
}

func TestPrefetchReportsReadFailureBeforeEOFAndCloseCleansUp(t *testing.T) {
	dir := t.TempDir()
	r, pw, full := prefetchRequest(t, 300<<10)
	go pw.Write(full[:1000])
	ex, err := Extract(r, Limits{MemBytes: 10 << 10, TempDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	ex.Prefetch(func(err error) { errs <- err }, nil)
	go func() { pw.Write(full[1000:100000]); pw.CloseWithError(io.ErrUnexpectedEOF) }()
	if err := <-errs; !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("onErr = %v", err)
	}
	ex.Body.Close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
}

func TestPrefetchCloseInterruptsABlockedRead(t *testing.T) {
	r, pw, full := prefetchRequest(t, 300<<10)
	go pw.Write(full[:1000])
	ex, err := Extract(r, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	ex.Prefetch(func(error) { t.Error("onErr after Close") }, func() { pw.CloseWithError(errors.New("deadline")) })
	done := make(chan struct{})
	go func() { ex.Body.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
}
