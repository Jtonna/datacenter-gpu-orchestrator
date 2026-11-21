package logship

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// JournalKey is the ring key of the machine's system log.
var JournalKey = Key{Source: agentproto.LogSourceJournal}

// ErrNoJournal: there is no journal reader on this machine.
var ErrNoJournal = errors.New("journalctl not found")

// JournalStart starts following the journal from now on and returns its
// JSON output, one entry per line. dir is a journal folder ("" for the
// machine's own journal). Closing the reader stops the follower.
type JournalStart func(ctx context.Context, dir string) (io.ReadCloser, error)

// ExecJournal follows the journal with a journalctl child process. The agent
// is a static binary without cgo, so it cannot use the journal's library.
// --lines=0 starts at "now": after an agent restart, what was written while
// it was down is not shipped (design section 5). --output-fields needs
// systemd 236 or later; the __ and _BOOT_ID fields are always printed.
func ExecJournal(ctx context.Context, dir string) (io.ReadCloser, error) {
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, ErrNoJournal
	}
	args := []string{"--follow", "--lines=0", "--output=json", "--quiet",
		"--output-fields=MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER"}
	if dir != "" {
		args = append(args, "--directory="+dir)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procReader{ReadCloser: out, cmd: cmd}, nil
}

type procReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (p *procReader) Close() error {
	p.cmd.Process.Kill()
	p.ReadCloser.Close()
	return p.cmd.Wait()
}

// Journal ships the machine's system log while shipping is on: all of it,
// less the units of deny_units and less the agent's own unit (its lines are
// shipped as source agent already).
//
// It never holds up anything: it reads in its own goroutine and keeps to
// max_bytes_per_s by dropping lines (counted), never by buffering them. A
// line longer than MaxRead is dropped unread.
type Journal struct {
	Ship  *Shipper
	Start JournalStart // nil: ExecJournal
	Dir   string
	// Monotonic reads CLOCK_MONOTONIC, the clock of the journal's
	// __MONOTONIC_TIMESTAMP. BootID is this boot's id as _BOOT_ID writes it.
	Monotonic func() (time.Duration, error)
	BootID    string
	// Retry is the pause before the follower is started again after it
	// ended (default 5 s, growing to 60 s).
	Retry time.Duration
	// Check is how often shipping on or off is looked at (default 1 s).
	Check   time.Duration
	MaxRead int // default 64 KiB
	OwnUnit string
}

type journalEntry struct {
	Message   json.RawMessage `json:"MESSAGE"`
	Priority  string          `json:"PRIORITY"`
	Unit      string          `json:"_SYSTEMD_UNIT"`
	Ident     string          `json:"SYSLOG_IDENTIFIER"`
	BootID    string          `json:"_BOOT_ID"`
	Monotonic string          `json:"__MONOTONIC_TIMESTAMP"`
}

var priorities = []string{"emerg", "alert", "crit", "error", "warn", "notice", "info", "debug"}

// Run follows the journal until ctx ends: started when shipping goes on,
// stopped when it goes off.
func (j *Journal) Run(ctx context.Context) {
	start := j.Start
	if start == nil {
		start = ExecJournal
	}
	check := j.Check
	if check <= 0 {
		check = time.Second
	}
	retry := j.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	wait := retry
	for ctx.Err() == nil {
		if !j.Ship.On() {
			if !sleepCtx(ctx, check) {
				return
			}
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		rc, err := start(rctx, j.Dir)
		if errors.Is(err, ErrNoJournal) {
			cancel()
			j.Ship.log.Load().Warn("journalctl not found: the system log is not shipped")
			return
		}
		if err != nil {
			cancel()
			j.Ship.log.Load().Warn("system log reader did not start", "err", err, "retry_in_s", wait.Seconds())
			if !sleepCtx(ctx, wait) {
				return
			}
			wait = min(wait*2, 60*time.Second)
			continue
		}
		// Stop the follower when shipping goes off.
		go func() {
			t := time.NewTicker(check)
			defer t.Stop()
			for {
				select {
				case <-rctx.Done():
					return
				case <-t.C:
					if !j.Ship.On() {
						cancel()
						return
					}
				}
			}
		}()
		closed := make(chan struct{})
		go func() {
			select {
			case <-rctx.Done():
			case <-closed:
			}
			rc.Close()
		}()
		n := j.read(rctx, rc)
		close(closed)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if !j.Ship.On() {
			continue
		}
		if n > 0 {
			wait = retry
		}
		j.Ship.log.Load().Warn("system log reader ended; starting it again", "retry_in_s", wait.Seconds())
		if !sleepCtx(ctx, wait) {
			return
		}
		wait = min(wait*2, 60*time.Second)
	}
}

// read reads entries until the reader ends; it returns how many it read.
func (j *Journal) read(ctx context.Context, rc io.Reader) int {
	maxRead := j.MaxRead
	if maxRead <= 0 {
		maxRead = 64 << 10
	}
	br := bufio.NewReaderSize(rc, maxRead)
	var tokens float64
	var last time.Time
	n := 0
	for {
		raw, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Too long: drop it unread, up to its end.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			j.Ship.Dropped(JournalKey, 1)
			if err != nil {
				return n
			}
			continue
		}
		if err != nil && len(raw) == 0 {
			return n
		}
		n++
		limits, deny, on := j.Ship.Limits()
		if !on || ctx.Err() != nil {
			return n
		}
		// The byte rate, as a bucket of one batch: over it, a line is
		// dropped before it is even parsed.
		now := time.Now()
		rate, burst := float64(limits.MaxBytesPerS), float64(limits.MaxBatchBytes)
		if last.IsZero() {
			tokens = burst
		} else {
			tokens = min(burst, tokens+now.Sub(last).Seconds()*rate)
		}
		last = now
		if tokens < float64(len(raw)) {
			j.Ship.Dropped(JournalKey, 1)
			continue
		}
		tokens -= float64(len(raw))
		j.entry(raw, deny)
		if err != nil {
			return n
		}
	}
}

func (j *Journal) entry(raw []byte, deny []string) {
	var e journalEntry
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	msg, ok := message(e.Message)
	if !ok {
		return
	}
	unit := e.Unit
	if unit == "" {
		unit = e.Ident
	}
	own := j.OwnUnit
	if own == "" {
		own = agentproto.AgentUnitName
	}
	if e.Unit == own || denied(unit, deny) {
		return
	}
	l := Line{Msg: msg, Unit: unit}
	if p, err := strconv.Atoi(e.Priority); err == nil && p >= 0 && p < len(priorities) {
		l.Level = priorities[p]
	}
	l.Mono = j.Ship.Now()
	// The entry's own time: its CLOCK_MONOTONIC stamp, through the same
	// anchor (its age now, taken off the agent's clock). An entry of another
	// boot has no usable stamp: it gets the time it arrived.
	if us, err := strconv.ParseInt(e.Monotonic, 10, 64); err == nil && j.Monotonic != nil && j.BootID != "" && e.BootID == j.BootID {
		if now, err := j.Monotonic(); err == nil {
			age := max(now-time.Duration(us)*time.Microsecond, 0)
			l.Mono -= age
		} else {
			l.TimeSource = TimeSourceArrival
		}
	} else {
		l.TimeSource = TimeSourceArrival
	}
	j.Ship.Add(JournalKey, l)
}

// message reads MESSAGE: a string, or an array of bytes for a message that
// is not text.
func message(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] == 'n' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s != ""
	}
	var b []byte
	var nums []int
	if json.Unmarshal(raw, &nums) != nil {
		return "", false
	}
	for _, n := range nums {
		b = append(b, byte(n))
	}
	return strings.ToValidUTF8(string(b), string(utf8.RuneError)), len(b) > 0
}

func denied(unit string, deny []string) bool {
	for _, d := range deny {
		if d == unit || d+".service" == unit {
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
