// Package agenttest holds helpers for potluck-agent tests: building the fake
// programs under the names the agent runs, and waiting for conditions.
package agenttest

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	once     sync.Once
	buildDir string
	fakeprog string
	buildErr error
)

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// FakeProg builds internal/testutil/fakeprog once per test process and
// returns its path.
func FakeProg(tb testing.TB) string {
	tb.Helper()
	once.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "fakeprog-build-")
		if buildErr != nil {
			return
		}
		fakeprog = filepath.Join(buildDir, "fakeprog"+exeSuffix())
		cmd := exec.Command("go", "build", "-o", fakeprog, "./internal/testutil/fakeprog")
		cmd.Dir = moduleRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildError{err, string(out)}
		}
	})
	if buildErr != nil {
		tb.Fatalf("building fakeprog: %v", buildErr)
	}
	return fakeprog
}

// Cleanup removes the build directory. Call it from TestMain.
func Cleanup() {
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// Install copies the fake program into dir under name (plus ".exe" on
// Windows) and returns the new path.
func Install(tb testing.TB, dir, name string) string {
	tb.Helper()
	src := FakeProg(tb)
	dst := filepath.Join(dir, name+exeSuffix())
	in, err := os.Open(src)
	if err != nil {
		tb.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		tb.Fatal(err)
	}
	out.Close()
	return dst
}

// Eventually polls cond every 10ms until it is true or timeout passes.
func Eventually(tb testing.TB, timeout time.Duration, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("timed out after %v waiting for: %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
