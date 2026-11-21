package logship

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Config sets up a Shipper.
type Config struct {
	// Base is the Potluck the agent registered with, "http://<ip>:<port>".
	// A logs url without a host is a path on it.
	Base    string
	StartID string // agent_start_id, sent as X-Agent-Start-Id
	// Now is the agent's clock (CLOCK_BOOTTIME). Lines are stamped with it
	// and turned into Potluck's time through the anchor; the node's wall
	// clock is never read.
	Now func() time.Duration
	// HTTP is the sender's own client. nil: one with a 10 s limit. It is
	// never the client of the heartbeat.
	HTTP        *http.Client
	BufferBytes int // the ring; DefaultBufferBytes when 0
	// FirstPause and MaxPause bound the growing pause after 429 or 503
	// without Retry-After (defaults 1 s and 30 s).
	FirstPause, MaxPause time.Duration
	// GoneWait is how long a 410 stops sending when no new registration
	// follows (default 60 s).
	GoneWait time.Duration
	// Quiet is the least time between two of the shipper's own warnings of
	// one kind (default 60 s), so its errors cannot flood the ring.
	Quiet time.Duration
}

type mode int32

const (
	// modeCollect: no answer yet. Lines are kept (the start of the agent's
	// log), nothing is sent.
	modeCollect mode = iota
	// modeOn: a logs object is held; lines are kept and sent.
	modeOn
	// modeOff: Potluck hands out no logs object. Nothing is kept or sent.
	modeOff
)

// conf is what the sender works from; replaced whole, read without a lock.
type conf struct {
	mode   mode
	obj    *agentproto.Logs // modeOn only
	limits agentproto.LogsLimits
	nodeID string
	gen    uint64
}

// anchor ties the agent's clock to Potluck's: Potluck's time at mono.
type anchor struct {
	mono   time.Duration
	unixNs int64
}

func (a *anchor) at(mono time.Duration) int64 { return a.unixNs + int64(mono-a.mono) }

// Shipper keeps the ring and sends it to Potluck. Every method the agent's
// heartbeat, registration and launch paths call returns at once: they set a
// value and wake the sender; they never wait for it.
type Shipper struct {
	cfg  Config
	ring *Ring
	log  atomic.Pointer[slog.Logger]

	conf   atomic.Pointer[conf]
	anchor atomic.Pointer[anchor]
	// goneAt is set when Potluck answered 410 (or refused the
	// registration): nothing is sent until the agent registers again, or at
	// most GoneWait, in case the 410 came from the log route alone.
	goneAt atomic.Int64 // the agent's clock (ns) + 1 when it happened; 0: not gone
	gen    atomic.Uint64

	wake  chan struct{} // the sender: something changed
	fetch chan int64    // the fetcher: read the object of this revision

	sent atomic.Uint64

	mu        sync.Mutex // short: never held during I/O
	lastErr   string
	lastErrAt time.Duration
	cancelReq context.CancelFunc
	warned    map[string]time.Time

	// The sender goroutine's own state.
	pending *batch
	pause   time.Duration
	notUntil time.Time // 429/503 pause
	rateAt   time.Time // byte rate: the time the sent bytes are paid for
}

// batch is lines of one key on their way: encoded chunks, each one request.
type batch struct {
	key    Key
	chunks []chunk
}

type chunk struct {
	lines [][]byte // one encoded line each, with its newline
	size  int
}

// New returns a Shipper. Call Run.
func New(cfg Config) *Shipper {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.FirstPause <= 0 {
		cfg.FirstPause = time.Second
	}
	if cfg.MaxPause <= 0 {
		cfg.MaxPause = 30 * time.Second
	}
	if cfg.GoneWait <= 0 {
		cfg.GoneWait = time.Minute
	}
	if cfg.Quiet <= 0 {
		cfg.Quiet = time.Minute
	}
	s := &Shipper{cfg: cfg, ring: NewRing(cfg.BufferBytes), wake: make(chan struct{}, 1),
		fetch: make(chan int64, 1), warned: map[string]time.Time{}}
	s.conf.Store(&conf{mode: modeCollect})
	s.log.Store(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s
}

// SetLogger sets the logger of the shipper's own lines. They are few and
// rate-limited (Config.Quiet), so they may go through the ring themselves.
func (s *Shipper) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log.Store(l)
	}
}

// Ring is the shipper's ring.
func (s *Shipper) Ring() *Ring { return s.ring }

// On reports whether lines are being shipped (a logs object is held).
func (s *Shipper) On() bool { return s.conf.Load().mode == modeOn }

// Limits are the limits in force; ok is false while nothing is shipped.
func (s *Shipper) Limits() (l agentproto.LogsLimits, denyUnits []string, ok bool) {
	c := s.conf.Load()
	if c.mode != modeOn {
		return normLimits(agentproto.LogsLimits{}), nil, false
	}
	return c.limits, c.obj.DenyUnits, true
}

// Add puts a line of key k into the ring, stamped now. It never waits.
func (s *Shipper) Add(k Key, l Line) {
	c := s.conf.Load()
	if c.mode == modeOff {
		return
	}
	// Cut at once, so one long line cannot take the ring; the encoder cuts
	// again to the limit in force when it is sent.
	keep := int(agentproto.LogsMaxLineBytes)
	if c.mode == modeOn {
		keep = int(c.limits.MaxLineBytes)
	}
	if m, whole := cut(l.Msg, keep); !whole {
		l.Msg, l.Truncated = m, true
	}
	s.stamp(&l)
	s.ring.Add(k, l)
}

// Dropped counts lines of k lost before the ring.
func (s *Shipper) Dropped(k Key, n uint64) {
	if s.conf.Load().mode == modeOff {
		return
	}
	s.ring.Dropped(k, n)
}

// stamp gives a line Potluck's time when an anchor is known. A line
// produced before the first anchor keeps only its Mono stamp and gets its
// time when it is taken out to be sent.
func (s *Shipper) stamp(l *Line) {
	if l.At != 0 {
		return
	}
	if a := s.anchor.Load(); a != nil {
		l.At = a.at(l.Mono)
	}
}

// Now is the agent's clock.
func (s *Shipper) Now() time.Duration { return s.cfg.Now() }

// setAnchor re-sets the anchor from an answer that Potluck stamped with its
// own clock. The answer was made between sent and recv; the middle is taken.
func (s *Shipper) setAnchor(serverMs int64, sent, recv time.Duration) {
	if serverMs <= 0 {
		return
	}
	if recv < sent {
		recv = sent
	}
	s.anchor.Store(&anchor{mono: sent + (recv-sent)/2, unixNs: serverMs * int64(time.Millisecond)})
}

func (s *Shipper) setGone() { s.goneAt.Store(int64(s.cfg.Now()) + 1) }

func (s *Shipper) isGone() bool {
	at := s.goneAt.Load()
	return at != 0 && s.cfg.Now()-time.Duration(at-1) < s.cfg.GoneWait
}

// Registered is called with an accepted registration answer: the node id,
// the logs object (nil: ship nothing), Potluck's time and when the request
// was sent and its answer read (agent clock).
func (s *Shipper) Registered(nodeID string, obj *agentproto.Logs, serverMs int64, sent, recv time.Duration) {
	s.setAnchor(serverMs, sent, recv)
	s.goneAt.Store(0)
	s.apply(nodeID, obj)
}

// Refused is called with a refused registration: nothing is sent until the
// agent is accepted again.
func (s *Shipper) Refused(serverMs int64, sent, recv time.Duration) {
	s.setAnchor(serverMs, sent, recv)
	s.setGone()
	s.poke()
}

// HeartbeatAnswered is called with every 200 heartbeat answer. revision 0
// (absent) means logging is off. A revision that differs from the object
// held makes the fetcher read the object again; the heartbeat does not wait
// for it.
func (s *Shipper) HeartbeatAnswered(revision, serverMs int64, sent, recv time.Duration) {
	s.setAnchor(serverMs, sent, recv)
	c := s.conf.Load()
	if revision == 0 {
		if c.mode != modeOff {
			s.apply(c.nodeID, nil)
		}
		return
	}
	if c.mode == modeOn && c.obj.Revision == revision {
		return
	}
	select {
	case s.fetch <- revision:
	default: // a fetch is asked already
	}
}

// apply puts a logs object (or none) in force.
func (s *Shipper) apply(nodeID string, obj *agentproto.Logs) {
	c := &conf{nodeID: nodeID, gen: s.gen.Add(1)}
	if obj == nil {
		c.mode = modeOff
	} else {
		c.mode, c.obj, c.limits = modeOn, obj, normLimits(obj.Limits)
	}
	old := s.conf.Swap(c)
	if c.mode == modeOff {
		s.ring.Clear()
		s.mu.Lock()
		if s.cancelReq != nil {
			s.cancelReq() // a request in flight is given up
		}
		s.mu.Unlock()
	}
	if old.mode != c.mode {
		if c.mode == modeOn {
			s.log.Load().Info("log shipping on", "revision", obj.Revision, "url", obj.URL)
		} else if old.mode == modeOn {
			s.log.Load().Info("log shipping off: Potluck hands out no logs object")
		}
	}
	s.poke()
}

func (s *Shipper) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// normLimits fills limits that are missing or out of range with the
// contract's figures.
func normLimits(l agentproto.LogsLimits) agentproto.LogsLimits {
	if l.MaxBatchBytes <= 0 {
		l.MaxBatchBytes = agentproto.LogsMaxBatchBytes
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = agentproto.LogsMaxLineBytes
	}
	l.MaxLineBytes = max(min(l.MaxLineBytes, l.MaxBatchBytes), 256)
	l.MaxBatchBytes = max(l.MaxBatchBytes, l.MaxLineBytes)
	if l.MaxBytesPerS <= 0 {
		l.MaxBytesPerS = agentproto.LogsMaxBytesPerS
	}
	if l.EveryS <= 0 {
		l.EveryS = agentproto.LogsEveryS
	}
	return l
}

// Figures are the heartbeat's logs object; nil while nothing is shipped.
func (s *Shipper) Figures() *agentproto.LogShipping {
	if s.conf.Load().mode != modeOn {
		return nil
	}
	f := &agentproto.LogShipping{LinesSent: s.sent.Load(), LinesDropped: s.ring.DroppedTotal()}
	s.mu.Lock()
	if s.lastErr != "" {
		f.LastError = s.lastErr
		age := max(float64((s.cfg.Now()-s.lastErrAt).Milliseconds())/1000, 0)
		f.LastErrorAgeS = &age
	}
	s.mu.Unlock()
	return f
}

func (s *Shipper) setErr(kind, text string) {
	s.mu.Lock()
	s.lastErr, s.lastErrAt = text, s.cfg.Now()
	last, said := s.warned[kind]
	now := time.Now()
	loud := !said || now.Sub(last) >= s.cfg.Quiet
	if loud {
		s.warned[kind] = now
	}
	s.mu.Unlock()
	if loud {
		s.log.Load().Warn("log shipping: "+text, "next_warning_in_s", s.cfg.Quiet.Seconds())
	}
}

// Run runs the sender and the fetcher until ctx ends.
func (s *Shipper) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.fetchLoop(ctx) }()
	s.sendLoop(ctx)
	wg.Wait()
}

// fetchLoop reads the logs object when the heartbeat names a new revision.
func (s *Shipper) fetchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rev := <-s.fetch:
			_ = rev // the object read is the newest; its own revision counts
			s.fetchObject(ctx)
		}
	}
}

func (s *Shipper) fetchObject(ctx context.Context) {
	c := s.conf.Load()
	if c.nodeID == "" {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, s.cfg.Base+fmt.Sprintf(agentproto.PathNodeLogsFmt, c.nodeID), nil)
	if err != nil {
		return
	}
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		s.setErr("fetch", "reading the logs object: "+err.Error())
		return // the next heartbeat asks again
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var obj agentproto.Logs
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&obj); err != nil {
			s.setErr("fetch", "reading the logs object: "+err.Error())
			return
		}
		if s.conf.Load().gen != c.gen {
			return // changed meanwhile; the next heartbeat decides again
		}
		s.apply(c.nodeID, &obj)
	case http.StatusNotFound:
		if s.conf.Load().gen == c.gen {
			s.apply(c.nodeID, nil)
		}
	case http.StatusGone:
		s.setGone()
	default:
		s.setErr("fetch", fmt.Sprintf("reading the logs object: HTTP %d", resp.StatusCode))
	}
}

// sendLoop is the one sender. A round runs at most every every_s; in it each
// key with lines gets one batch, sent one request at a time.
func (s *Shipper) sendLoop(ctx context.Context) {
	var lastRound time.Time
	for {
		c := s.conf.Load()
		ready := c.mode == modeOn && !s.isGone() && (s.pending != nil || !s.ring.Empty())
		if c.mode != modeOn {
			s.pending = nil
		}
		if ready {
			at := lastRound.Add(time.Duration(c.limits.EveryS * float64(time.Second)))
			if s.notUntil.After(at) {
				at = s.notUntil
			}
			d := time.Until(at)
			if d <= 0 {
				lastRound = time.Now()
				s.round(ctx, c)
				if ctx.Err() != nil {
					return
				}
				continue
			}
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-s.wake:
			case <-t.C:
			}
			t.Stop()
			continue
		}
		// Nothing to do: wake on a change, and look at the ring now and then.
		idle := time.Duration(c.limits.EveryS * float64(time.Second))
		if idle <= 0 {
			idle = time.Second
		}
		t := time.NewTimer(idle)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// round sends one batch of each key that has lines.
func (s *Shipper) round(ctx context.Context, c *conf) {
	seen := map[Key]bool{}
	for ctx.Err() == nil && s.conf.Load().gen == c.gen && !s.isGone() {
		if s.pending == nil {
			s.pending = s.take(c)
			if s.pending == nil {
				return
			}
		}
		if seen[s.pending.key] {
			return // the next round
		}
		seen[s.pending.key] = true
		if !s.sendBatch(ctx, c) {
			return
		}
	}
}

// take takes the next batch out of the ring and encodes it.
func (s *Shipper) take(c *conf) *batch {
	maxLine := int(c.limits.MaxLineBytes)
	k, lines, dropped, ok := s.ring.Take(int(c.limits.MaxBatchBytes), estimate(maxLine))
	if !ok {
		return nil
	}
	b := &batch{key: k}
	var enc [][]byte
	var buf bytes.Buffer
	add := func(l *Line) {
		s.stamp(l)
		buf.Reset()
		encodeLine(&buf, l, maxLine)
		enc = append(enc, bytes.Clone(buf.Bytes()))
	}
	if dropped > 0 {
		mono := s.cfg.Now()
		if len(lines) > 0 {
			mono = lines[0].Mono
		}
		add(&Line{Mono: mono, Level: "warn", Msg: fmt.Sprintf(
			"potluck-agent: %d lines of this source were dropped before this one (log buffer full, over the byte rate, too long to read, or refused)", dropped)})
	}
	for i := range lines {
		add(&lines[i])
	}
	// Chunks within max_batch_bytes. Lines were taken by an estimate;
	// escaping can make them longer, so the real size decides here.
	var cur chunk
	for _, e := range enc {
		if cur.size > 0 && cur.size+len(e) > int(c.limits.MaxBatchBytes) {
			b.chunks = append(b.chunks, cur)
			cur = chunk{}
		}
		cur.lines = append(cur.lines, e)
		cur.size += len(e)
	}
	if cur.size > 0 {
		b.chunks = append(b.chunks, cur)
	}
	if len(b.chunks) == 0 {
		return nil
	}
	return b
}

// sendBatch sends the pending batch, chunk by chunk. It returns false when
// the round must end (a pause, 410, a change).
func (s *Shipper) sendBatch(ctx context.Context, c *conf) bool {
	b := s.pending
	for len(b.chunks) > 0 {
		if s.conf.Load().gen != c.gen || s.isGone() {
			return false
		}
		ch := b.chunks[0]
		body, gz := encodeBody(ch)
		if !s.waitRate(ctx, c, len(body)) {
			return false
		}
		res, retry := s.post(ctx, c, b.key, body, gz)
		switch res {
		case resOK:
			s.sent.Add(uint64(len(ch.lines)))
			s.pause = 0
			b.chunks = b.chunks[1:]
		case resRetry:
			if retry <= 0 {
				if s.pause == 0 {
					s.pause = s.cfg.FirstPause
				} else {
					s.pause = min(s.pause*2, s.cfg.MaxPause)
				}
				retry = s.pause
			}
			s.notUntil = time.Now().Add(min(retry, s.cfg.MaxPause))
			return false
		case resGone:
			s.setGone()
			return false
		case resSplit:
			if len(ch.lines) == 1 {
				s.ring.Dropped(b.key, 1)
				b.chunks = b.chunks[1:]
				continue
			}
			h := len(ch.lines) / 2
			a, z := chunk{lines: ch.lines[:h]}, chunk{lines: ch.lines[h:]}
			for _, l := range a.lines {
				a.size += len(l)
			}
			z.size = ch.size - a.size
			b.chunks = append([]chunk{a, z}, b.chunks[1:]...)
		case resDrop:
			s.ring.Dropped(b.key, uint64(len(ch.lines)))
			b.chunks = b.chunks[1:]
		}
	}
	s.pending = nil
	return true
}

// waitRate waits until n more bytes keep within max_bytes_per_s, with a
// burst of one batch.
func (s *Shipper) waitRate(ctx context.Context, c *conf, n int) bool {
	rate := float64(c.limits.MaxBytesPerS)
	burst := time.Duration(float64(c.limits.MaxBatchBytes) / rate * float64(time.Second))
	now := time.Now()
	if s.rateAt.Before(now.Add(-burst)) {
		s.rateAt = now.Add(-burst)
	}
	s.rateAt = s.rateAt.Add(time.Duration(float64(n) / rate * float64(time.Second)))
	d := s.rateAt.Sub(now)
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-s.wake:
			if s.conf.Load().gen != c.gen {
				s.poke()
				return false
			}
		}
	}
}

// encodeBody joins a chunk's lines, gzipped when that makes it smaller.
func encodeBody(ch chunk) ([]byte, bool) {
	raw := make([]byte, 0, ch.size)
	for _, l := range ch.lines {
		raw = append(raw, l...)
	}
	if len(raw) < 1024 {
		return raw, false
	}
	var z bytes.Buffer
	w, _ := gzip.NewWriterLevel(&z, gzip.BestSpeed)
	w.Write(raw)
	w.Close()
	if z.Len() < len(raw) {
		return z.Bytes(), true
	}
	return raw, false
}

type result int

const (
	resOK result = iota
	resRetry
	resGone
	resSplit
	resDrop
)

// post sends one request and says what to do with its lines.
func (s *Shipper) post(ctx context.Context, c *conf, k Key, body []byte, gz bool) (result, time.Duration) {
	u := c.obj.URL
	if !strings.Contains(u, "://") {
		u = s.cfg.Base + u
	}
	q := url.Values{"source": {k.Source}}
	if k.Source == agentproto.LogSourceLaunch {
		q.Set("launch_id", k.LaunchID)
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.cancelReq = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.cancelReq = nil; s.mu.Unlock() }()
	if s.conf.Load().gen != c.gen {
		return resRetry, time.Millisecond
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, u+sep+q.Encode(), bytes.NewReader(body))
	if err != nil {
		s.setErr("request", "bad push address: "+err.Error())
		return resDrop, 0
	}
	req.Header.Set("Content-Type", "application/stream+json")
	req.Header.Set(agentproto.HeaderAgentStartID, s.cfg.StartID)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if a := c.obj.Auth; a != nil && a.Header != "" {
		req.Header.Set(a.Header, a.Value)
	}
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil || s.conf.Load().gen != c.gen {
			return resRetry, time.Millisecond
		}
		// A broken connection while writing is what Potluck's refusal of a
		// large body can look like (contract 11.3 item 7): wait and retry.
		s.setErr("post", "push failed: "+err.Error())
		return resRetry, 0
	}
	reason := readReason(resp.Body)
	resp.Body.Close()
	st := resp.StatusCode
	text := strconv.Itoa(st)
	if reason != "" {
		text += " " + reason
	}
	switch {
	case st >= 200 && st < 300:
		return resOK, 0
	case st == http.StatusTooManyRequests || st == http.StatusServiceUnavailable:
		s.setErr("busy", text)
		return resRetry, retryAfter(resp.Header.Get("Retry-After"))
	case st == http.StatusGone:
		s.setErr("gone", text+": Potluck does not know this agent; nothing is sent until it registers again")
		return resGone, 0
	case st == http.StatusRequestEntityTooLarge:
		s.setErr("too_large", text+": batch split")
		return resSplit, 0
	case st == http.StatusBadRequest || st == http.StatusUnsupportedMediaType:
		s.setErr("rejected", text+": batch dropped")
		return resDrop, 0
	default:
		s.setErr("other", text)
		return resRetry, 0
	}
}

func readReason(r io.Reader) string {
	var e struct {
		Reason string `json:"reason"`
	}
	b, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	json.Unmarshal(b, &e)
	return e.Reason
}

func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 0 {
		return 0
	}
	if n == 0 {
		return time.Millisecond
	}
	return time.Duration(n) * time.Second
}
