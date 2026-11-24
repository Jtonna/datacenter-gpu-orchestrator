package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Expiry warnings (07 P5): once an hour, each key that is not revoked and
// has an expiry gets a history event agentKeyExpiring once when 7 days or
// fewer are left and once when 1 day or fewer is left, and agentKeyExpired
// once when it has expired. What was sent is kept with the key (warned), so
// a restart does not send it again; a new expiry clears it.
const keyExpiryEvery = time.Hour

func (s *Server) startKeyExpiry() {
	if s.store == nil || s.opts.keyExpiryEvery < 0 { // < 0: off (tests)
		return
	}
	every := orDur(s.opts.keyExpiryEvery, keyExpiryEvery)
	s.bgWG.Add(1)
	supervise.Go(s.bg, supervise.Spec{Name: "api.keyexpiry", Policy: supervise.Restart, Done: s.bgWG.Done}, func(ctx context.Context) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.checkKeyExpiry(ctx, s.opts.Now())
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// dueWarning is the warning a key is due at now, and the marks it leaves:
// "" when none is due.
func dueWarning(a *cfgstore.Agent, now time.Time) (mark string, all []string) {
	if a.RevokedAt > 0 || a.Access == nil || a.Access.ExpiresAt == 0 {
		return "", nil
	}
	sent := map[string]bool{}
	for _, w := range strings.Split(a.Access.Warned, ",") {
		sent[w] = true
	}
	left := time.UnixMilli(a.Access.ExpiresAt).Sub(now)
	var due []string // the marks reached, the strongest last
	if left <= KeyExpiringSoon {
		due = append(due, expiryWarnFirst)
	}
	if left <= 24*time.Hour {
		due = append(due, expiryWarnSecond)
	}
	if left <= 0 {
		due = append(due, expiryWarnGone)
	}
	if len(due) == 0 || sent[due[len(due)-1]] {
		return "", nil
	}
	// one event: the strongest reached; the weaker ones count as sent
	return due[len(due)-1], due
}

// checkKeyExpiry records the expiry events that are due.
func (s *Server) checkKeyExpiry(ctx context.Context, now time.Time) {
	list, err := s.agentsNow(ctx)
	if err != nil {
		return
	}
	changed := false
	for i := range list {
		a := &list[i]
		mark, all := dueWarning(a, now)
		if mark == "" {
			continue
		}
		if err := s.store.MarkAgentWarned(a.ID, strings.Join(all, ",")); err != nil {
			continue // try again next time rather than send it twice
		}
		changed = true
		s.hist.Record(keyExpiryEvent(a, mark, now))
	}
	if changed {
		s.agents.reset()
	}
	// P6: keep UsageKeepDays of per-key usage
	today := now.UnixMilli() - now.UnixMilli()%dayMS
	s.store.TrimAgentUsage(today - (UsageKeepDays-1)*dayMS)
}

// keyExpiryEvent names the key by its agent and hint, never by the key.
func keyExpiryEvent(a *cfgstore.Agent, mark string, now time.Time) history.Event {
	exp := time.UnixMilli(a.Access.ExpiresAt).UTC()
	data := map[string]any{"agent_id": a.ID, "name": a.Name, "key_hint": keyHint(a), "expires_at": a.Access.ExpiresAt}
	ev := history.Event{Ts: now.UnixMilli(), Type: history.TAgentKeyExpiring, Severity: history.Warn, Nodes: []string{}, Data: data}
	if mark == expiryWarnGone {
		ev.Type = history.TAgentKeyExpired
		ev.Summary = fmt.Sprintf("The access key of %s expired on %s. Requests with it are refused; extend or rotate it on the MCP page.", a.Name, exp.Format("2 Jan 2006"))
		return ev
	}
	_, days := keyStatus(a, now)
	data["days_left"] = days
	ev.Summary = fmt.Sprintf("The access key of %s expires in %s (%s). Extend it on the MCP page if the agent still needs it.", a.Name, plural(days, "day", "days"), exp.Format("2 Jan 2006"))
	return ev
}
