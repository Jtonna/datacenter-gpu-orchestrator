// DEV-ONLY MOCK of a server contract that is not built on this branch: the
// audit log behind GET /api/audit and GET /api/audit/{id}, shaped like
// audit.Entry and the handlers on the bench branch. In memory only.

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type auditEntry struct {
	ID         int64       `json:"id"`
	Ts         int64       `json:"ts"` // unix ms
	Source     string      `json:"source"`
	Actor      string      `json:"actor"`
	RemoteAddr string      `json:"remote_addr"`
	UserAgent  string      `json:"user_agent"`
	Action     string      `json:"action"`
	Section    string      `json:"section"`
	Target     string      `json:"target"`
	Before     any         `json:"before"`
	After      any         `json:"after"`
	Diff       []diffEntry `json:"diff"`
	Revision   int         `json:"revision"` // 0 when no configuration revision was created
	Comment    string      `json:"comment"`
}

type auditLog struct {
	mu      sync.Mutex
	entries []auditEntry // oldest first
	next    int64
}

func (a *auditLog) add(e auditEntry) auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	e.ID = a.next
	if e.Diff == nil {
		e.Diff = []diffEntry{}
	}
	e.Before, e.After = clone(e.Before), clone(e.After)
	a.entries = append(a.entries, e)
	return e
}

func (a *auditLog) reset() {
	a.mu.Lock()
	a.entries, a.next = nil, 0
	a.mu.Unlock()
}

// renumber sorts by time and gives ids 1..n (seeding only).
func (a *auditLog) renumber() {
	a.mu.Lock()
	defer a.mu.Unlock()
	sort.SliceStable(a.entries, func(i, j int) bool { return a.entries[i].Ts < a.entries[j].Ts })
	for i := range a.entries {
		a.entries[i].ID = int64(i + 1)
	}
	a.next = int64(len(a.entries))
}

// timeParam accepts unix milliseconds or an RFC 3339 time.
func timeParam(v string) (int64, bool) {
	if v == "" {
		return 0, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func (a *auditLog) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, ok1 := timeParam(q.Get("since"))
	until, ok2 := timeParam(q.Get("until"))
	if !ok1 || !ok2 {
		apiBad(w, "since and until are unix milliseconds or RFC 3339 times")
		return
	}
	limit := qInt(r, "limit", 100)
	if limit < 1 {
		limit = 100
	}
	asc := strings.EqualFold(q.Get("order"), "asc")
	text := strings.ToLower(q.Get("q"))
	a.mu.Lock()
	all := append([]auditEntry(nil), a.entries...)
	a.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool {
		if asc {
			return all[i].ID < all[j].ID
		}
		return all[i].ID > all[j].ID
	})
	out := []auditEntry{}
	total := 0
	for _, e := range all {
		if since > 0 && e.Ts < since || until > 0 && e.Ts > until {
			continue
		}
		if v := q.Get("source"); v != "" && e.Source != v {
			continue
		}
		if v := q.Get("actor"); v != "" && e.Actor != v {
			continue
		}
		if v := q.Get("section"); v != "" && e.Section != v {
			continue
		}
		if v := q.Get("target"); v != "" && e.Target != v {
			continue
		}
		if v := q.Get("action"); v != "" && e.Action != v {
			continue
		}
		if text != "" {
			b, _ := json.Marshal(e)
			if !strings.Contains(strings.ToLower(string(b)), text) {
				continue
			}
		}
		total++
		if len(out) < limit {
			out = append(out, e)
		}
	}
	writeJSON(w, 200, map[string]any{"entries": out, "total_matching": total})
}

func (a *auditLog) get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiBad(w, "the audit id must be a number")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.ID == id {
			writeJSON(w, 200, e)
			return
		}
	}
	writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "no audit entry " + strconv.FormatInt(id, 10), "type": "invalid_request_error"}})
}
