package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/core"
)

// Member is the subset of a fleet member (contract/fleet.schema.json) used
// here. It is also the body of GET /api/nodes/:id.
type Member struct {
	ID         string   `json:"id"`
	Hostname   string   `json:"hostname"`
	IP         *string  `json:"ip"`
	Role       string   `json:"role"`
	Source     string   `json:"source"`
	Online     bool     `json:"online"`
	LastSeen   int64    `json:"last_seen"`
	RPCPort    *int     `json:"rpc_port"`
	RAMTotalGB *float64 `json:"ram_total_gb"`
	RAMUsedGB  *float64 `json:"ram_used_gb"`
	VRAMTotal  *float64 `json:"vram_total"` // GB
	VRAMUsed   *float64 `json:"vram_used"`  // GB
}

// ProbeRunner runs the local probe command line and returns its stdout.
type ProbeRunner func(ctx context.Context, cmd string) ([]byte, error)

// Options tune a Client. The zero value selects the production defaults.
type Options struct {
	HTTPClient     *http.Client     // default: a plain client (no overall timeout; streams are long-lived)
	Clock          func() time.Time // for Age and LastReconnect; default time.Now
	PollInterval   time.Duration    // GET /api/fleet period while disconnected; default 5s
	ProbeInterval  time.Duration    // local probe period; default 2s
	Backoff        []time.Duration  // reconnect delays, last one repeats; default 1s, 2s, 5s
	IdleTimeout    time.Duration    // no bytes for this long drops the stream; default 15s
	RequestTimeout time.Duration    // polls, FetchNode and one probe run; default 5s
	RunProbe       ProbeRunner      // default: exec of local_probe_cmd split on whitespace, no shell
	Logger         *slog.Logger     // default slog.Default()
}

// Client is the fleet console client, the local probe and the merged node
// view. Create it with New and run it with Run; it implements View.
type Client struct {
	cfg  *config.Config
	opts Options
	base string

	mu              sync.Mutex
	members         []Member
	haveByte        bool
	lastByte        time.Time
	connected       bool
	lastReconnect   time.Time
	nodes           []core.Node
	probeOK         bool
	probeTotal      int
	probeUsed       int
	probeWarned     bool
	noTierLogged    map[string]bool
	ambiguousLogged map[string]string
	notify          chan struct{}
	created         time.Time
	feedDownLogged  bool
	staleLogged     bool

	nextPoll time.Time // feed goroutine only
}

// New builds a Client for cfg (tiers, node_overrides, fleet.*, local_probe_cmd).
func New(cfg *config.Config, opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = 2 * time.Second
	}
	if len(opts.Backoff) == 0 {
		opts.Backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 15 * time.Second
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 5 * time.Second
	}
	if opts.RunProbe == nil {
		opts.RunProbe = execProbe
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		created: opts.Clock(),
		cfg:     cfg, opts: opts, base: strings.TrimRight(cfg.Fleet.URL, "/"),
		noTierLogged: map[string]bool{}, ambiguousLogged: map[string]string{},
		notify: make(chan struct{}, 1),
	}
}

// execProbe runs cmd without a shell (split on whitespace).
func execProbe(ctx context.Context, cmd string) ([]byte, error) {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return nil, errors.New("empty local_probe_cmd")
	}
	return exec.CommandContext(ctx, f[0], f[1:]...).Output()
}

var _ View = (*Client)(nil)

// Run drives the feed and the local probe until ctx is done, then returns
// after both goroutines have exited.
func (c *Client) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.feedLoop(ctx) }()
	go func() { defer wg.Done(); c.probeLoop(ctx) }()
	wg.Wait()
	c.opts.HTTPClient.CloseIdleConnections()
	return nil
}

// ---- View ----

// Nodes implements View.
func (c *Client) Nodes() []core.Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.Node(nil), c.nodes...)
}

// Age implements View. Bytes from a successful poll count too.
func (c *Client) Age() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.haveByte {
		return NeverReceived
	}
	if d := c.opts.Clock().Sub(c.lastByte); d > 0 {
		return d
	}
	return 0
}

// Connected implements View.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// LastReconnect implements View.
func (c *Client) LastReconnect() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastReconnect
}

// Changed implements View.
func (c *Client) Changed() <-chan struct{} { return c.notify }

// Resolve implements View.
func (c *Client) Resolve(name string) (core.NodeID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isLocalNameLocked(name) {
		return c.localIDLocked(), nil
	}
	m, amb, err := c.resolveLocked(name)
	switch {
	case amb != nil:
		c.logAmbiguousLocked(name, amb)
		return "", &AmbiguousError{Name: name, IDs: amb}
	case err == nil:
		delete(c.ambiguousLogged, name)
		if c.isLocalMemberLocked(*m) {
			return c.localIDLocked(), nil
		}
		return core.NodeID(m.ID), nil
	}
	return "", err
}

// FetchNode implements View. For the local node it runs the probe once; if
// the probe cannot run it asks the console like for any other node.
func (c *Client) FetchNode(ctx context.Context, id core.NodeID) (core.Node, error) {
	c.mu.Lock()
	isLocal := id == c.localIDLocked()
	var memberID string
	if lm := c.localMemberLocked(); lm != nil {
		memberID = lm.ID
	}
	c.mu.Unlock()
	if isLocal {
		pctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
		total, used, err := c.runProbe(pctx)
		cancel()
		if err == nil {
			c.mu.Lock()
			defer c.mu.Unlock()
			n, ok := c.localFromProbeLocked(total, used)
			if !ok {
				return core.Node{}, errors.New("fleet: local node matches no tier")
			}
			return n, nil
		}
		// Probe unavailable: ask the console about the fleet member for the
		// local node, never about the local id itself.
		if memberID == "" {
			return core.Node{}, fmt.Errorf("%w: local node %s has no fleet member and the probe failed", ErrNotFound, id)
		}
		return c.fetchMember(ctx, memberID, true, id)
	}
	return c.fetchMember(ctx, string(id), false, id)
}

// fetchMember reads GET /api/nodes/<memberID> and converts it; the node is
// published under id.
func (c *Client) fetchMember(ctx context.Context, memberID string, isLocal bool, id core.NodeID) (core.Node, error) {
	rctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.base+"/api/nodes/"+url.PathEscape(memberID), nil)
	if err != nil {
		return core.Node{}, err
	}
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return core.Node{}, fmt.Errorf("fleet: fetch node %s: %w", id, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return core.Node{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	case resp.StatusCode != http.StatusOK:
		return core.Node{}, fmt.Errorf("fleet: fetch node %s: status %d", id, resp.StatusCode)
	}
	var m Member
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return core.Node{}, fmt.Errorf("fleet: fetch node %s: %w", id, err)
	}
	n, ok := nodeFromMember(c.cfg, m, isLocal)
	if !ok {
		return core.Node{}, fmt.Errorf("fleet: node %s matches no tier", id)
	}
	n.ID = id
	return n, nil
}

// ---- state ----

// resolveLocked implements the design 6 rule. Exactly one of the results is
// set: the member, the ambiguous online ids, or an error.
func (c *Client) resolveLocked(name string) (*Member, []core.NodeID, error) {
	for i := range c.members {
		if strings.EqualFold(c.members[i].ID, name) {
			return &c.members[i], nil, nil
		}
	}
	var cands, online []*Member
	for i := range c.members {
		if strings.EqualFold(c.members[i].Hostname, name) {
			cands = append(cands, &c.members[i])
			if c.members[i].Online {
				online = append(online, &c.members[i])
			}
		}
	}
	switch {
	case len(online) >= 2:
		ids := make([]core.NodeID, len(online))
		for i, m := range online {
			ids[i] = core.NodeID(m.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return nil, ids, nil
	case len(online) == 1:
		return online[0], nil, nil
	case len(cands) == 0:
		return nil, nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	best := cands[0] // all offline: the most recently seen
	for _, m := range cands[1:] {
		if m.LastSeen > best.LastSeen || (m.LastSeen == best.LastSeen && m.ID < best.ID) {
			best = m
		}
	}
	return best, nil, nil
}

func (c *Client) logAmbiguousLocked(name string, ids []core.NodeID) {
	key := fmt.Sprint(ids)
	if c.ambiguousLogged[name] == key {
		return
	}
	c.ambiguousLogged[name] = key
	c.opts.Logger.Error("node name ambiguous, matches none", "name", name, "online_ids", key)
}

// localMemberLocked is the fleet member for fleet.local_node, if unambiguous.
func (c *Client) localMemberLocked() *Member {
	name := c.cfg.Fleet.LocalNode
	if name == "" {
		return nil
	}
	m, amb, _ := c.resolveLocked(name)
	if amb != nil {
		c.logAmbiguousLocked(name, amb)
	} else if m != nil {
		delete(c.ambiguousLogged, name)
	}
	return m
}

// localIDLocked is the local node's one fixed id: the configured
// fleet.local_node, whether or not (or when) its fleet member shows up.
func (c *Client) localIDLocked() core.NodeID {
	if c.cfg.Fleet.LocalNode == "" {
		return "local"
	}
	return core.NodeID(c.cfg.Fleet.LocalNode)
}

func (c *Client) isLocalNameLocked(name string) bool {
	return c.cfg.Fleet.LocalNode != "" && strings.EqualFold(name, c.cfg.Fleet.LocalNode)
}

// isLocalMemberLocked: the member's id or hostname is fleet.local_node.
func (c *Client) isLocalMemberLocked(m Member) bool {
	return c.isLocalNameLocked(m.ID) || c.isLocalNameLocked(m.Hostname)
}

func (c *Client) localFromProbeLocked(total, used int) (core.Node, bool) {
	m := c.localMemberLocked()
	n := core.Node{ID: c.localIDLocked(), Hostname: c.cfg.Fleet.LocalNode, Local: true, Online: true, TotalMiB: total, FreeMiB: clampZero(total - used)}
	var role, source string
	if m != nil {
		n.Hostname, role, source = m.Hostname, m.Role, m.Source
	}
	t, ok := c.cfg.MatchTier(n.Hostname, role, source, true)
	if !ok {
		return core.Node{}, false
	}
	n.Priority, n.HeadroomMiB, n.SharedMemory, n.LinkMBs, n.Settle = t.Priority, t.HeadroomMiB, t.SharedMemory, t.LinkMBs, t.Settle
	return n, true
}

// rebuildLocked recomputes the node list and wakes Changed if it differs.
func (c *Client) rebuildLocked() {
	local := c.localMemberLocked()
	var nodes []core.Node
	for _, m := range c.members {
		if c.isLocalMemberLocked(m) {
			continue
		}
		n, ok := nodeFromMember(c.cfg, m, false)
		if !ok {
			if !c.noTierLogged[m.ID] {
				c.noTierLogged[m.ID] = true
				c.opts.Logger.Info("fleet member matches no tier, excluded", "id", m.ID, "hostname", m.Hostname, "role", m.Role, "source", m.Source)
			}
			continue
		}
		nodes = append(nodes, n)
	}
	var ln core.Node
	var lok bool
	if c.probeOK {
		ln, lok = c.localFromProbeLocked(c.probeTotal, c.probeUsed)
	} else if local != nil {
		ln, lok = nodeFromMember(c.cfg, *local, true)
		ln.ID = c.localIDLocked()
	}
	if lok {
		nodes = append(nodes, ln)
	} else if (c.probeOK || local != nil) && !c.noTierLogged["<local>"] {
		c.noTierLogged["<local>"] = true
		c.opts.Logger.Info("local node matches no tier, excluded")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	if !reflect.DeepEqual(nodes, c.nodes) && !(len(nodes) == 0 && len(c.nodes) == 0) {
		c.nodes = nodes
		c.wakeLocked()
	}
}

func (c *Client) wakeLocked() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *Client) touch() {
	c.mu.Lock()
	c.haveByte, c.lastByte = true, c.opts.Clock()
	c.checkStaleLocked()
	c.mu.Unlock()
}

// checkStaleLocked logs the view becoming stale (age > stale_after) and
// becoming fresh again, once per transition and never per retry. Before the
// first byte the age counts from the client's creation.
func (c *Client) checkStaleLocked() {
	var age time.Duration
	if c.haveByte {
		age = c.opts.Clock().Sub(c.lastByte)
	} else {
		age = c.opts.Clock().Sub(c.created)
	}
	switch stale := age > c.cfg.Fleet.StaleAfter; {
	case stale && !c.staleLogged:
		c.staleLogged = true
		c.opts.Logger.Warn("fleet view stale: remote nodes are ineligible until the console is heard from",
			"age", age.Round(time.Second), "stale_after", c.cfg.Fleet.StaleAfter)
	case !stale && c.staleLogged:
		c.staleLogged = false
		c.opts.Logger.Info("fleet view fresh again")
	}
}

// feedFailed logs the first failure of an outage (a Warn with the error);
// further failures are silent until the feed has recovered.
func (c *Client) feedFailed(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.feedDownLogged {
		c.feedDownLogged = true
		c.opts.Logger.Warn("fleet console unreachable: feed disconnected, polling until it is back", "url", c.base, "err", err)
	}
	c.checkStaleLocked()
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected != v {
		c.connected = v
		c.wakeLocked()
	}
	if v {
		c.lastReconnect = c.opts.Clock()
		if c.feedDownLogged {
			c.feedDownLogged = false
			c.opts.Logger.Info("fleet feed recovered")
		}
	}
}

func (c *Client) applySnapshot(ms []Member) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.members = ms
	c.rebuildLocked()
}

// ---- feed ----

type fleetDoc struct {
	Members *[]Member `json:"members"`
}

func (c *Client) applyDoc(data []byte) bool {
	var d fleetDoc
	err := json.Unmarshal(data, &d)
	if err == nil && d.Members == nil {
		err = errors.New("no members array")
	}
	if err != nil {
		c.opts.Logger.Warn("bad fleet payload ignored", "err", err)
		return false
	}
	c.applySnapshot(*d.Members)
	return true
}

func (c *Client) feedLoop(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		gotEvent := c.stream(ctx)
		c.setConnected(false)
		if ctx.Err() != nil {
			return
		}
		if gotEvent {
			attempt = 0
		}
		delay := c.opts.Backoff[min(attempt, len(c.opts.Backoff)-1)]
		attempt++
		c.pollUntil(ctx, time.Now().Add(delay))
	}
}

// pollUntil polls GET /api/fleet every PollInterval until the retry time.
func (c *Client) pollUntil(ctx context.Context, retryAt time.Time) {
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(c.nextPoll) {
			c.poll(ctx)
			now = time.Now()
			c.nextPoll = now.Add(c.opts.PollInterval)
		}
		wake := c.nextPoll
		if retryAt.Before(wake) {
			wake = retryAt
		}
		if d := wake.Sub(now); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		if !time.Now().Before(retryAt) {
			return
		}
	}
}

func (c *Client) poll(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.base+"/api/fleet", nil)
	if err != nil {
		return
	}
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		c.opts.Logger.Debug("fleet poll failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.opts.Logger.Debug("fleet poll failed", "status", resp.StatusCode)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		c.opts.Logger.Debug("fleet poll failed", "err", err)
		return
	}
	c.touch()
	c.applyDoc(body)
}

// touchReader reports every successful read (any byte is proof of life).
type touchReader struct {
	r  io.Reader
	on func()
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.on()
	}
	return n, err
}

// stream holds one SSE connection until it ends or fails. It reports whether
// at least one event arrived (which resets the reconnect backoff).
func (c *Client) stream(ctx context.Context) (gotEvent bool) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := time.AfterFunc(c.opts.IdleTimeout, cancel) // connect and idle watchdog
	defer wd.Stop()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, c.base+"/api/stream?topics=fleet,models", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		c.opts.Logger.Debug("fleet stream connect failed", "err", err)
		if ctx.Err() == nil {
			c.feedFailed(err)
		}
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.opts.Logger.Debug("fleet stream refused", "status", resp.StatusCode)
		c.feedFailed(fmt.Errorf("stream refused: status %d", resp.StatusCode))
		return false
	}
	wd.Reset(c.opts.IdleTimeout)
	c.touch()
	c.setConnected(true)

	sc := bufio.NewScanner(&touchReader{r: resp.Body, on: func() { wd.Reset(c.opts.IdleTimeout); c.touch() }})
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data []string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			if event == "fleet" && len(data) > 0 {
				if c.applyDoc([]byte(strings.Join(data, "\n"))) {
					gotEvent = true
				}
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
			// comment, e.g. ": ping"
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if ctx.Err() == nil {
		err := sc.Err()
		if err == nil {
			err = errors.New("stream closed by the console")
		}
		c.feedFailed(err)
	}
	return gotEvent
}

// ---- local probe ----

func (c *Client) runProbe(ctx context.Context) (total, used int, err error) {
	out, err := c.opts.RunProbe(ctx, c.cfg.LocalProbeCmd)
	if err != nil {
		return 0, 0, err
	}
	return parseSMI(out)
}

func (c *Client) probeLoop(ctx context.Context) {
	for {
		c.mu.Lock()
		c.checkStaleLocked() // a quiet feed produces no events to notice it on
		c.mu.Unlock()
		c.probeOnce(ctx)
		t := time.NewTimer(c.opts.ProbeInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (c *Client) probeOnce(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	total, used, err := c.runProbe(pctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if !c.probeWarned {
			c.probeWarned = true
			c.opts.Logger.Warn("local GPU probe failed, falling back to the fleet entry for the local node", "cmd", c.cfg.LocalProbeCmd, "err", err)
		}
		if !c.probeOK {
			return
		}
		c.probeOK = false
	} else {
		c.probeWarned = false
		if c.probeOK && c.probeTotal == total && c.probeUsed == used {
			return
		}
		c.probeOK, c.probeTotal, c.probeUsed = true, total, used
	}
	c.rebuildLocked()
}
