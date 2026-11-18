package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Launcher starts child processes for the scheduler core. The core depends on
// this interface and on Prober so it can be tested with fakes; ProcessLauncher
// is the real implementation.
//
// Lifecycle of a Child, as the core uses it:
//
//  1. Start returns once the process is running (not yet healthy).
//  2. WaitHealthy blocks until the health endpoint answers 200, the context
//     ends, or the child exits (ErrChildExited). The core runs it in a
//     goroutine bounded by the load timeout and turns the result into an event.
//  3. Exited delivers exactly one ExitStatus, whatever the cause (crash,
//     Stop, kill), then closes. The core turns it into a "child exited" event
//     and only then releases the child's nodes.
//  4. Stop asks the child to end; it never blocks, and completion is observed
//     through Exited. It is idempotent and may be called at any time, also
//     concurrently with the child exiting.
type Launcher interface {
	// Start allocates a port, asks spec.Build for the argv, and starts the
	// process in its own process group with every LLAMA_ARG_* variable
	// removed from its environment. The port is released when the child has
	// been reaped, or at once if Start fails.
	Start(spec StartSpec) (Child, error)
}

// StartSpec says what to start.
type StartSpec struct {
	// Build receives the allocated port and returns the full argv
	// (argv[0] is the program), typically via BuildArgs.
	Build func(port int) ([]string, error)
	// HealthPath is the path polled for readiness: "/health" for llama (the
	// default when empty), the profile's check_endpoint for kind: command.
	HealthPath string
	// Env are KEY=VALUE entries set in the child's environment after the
	// LLAMA_ARG_* scrub, replacing any inherited variable of the same name
	// (names compared case-insensitively). An entry "KEY=" sets it empty.
	Env []string
	// Profile and InstanceID name the child's log file when the launcher has
	// a LogDir; they are not used otherwise.
	Profile    string
	InstanceID uint64
}

// StopMode selects how Stop ends a child.
type StopMode int

const (
	// StopGraceful sends SIGTERM to the child's process group and SIGKILL
	// after the launcher's grace period (10 s by default). On Windows it
	// kills the process tree at once.
	StopGraceful StopMode = iota
	// StopKill kills the process group at once (node loss, forced unload).
	// After a StopGraceful it cancels the remaining grace.
	StopKill
)

// Child is one running process.
type Child interface {
	Port() int
	PID() int
	// Argv is the command line the child was started with.
	Argv() []string
	// WaitHealthy polls the health endpoint until it answers 200. It returns
	// ctx.Err() if the context ends first and an error wrapping ErrChildExited
	// if the child exits first.
	WaitHealthy(ctx context.Context) error
	// Exited yields one ExitStatus and then closes. There must be one reader.
	Exited() <-chan ExitStatus
	// Stop requests termination; see StopMode. It does not wait.
	Stop(mode StopMode)
	// Tail returns at most n of the newest output lines (stdout and stderr).
	Tail(n int) []string
	// Load returns the buffer sizes read from the child's load log so far.
	Load() LoadStats
}

// ErrChildExited is wrapped by WaitHealthy's error when the child exits first.
var ErrChildExited = errors.New("instance: child exited")

// ExitStatus describes how a child ended.
type ExitStatus struct {
	Code      int      // exit code; -1 when killed by a signal
	Signal    string   // signal name such as "killed" on Unix; "" otherwise
	Requested bool     // true if Stop had been called before the child ended
	Tail      []string // last output lines
	Err       error    // unusual wait error (for example output pipes held open), else nil
}

func (s ExitStatus) String() string {
	var b strings.Builder
	switch {
	case s.Signal != "":
		fmt.Fprintf(&b, "killed by signal (%s)", s.Signal)
	default:
		fmt.Fprintf(&b, "exit code %d", s.Code)
	}
	if s.Requested {
		b.WriteString(", stop requested")
	}
	return b.String()
}
