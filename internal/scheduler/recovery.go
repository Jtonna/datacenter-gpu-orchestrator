package scheduler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/core"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
)

// Recovery requests (design 8.4b). When worker health marks a remote worker
// hung, and recovery.enabled is on, the scheduler asks the fleet manager to
// relay a recovery request to that node: restart its rpc-server, restart
// docker, or reboot it, escalating one step at a time. A small service on the
// node polls the fleet manager, acts, and reports the outcome, which comes
// back to the scheduler as last_recovery_action on the fleet member. The
// scheduler never contacts the node itself.
//
// The HTTP POST runs off the event loop (effRecoveryPost) and its answer comes
// back as evRecoveryPosted, like every other slow job. Nothing here waits.

const (
	recoveryRetryDelay = 10 * time.Second // a 5xx or timeout is retried once after this
	recoveryNonceKeep  = 8                // nonces remembered per node, to recognise outcomes

	// Request status values in status.recovery.last_request.
	RecoveryPending = "pending" // posted, no answer yet
	RecoveryStored  = "stored"  // 200: the fleet manager stored it
	RecoveryRefused = "refused" // a 4xx: not retried
	RecoveryError   = "error"   // 5xx or timeout (after the one retry, if any)
)

// RecoveryResponse is the fleet manager's answer to a recovery POST.
type RecoveryResponse struct {
	Status   int
	BodyTail string // the last bytes of the response body, for the log
}

// RecoveryPoster sends one recovery request. A non-nil error means no answer
// (connection failure or timeout).
type RecoveryPoster interface {
	Post(ctx context.Context, url string, body []byte, timeout time.Duration) (RecoveryResponse, error)
}

// HTTPRecoveryPoster is the production RecoveryPoster.
type HTTPRecoveryPoster struct{ Client *http.Client }

// Post implements RecoveryPoster.
func (p HTTPRecoveryPoster) Post(ctx context.Context, u string, body []byte, timeout time.Duration) (RecoveryResponse, error) {
	cl := p.Client
	if cl == nil {
		cl = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return RecoveryResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return RecoveryResponse{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if len(b) > 200 {
		b = b[len(b)-200:]
	}
	return RecoveryResponse{Status: resp.StatusCode, BodyTail: strings.TrimSpace(string(b))}, nil
}

// recoveryURL is <fleet.url>/api/recovery/<node id>, the id percent-encoded
// (colons too: a MAC-style id is one path segment).
func recoveryURL(base string, id core.NodeID) string {
	seg := strings.ReplaceAll(url.PathEscape(string(id)), ":", "%3A")
	return strings.TrimRight(base, "/") + "/api/recovery/" + seg
}

// recoveryRecord is the JSON body of a recovery request (contract v1).
type recoveryRecord struct {
	V            int    `json:"v"`
	TargetNodeID string `json:"target_node_id"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	IssuedAt     int64  `json:"issued_at"`
	ExpiresAt    int64  `json:"expires_at"`
	Nonce        string `json:"nonce"`
}

func randomNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on the supported platforms
	}
	return hex.EncodeToString(b[:])
}

// recoveryNode is the scheduler's record of one node's recovery requests.
type recoveryNode struct {
	// The current hung episode.
	active  bool
	epoch   uint64 // changes at every episode start and reset
	started time.Time
	step    int       // steps sent this episode (0 to len(actions))
	stepAt  time.Time // when the latest request of the episode was posted

	curAction string
	curNonce  string
	posting   bool      // a POST is in flight
	retryAt   time.Time // a retry is due then; zero when none
	retried   bool      // the current step already used its retry
	advance   bool      // the node reported curNonce failed: do not wait step_wait
	spent     bool      // "sequence finished" logged for this episode
	stopped   bool      // a request was refused: no more steps this episode

	// Across episodes.
	lastSent time.Time // when a request was last posted; min_interval counts from it
	sent     []string  // recent nonces, oldest first
	lastReq  *RecoveryRequestStatus
	lastOut  *core.RecoveryOutcome
	outKey   string
}

func (c *schedCore) recoveryOn() bool { return c.cfg.Recovery.Enabled }

// recoveryStep runs the whole feature for one step, after worker health: read
// outcomes, then start, continue or reset each node's episode.
func (c *schedCore) recoveryStep() {
	if !c.recoveryOn() {
		return
	}
	c.recoveryOutcomes()
	seen := map[core.NodeID]bool{}
	var ids []core.NodeID
	for id := range c.health {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range c.recov {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		h, rs := c.health[id], c.recov[id]
		if h == nil || h.state != HealthHung || !c.recoveryAllowed(id) {
			if rs != nil && rs.active {
				c.recoveryReset(id, rs, h)
			}
			continue
		}
		if rs == nil {
			rs = &recoveryNode{}
			c.recov[id] = rs
		}
		if !rs.active {
			rs.active, rs.started, rs.step, rs.stepAt = true, c.now, 0, timeZero
			rs.retryAt, rs.retried, rs.advance, rs.spent, rs.stopped = timeZero, false, false, false, false
			rs.curAction, rs.curNonce = "", ""
			rs.epoch++
			c.log.Info("recovery episode started", "node", id, "actions", strings.Join(c.cfg.Recovery.Actions, ","))
			c.changed = true
		}
		c.recoverySend(id, h, rs)
	}
}

// recoveryAllowed: never for the local node or an excluded node. (Health only
// exists for a remote node an instance uses, so this is a second line.)
func (c *schedCore) recoveryAllowed(id core.NodeID) bool {
	if n, ok := c.raw[id]; ok && (n.Local || c.excluded(n)) {
		return false
	}
	return true
}

// recoveryReset ends the episode: the node is ok again, or no instance uses it.
func (c *schedCore) recoveryReset(id core.NodeID, rs *recoveryNode, h *workerHealth) {
	why := "no instance uses the node"
	if h != nil {
		why = "worker health is " + h.state
	}
	c.log.Info("recovery sequence reset", "node", id, "reason", why, "steps_sent", rs.step,
		"episode", c.now.Sub(rs.started).Round(time.Second))
	rs.active, rs.step, rs.stopped = false, 0, false
	rs.retryAt, rs.retried, rs.advance = timeZero, false, false
	rs.epoch++
	c.changed = true
}

// recoveryNextAt is when the next step may be sent: min_interval after the last
// request, and (after the first step) step_wait after the latest step unless
// the node reported that step failed. ok is false when every step was sent.
func (c *schedCore) recoveryNextAt(rs *recoveryNode) (time.Time, bool) {
	r := c.cfg.Recovery
	if rs.step >= len(r.Actions) {
		return timeZero, false
	}
	at := rs.lastSent.Add(r.MinInterval)
	if rs.step > 0 && !rs.advance {
		if w := rs.stepAt.Add(r.StepWait); w.After(at) {
			at = w
		}
	}
	return at, true
}

// recoverySend sends the next request of the episode if one is due and the
// view is live.
func (c *schedCore) recoverySend(id core.NodeID, h *workerHealth, rs *recoveryNode) {
	if rs.posting || rs.stopped {
		return
	}
	if !rs.retryAt.IsZero() {
		if c.now.Before(rs.retryAt) || !c.fresh {
			return
		}
		rs.retryAt, rs.retried = timeZero, true
		c.recoveryPost(id, h, rs, rs.curAction, "; retry after a failed send")
		return
	}
	at, ok := c.recoveryNextAt(rs)
	if !ok {
		if !rs.spent && !c.now.Before(rs.stepAt.Add(c.cfg.Recovery.StepWait)) {
			rs.spent = true
			c.log.Warn("recovery sequence finished and worker still hung", "node", id,
				"actions", strings.Join(c.cfg.Recovery.Actions, ","))
			c.record(history.TRecoveryStopped, history.Warn, "",
				fmt.Sprintf("The recovery sequence for %s finished (%s) and the worker is still hung.", c.nodeName(id),
					strings.Join(c.cfg.Recovery.Actions, ", ")), []string{c.nodeName(id)}, "",
				map[string]any{"reason": "every action was sent", "actions": c.cfg.Recovery.Actions})
		}
		return
	}
	if c.now.Before(at) || !c.fresh {
		return
	}
	action := c.cfg.Recovery.Actions[rs.step]
	suffix := ""
	if rs.step > 0 {
		suffix = "; escalating after " + rs.curAction
	}
	rs.step++
	rs.retried, rs.advance = false, false
	c.recoveryPost(id, h, rs, action, suffix)
}

// recoveryReason is "hung: heartbeat silent 95s, upload silent 40s" plus suffix.
func (c *schedCore) recoveryReason(h *workerHealth, suffix string) string {
	var parts []string
	for _, i := range []int{chHeartbeat, chUpload, chProbe} {
		if h.ch[i] == ChanSilent {
			parts = append(parts, fmt.Sprintf("%s silent %ds", channelNames[i], int(c.now.Sub(h.silentAt[i]).Seconds())))
		}
	}
	reason := "hung: no channel reports ok"
	if len(parts) > 0 {
		reason = "hung: " + strings.Join(parts, ", ")
	}
	return reason + suffix
}

// recoveryPost builds a record with a new nonce and fresh times and emits the POST.
func (c *schedCore) recoveryPost(id core.NodeID, h *workerHealth, rs *recoveryNode, action, suffix string) {
	r := c.cfg.Recovery
	issued := c.now.UnixMilli()
	rec := recoveryRecord{V: 1, TargetNodeID: string(id), Action: action, Reason: c.recoveryReason(h, suffix),
		IssuedAt: issued, ExpiresAt: issued + r.RequestTTL.Milliseconds(), Nonce: c.nonce()}
	body, _ := json.Marshal(rec)
	rs.curAction, rs.curNonce, rs.posting = action, rec.Nonce, true
	rs.stepAt, rs.lastSent = c.now, c.now
	rs.sent = append(rs.sent, rec.Nonce)
	if len(rs.sent) > recoveryNonceKeep {
		rs.sent = rs.sent[len(rs.sent)-recoveryNonceKeep:]
	}
	rs.lastReq = &RecoveryRequestStatus{Action: action, Nonce: rec.Nonce, SentAt: issued, Status: RecoveryPending}
	c.log.Info("recovery request sent", "node", id, "action", action, "nonce", rec.Nonce, "reason", rec.Reason,
		"step", rs.step, "of", len(r.Actions))
	c.changed = true
	c.record(history.TRecoveryRequest, history.Info, "",
		fmt.Sprintf("Recovery request %s sent for %s (step %d of %d): %s.", action, c.nodeName(id), rs.step, len(r.Actions), rec.Reason),
		[]string{c.nodeName(id)}, "", map[string]any{"action": action, "nonce": rec.Nonce, "step": rs.step, "of": len(r.Actions),
			"reason": rec.Reason, "expires_at": rec.ExpiresAt})
	c.emitRecovery(id, "sent")
	c.effects = append(c.effects, effRecoveryPost{id: id, epoch: rs.epoch, action: action, nonce: rec.Nonce,
		url: recoveryURL(c.cfg.Fleet.URL, id), body: body, timeout: r.HTTPTimeout})
}

// recoveryPosted takes the fleet manager's answer to a POST.
func (c *schedCore) recoveryPosted(e evRecoveryPosted) {
	rs := c.recov[e.id]
	if rs == nil {
		return
	}
	current := rs.curNonce == e.nonce
	if current {
		rs.posting = false
	}
	status, retry := RecoveryStored, false
	switch {
	case e.err != nil:
		status, retry = RecoveryError, true
		c.log.Warn("recovery request failed", "node", e.id, "action", e.action, "nonce", e.nonce, "err", e.err)
	case e.status >= 200 && e.status < 300:
		c.log.Info("recovery request stored", "node", e.id, "action", e.action, "nonce", e.nonce, "status", e.status)
	case e.status >= 500:
		status, retry = RecoveryError, true
		c.log.Warn("recovery request failed", "node", e.id, "action", e.action, "nonce", e.nonce, "status", e.status, "body", e.tail)
	default:
		status = RecoveryRefused
		c.log.Warn("recovery request refused", "node", e.id, "action", e.action, "nonce", e.nonce, "status", e.status, "body", e.tail)
	}
	if rs.lastReq != nil && rs.lastReq.Nonce == e.nonce {
		rs.lastReq.Status, rs.lastReq.HTTPStatus = status, e.status
	}
	sev := history.Info
	if status != RecoveryStored {
		sev = history.Warn
	}
	name := c.nodeName(e.id)
	resp := map[string]any{"action": e.action, "nonce": e.nonce, "outcome": status, "http_status": e.status}
	text := fmt.Sprintf("The fleet manager answered the %s request for %s with HTTP %d (%s).", e.action, name, e.status, status)
	if e.err != nil {
		resp["err"] = e.err.Error()
		text = fmt.Sprintf("The %s request for %s got no answer from the fleet manager: %v.", e.action, name, e.err)
	} else if e.tail != "" {
		resp["body"] = e.tail
	}
	c.record(history.TRecoveryResponse, sev, "", text, []string{name}, "", resp)
	if status == RecoveryRefused && current && rs.active && rs.epoch == e.epoch {
		// A refused request ends the escalation for this hung episode: the
		// next step (a reboot, say) must not follow a restart that was refused.
		rs.stopped, rs.retryAt = true, timeZero
		c.log.Warn("recovery sequence stopped", "node", e.id, "action", e.action, "status", e.status,
			"reason", "the fleet manager refused the request; no further steps this episode", "body", e.tail)
		c.record(history.TRecoveryStopped, history.Warn, "",
			fmt.Sprintf("The recovery sequence for %s stopped: the %s request was refused with HTTP %d, so no further step is sent this episode.",
				name, e.action, e.status), []string{name}, "",
			map[string]any{"reason": "request refused", "action": e.action, "http_status": e.status, "body": e.tail})
	}
	c.changed = true
	if retry && current && rs.active && rs.epoch == e.epoch && !rs.retried {
		rs.retryAt = c.now.Add(recoveryRetryDelay)
		c.log.Info("recovery request will be retried once", "node", e.id, "action", e.action, "in", recoveryRetryDelay)
	}
}

// recoveryOutcomes reads last_recovery_action of every remote node: a new one
// is logged, kept for status and sent as an event. A failed outcome for the
// request in flight lets the next step go as soon as min_interval allows.
func (c *schedCore) recoveryOutcomes() {
	ids := make([]core.NodeID, 0, len(c.raw))
	for id := range c.raw {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		n := c.raw[id]
		if n.Local || !n.Recovery.Present || c.excluded(n) {
			continue
		}
		o := n.Recovery
		key := fmt.Sprintf("%s@%d", o.Nonce, o.At)
		rs := c.recov[id]
		if rs != nil && rs.outKey == key {
			continue
		}
		if rs == nil {
			rs = &recoveryNode{}
			c.recov[id] = rs
		}
		rs.outKey = key
		out := o
		rs.lastOut = &out
		c.log.Info("recovery outcome", "node", id, "action", o.Action, "outcome", o.Outcome, "nonce", o.Nonce,
			"reason", o.Reason, "duration_ms", o.DurationMs, "ours", slices.Contains(rs.sent, o.Nonce))
		osev := history.Info
		if o.Outcome == "failed" {
			osev = history.Warn
		}
		c.record(history.TRecoveryOutcome, osev, "",
			fmt.Sprintf("Node %s reported %s of %s after %d ms.", c.nodeName(id), o.Outcome, o.Action, o.DurationMs),
			[]string{c.nodeName(id)}, "", map[string]any{"action": o.Action, "outcome": o.Outcome, "nonce": o.Nonce, "reason": o.Reason,
				"duration_ms": o.DurationMs, "ours": slices.Contains(rs.sent, o.Nonce), "at": o.At})
		if rs.active && o.Nonce == rs.curNonce && o.Outcome == "failed" {
			rs.advance = true
		}
		c.changed = true
		c.emitRecovery(id, "outcome")
	}
}

func (c *schedCore) emitRecovery(id core.NodeID, phase string) {
	st := c.recoveryStatus(c.recov[id])
	c.emit(FeedRecovery, "", func(e *FeedEvent) {
		e.Recovery = &RecoveryEvent{Node: id, Phase: phase, Recovery: st}
	})
}

// recoveryStatus is status.recovery of a node; nil when nothing was sent or reported.
func (c *schedCore) recoveryStatus(rs *recoveryNode) *RecoveryStatus {
	if rs == nil || (rs.lastReq == nil && rs.lastOut == nil) {
		return nil
	}
	st := &RecoveryStatus{Step: rs.step}
	if rs.lastReq != nil {
		r := *rs.lastReq
		st.LastRequest = &r
	}
	if o := rs.lastOut; o != nil {
		st.LastOutcome = &RecoveryOutcomeStatus{Action: o.Action, At: o.At, Outcome: o.Outcome, Reason: o.Reason,
			Nonce: o.Nonce, DurationMs: o.DurationMs}
	}
	if rs.active {
		ms := rs.started.UnixMilli()
		st.EpisodeStarted = &ms
	}
	return st
}

// recoveryDeadlines adds the times at which a recovery step may send something.
func (c *schedCore) recoveryDeadlines(add func(time.Time)) {
	if !c.recoveryOn() {
		return
	}
	for _, rs := range c.recov {
		if !rs.active || rs.posting || rs.stopped {
			continue
		}
		if !rs.retryAt.IsZero() {
			add(rs.retryAt)
		} else if at, ok := c.recoveryNextAt(rs); ok {
			add(at)
		} else {
			add(rs.stepAt.Add(c.cfg.Recovery.StepWait))
		}
	}
}

func (c *schedCore) nonce() string {
	if c.nonceFn != nil {
		return c.nonceFn()
	}
	return randomNonce()
}
