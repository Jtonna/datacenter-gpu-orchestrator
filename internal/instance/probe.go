package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultProbeTimeout is the pre-flight timeout from design 7.5.
const DefaultProbeTimeout = 10 * time.Second

// ProbeResult is what a successful pre-flight measured for one worker.
type ProbeResult struct {
	TotalMiB int
	FreeMiB  int
}

// Prober runs the pre-flight against one worker (design 7.5). The scheduler
// core depends on this interface so it can be tested with a fake.
type Prober interface {
	// Probe asks the rpc-server at addr ("ip:port") for its device. Failures
	// are *ProbeError values (possibly wrapped).
	Probe(ctx context.Context, addr string) (ProbeResult, error)
}

// ProbeFailure says why a probe failed. The zero value is not a failure.
type ProbeFailure int

const (
	ProbeStart            ProbeFailure = iota + 1 // the binary could not be started
	ProbeTimeout                                  // the probe timeout passed (busy or silent worker)
	ProbeCanceled                                 // the caller's context ended first; not a verdict on the node
	ProbeExit                                     // non-zero exit or death by signal (closed port aborts)
	ProbeNoRPCLine                                // exit 0 but no usable RPC device line
	ProbeMultipleRPCLines                         // more than one RPC device line: owner must pin -d on the worker
)

func (f ProbeFailure) String() string {
	switch f {
	case ProbeStart:
		return "start"
	case ProbeTimeout:
		return "timeout"
	case ProbeCanceled:
		return "canceled"
	case ProbeExit:
		return "exit"
	case ProbeNoRPCLine:
		return "no-rpc-line"
	case ProbeMultipleRPCLines:
		return "multiple-rpc-lines"
	}
	return "unknown"
}

// ProbeError is the error type of every failed probe.
type ProbeError struct {
	Kind     ProbeFailure
	Addr     string
	ExitCode int    // for ProbeExit; -1 when killed by a signal
	Detail   string // human-readable specifics
	Stderr   string // tail of the process's stderr, if any
	Err      error  // underlying error, if any
}

func (e *ProbeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pre-flight of %s failed (%s)", e.Addr, e.Kind)
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		b.WriteString("; stderr: " + s)
	}
	return b.String()
}

func (e *ProbeError) Unwrap() error { return e.Err }

var (
	// rpcLineRE matches a stdout line whose device name starts with RPC;
	// the description may contain anything, including parentheses and colons,
	// so the trailing "(<total> MiB, <free> MiB free)" is anchored at the end.
	rpcLineRE   = regexp.MustCompile(`^\s*RPC\d*:\s`)
	rpcDeviceRE = regexp.MustCompile(`^\s*RPC\d*:\s+.*\((\d+) MiB, (\d+) MiB free\)\s*$`)
)

// ParseListDevices extracts the single RPC device from `--list-devices`
// stdout (design 3.1, 7.5). Lines for other devices are ignored.
func ParseListDevices(stdout string) (ProbeResult, error) {
	var rpcLines []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if rpcLineRE.MatchString(line) {
			rpcLines = append(rpcLines, line)
		}
	}
	switch len(rpcLines) {
	case 0:
		return ProbeResult{}, &ProbeError{Kind: ProbeNoRPCLine, Detail: "no RPC device line in output"}
	case 1:
		m := rpcDeviceRE.FindStringSubmatch(rpcLines[0])
		if m == nil {
			return ProbeResult{}, &ProbeError{Kind: ProbeNoRPCLine, Detail: fmt.Sprintf("malformed RPC device line %q", rpcLines[0])}
		}
		total, err1 := strconv.Atoi(m[1])
		free, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			return ProbeResult{}, &ProbeError{Kind: ProbeNoRPCLine, Detail: fmt.Sprintf("unparsable sizes in %q", rpcLines[0])}
		}
		return ProbeResult{TotalMiB: total, FreeMiB: free}, nil
	default:
		return ProbeResult{}, &ProbeError{Kind: ProbeMultipleRPCLines,
			Detail: fmt.Sprintf("%d RPC device lines; pin one device with -d on the worker", len(rpcLines))}
	}
}

// ExecProber is the real Prober: it runs
//
//	CUDA_VISIBLE_DEVICES="" <ServerBin> --rpc <addr> --list-devices
//
// with every LLAMA_ARG_* variable removed, under a timeout.
type ExecProber struct {
	ServerBin string        // defaults.server_bin
	Timeout   time.Duration // 0 means DefaultProbeTimeout
	Env       []string      // base environment; nil means os.Environ()
}

// Probe implements Prober.
func (p *ExecProber) Probe(ctx context.Context, addr string) (ProbeResult, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := p.Env
	if base == nil {
		base = os.Environ()
	}
	cmd := exec.CommandContext(tctx, p.ServerBin, "--rpc", addr, "--list-devices")
	cmd.Env = probeEnv(base)
	cmd.WaitDelay = 2 * time.Second // do not hang on pipes held by stray children
	stdout := &capBuffer{max: 1 << 20}
	stderr := &capBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	fail := func(kind ProbeFailure, detail string, wrapped error) error {
		return &ProbeError{Kind: kind, Addr: addr, Detail: detail, Stderr: stderr.String(), Err: wrapped, ExitCode: exitCode(err)}
	}
	switch {
	case ctx.Err() != nil:
		return ProbeResult{}, fail(ProbeCanceled, "canceled by caller", ctx.Err())
	case tctx.Err() != nil:
		return ProbeResult{}, fail(ProbeTimeout, fmt.Sprintf("no answer within %v", timeout), context.DeadlineExceeded)
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ProbeResult{}, fail(ProbeExit, ee.ProcessState.String(), err)
		}
		return ProbeResult{}, fail(ProbeStart, err.Error(), err)
	}
	res, perr := ParseListDevices(stdout.String())
	if perr != nil {
		var pe *ProbeError
		if errors.As(perr, &pe) {
			pe.Addr, pe.Stderr = addr, stderr.String()
		}
		return ProbeResult{}, perr
	}
	return res, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

// probeEnv returns base without any LLAMA_ARG_* variable and with
// CUDA_VISIBLE_DEVICES set to the empty string (design 7.5, H3). Names are
// compared case-insensitively because Windows environments are.
func probeEnv(base []string) []string {
	out := make([]string, 0, len(base)+1)
	out = append(out, scrubEnv(base)...)
	kept := out[:0]
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "CUDA_VISIBLE_DEVICES") {
			continue
		}
		kept = append(kept, kv)
	}
	return append(kept, "CUDA_VISIBLE_DEVICES=")
}

// scrubEnv removes every LLAMA_ARG_* variable (design 6: children are started
// without them, because they would silently override the command line).
func scrubEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if len(k) >= len("LLAMA_ARG_") && strings.EqualFold(k[:len("LLAMA_ARG_")], "LLAMA_ARG_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// capBuffer keeps at most max bytes (the first ones) and discards the rest.
type capBuffer struct {
	b   bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

func (c *capBuffer) String() string { return c.b.String() }
