// Package fakefleet is an in-process fake of the fleet console backend
// (branch fleet-scheduler-fields of ps5-launcher-thin-image), for tests.
//
// It serves, in the real wire format (contract/fleet.schema.json):
//
//	GET /api/fleet          the fleet document: {v, updated, count, online_count,
//	                        total_*, members:[...]}
//	GET /api/nodes/:id      one member object, or 404 {"error":"unknown node"}
//	GET /api/stream         text/event-stream. On connect one event per requested
//	                        topic (fleet, models): "event: <topic>\nid: <updated>\n
//	                        data: <json>\n\n". Afterwards only what the test
//	                        pushes: Push, PushModels, Ping, Raw.
//
// Unlike the real console it never pushes or pings on its own, so tests are
// deterministic. Every endpoint can be made to fail with a status code or to
// hang, and requests are counted per endpoint.
package fakefleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Endpoint names one of the served endpoints.
type Endpoint int

const (
	// EpFleet is GET /api/fleet.
	EpFleet Endpoint = iota
	// EpNode is GET /api/nodes/:id.
	EpNode
	// EpStream is GET /api/stream.
	EpStream
	// EpRecovery is POST /api/recovery/:id.
	EpRecovery
	numEndpoints
)

// Member is one fleet member in the real JSON shape. Nullable fields are
// pointers (see Ptr).
type Member struct {
	ID         string   `json:"id"`
	Hostname   string   `json:"hostname"`
	MAC        *string  `json:"mac"`
	IP         *string  `json:"ip"`
	Name       string   `json:"name"`
	Role       string   `json:"role"`
	Source     string   `json:"source"`
	Online     bool     `json:"online"`
	LastSeen   int64    `json:"last_seen"`
	RPC        *bool    `json:"rpc"`
	RPCPort    *int     `json:"rpc_port"`
	PayloadVer *string  `json:"payload_version"`
	Kernel     *string  `json:"kernel"`
	OS         string   `json:"os"`
	CPUPct     *float64 `json:"cpu_pct"`
	TempC      *float64 `json:"temp_c"`
	RAMTotalGB *float64 `json:"ram_total_gb"`
	RAMUsedGB  *float64 `json:"ram_used_gb"`
	VRAMTotal  *float64 `json:"vram_total"` // GB
	VRAMUsed   *float64 `json:"vram_used"`  // GB
	GPUName    *string  `json:"gpu_name"`
	GPUUtilPct *float64 `json:"gpu_util_pct"`
	GPUTempC   *float64 `json:"gpu_temp_c"`
	FanRPM     *float64 `json:"fan_rpm"`
	// LastRecovery is the node's reported recovery outcome; null when none.
	LastRecovery *RecoveryAction `json:"last_recovery_action"`
}

// RecoveryAction is a member's last_recovery_action.
type RecoveryAction struct {
	Action     string `json:"action"`
	At         int64  `json:"at"` // unix ms
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason"`
	Nonce      string `json:"nonce"`
	DurationMs int64  `json:"duration_ms"`
}

// RecoveryRequest is one POST /api/recovery/:id the fake received.
type RecoveryRequest struct {
	// Path is the decoded URL path, RawPath the percent-encoded one as sent.
	Path, RawPath string
	// NodeID is the decoded id segment.
	NodeID string
	Body   []byte
	// Status is what the fake answered.
	Status int
}

// RecoveryRecord is the JSON body of a recovery request (contract v1).
type RecoveryRecord struct {
	V            int    `json:"v"`
	TargetNodeID string `json:"target_node_id"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	IssuedAt     int64  `json:"issued_at"`
	ExpiresAt    int64  `json:"expires_at"`
	Nonce        string `json:"nonce"`
}

// Record decodes the request body.
func (r RecoveryRequest) Record() (RecoveryRecord, error) {
	var rec RecoveryRecord
	err := json.Unmarshal(r.Body, &rec)
	return rec, err
}

// Ptr returns a pointer to v, for nullable Member fields.
func Ptr[T any](v T) *T { return &v }

// PS5 builds an online, self-reporting PS5 member with rpc port 50052.
func PS5(id, hostname, ip string, vramTotalGB, vramUsedGB, ramTotalGB, ramUsedGB float64) Member {
	return Member{
		ID: id, Hostname: hostname, Name: hostname, MAC: Ptr(id), IP: Ptr(ip),
		Role: "ps5-linux", Source: "self", Online: true, RPC: Ptr(true), RPCPort: Ptr(50052), OS: "linux",
		RAMTotalGB: Ptr(ramTotalGB), RAMUsedGB: Ptr(ramUsedGB),
		VRAMTotal: Ptr(vramTotalGB), VRAMUsed: Ptr(vramUsedGB), GPUName: Ptr("AMD Oberon APU"),
	}
}

// Relay builds an online member relayed by the OMEN hub (role "worker",
// source "relay:omen-hub", rpc port 50052).
func Relay(id, hostname, ip string, vramTotalGB, vramUsedGB float64) Member {
	return Member{
		ID: id, Hostname: hostname, Name: hostname, IP: Ptr(ip),
		Role: "worker", Source: "relay:omen-hub", Online: true, RPC: Ptr(true), RPCPort: Ptr(50052), OS: "orbis",
		RAMTotalGB: Ptr(16.0), RAMUsedGB: Ptr(4.0),
		VRAMTotal: Ptr(vramTotalGB), VRAMUsed: Ptr(vramUsedGB), GPUName: Ptr("NVIDIA GeForce RTX 3070 Ti"),
	}
}

type stream struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

func (s *stream) drop() { s.once.Do(func() { close(s.done) }) }

// Fleet is the fake console. Create it with New.
type Fleet struct {
	srv *httptest.Server

	mu       sync.Mutex
	members  []Member
	seq      int64
	fail     [numEndpoints]int // non-zero: respond with this status
	hang     [numEndpoints]bool
	hangWake chan struct{} // closed and replaced by Heal
	reqs     [numEndpoints]int
	recov    []RecoveryRequest
	recovSeq []int // statuses for the next recovery requests, consumed in order
	nonces   map[string]bool
	streams  map[*stream]struct{}
	conns    int
	closed   chan struct{}
}

// New starts the fake on a loopback port and registers cleanup with t.
func New(t testing.TB) *Fleet {
	f := &Fleet{streams: map[*stream]struct{}{}, hangWake: make(chan struct{}), closed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fleet", f.handleFleet)
	mux.HandleFunc("/api/nodes/", f.handleNode)
	mux.HandleFunc("/api/stream", f.handleStream)
	mux.HandleFunc("/api/recovery/", f.handleRecovery)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// URL is the base URL, e.g. http://127.0.0.1:41234.
func (f *Fleet) URL() string { return f.srv.URL }

// Close drops every stream and stops the server. Safe to call twice.
func (f *Fleet) Close() {
	f.mu.Lock()
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	for s := range f.streams {
		s.drop()
	}
	f.mu.Unlock()
	f.srv.CloseClientConnections()
	f.srv.Close()
}

// SetMembers replaces the whole member list. It does not push to streams.
func (f *Fleet) SetMembers(ms ...Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = append([]Member(nil), ms...)
}

// Upsert adds a member or replaces the one with the same ID.
func (f *Fleet) Upsert(m Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.members {
		if f.members[i].ID == m.ID {
			f.members[i] = m
			return
		}
	}
	f.members = append(f.members, m)
}

// Remove deletes a member (as eviction does after 10 minutes).
func (f *Fleet) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.members {
		if f.members[i].ID == id {
			f.members = append(f.members[:i], f.members[i+1:]...)
			return
		}
	}
}

// SetOnline marks a member online or offline.
func (f *Fleet) SetOnline(id string, online bool) {
	f.Modify(id, func(m *Member) { m.Online = online })
}

// Modify applies fn to the member with the given id, if present.
func (f *Fleet) Modify(id string, fn func(*Member)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.members {
		if f.members[i].ID == id {
			fn(&f.members[i])
		}
	}
}

// RecoveryRequests is every recovery POST received so far, oldest first.
func (f *Fleet) RecoveryRequests() []RecoveryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecoveryRequest(nil), f.recov...)
}

// QueueRecoveryStatus makes the next recovery requests answer with these
// statuses, one each in order, before Fail(EpRecovery, ...) or the normal
// handling applies again. 200 stores the request.
func (f *Fleet) QueueRecoveryStatus(codes ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovSeq = append(f.recovSeq, codes...)
}

// SetLastRecovery sets (or with nil clears) last_recovery_action on a member,
// as the node's reported outcome would.
func (f *Fleet) SetLastRecovery(id string, a *RecoveryAction) {
	f.Modify(id, func(m *Member) { m.LastRecovery = a })
}

// Push sends the current fleet snapshot to every open stream.
func (f *Fleet) Push() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast(f.eventLocked("fleet"))
}

// PushModels sends an (empty) models snapshot, which clients must ignore.
func (f *Fleet) PushModels() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast(f.eventLocked("models"))
}

// Ping sends the heartbeat comment line ": ping".
func (f *Fleet) Ping() { f.Raw(": ping\n\n") }

// Raw writes s to every open stream verbatim (for partial or odd chunks).
func (f *Fleet) Raw(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast([]byte(s))
}

// DropStreams ends every open stream from the server side.
func (f *Fleet) DropStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.streams {
		s.drop()
	}
}

// Fail makes an endpoint answer with the given status (e.g. 500, 503, 404).
func (f *Fleet) Fail(ep Endpoint, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[ep] = status
}

// Hang makes an endpoint accept the request and never answer (until Heal,
// Close or the client gives up).
func (f *Fleet) Hang(ep Endpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang[ep] = true
}

// Heal undoes Fail and Hang for an endpoint. Requests already hanging then
// proceed normally.
func (f *Fleet) Heal(ep Endpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[ep] = 0
	f.hang[ep] = false
	close(f.hangWake)
	f.hangWake = make(chan struct{})
}

// Requests is the number of requests the endpoint has received.
func (f *Fleet) Requests(ep Endpoint) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[ep]
}

// StreamConnections is the number of stream connections ever accepted.
func (f *Fleet) StreamConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// ActiveStreams is the number of stream connections currently open.
func (f *Fleet) ActiveStreams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

// ---- internals ----

func fv(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

type fleetDoc struct {
	V               int      `json:"v"`
	Updated         int64    `json:"updated"`
	Count           int      `json:"count"`
	OnlineCount     int      `json:"online_count"`
	TotalVRAMGB     float64  `json:"total_vram_gb"`
	TotalVRAMUsedGB float64  `json:"total_vram_used_gb"`
	TotalRAMGB      float64  `json:"total_ram_gb"`
	TotalRAMUsedGB  float64  `json:"total_ram_used_gb"`
	Members         []Member `json:"members"`
}

func (f *Fleet) docLocked() fleetDoc {
	f.seq++
	var online int
	var vt, vu, rt, ru float64
	for _, m := range f.members {
		if m.Online {
			online++
			vt += fv(m.VRAMTotal)
			vu += fv(m.VRAMUsed)
			rt += fv(m.RAMTotalGB)
			ru += fv(m.RAMUsedGB)
		}
	}
	ms := f.members
	if ms == nil {
		ms = []Member{}
	}
	return fleetDoc{V: 1, Updated: 1763300000000 + f.seq*1000, Count: len(ms), OnlineCount: online,
		TotalVRAMGB: vt, TotalVRAMUsedGB: vu, TotalRAMGB: rt, TotalRAMUsedGB: ru, Members: ms}
}

func (f *Fleet) eventLocked(topic string) []byte {
	var doc any
	var updated int64
	if topic == "fleet" {
		d := f.docLocked()
		doc, updated = d, d.Updated
	} else {
		f.seq++
		updated = 1763300000000 + f.seq*1000
		doc = struct {
			V       int   `json:"v"`
			Updated int64 `json:"updated"`
			Models  []any `json:"models"`
		}{1, updated, []any{}}
	}
	b, _ := json.Marshal(doc)
	return []byte("event: " + topic + "\nid: " + strconv.FormatInt(updated, 10) + "\ndata: " + string(b) + "\n\n")
}

func (f *Fleet) broadcast(b []byte) {
	for s := range f.streams {
		select {
		case s.ch <- b:
		default: // a reader that never drains loses events, as in the real console
		}
	}
}

// gate counts the request and applies Fail/Hang. It reports whether the
// handler should continue.
func (f *Fleet) gate(ep Endpoint, w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	f.reqs[ep]++
	f.mu.Unlock()
	for {
		f.mu.Lock()
		status, hang, wake := f.fail[ep], f.hang[ep], f.hangWake
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"error":"injected failure"}`, status)
			return false
		}
		if !hang {
			return true
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return false
		case <-f.closed:
			return false
		}
	}
}

func (f *Fleet) handleFleet(w http.ResponseWriter, r *http.Request) {
	if !f.gate(EpFleet, w, r) {
		return
	}
	f.mu.Lock()
	b, _ := json.Marshal(f.docLocked())
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (f *Fleet) handleNode(w http.ResponseWriter, r *http.Request) {
	if !f.gate(EpNode, w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	f.mu.Lock()
	var found *Member
	for i := range f.members {
		if f.members[i].ID == id {
			m := f.members[i]
			found = &m
		}
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if found == nil {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"unknown node"}`))
		return
	}
	b, _ := json.Marshal(found)
	w.Write(b)
}

// handleRecovery is POST /api/recovery/:id. Like the real manager it answers
// 400 for a body that is not the v1 record (or whose target does not match the
// path), 404 for an unknown node, 410 for a record already expired, 409 for a
// nonce seen before and 200 when stored. Every request is recorded, with the
// status it got, whatever that was. Queued statuses and Fail(EpRecovery, n)
// override the handling.
func (f *Fleet) handleRecovery(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := RecoveryRequest{Path: r.URL.Path, RawPath: r.URL.EscapedPath(), NodeID: strings.TrimPrefix(r.URL.Path, "/api/recovery/"), Body: body}
	f.mu.Lock()
	f.reqs[EpRecovery]++
	f.recov = append(f.recov, req)
	idx := len(f.recov) - 1
	f.mu.Unlock()
	finish := func(status int, msg string) {
		f.mu.Lock()
		f.recov[idx].Status = status
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 200 {
			io.WriteString(w, `{"ok":true}`)
			return
		}
		fmt.Fprintf(w, `{"error":%q}`, msg)
	}
	if r.Method != http.MethodPost {
		finish(405, "POST only")
		return
	}
	f.mu.Lock()
	forced := 0
	if len(f.recovSeq) > 0 {
		forced, f.recovSeq = f.recovSeq[0], f.recovSeq[1:]
	} else {
		forced = f.fail[EpRecovery]
	}
	f.mu.Unlock()
	if forced != 0 && forced != 200 {
		finish(forced, "injected failure")
		return
	}
	var rec RecoveryRecord
	if err := json.Unmarshal(body, &rec); err != nil || rec.V != 1 || rec.TargetNodeID != req.NodeID || rec.Nonce == "" {
		finish(400, "malformed")
		return
	}
	f.mu.Lock()
	known := false
	for _, m := range f.members {
		if m.ID == req.NodeID {
			known = true
		}
	}
	dup := f.nonces[rec.Nonce]
	stored := known && !dup && rec.ExpiresAt > time.Now().UnixMilli()
	if stored {
		if f.nonces == nil {
			f.nonces = map[string]bool{}
		}
		f.nonces[rec.Nonce] = true
	}
	f.mu.Unlock()
	switch {
	case !known:
		finish(404, "unknown node")
	case dup:
		finish(409, "duplicate nonce")
	case !stored:
		finish(410, "expired")
	default:
		finish(200, "")
	}
}

func (f *Fleet) handleStream(w http.ResponseWriter, r *http.Request) {
	if !f.gate(EpStream, w, r) {
		return
	}
	var topics []string
	raw := strings.TrimSpace(r.URL.Query().Get("topics"))
	if raw == "" {
		topics = []string{"fleet", "models"}
	}
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t == "fleet" || t == "models" {
			topics = append(topics, t)
		}
	}
	if len(topics) == 0 {
		http.Error(w, `{"error":"topics must include fleet and/or models"}`, 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	w.WriteHeader(200)
	fl.Flush()

	s := &stream{ch: make(chan []byte, 64), done: make(chan struct{})}
	f.mu.Lock()
	f.conns++
	f.streams[s] = struct{}{}
	for _, t := range topics {
		s.ch <- f.eventLocked(t)
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.streams, s)
		f.mu.Unlock()
	}()
	for {
		select {
		case b := <-s.ch:
			if _, err := w.Write(b); err != nil {
				return
			}
			fl.Flush()
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		case <-f.closed:
			return
		}
	}
}
