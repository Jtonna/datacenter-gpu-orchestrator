package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Zone facts on each node entry of GET /api/nodes and of /api/status's
// agents (pool.ZoneFacts): the node's role and share in the instance on it,
// why it is set aside, who paused or excluded it and when, and when a
// claimed or settling node is expected to be free. Who and when come from
// the audit log (no new stored state): the newest pause of the node, and
// the newest configuration change that added its exclude_nodes entry.

// actorTTL is how long one audit lookup is reused. A pause or unpause by
// this server forgets the node's at once.
const actorTTL = time.Minute

// auditLookback is how many exclude_nodes changes are searched for the one
// that added an entry.
const auditLookback = 200

type actorFact struct {
	at    int64 // unix ms; 0 when the audit log has none
	actor string
	read  time.Time
}

// actorCache keeps the audit lookups of the node views.
type actorCache struct {
	mu sync.Mutex
	m  map[string]actorFact
}

func (c *actorCache) get(key string, now time.Time) (actorFact, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.m[key]
	return f, ok && now.Sub(f.read) < actorTTL
}

func (c *actorCache) put(key string, f actorFact) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]actorFact{}
	}
	c.m[key] = f
}

func (c *actorCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// zoneActors is the process's cache; keys carry the server, then the node
// id or the entry.
var zoneActors actorCache

func (s *Server) pauseKey(id string) string      { return fmt.Sprintf("%p|pause|%s", s, id) }
func (s *Server) excludeKey(entry string) string { return fmt.Sprintf("%p|exclude|%s", s, entry) }

// forgetPauseActor drops the cached pause lookup of a node (nodes.go,
// after a pause or unpause).
func (s *Server) forgetPauseActor(id string) { zoneActors.forget(s.pauseKey(id)) }

// auditLookup answers one cached audit lookup, asking find on a miss.
func (s *Server) auditLookup(key string, find func(ctx context.Context, st AuditStore) actorFact) actorFact {
	now := s.opts.Now()
	if f, ok := zoneActors.get(key, now); ok {
		return f
	}
	if s.opts.Audit == nil {
		return actorFact{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	f := find(ctx, s.opts.Audit)
	f.read = now
	zoneActors.put(key, f)
	return f
}

// pausedBy is the newest pause of node id in the audit log.
func (s *Server) pausedBy(id string) actorFact {
	return s.auditLookup(s.pauseKey(id), func(ctx context.Context, st AuditStore) actorFact {
		es, _, err := st.Query(ctx, cfgstore.Filter{Section: SectionNodes, Target: "node:" + id, Action: "pause", Limit: 1})
		if err != nil || len(es) == 0 {
			return actorFact{}
		}
		return actorFact{at: es[0].TS, actor: es[0].Actor}
	})
}

// excludedBy is the newest configuration change that added entry to
// exclude_nodes (an entry may match by pattern: the change that added the
// matching entry is the one reported).
func (s *Server) excludedBy(entry string) actorFact {
	return s.auditLookup(s.excludeKey(entry), func(ctx context.Context, st AuditStore) actorFact {
		es, _, err := st.Query(ctx, cfgstore.Filter{Section: "exclude_nodes", Limit: auditLookback})
		if err != nil {
			return actorFact{}
		}
		for _, e := range es { // newest first
			if listHas(e.After, entry) && !listHas(e.Before, entry) {
				return actorFact{at: e.TS, actor: e.Actor}
			}
		}
		return actorFact{}
	})
}

func listHas(raw json.RawMessage, entry string) bool {
	var l []string
	if json.Unmarshal(raw, &l) != nil {
		return false
	}
	return slices.Contains(l, entry)
}

// zoneFactsOf builds the zone facts of node id from the status (nil when
// it could not be read) and the node's paused and excluded facts.
func (s *Server) zoneFactsOf(id string, st *scheduler.Status, paused bool, excludedBy string) pool.ZoneFacts {
	var z pool.ZoneFacts
	if paused {
		if f := s.pausedBy(id); f.at > 0 {
			z.PausedAt, z.PausedActor = isoTime(time.UnixMilli(f.at)), f.actor
		}
	}
	if excludedBy != "" {
		if f := s.excludedBy(excludedBy); f.at > 0 {
			z.ExcludedAt, z.ExcludedActor = isoTime(time.UnixMilli(f.at)), f.actor
		}
	}
	if st == nil {
		return z
	}
	for _, in := range st.Instances {
		for _, m := range in.Nodes {
			if string(m.ID) == id && m.Role != "" {
				z.Member = &pool.MemberFacts{Profile: in.Profile, State: in.State, Role: m.Role, Share: m.Share, Host: in.Host, HostNode: string(in.HostNode)}
			}
		}
	}
	for _, q := range st.Quarantines {
		if string(q.Node) == id {
			z.SetAside = &pool.SetAsideFacts{Reason: q.Reason, Message: q.Message, Profile: q.Profile, Until: isoTime(q.Until), Level: q.Level}
			if !q.Since.IsZero() {
				z.SetAside.Since = isoTime(q.Since)
			}
		}
	}
	for _, n := range st.View.Nodes {
		if string(n.ID) != id || n.FreeBasis == "" {
			continue
		}
		z.FreeBasis, z.Next = n.FreeBasis, n.ClaimedBy
		if !n.FreeAt.IsZero() {
			z.FreeAt = isoTime(n.FreeAt)
			left := max(0, n.FreeAt.Sub(st.Time).Seconds())
			z.FreeInS = &left
		}
	}
	return z
}

// zoneFacts fills the zone facts of every agent node.
func (s *Server) zoneFacts(facts []pool.NodeFacts, st *scheduler.Status) {
	for i := range facts {
		f := &facts[i]
		f.ZoneFacts = s.zoneFactsOf(f.ID, st, f.Paused, f.ExcludedBy)
	}
}
