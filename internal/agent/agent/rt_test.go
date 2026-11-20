package agent

import (
	"log/slog"
	"os"
	"runtime"
)

const runtimeGOOS, runtimeGOARCH = runtime.GOOS, runtime.GOARCH

func testLogger() *slog.Logger {
	if os.Getenv("AGENT_TEST_LOG") == "" {
		return nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
