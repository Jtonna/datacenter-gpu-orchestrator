// Package harness holds helpers shared by tests that run real subprocesses.
package harness

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	buildDir  string
	buildPath string
	buildErr  error
)

// BuildFakeLlama builds internal/testutil/fakellama once per test process and
// returns the binary's path. Call Cleanup from TestMain to delete it.
func BuildFakeLlama(tb testing.TB) string {
	tb.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "fakellama-build-")
		if buildErr != nil {
			return
		}
		name := "fakellama"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		buildPath = filepath.Join(buildDir, name)
		cmd := exec.Command("go", "build", "-o", buildPath, "./internal/testutil/fakellama")
		cmd.Dir = moduleRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildError{err: err, output: string(out)}
		}
	})
	if buildErr != nil {
		tb.Fatalf("building fakellama: %v", buildErr)
	}
	return buildPath
}

// Cleanup removes the directory BuildFakeLlama created, if any.
func Cleanup() {
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
}

type buildError struct {
	err    error
	output string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.output }

// moduleRoot is two levels above this package's directory's parent:
// <root>/internal/testutil/harness.
func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// FreePort returns a TCP port that was free a moment ago. There is a small
// window in which another process can take it.
func FreePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("FreePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// WaitForStatus polls url until it answers with the wanted status code, or
// fails the test after timeout.
func WaitForStatus(tb testing.TB, url string, want int, timeout time.Duration) {
	tb.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	last := "no response"
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			tb.Fatalf("GET %s: wanted status %d within %v, last: %s", url, want, timeout, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// WaitForOK polls url until it answers 200, or fails the test after timeout.
func WaitForOK(tb testing.TB, url string, timeout time.Duration) {
	tb.Helper()
	WaitForStatus(tb, url, http.StatusOK, timeout)
}
