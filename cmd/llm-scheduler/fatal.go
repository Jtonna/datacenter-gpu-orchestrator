package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/instance"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// The fatal path (background-task isolation): a background task that must
// not continue after a failure (the scheduler's event loop, the pool's
// lost-node check, applying a configuration change), or any task that failed
// a third time within a minute, ends the process here. It does not reuse the
// graceful shutdown, which waits on the event loop that may be the one that
// died: it logs, records, stops accepting, kills the model children and
// exits with code 3, all within fatalWatchdog. The container then starts
// Potluck again, which rebuilds everything from the database.

const (
	fatalWatchdog = 5 * time.Second
	// fatalAnswerGrace is how long the fatal path lets requests in flight
	// finish their answer (streams are cut after it).
	fatalAnswerGrace = time.Second
)

// fatalExit ends the process when the fatal path overruns its watchdog
// (tests replace it).
var fatalExit = os.Exit

type fatalPath struct {
	ch    chan string // the reason, once
	once  sync.Once
	fired atomic.Bool
	mu    sync.Mutex
	timer *time.Timer
}

func newFatalPath() *fatalPath { return &fatalPath{ch: make(chan string, 1)} }

// trigger starts the fatal path (supervise's Fatal). Only the first call
// counts; it never blocks.
func (f *fatalPath) trigger(reason string) {
	f.once.Do(func() {
		f.fired.Store(true)
		f.mu.Lock()
		f.timer = time.AfterFunc(fatalWatchdog, func() {
			fmt.Fprintln(os.Stderr, "llm-scheduler: the fatal path overran its", fatalWatchdog, "bound; exiting now:", reason)
			fatalExit(supervise.ExitCode)
		})
		f.mu.Unlock()
		f.ch <- reason
	})
}

// disarm stops the watchdog once serveUntil has returned (main exits next).
func (f *fatalPath) disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timer != nil {
		f.timer.Stop()
	}
}

// fatalStop is the fatal path proper, run by serveUntil.
func fatalStop(reason string, log *slog.Logger, hist *history.Store, srv *http.Server, launcher *instance.ProcessLauncher, stopBg func()) int {
	log.Error("a background task failed and must not continue: Potluck stops now so that it is started again (exit code 3)", "reason", reason)
	hist.Record(stoppingEvent("internal failure in "+reason, time.Now()))
	// Stop accepting, and give the requests in flight a moment to answer
	// (a configuration change that failed to apply answers 500 first).
	sctx, cancel := context.WithTimeout(context.Background(), fatalAnswerGrace)
	if srv.Shutdown(sctx) != nil {
		srv.Close()
	}
	cancel()
	if n := launcher.KillAll(); n > 0 {
		log.Error("the model children were killed", "children", n)
	}
	stopBg()
	supervise.WaitRecords(time.Second) // the internalFailure event that led here
	hist.Close()
	return supervise.ExitCode
}

// failureEvent is the internalFailure history record of a recovered panic.
func failureEvent(f supervise.Failure) history.Event {
	var what string
	switch {
	case f.Escalating:
		what = "failed; Potluck stops so that it is started again"
	case f.Kind == "restart":
		what = "failed and was restarted"
	case f.Kind == "once":
		what = "failed; its result became an error"
	case f.Kind == "guard":
		what = "failed; its caller continued"
	default:
		what = "failed and was recovered"
	}
	summary := fmt.Sprintf("Background task %s %s", f.Task, what)
	if f.FailuresLastMin > 1 {
		summary += fmt.Sprintf(" (%s time in the last minute)", ordinal(f.FailuresLastMin))
	}
	lines := strings.Split(f.Stack, "\n")
	if len(lines) > 20 {
		lines = lines[:20]
	}
	return history.Event{Ts: f.At.UnixMilli(), Type: history.TInternalFailure, Severity: history.Error, Summary: summary + ".",
		Data: map[string]any{"task": f.Task, "kind": f.Kind, "panic": f.Panic, "stack": strings.Join(lines, "\n"),
			"failures_last_min": f.FailuresLastMin, "restarts": f.Restarts, "escalating": f.Escalating}}
}

func ordinal(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}
