package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// A child whose host program runs on another machine (phase 6, a console as
// host): llama-server started by that node's agent as a host launch. The
// scheduler sees it as any other Child; the pool drives it from the agent's
// reports (RemoteChild.Feed, SetReady, Finish) and does what it asks for
// (the stop function).

// Addresser is implemented by a Child that does not listen on 127.0.0.1.
type Addresser interface {
	// Addr is the address requests are sent to (host:port).
	Addr() string
}

// Keyed is implemented by a Child whose server wants a key on every request
// but /health ("Authorization: Bearer <key>").
type Keyed interface {
	APIKey() string
}

// AddrOf is where requests for ch go: its own address, else 127.0.0.1 and
// its port.
func AddrOf(ch Child) string {
	if a, ok := ch.(Addresser); ok {
		return a.Addr()
	}
	return "127.0.0.1:" + strconv.Itoa(ch.Port())
}

// APIKeyOf is the key requests to ch carry; "" when none.
func APIKeyOf(ch Child) string {
	if k, ok := ch.(Keyed); ok {
		return k.APIKey()
	}
	return ""
}

// RemoteTailLines is how many output lines a RemoteChild keeps for Tail.
const RemoteTailLines = 400

// RemoteChild is a Child that runs on a node. Create it with NewRemoteChild.
type RemoteChild struct {
	addr   string
	port   int
	argv   []string
	key    string
	stop   func(StopMode)
	health func(ctx context.Context, url string) error

	ring    *ring
	load    *LoadLog
	linesMu sync.Mutex // a LineWriter takes one writer at a time
	lines   *LineWriter

	mu        sync.Mutex
	ready     chan struct{} // closed when the agent says ready
	readyOnce sync.Once
	stopReq   bool
	stopMode  StopMode
	stopCh    chan StopMode
	done      chan struct{} // closed once the exit is final
	finOnce   sync.Once
	status    ExitStatus
	exitCh    chan ExitStatus
	// HealthTries and HealthEvery bound the scheduler's own /health check
	// after the agent said ready (defaults 5 and 1 s).
	HealthTries int
	HealthEvery time.Duration
}

var _ Child = (*RemoteChild)(nil)

// NewRemoteChild returns the child of a host launch that listens on addr
// (host:port). stop is called (at most once per mode, never under a lock of
// the child) when the scheduler stops it; the caller then reports the end
// with Finish. health checks one URL; nil uses a plain GET with a 3 s limit.
func NewRemoteChild(addr string, argv []string, key string, stop func(StopMode), health func(ctx context.Context, url string) error) *RemoteChild {
	port := 0
	if i := lastColon(addr); i >= 0 {
		port, _ = strconv.Atoi(addr[i+1:])
	}
	if health == nil {
		health = getHealth
	}
	c := &RemoteChild{addr: addr, port: port, argv: append([]string(nil), argv...), key: key, stop: stop, health: health,
		ring: newRing(RemoteTailLines), load: NewLoadLog(),
		ready: make(chan struct{}), done: make(chan struct{}), exitCh: make(chan ExitStatus, 1)}
	c.lines = NewLineWriter(func(s string) {
		c.ring.Add(s)
		c.load.Feed(s)
	})
	return c
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func getHealth(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/health answered %d", resp.StatusCode)
	}
	return nil
}

// Port is the port it listens on (part of Addr).
func (c *RemoteChild) Port() int { return c.port }

// PID is 0: the process is not on this machine.
func (c *RemoteChild) PID() int { return 0 }

// Argv is the command line the agent reported.
func (c *RemoteChild) Argv() []string { return append([]string(nil), c.argv...) }

// Addr implements Addresser.
func (c *RemoteChild) Addr() string { return c.addr }

// APIKey implements Keyed.
func (c *RemoteChild) APIKey() string { return c.key }

// Tail returns at most n of the newest output lines.
func (c *RemoteChild) Tail(n int) []string { return c.ring.Last(n) }

// Load returns the buffer sizes read from the output so far.
func (c *RemoteChild) Load() LoadStats { return c.load.Snapshot() }

// Exited implements Child.
func (c *RemoteChild) Exited() <-chan ExitStatus { return c.exitCh }

// Feed takes the next bytes of the launch's output (the copied launch log).
func (c *RemoteChild) Feed(b []byte) {
	c.linesMu.Lock()
	defer c.linesMu.Unlock()
	c.lines.Write(b)
}

// SetReady says the agent reports the launch ready.
func (c *RemoteChild) SetReady() { c.readyOnce.Do(func() { close(c.ready) }) }

// Finish reports the end of the launch once; later calls are ignored.
// Requested is set when Stop had been called.
func (c *RemoteChild) Finish(st ExitStatus) {
	c.finOnce.Do(func() {
		c.closeLines()
		st = c.settle(st)
		c.exitCh <- st
		close(c.exitCh)
	})
}

func (c *RemoteChild) closeLines() {
	c.linesMu.Lock()
	defer c.linesMu.Unlock()
	c.lines.Close()
}

// settle makes st the final status and ends the stops.
func (c *RemoteChild) settle(st ExitStatus) ExitStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st.Requested = st.Requested || c.stopReq
	if st.Tail == nil {
		st.Tail = c.ring.Lines()
	}
	c.status = st
	close(c.done) // under mu: Stop sends nothing after this
	if c.stopCh != nil {
		close(c.stopCh)
	}
	return st
}

// Done is closed when the exit is final.
func (c *RemoteChild) Done() <-chan struct{} { return c.done }

// Stop implements Child: it never blocks. A kill after a graceful stop is
// passed on; anything else after the first call is not.
func (c *RemoteChild) Stop(mode StopMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	if c.stopReq && (c.stopMode == StopKill || mode == StopGraceful) {
		return
	}
	c.stopReq, c.stopMode = true, mode
	if c.stopCh == nil {
		// One goroutine runs the stops in order: a graceful stop, then the
		// kill that follows it. At most two are ever sent, so the send
		// below never blocks.
		c.stopCh = make(chan StopMode, 2)
		go func(ch <-chan StopMode) {
			for m := range ch {
				c.stop(m)
			}
		}(c.stopCh)
	}
	c.stopCh <- mode
}

// ErrHostUnreachable wraps the scheduler's failed /health check of a host
// whose agent said ready.
var ErrHostUnreachable = errors.New("host_unreachable")

// WaitHealthy waits until the agent says ready, then checks /health from
// this machine itself (the path requests take). When that fails the launch
// is stopped and the error wraps ErrHostUnreachable; the end then arrives
// on Exited.
func (c *RemoteChild) WaitHealthy(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.exitedErr()
	case <-c.ready:
	}
	tries, every := c.HealthTries, c.HealthEvery
	if tries <= 0 {
		tries = 5
	}
	if every <= 0 {
		every = time.Second
	}
	url := "http://" + c.addr + "/health"
	var err error
	for i := 0; i < tries; i++ {
		if err = c.health(ctx, url); err == nil {
			return nil
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-c.done:
			t.Stop()
			return c.exitedErr()
		case <-t.C:
		}
	}
	line := fmt.Sprintf("potluck: %s: the agent reports the host ready, but %s does not answer from the scheduler: %v", ErrHostUnreachable, url, err)
	c.ring.Add(line)
	c.Stop(StopKill)
	return fmt.Errorf("%w: %s: %v", ErrHostUnreachable, url, err)
}

func (c *RemoteChild) exitedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Errorf("%w: %s", ErrChildExited, c.status)
}
