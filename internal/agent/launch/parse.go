package launch

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// workerStartedRE matches the line rpc-server prints when it starts serving.
// Upstream prints "Starting RPC server v3.0.0" followed by an endpoint line
// (older builds: "Starting RPC server on host:port"); "listening" covers
// forks that say so. Readiness also needs a connect-and-close probe of the
// port to succeed, so a match here alone never makes a worker ready.
var workerStartedRE = regexp.MustCompile(`(?i)(starting rpc server|listening)`)

var (
	fileLoadRE     = regexp.MustCompile(`\[file_load\] progress bytes=(\d+) total=(\d+)`)
	hostProgressRE = regexp.MustCompile(`load_progress: fraction=([0-9]*\.?[0-9]+)`)
	requiredRE     = regexp.MustCompile(`rpc_file_load_required:.*\breason=([A-Za-z_]+)`)
)

// logScan is what the agent learns from a launch log, line by line.
type logScan struct {
	started     bool // worker start line seen
	timeoutTok  bool // rpc_file_load_timeout seen
	requiredTok bool
	requiredWhy string
	bytesLoaded int64
	bytesTotal  int64
	haveBytes   bool
	fraction    float64
	haveFrac    bool
	tail        []string // last tailLines lines
}

const tailLines = 40

func (s *logScan) line(l string) {
	l = strings.TrimRight(l, "\r")
	s.tail = append(s.tail, l)
	if len(s.tail) > tailLines {
		s.tail = s.tail[len(s.tail)-tailLines:]
	}
	if !s.started && workerStartedRE.MatchString(l) {
		s.started = true
	}
	if m := fileLoadRE.FindStringSubmatch(l); m != nil {
		b, err1 := strconv.ParseInt(m[1], 10, 64)
		t, err2 := strconv.ParseInt(m[2], 10, 64)
		if err1 == nil && err2 == nil {
			s.bytesLoaded, s.bytesTotal, s.haveBytes = b, t, true
		}
	}
	if m := hostProgressRE.FindStringSubmatch(l); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			s.fraction, s.haveFrac = f, true
		}
	}
	if strings.Contains(l, agentproto.TokenFileLoadTimeout) {
		s.timeoutTok = true
	}
	if strings.Contains(l, agentproto.TokenFileLoadRequired) {
		s.requiredTok = true
		if m := requiredRE.FindStringSubmatch(l); m != nil {
			s.requiredWhy = m[1]
		}
	}
}

// feed consumes a chunk of log bytes; an incomplete last line is kept in
// partial until the rest arrives.
func (s *logScan) feed(partial *[]byte, b []byte) {
	buf := append(*partial, b...)
	for {
		i := indexByte(buf, '\n')
		if i < 0 {
			break
		}
		s.line(string(buf[:i]))
		buf = buf[i+1:]
	}
	// Keep a bounded partial line.
	if len(buf) > 64<<10 {
		s.line(string(buf))
		buf = nil
	}
	*partial = append([]byte(nil), buf...)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (s *logScan) began() bool {
	return (s.haveBytes && s.bytesLoaded > 0) || (s.haveFrac && s.fraction > 0)
}

// decideExit applies the exit reason order of contract 6.3. oomKilled is
// only asked when the order reaches it.
func decideExit(role, cause, causeMsg string, scan *logScan, everReady, loadBegan bool, signal string, oomKilled func() bool) (string, string) {
	if cause != "" {
		return cause, causeMsg
	}
	if role == agentproto.RoleHost && scan.timeoutTok {
		return agentproto.ExitFileLoadTimeout, ""
	}
	if role == agentproto.RoleHost && scan.requiredTok {
		return agentproto.ExitFileLoadRequired, scan.requiredWhy
	}
	if !everReady && !loadBegan {
		return agentproto.ExitStartFailed, ""
	}
	if signal != "" && oomKilled != nil && oomKilled() {
		return agentproto.ExitOOMKill, ""
	}
	return agentproto.ExitCrashed, ""
}
