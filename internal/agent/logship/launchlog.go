package logship

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/launch"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// LaunchTail ships the output of launches.
//
// On both installs a launch writes its stdout and stderr into its launch log
// file under the data folder (systemd: StandardOutput=append:, child: the
// file as its stdout), the file GET /launch/log serves. The tail reads that
// file by offset. The launch process never waits for it: it writes into a
// file in RAM, never into a pipe the agent must drain. The file mixes both
// streams, so a line has no stream field.
//
// The tail reads at most max_bytes_per_s; when the launch writes faster, the
// part it cannot read in time is skipped (a line says how much), so a flood
// costs no CPU beyond the rate.
type LaunchTail struct {
	Ship *Shipper
	// Current returns the current or last launch's id and log file.
	Current func() (id string, lf *launch.LogFile, ok bool)
	Every   time.Duration // default 250 ms

	id      string
	off     int64
	partial []byte
	skipped int64
	tokens  float64
	last    time.Time
	wasOn   bool
	begun   bool
}

// Begin takes the starting point: the launch that exists now (re-adopted
// after an agent restart) is followed from its current end, as the journal
// is; its earlier lines were shipped by the agent before. Run calls it when
// it was not called; a caller that must know the point is taken (a test)
// calls it first, in its own goroutine, before Run.
func (t *LaunchTail) Begin() {
	if t.begun {
		return
	}
	t.begun = true
	if id, lf, ok := t.Current(); ok {
		t.id, t.off = id, lf.Size()
	}
}

// Run follows launch logs until ctx ends (Begin first).
func (t *LaunchTail) Run(ctx context.Context) {
	every := t.Every
	if every <= 0 {
		every = 250 * time.Millisecond
	}
	t.Begin()
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.tick()
		}
	}
}

func (t *LaunchTail) key() Key { return Key{Source: agentproto.LogSourceLaunch, LaunchID: t.id} }

func (t *LaunchTail) tick() {
	limits, _, on := t.Ship.Limits()
	id, lf, ok := t.Current()
	if !on {
		// Off: nothing is read. When it goes on, a running launch is
		// followed from that moment.
		t.wasOn = false
		if ok {
			t.id, t.off, t.partial = id, lf.Size(), nil
		}
		return
	}
	if !t.wasOn {
		t.wasOn, t.last = true, time.Time{}
	}
	if !ok {
		return
	}
	if id != t.id {
		t.flush()
		t.id, t.off, t.partial, t.skipped = id, 0, nil, 0
	}
	now := time.Now()
	rate, burst := float64(limits.MaxBytesPerS), float64(limits.MaxBatchBytes)
	if t.last.IsZero() {
		t.tokens = burst
	} else {
		t.tokens = min(burst, t.tokens+now.Sub(t.last).Seconds()*rate)
	}
	t.last = now
	size := lf.Size()
	if behind := size - t.off - int64(t.tokens); behind > 0 {
		// More than the rate allows: skip the older part.
		t.off += behind
		t.skipped += behind
		t.partial = nil
	}
	maxLine := int(limits.MaxLineBytes)
	for t.off < size && t.tokens >= 1 {
		n := min(64<<10, int(t.tokens), int(size-t.off))
		data, next, dropped, err := lf.Read(t.off, n)
		if err != nil || len(data) == 0 {
			t.skipped += dropped
			if next > t.off {
				t.off = next
				continue
			}
			break
		}
		t.skipped += dropped
		t.off = next
		t.tokens -= float64(len(data))
		t.lines(data, maxLine)
	}
	if t.skipped > 0 {
		t.Ship.Dropped(t.key(), 1)
		t.Ship.Add(t.key(), Line{Mono: t.Ship.Now(), Level: "warn",
			Msg: fmt.Sprintf("potluck-agent: %d bytes of this launch's output were skipped (written faster than the shipping byte rate)", t.skipped)})
		t.skipped = 0
	}
}

// lines splits data into lines and adds them. A line longer than maxLine is
// shipped cut, and the rest of it up to its newline is skipped.
func (t *LaunchTail) lines(data []byte, maxLine int) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if len(t.partial) < maxLine {
				t.partial = append(t.partial, data[:min(len(data), maxLine-len(t.partial)+1)]...)
			}
			return
		}
		if len(t.partial) < maxLine {
			t.partial = append(t.partial, data[:min(i, maxLine-len(t.partial)+1)]...)
		}
		t.add(t.partial)
		t.partial = t.partial[:0]
		data = data[i+1:]
	}
}

func (t *LaunchTail) add(b []byte) {
	b = bytes.TrimRight(b, "\r")
	if len(b) == 0 {
		return
	}
	t.Ship.Add(t.key(), Line{Mono: t.Ship.Now(), Msg: string(b)})
}

// flush ships a last line without its newline (the launch ended).
func (t *LaunchTail) flush() {
	if len(t.partial) > 0 && t.id != "" {
		t.add(t.partial)
	}
	t.partial = nil
}
