package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	agentdeploy "github.com/Jtonna/datacenter-gpu-orchestrator/deploy/agent"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/pool"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Create new zone (Zones-Create): a pending zone is a name (and a group)
// chosen before the machine has an agent. Potluck serves an install
// script with its own address, the name and the model source filled in;
// the first agent that registers with that name after the pending zone was
// made joins it: it is put in the group and the pending zone reads joined.
//
//	POST   /api/zones/pending        {name, group?, model_source?, mode?, ttl_min?, comment?}
//	GET    /api/zones/pending        every pending zone (waiting, joined, expired, cancelled), newest first
//	GET    /api/zones/pending/{id}
//	DELETE /api/zones/pending/{id}   cancel a waiting one
//	GET    /install.sh?zone=<id>     the install script (&download=1: as a file)
//	GET    /install/potluck-agent-setup   the setup script the install script runs
//
// Pending zones are kept in memory (no schema change): a restart of
// Potluck forgets them; the machine still joins with its name, without the
// group. They expire after ttl_min (default 60, at most 1440). The groups
// topic on /ws carries them (pending_zones) and is sent when one changes.

// Pending zone states.
const (
	PendingWaiting   = "waiting"
	PendingJoined    = "joined"
	PendingExpired   = "expired"
	PendingCancelled = "cancelled"
)

// PendingZone is one zone made before its machine joined.
type PendingZone struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	ModelSource string `json:"model_source"`
	Mode        string `json:"mode"` // bare or docker
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
	JoinedNode  string `json:"joined_node,omitempty"`
	JoinedAt    string `json:"joined_at,omitempty"`
	// Command is the line to run on the machine; ScriptPath and
	// DownloadPath are on Potluck's address (the one the request used).
	Command      string `json:"command"`
	ScriptPath   string `json:"script_path"`
	DownloadPath string `json:"download_path"`
	DownloadName string `json:"download_name"`

	created, expires time.Time
	host             string // Potluck's address as the creator reached it
}

type pendingZones struct {
	mu   sync.Mutex
	byID map[string]*PendingZone
	keep int // finished ones kept for the list
}

var (
	zoneNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	modelSourceRE = regexp.MustCompile(`^(push|share:[^\s'"\\$%:]+:/[^\s'"\\$%]*|dir:/[^\s'"\\$%]*)$`)
	hostRE        = regexp.MustCompile(`^[A-Za-z0-9.\-]+:[0-9]{1,5}$|^\[[0-9A-Fa-f:.]+\]:[0-9]{1,5}$`)
)

const (
	pendingDefaultTTL = time.Hour
	pendingMaxTTL     = 24 * time.Hour
	pendingKeep       = 50
)

func newPendingZones() *pendingZones {
	return &pendingZones{byID: map[string]*PendingZone{}, keep: pendingKeep}
}

func isoT(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// expireLocked marks the waiting ones past their time and drops the
// oldest finished ones beyond keep; it says whether anything changed.
func (p *pendingZones) expireLocked(now time.Time) bool {
	changed := false
	var done []*PendingZone
	for _, z := range p.byID {
		if z.State == PendingWaiting && !now.Before(z.expires) {
			z.State = PendingExpired
			changed = true
		}
		if z.State != PendingWaiting {
			done = append(done, z)
		}
	}
	if len(done) > p.keep {
		sort.Slice(done, func(i, j int) bool { return done[i].created.After(done[j].created) })
		for _, z := range done[p.keep:] {
			delete(p.byID, z.ID)
		}
	}
	return changed
}

func (p *pendingZones) list(now time.Time) ([]PendingZone, bool) {
	p.mu.Lock()
	defer supervise.Unlocked("pending", p.mu.Unlock)
	changed := p.expireLocked(now)
	out := make([]PendingZone, 0, len(p.byID))
	for _, z := range p.byID {
		out = append(out, *z)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.After(out[j].created)
		}
		return out[i].ID > out[j].ID
	})
	return out, changed
}

// join marks the waiting pending zone of that name joined by node id; it
// returns a copy of it, or false when none waits for the name.
func (p *pendingZones) join(id, name string, now time.Time) (PendingZone, bool) {
	p.mu.Lock()
	defer supervise.Unlocked("pending", p.mu.Unlock)
	p.expireLocked(now)
	for _, z := range p.byID {
		if z.State == PendingWaiting && strings.EqualFold(z.Name, name) {
			z.State, z.JoinedNode, z.JoinedAt = PendingJoined, id, isoT(now)
			return *z, true
		}
	}
	return PendingZone{}, false
}

func newPendingID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "pz-" + hex.EncodeToString(b)
}

// requestHost is Potluck's address as the caller reached it, with a port.
func requestHost(r *http.Request) (string, bool) {
	h := r.Host
	if _, _, err := net.SplitHostPort(h); err != nil {
		h = net.JoinHostPort(strings.Trim(h, "[]"), "80")
	}
	return h, hostRE.MatchString(h)
}

// defaultModelSource is the model source most registered agents use
// (share:<target>, or dir:<path> for a folder); "" when none is known.
func (s *Server) defaultModelSource() string {
	if s.opts.Pool == nil {
		return ""
	}
	count := map[string]int{}
	for _, f := range s.opts.Pool.Facts() {
		ms, _ := f.ModelSource.(pool.FeedModelSource)
		t := ms.Target
		if t == "" || f.State == pool.StateRemoved {
			continue
		}
		src := "share:" + t
		if strings.HasPrefix(t, "/") {
			src = "dir:" + t
		}
		if modelSourceRE.MatchString(src) {
			count[src]++
		}
	}
	best, n := "", 0
	for src, c := range count {
		if c > n || (c == n && src < best) {
			best, n = src, c
		}
	}
	return best
}

func (s *Server) pendingRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/zones/pending", s.pendingCreate)
	m.HandleFunc("GET /api/zones/pending", s.pendingList)
	m.HandleFunc("GET /api/zones/pending/{id}", s.pendingGet)
	m.HandleFunc("DELETE /api/zones/pending/{id}", s.pendingCancel)
	m.HandleFunc("GET /install.sh", s.installScript)
	m.HandleFunc("GET /install/potluck-agent-setup", s.installSetup)
}

func (s *Server) pendingCreate(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var b struct {
		Name        string `json:"name"`
		Group       string `json:"group"`
		ModelSource string `json:"model_source"`
		Mode        string `json:"mode"`
		TTLMin      int    `json:"ttl_min"`
		Comment     string `json:"comment"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		apiErr(w, http.StatusBadRequest, "the body must be a JSON object: %v", err)
		return
	}
	if !zoneNameRE.MatchString(b.Name) {
		apiErr(w, http.StatusBadRequest, "a zone name is 1 to 63 letters, digits, '.', '_' or '-', starting with a letter or digit")
		return
	}
	if b.Group != "" {
		g, ok := s.groups.Get(b.Group)
		if !ok {
			apiErr(w, http.StatusNotFound, "no zone group %q", b.Group)
			return
		}
		b.Group = g.Name
	}
	if b.Mode == "" {
		b.Mode = "bare"
	}
	if b.Mode != "bare" && b.Mode != "docker" {
		apiErr(w, http.StatusBadRequest, "mode is bare or docker")
		return
	}
	if b.ModelSource == "" {
		b.ModelSource = s.defaultModelSource()
	}
	if !modelSourceRE.MatchString(b.ModelSource) {
		apiErr(w, http.StatusBadRequest, "model_source is required (share:<ip>:/<path>, dir:/<path> or push): no registered machine has one to copy")
		return
	}
	ttl := pendingDefaultTTL
	if b.TTLMin != 0 {
		ttl = time.Duration(b.TTLMin) * time.Minute
		if ttl < 5*time.Minute || ttl > pendingMaxTTL {
			apiErr(w, http.StatusBadRequest, "ttl_min is 5 to %d", int(pendingMaxTTL.Minutes()))
			return
		}
	}
	host, ok := requestHost(r)
	if !ok {
		apiErr(w, http.StatusBadRequest, "Potluck's address as reached (%q) cannot be written into a script", r.Host)
		return
	}
	now := s.opts.Now()
	z := &PendingZone{ID: newPendingID(), Name: b.Name, Group: b.Group, ModelSource: b.ModelSource, Mode: b.Mode, State: PendingWaiting,
		created: now, expires: now.Add(ttl), host: host}
	z.CreatedAt, z.ExpiresAt = isoT(z.created), isoT(z.expires)
	z.ScriptPath = "/install.sh?zone=" + z.ID
	z.DownloadPath = z.ScriptPath + "&download=1"
	z.DownloadName = "potluck-zone-" + z.Name + ".sh"
	z.Command = fmt.Sprintf("curl -fsSL 'http://%s%s' | sudo sh", host, z.ScriptPath)
	s.pending.mu.Lock()
	for _, o := range s.pending.byID {
		if o.State == PendingWaiting && now.Before(o.expires) && strings.EqualFold(o.Name, z.Name) {
			s.pending.mu.Unlock()
			apiErr(w, http.StatusConflict, "a pending zone named %s is already waiting (%s)", o.Name, o.ID)
			return
		}
	}
	s.pending.byID[z.ID] = z
	out := *z
	s.pending.mu.Unlock()
	s.auditPending(r, "create_pending", out, strings.TrimSpace(b.Comment))
	s.groupsChanged()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) auditPending(r *http.Request, action string, z PendingZone, comment string) {
	if s.opts.Audit == nil {
		return
	}
	if comment == "" {
		comment = audit.FromRequest(r).Comment
	}
	after, _ := json.Marshal(map[string]any{"id": z.ID, "name": z.Name, "group": z.Group, "model_source": z.ModelSource, "mode": z.Mode,
		"state": z.State, "expires_at": z.ExpiresAt})
	e := audit.Entry{Action: action, Section: SectionGroups, Target: "pending:" + z.ID, After: after, TS: time.Now().UnixMilli(), Comment: comment}
	audit.FromRequest(r).Apply(&e)
	if id, err := s.opts.Audit.Record(r.Context(), e); err == nil {
		s.announce(r, SectionGroups, "pending:"+z.ID, id)
	}
}

func (s *Server) pendingList(w http.ResponseWriter, r *http.Request) {
	list, changed := s.pending.list(s.opts.Now())
	if changed {
		s.groupsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": list})
}

func (s *Server) pendingByID(id string) (PendingZone, bool) {
	list, _ := s.pending.list(s.opts.Now())
	for _, z := range list {
		if z.ID == id {
			return z, true
		}
	}
	return PendingZone{}, false
}

func (s *Server) pendingGet(w http.ResponseWriter, r *http.Request) {
	z, ok := s.pendingByID(r.PathValue("id"))
	if !ok {
		apiErr(w, http.StatusNotFound, "no pending zone %s", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, z)
}

func (s *Server) pendingCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.pending.mu.Lock()
	z, ok := s.pending.byID[id]
	var out PendingZone
	var err error
	switch {
	case !ok:
		err = errors.New("no pending zone " + id)
	case z.State != PendingWaiting:
		err = fmt.Errorf("pending zone %s is %s, not waiting", id, z.State)
	default:
		z.State = PendingCancelled
		out = *z
	}
	s.pending.mu.Unlock()
	if !ok {
		apiErr(w, http.StatusNotFound, "%v", err)
		return
	}
	if err != nil {
		apiErr(w, http.StatusConflict, "%v", err)
		return
	}
	s.auditPending(r, "cancel_pending", out, "")
	s.groupsChanged()
	writeJSON(w, http.StatusOK, out)
}

// pendingJoined is called for an accepted registration: a waiting pending
// zone of that name is joined and the machine put in its group.
func (s *Server) pendingJoined(r *http.Request, id, name string) {
	z, ok := s.pending.join(id, name, s.opts.Now())
	if !ok {
		return
	}
	if z.Group != "" {
		before, _ := s.groups.Get(z.Group)
		if moved, err := s.groups.AddMembers(z.Group, []string{id}); err == nil {
			after, _ := s.groups.Get(z.Group)
			s.auditGroup(r, "add_members", after.Name, "joined as pending zone "+z.ID+" ("+z.Name+")",
				map[string]any{"members": before.Members}, map[string]any{"members": after.Members, "moved_from": moved})
		} else {
			s.log.Warn("pending zone joined but its group could not take it", "pending", z.ID, "group", z.Group, "err", err)
		}
	}
	s.log.Info("pending zone joined", "pending", z.ID, "name", z.Name, "node", id, "group", z.Group)
	s.groupsChanged()
}

// installScript serves the script a machine runs to join as a zone.
func (s *Server) installScript(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("zone")
	z, ok := s.pendingByID(id)
	if !ok {
		apiErr(w, http.StatusNotFound, "no pending zone %q: make one first (POST /api/zones/pending)", id)
		return
	}
	if z.State != PendingWaiting {
		apiErr(w, http.StatusGone, "pending zone %s is %s", z.ID, z.State)
		return
	}
	host, ok := requestHost(r)
	if !ok {
		host = z.host
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+z.DownloadName+`"`)
	}
	_, _ = w.Write([]byte(installScriptText(z, host)))
}

// installScriptText is the install script of a pending zone. Every value
// in it passed the checks above (no quote, space, '$' or backslash).
func installScriptText(z PendingZone, host string) string {
	group := "none"
	if z.Group != "" {
		group = z.Group
	}
	return "#!/bin/sh\n" +
		"# Potluck: install this machine as zone " + z.Name + " (group " + group + ", pending zone " + z.ID + ").\n" +
		"# Made by Potluck at " + host + "; valid until " + z.ExpiresAt + ". Run as root.\n" +
		"# Extra arguments are passed to potluck-agent-setup (for example --allow-from or --no-firewall).\n" +
		"set -eu\n" +
		"command -v curl >/dev/null 2>&1 || { echo 'potluck: curl is required' >&2; exit 1; }\n" +
		"setup=$(mktemp)\n" +
		"trap 'rm -f \"$setup\"' EXIT\n" +
		"curl -fsS --connect-timeout 5 --max-time 60 -o \"$setup\" 'http://" + host + "/install/potluck-agent-setup'\n" +
		"sh \"$setup\" --" + z.Mode + " --potluck '" + host + "' --model-source '" + z.ModelSource + "' --name '" + z.Name + "' \"$@\"\n"
}

func (s *Server) installSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(agentdeploy.Setup)))
	_, _ = w.Write(agentdeploy.Setup)
}
