// Package runner starts and watches the processes the agent runs: launches
// and exporters. Two implementations sit behind one interface (contract 6.1):
//
//   - Systemd: each process is a systemd transient unit (bare-metal install).
//     Units survive an agent restart and can be adopted again.
//   - Child: each process is a child of the agent (Docker install). Children
//     die with the container; nothing can be adopted.
package runner

import (
	"context"
	"errors"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

// Spec describes one process to start.
type Spec struct {
	// Unit is the unit name without ".service" (for example
	// "potluck-launch-ln-1"), also used as the child's name.
	Unit string
	Path string
	Args []string
	Env  []string // extra "K=V" entries
	// LogPath receives stdout and stderr, appended. Empty discards output.
	LogPath string
	// ExitPath, when set, receives "<code> <status>" after the process
	// exits (systemd: written by ExecStopPost so it survives an agent
	// restart; child: written by the runner).
	ExitPath string
	// OOMScoreAdjust, when not 0, is the process's OOM score adjustment
	// (systemd: OOMScoreAdjust=; child: /proc/<pid>/oom_score_adj).
	OOMScoreAdjust int
}

// Exit is how a process ended.
type Exit struct {
	Known  bool   // false when the status could not be learned
	Code   int    // exit code; 128+n when killed by signal n
	Signal string // signal name without "SIG" when killed by a signal, else ""
}

// Proc is one running (or finished) process.
type Proc interface {
	Unit() string
	// PID is the main process id, 0 when unknown.
	PID() int
	Signal(sig osx.Sig) error
	// Done is closed once the process is gone.
	Done() <-chan struct{}
	// Exit is valid after Done is closed.
	Exit() Exit
}

// Runner starts and finds processes.
type Runner interface {
	Start(ctx context.Context, s Spec) (Proc, error)
	// Adopt returns a handle to an existing unit, or nil when there is none
	// running. exitPath is the unit's exit file (for a unit that already
	// ended). The child runner never adopts.
	Adopt(ctx context.Context, unit, exitPath string) (Proc, error)
	// List returns the live units whose name starts with prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Stop stops a unit by name, whether or not this process started it
	// (used for exporter units left by a previous agent).
	Stop(ctx context.Context, unit string) error
	// Kind is "systemd" or "child".
	Kind() string
	// Survives reports whether processes outlive the agent (systemd).
	Survives() bool
}

// ErrExists is returned by Start when a unit of that name is still running.
var ErrExists = errors.New("unit already running")

// ParseExitFile parses the "<code> <status>" text written by ExecStopPost
// ($EXIT_CODE is exited, killed or dumped; $EXIT_STATUS is the exit code or
// the signal name or number).
func ParseExitFile(s string) Exit {
	var kind, status string
	n := 0
	for _, f := range splitFields(s) {
		if n == 0 {
			kind = f
		} else if n == 1 {
			status = f
		}
		n++
	}
	switch kind {
	case "exited":
		c, ok := atoi(status)
		if !ok {
			return Exit{}
		}
		return Exit{Known: true, Code: c}
	case "killed", "dumped":
		num, name := signalFromStatus(status)
		if num == 0 {
			return Exit{}
		}
		return Exit{Known: true, Code: 128 + num, Signal: name}
	}
	return Exit{}
}

var signalNumbers = map[string]int{"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "BUS": 7,
	"FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15}

func signalFromStatus(s string) (int, string) {
	if n, ok := atoi(s); ok {
		return n, osx.SignalName(n)
	}
	name := s
	if len(name) > 3 && name[:3] == "SIG" {
		name = name[3:]
	}
	if n, ok := signalNumbers[name]; ok {
		return n, name
	}
	return 0, ""
}

func splitFields(s string) []string {
	var out []string
	cur := []byte{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			continue
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
