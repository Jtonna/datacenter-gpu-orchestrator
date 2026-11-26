package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// schedSpec is one scheduler under test: a binary on this machine, or a
// container image ("image:NAME") run with docker on the host network.
type schedSpec struct {
	Bin   string
	Image string
}

func parseSpec(s string) (schedSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return schedSpec{}, errors.New("no scheduler given")
	}
	if img, ok := strings.CutPrefix(s, "image:"); ok {
		if img == "" {
			return schedSpec{}, errors.New("image: needs an image name")
		}
		return schedSpec{Image: img}, nil
	}
	return schedSpec{Bin: s}, nil
}

func (s schedSpec) String() string {
	if s.Image != "" {
		return "image:" + s.Image
	}
	return s.Bin
}

// check reports a binary that does not exist. An image is not checked here:
// docker run says so when it is missing.
func (s schedSpec) check() error {
	if s.Bin == "" {
		return nil
	}
	if fi, err := os.Stat(s.Bin); err != nil || fi.IsDir() {
		return fmt.Errorf("scheduler binary %s: not a file", s.Bin)
	}
	return nil
}

// runOpts is how one scheduler run is started.
type runOpts struct {
	Listen   string // 127.0.0.1:PORT; the scheduler serves http://<Listen>
	Data     string // the data folder (always a copy)
	Models   string
	Programs string   // optional --programs-dir
	Args     []string // extra scheduler flags
	Env      []string // extra environment, K=V
	Log      io.Writer
}

var containerSeq atomic.Int64

// argv is the scheduler command line (without the program) and, for an image,
// the docker run command line that wraps it.
func (s schedSpec) argv(o runOpts) (prog string, args []string, container string) {
	data, models, programs := o.Data, o.Models, o.Programs
	if s.Image != "" {
		data, models = "/gate/data", "/gate/models"
		if programs != "" {
			programs = "/gate/programs"
		}
	}
	sa := []string{"--listen", o.Listen, "--data-dir", data, "--models-dir", models}
	if programs != "" {
		sa = append(sa, "--programs-dir", programs)
	}
	sa = append(sa, o.Args...)
	if s.Image == "" {
		return s.Bin, sa, ""
	}
	container = fmt.Sprintf("potluck-gate-%d-%d", os.Getpid(), containerSeq.Add(1))
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	da := []string{"run", "--rm", "--name", container, "--network", "host",
		"-v", abs(o.Data) + ":/gate/data", "-v", abs(o.Models) + ":/gate/models:ro"}
	if o.Programs != "" {
		da = append(da, "-v", abs(o.Programs)+":/gate/programs:ro")
	}
	for _, kv := range o.Env {
		da = append(da, "-e", kv)
	}
	da = append(da, s.Image)
	return "docker", append(da, sa...), container
}

// running is a started scheduler.
type running struct {
	spec      schedSpec
	base      string
	cmd       *exec.Cmd
	container string
	done      chan struct{}
	exitErr   error
}

func startScheduler(s schedSpec, o runOpts) (*running, error) {
	prog, args, container := s.argv(o)
	cmd := exec.Command(prog, args...)
	cmd.Env = append(os.Environ(), o.Env...)
	if o.Log != nil {
		cmd.Stdout, cmd.Stderr = o.Log, o.Log
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", s, err)
	}
	r := &running{spec: s, base: "http://" + o.Listen, cmd: cmd, container: container, done: make(chan struct{})}
	go func() { r.exitErr = cmd.Wait(); close(r.done) }()
	return r, nil
}

func (r *running) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// waitReady waits until GET /health answers 200.
func (r *running) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: 2 * time.Second}
	for {
		if r.exited() {
			return fmt.Errorf("the scheduler exited before it was ready: %v", r.exitErr)
		}
		resp, err := c.Get(r.base + "/health")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the scheduler at %s was not ready within %s", r.base, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// kill stops the scheduler at once (SIGKILL; TerminateProcess on Windows;
// docker kill --signal KILL for an image) and waits for the exit.
func (r *running) kill() error {
	if r.container != "" {
		exec.Command("docker", "kill", "--signal", "KILL", r.container).Run()
	} else if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	return r.wait(60 * time.Second)
}

// stop asks for a clean shutdown (SIGINT, or docker stop) and kills after
// grace. On Windows a child cannot be interrupted: it is killed.
func (r *running) stop(grace time.Duration) error {
	if r.exited() {
		return nil
	}
	switch {
	case r.container != "":
		exec.Command("docker", "stop", "-t", strconv.Itoa(int(grace/time.Second)), r.container).Run()
	case runtime.GOOS == "windows":
		return r.kill()
	default:
		r.cmd.Process.Signal(os.Interrupt)
	}
	if r.wait(grace+5*time.Second) == nil {
		return nil
	}
	return r.kill()
}

func (r *running) wait(d time.Duration) error {
	select {
	case <-r.done:
		return nil
	case <-time.After(d):
		return fmt.Errorf("the scheduler did not exit within %s", d)
	}
}

// logBuf is the scheduler's output, kept whole for the log scan and copied
// to a file when one is given.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
	f  io.Writer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Write(p)
	if l.f != nil {
		l.f.Write(p)
	}
	return len(p), nil
}

func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

var (
	panicLine = regexp.MustCompile(`^(panic: |fatal error: )`)
	errorLine = regexp.MustCompile(`\blevel=ERROR\b|"level":"ERROR"`)
)

// logScan counts the panic and ERROR lines of a scheduler log. A line that
// matches one of allow is not counted (an ERROR the caller expects, such as
// a copy without a program release); samples holds the first few counted.
type logScan struct {
	Panics  int      `json:"panics"`
	Errors  int      `json:"errors"`
	Allowed int      `json:"allowed"`
	Samples []string `json:"samples,omitempty"`
}

func scanLog(text string, allow []*regexp.Regexp) logScan {
	var s logScan
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		isPanic, isErr := panicLine.MatchString(line), errorLine.MatchString(line)
		if !isPanic && !isErr {
			continue
		}
		if matchesAny(line, allow) {
			s.Allowed++
			continue
		}
		if isPanic {
			s.Panics++
		} else {
			s.Errors++
		}
		if len(s.Samples) < 10 {
			s.Samples = append(s.Samples, truncate(line, 400))
		}
	}
	return s
}

func matchesAny(s string, rs []*regexp.Regexp) bool {
	for _, r := range rs {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// copyDir copies the folder src into dst, which must not exist or be empty.
// The gate programs work on this copy only; the source is never opened for
// writing. Take the source from a stopped scheduler (or a snapshot of one):
// a database being written while it is copied may be copied half-written.
func copyDir(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("data folder: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("data folder %s: not a folder", src)
	}
	if ents, err := os.ReadDir(dst); err == nil && len(ents) > 0 {
		return fmt.Errorf("the copy's folder %s is not empty", dst)
	}
	as, _ := filepath.Abs(src)
	ad, _ := filepath.Abs(dst)
	if rel, err := filepath.Rel(as, ad); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("the copy's folder %s is inside the data folder %s", dst, src)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		info, _ := d.Info()
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, in); err != nil {
			w.Close()
			return err
		}
		return w.Close()
	})
}

// freeAddr is a free TCP address on host (port 0 bound and released).
func freeAddr(host string) (string, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// client is the gate's HTTP client: every write names the gate as its actor.
type client struct {
	base string
	hc   *http.Client
}

func newClient(base string) *client {
	return &client{base: base, hc: &http.Client{Timeout: 60 * time.Second}}
}

// call sends one request and decodes a JSON answer into out (when not nil).
// raw is the answer's body. A transport error is err; an answer that does not
// parse is err too, with the status.
func (c *client) call(ctx context.Context, method, path string, body any, out any) (status int, raw []byte, err error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-Actor", "potluck-gate")
		req.Header.Set("X-Comment", "potluck-gate: a test on a copy")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, raw, err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("%s %s: HTTP %d, the answer does not parse: %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode, raw, nil
}

// multiFlag is a flag given several times.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func compileAll(pats []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range pats {
		r, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
