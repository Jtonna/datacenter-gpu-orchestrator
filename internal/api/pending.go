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
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
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
// group. They expire after ttl_min (5 to 60, default 60: at most an hour). The groups
// topic on /ws carries them (pending_zones) and is sent when one changes.

// Pending zone states.
const (
	PendingWaiting   = "waiting"
	PendingJoined    = "joined"
	PendingExpired   = "expired"
	PendingCancelled = "cancelled"
)

// pendingStateText is State in words.
var pendingStateText = map[string]string{
	PendingWaiting:   "Waiting for the machine to check in",
	PendingJoined:    "Joined",
	PendingExpired:   "Expired: the machine did not check in in time",
	PendingCancelled: "Cancelled",
}

func (z *PendingZone) setState(st string) { z.State, z.StateText = st, pendingStateText[st] }

// PendingZone is one zone made before its machine joined.
type PendingZone struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	ModelSource string `json:"model_source"`
	Mode        string `json:"mode"` // bare or docker
	State       string `json:"state"`
	// StateText is the state in words for the page: "Waiting for the
	// machine to check in" while waiting; a waiting zone is never down or
	// lost and is counted nowhere (not a pool node).
	StateText  string `json:"state_text"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	JoinedNode string `json:"joined_node,omitempty"`
	JoinedAt   string `json:"joined_at,omitempty"`
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
	modelSourceRE = config.ModelSourceRE
	hostRE        = regexp.MustCompile(`^[A-Za-z0-9.\-]+:[0-9]{1,5}$|^\[[0-9A-Fa-f:.]+\]:[0-9]{1,5}$`)
)

const (
	pendingDefaultTTL = time.Hour
	pendingMaxTTL     = time.Hour
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
			z.setState(PendingExpired)
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
			z.setState(PendingJoined)
			z.JoinedNode, z.JoinedAt = id, isoT(now)
			return *z, true
		}
	}
	return PendingZone{}, false
}

// add keeps z unless a waiting pending zone has its name.
func (p *pendingZones) add(z *PendingZone, now time.Time) (PendingZone, error) {
	p.mu.Lock()
	defer supervise.Unlocked("pending", p.mu.Unlock)
	for _, o := range p.byID {
		if o.State == PendingWaiting && now.Before(o.expires) && strings.EqualFold(o.Name, z.Name) {
			return PendingZone{}, fmt.Errorf("a pending zone named %s is already waiting (%s)", o.Name, o.ID)
		}
	}
	p.byID[z.ID] = z
	return *z, nil
}

// cancel marks a waiting pending zone cancelled; ok is false when there is
// no such id, err set when it is not waiting.
func (p *pendingZones) cancel(id string) (PendingZone, bool, error) {
	p.mu.Lock()
	defer supervise.Unlocked("pending", p.mu.Unlock)
	z, ok := p.byID[id]
	switch {
	case !ok:
		return PendingZone{}, false, errors.New("no pending zone " + id)
	case z.State != PendingWaiting:
		return PendingZone{}, true, fmt.Errorf("pending zone %s is %s, not waiting", id, z.State)
	}
	z.setState(PendingCancelled)
	return *z, true, nil
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

// NoModelSource is the refusal when neither the request nor the setting
// default_model_source gives a model source.
const NoModelSource = "no model source: set the default model source for new machines or give one for this zone"

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
	if s.opts.Pool != nil {
		if id, taken := s.opts.Pool.TakenBy(b.Name); taken {
			apiErr(w, http.StatusConflict, "the name %s is already taken by the zone %s (active, lost or removed): choose another name", b.Name, id)
			return
		}
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
		if c := s.conf(); c != nil {
			b.ModelSource = c.DefaultModelSource
		}
	}
	if b.ModelSource == "" {
		apiErr(w, http.StatusBadRequest, "%s", NoModelSource)
		return
	}
	if !modelSourceRE.MatchString(b.ModelSource) {
		apiErr(w, http.StatusBadRequest, "model_source is share:<host>:/<path>, dir:/<path> or push, got %q", b.ModelSource)
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
	z := &PendingZone{ID: newPendingID(), Name: b.Name, Group: b.Group, ModelSource: b.ModelSource, Mode: b.Mode, State: PendingWaiting, StateText: pendingStateText[PendingWaiting],
		created: now, expires: now.Add(ttl), host: host}
	z.CreatedAt, z.ExpiresAt = isoT(z.created), isoT(z.expires)
	z.ScriptPath = "/install.sh?zone=" + z.ID
	z.DownloadPath = z.ScriptPath + "&download=1"
	z.DownloadName = "potluck-zone-" + z.Name + ".sh"
	z.Command = installCommand(*z, host)
	out, err := s.pending.add(z, now)
	if err != nil {
		apiErr(w, http.StatusConflict, "%v", err)
		return
	}
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
	def := "" // the setting default_model_source, for the Create dialog ("": it must ask)
	if c := s.conf(); c != nil {
		def = c.DefaultModelSource
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": list, "default_model_source": def})
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
	out, ok, err := s.pending.cancel(id)
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
		scriptRefusal(w, http.StatusNotFound, PendingNotFound)
		return
	}
	if z.State != PendingWaiting {
		said := "this zone was " + z.State + ": create it again in Potluck"
		if z.State == PendingJoined {
			said = "this zone has already joined Potluck as " + z.JoinedNode + ": nothing to do"
		}
		scriptRefusal(w, http.StatusGone, said)
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

// PendingNotFound is what the script route says, and the page shows, for a
// zone id Potluck does not know: never made, or made before a restart
// (pending zones live in memory only).
const PendingNotFound = "this zone was not found: create it again in Potluck"

// scriptRefusal answers the script route with a script that prints said
// and exits 1, so `curl -sSL ... | sudo sh` shows the sentence to the person
// running it. The status stays 404 or 410 for anything that checks it.
// said holds no single quote (it is built from fixed words, a state and a
// node id).
func scriptRefusal(w http.ResponseWriter, code int, said string) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte("#!/bin/sh\necho 'potluck: " + strings.ReplaceAll(said, "'", "") + "' >&2\nexit 1\n"))
}

// installScriptText is the install script of a pending zone. Every value
// in it passed the checks above (no quote, space, '$' or backslash).
func installScriptText(z PendingZone, host string) string {
	group := "none"
	if z.Group != "" {
		group = z.Group
	}
	addr := host
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.HasSuffix(addr, "]") {
		addr = addr[:i]
	}
	addr = strings.Trim(addr, "[]")
	what := "  - install /usr/local/bin/potluck-agent and the systemd unit potluck-agent.service, then start (or restart) only that unit\n"
	if z.Mode == "docker" {
		what = "  - start the potluck-agent container (an existing one of that name is replaced); with a share: add a read-only mount unit for it\n" +
			"    and a drop-in so Docker starts after it (Docker itself is not restarted)\n"
	}
	return "#!/bin/sh\n" +
		"# Potluck: install this machine as zone " + z.Name + " (group " + group + ", pending zone " + z.ID + ").\n" +
		"# Made by Potluck at " + host + "; valid until " + z.ExpiresAt + ". Run as root.\n" +
		"# It prints what it will do first; --dry-run prints that and stops.\n" +
		"# Extra arguments are passed to potluck-agent-setup (for example --allow-from or --no-firewall).\n" +
		"# It refuses to run on a console (consoles are set up by the fleet console)\n" +
		"# and on the machine that runs the Potluck scheduler.\n" +
		"set -eu\n" +
		"PATH=${POTLUCK_INSTALL_TEST_PATH:+$POTLUCK_INSTALL_TEST_PATH:}$PATH\n" +
		"root=${POTLUCK_INSTALL_TEST_ROOT:-}\n" +
		"potluck='" + host + "'\n" +
		"potluck_addr='" + addr + "'\n" +
		"refuse() { echo \"potluck: $*\" >&2; exit 1; }\n" +
		"# Arguments: --dry-run prints the plan and stops; the rest go to potluck-agent-setup.\n" +
		"dry=no\n" +
		"override=no\n" +
		"n=$#\n" +
		"while [ \"$n\" -gt 0 ]; do\n" +
		"\ta=$1\n" +
		"\tshift\n" +
		"\tn=$((n - 1))\n" +
		"\tcase $a in\n" +
		"\t--dry-run) dry=yes ;;\n" +
		"\t" + consoleOverrideFlag + ") override=yes ;;\n" +
		"\t*) set -- \"$@\" \"$a\" ;;\n" +
		"\tesac\n" +
		"done\n" +
		"# A console. Any one marker refuses: the fleet console's payload version file or its units,\n" +
		"# the starter settings, a root file system in memory, or the console GPU.\n" +
		"console=''\n" +
		"if [ -e \"$root/etc/node-payload-version\" ]; then console='it has the fleet console payload version file (/etc/node-payload-version)'; fi\n" +
		"for unit in ps5-bootstrap.timer node-agent.service; do\n" +
		"\tfor d in /etc/systemd/system /run/systemd/system /run/systemd/transient /lib/systemd/system /usr/lib/systemd/system; do\n" +
		"\t\tif [ -z \"$console\" ] && [ -e \"$root$d/$unit\" ]; then console=\"it has the fleet console unit $unit ($d)\"; fi\n" +
		"\tdone\n" +
		"done\n" +
		"if [ -z \"$console\" ] && [ -e \"$root/etc/potluck/starter.env\" ]; then console='it has the console starter settings (/etc/potluck/starter.env)'; fi\n" +
		"if [ -z \"$console\" ] && [ -r \"$root/proc/mounts\" ]; then\n" +
		"\trootfs=''\n" +
		"\twhile read -r _dev mnt fs _rest; do if [ \"$mnt\" = / ]; then rootfs=$fs; fi; done <\"$root/proc/mounts\"\n" +
		"\tcase $rootfs in tmpfs|ramfs) console=\"its root file system is in memory ($rootfs)\" ;; esac\n" +
		"fi\n" +
		"if [ -z \"$console\" ] && command -v lspci >/dev/null 2>&1 && lspci 2>/dev/null | grep -qi oberon; then\n" +
		"\tconsole='it has the console GPU (Oberon)'\n" +
		"fi\n" +
		"# In doubt, refuse: a host name starting with ps5- (unless the override is given).\n" +
		"name=$(cat \"$root/proc/sys/kernel/hostname\" 2>/dev/null || uname -n 2>/dev/null || echo '')\n" +
		"if [ -z \"$console\" ] && [ $override = no ]; then\n" +
		"\tcase $name in ps5-*) console=\"its host name is $name\" ;; esac\n" +
		"fi\n" +
		"[ -z \"$console\" ] || refuse \"this machine looks like a console: $console. Consoles are set up by the fleet console, not by this script; nothing was changed.\"\n" +
		"# The machine that runs the scheduler: Potluck's address is one of its own.\n" +
		"case $potluck_addr in\n" +
		"\tlocalhost|127.*|::1) refuse \"this machine runs the Potluck scheduler (Potluck is at $potluck); install the agent on another machine. Nothing was changed.\" ;;\n" +
		"esac\n" +
		"targets=$potluck_addr\n" +
		"case $potluck_addr in\n" +
		"\t*[!0-9.]*) case $potluck_addr in *:*) ;; *) targets=$(getent hosts \"$potluck_addr\" 2>/dev/null | while read -r a _n; do echo \"$a\"; done) || targets='' ;; esac ;;\n" +
		"esac\n" +
		"mine=''\n" +
		"if command -v ip >/dev/null 2>&1; then\n" +
		"\tmine=$(ip -o addr show 2>/dev/null | sed -n 's/.* inet6\\{0,1\\} \\([^/ ]*\\).*/\\1/p') || mine=''\n" +
		"fi\n" +
		"for t in $targets; do for a in $mine; do\n" +
		"\tif [ \"$t\" = \"$a\" ]; then refuse \"this machine runs the Potluck scheduler (it has Potluck's address $t); install the agent on another machine. Nothing was changed.\"; fi\n" +
		"done; done\n" +
		"echo 'potluck: this will, on this machine:'\n" +
		"echo \"  - download potluck-agent-setup from http://$potluck/install/potluck-agent-setup into a temporary file\"\n" +
		"echo \"  - run it: potluck-agent-setup --" + z.Mode + " --potluck $potluck --model-source " + z.ModelSource + " --name " + z.Name + " $*\"\n" +
		"printf '%s' '" + strings.ReplaceAll(what, "'", "") + "'\n" +
		"echo '  - when ufw is active, allow TCP 50060, 50052 and 50061 from Potluck (comment potluck-agent); --no-firewall skips this'\n" +
		"echo '  - nothing is rebooted and no other service is restarted; running it again gives the same result'\n" +
		"[ $dry = no ] || exit 0\n" +
		"command -v curl >/dev/null 2>&1 || { echo 'potluck: curl is required' >&2; exit 1; }\n" +
		"setup=$(mktemp)\n" +
		"trap 'rm -f \"$setup\"' EXIT\n" +
		"curl -fsS --connect-timeout 5 --max-time 60 -o \"$setup\" \"http://$potluck/install/potluck-agent-setup\"\n" +
		"sh \"$setup\" --" + z.Mode + " --potluck \"$potluck\" --model-source '" + z.ModelSource + "' --name '" + z.Name + "' \"$@\"\n"
}

// installCommand is the line the Create dialog shows. It never carries
// consoleOverrideFlag.
func installCommand(z PendingZone, host string) string {
	// No -f: a zone Potluck no longer has answers 404 or 410 with a script
	// that says so (scriptRefusal), which sh then prints.
	return fmt.Sprintf("curl -sSL 'http://%s/install.sh?zone=%s' | sudo sh", host, z.ID)
}

// consoleOverrideFlag lets the script run on a machine whose host name
// starts with ps5- but that carries none of the console markers. Named in
// the runbook only; the dialog never prints it, and it does not override a
// marker.
const consoleOverrideFlag = "--not-a-console"

func (s *Server) installSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(agentdeploy.Setup)))
	_, _ = w.Write(agentdeploy.Setup)
}
