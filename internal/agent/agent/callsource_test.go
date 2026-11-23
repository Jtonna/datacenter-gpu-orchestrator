package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// 0.1.10: every allowed call on the agent API is named at DEBUG with its
// source address, once per distinct source per minute (behind Docker
// Desktop every caller arrives from the gateway; this shows which network
// the calls come from). Refused calls are not logged by this line.
func TestAllowedCallSourceIsLoggedOncePerMinute(t *testing.T) {
	var buf lockedBuf
	e := newTenvWith(t, func(o *Options, d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	e.a.RecoverLaunch(t.Context())
	count := func(src string) int {
		n := 0
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, "agent API call allowed") && strings.Contains(l, "source="+src) {
				n++
			}
		}
		return n
	}
	for i := 0; i < 5; i++ {
		e.do("GET", "/launch", nil, fromPotluck)
		e.do("GET", "/launch", nil, "172.30.0.5:1234")
	}
	e.do("GET", "/launch", nil, "192.0.2.50:1234") // refused: not this line
	if count("127.0.0.1") != 1 || count("172.30.0.5") != 1 || count("192.0.2.50") != 0 {
		t.Fatalf("log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), "path=/launch") {
		t.Fatalf("log:\n%s", buf.String())
	}
	e.clock.Advance(59 * time.Second)
	e.do("GET", "/launch", nil, fromPotluck)
	if count("127.0.0.1") != 1 {
		t.Fatalf("logged again within the minute:\n%s", buf.String())
	}
	e.clock.Advance(2 * time.Second)
	e.do("GET", "/launch", nil, fromPotluck)
	if count("127.0.0.1") != 2 {
		t.Fatalf("not logged again after a minute:\n%s", buf.String())
	}
}
