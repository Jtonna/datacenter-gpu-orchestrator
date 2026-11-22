package bench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// API is the scheduler's own HTTP API as the runner uses it. The runner talks
// to nothing else, so a benchmark exercises exactly the path a client uses.
type API interface {
	// Plan is POST /api/plan (changes nothing).
	Plan(ctx context.Context, model string) (*PlanInfo, error)
	// Load is POST /api/load: start loading a profile without sending a chat
	// request. It answers at once (202 when the load is under way or not
	// needed, else the refusal a request would get); the runner then watches
	// Status until the instance is ready.
	Load(ctx context.Context, model string) (*LoadAnswer, error)
	// Unload is POST /api/unload.
	Unload(ctx context.Context, model string, force bool) (*UnloadInfo, error)
	// Status is GET /api/status, reduced to what the runner reads.
	Status(ctx context.Context) (*StatusInfo, error)
	// Chat is POST /v1/chat/completions, marked with X-Bench-Run. onProgress
	// (may be nil) is called while a streamed answer arrives.
	Chat(ctx context.Context, runID string, req ChatRequest, onProgress func(ChatProgress)) (*ChatResponse, error)
	// LoadHistory is the loadFinished history record of a load session, or nil
	// when history is off or has no such record.
	LoadHistory(ctx context.Context, sessionID string) (*HistoryLoad, error)
	// Refusals are the history records of the given types (requestRefused,
	// preflightFailed, profileBackedOff) of a profile since a unix-ms time,
	// newest first. It is the fallback for a scheduler whose status carries no
	// last_refusals; an error or an empty list means nothing is known.
	Refusals(ctx context.Context, profile string, sinceMS int64, types ...string) ([]Refusal, error)
}

// Refusal is a record of the scheduler turning a profile down before any child
// started: the newest one of a profile in the status (last_refusals) or a
// history event.
type Refusal struct {
	Profile  string
	AtMS     int64
	Type     string // history event type; empty for a status record
	Status   int    // the HTTP status the refused request got, when known
	Reason   string // the scheduler's reason code ("does not fit", "backoff", ...)
	Message  string // the scheduler's words
	BenchRun string // the bench run whose request was refused, when it carried one
}

// Text is the refusal as a sentence for a step, worded like the answer a
// refused request gets: "<reason>: <message>". With no message it is the
// reason code alone.
func (r Refusal) Text() string {
	switch {
	case r.Message != "" && r.Reason != "" && !strings.HasPrefix(r.Message, r.Reason):
		return r.Reason + ": " + r.Message
	case r.Message != "":
		return r.Message
	case r.Reason != "":
		return r.Reason
	}
	return "refused by the scheduler"
}

// PlanInfo is the answer of /api/plan.
type PlanInfo struct {
	Action string
	Reason string
	Nodes  []string // the nodes the plan would use (ids)
	// LoadMode and Reload are the plan's load_mode and reload_from_cache
	// (node ids in Reload[].ID, no names); empty from an older scheduler.
	LoadMode string
	Reload   []NodeReload
	Raw      json.RawMessage
}

// LoadAnswer is the answer of /api/load.
type LoadAnswer struct {
	Status int
	Msg    string // the scheduler's words for a refusal
	Reason string
}

// UnloadInfo is the answer of /api/unload.
type UnloadInfo struct {
	Status int
	Msg    string
}

// LoadInfo is the `load` object of a loading instance.
type LoadInfo struct {
	SessionID  string
	ElapsedS   float64
	BytesTotal *int64
	BytesSent  *int64
	Progress   *float64
	EtaS       *float64
	Raw        json.RawMessage
}

// InstanceNodeInfo is one placed node of an instance with its measured memory.
type InstanceNodeInfo struct {
	ID        string
	Local     bool
	MemoryMiB float64
}

// InstanceInfo is one instance of /api/status.
type InstanceInfo struct {
	Profile        string
	State          string
	InFlight       int
	Waiters        int
	LogFile        string
	Cmd            string
	ModelFile      string
	DeviceTotalMiB float64
	Load           *LoadInfo
	Nodes          []InstanceNodeInfo
	LoadMode       string // file_read, pushed, local; empty from an older scheduler
}

// WorkerInfo is a node of the scheduler's view with what it reports about its rpc-server.
type WorkerInfo struct {
	ID               string
	Hostname         string
	Local            bool
	RPCServerVersion string
	// AvailMiB is the node's available memory: for an agent node whose
	// device has memory of its own, the GPU's free memory from its live stats;
	// else its agent's mem_available_mib; for the local host the node's free
	// figure (its GPU). Nil when none is known. AvailKind says which
	// (MemKindSystem or MemKindGPU; empty is system), AvailNote why a dedicated
	// GPU's node has a system figure (memkind.go).
	AvailMiB  *int64
	AvailKind string
	AvailNote string
}

// RecentLoad is an entry of recent_loads.
type RecentLoad struct {
	SessionID  string
	Model      string
	Outcome    string
	Reason     string
	FinishedAt int64
	LoadS      float64
	Workers    []string
}

// StatusInfo is what the runner reads of /api/status.
type StatusInfo struct {
	Instances   []InstanceInfo
	QueueLen    int
	RecentLoads []RecentLoad
	// LastRefusals is the newest refusal of each profile (status last_refusals); empty on a build without it.
	LastRefusals []Refusal
	Hostnames    map[string]string // node id to hostname
	Workers      []WorkerInfo
	// ConfigRevision is the revision of the scheduler's configuration when the
	// status exposes one (it does not on every build); empty otherwise.
	ConfigRevision string
	Time           time.Time
}

// Instance finds the instance of a profile.
func (s *StatusInfo) Instance(profile string) *InstanceInfo {
	for i := range s.Instances {
		if s.Instances[i].Profile == profile {
			return &s.Instances[i]
		}
	}
	return nil
}

// HistoryLoad is what the history knows of a finished load.
type HistoryLoad struct {
	LoadS  float64
	Bytes  int64
	AvgMBs float64
	Nodes  []NodeLoad
}

// ChatRequest is one request to send.
type ChatRequest struct {
	Model  string
	Body   []byte
	Stream bool
}

// ChatProgress is reported while a streamed answer arrives.
type ChatProgress struct {
	Tokens    int
	ElapsedMS float64
}

// ChatResponse is the outcome of one request.
type ChatResponse struct {
	Status     int
	WallMS     float64
	TTFTMS     *float64
	Timings    json.RawMessage
	Message    []byte // canonical JSON of the whole message object
	Text       string
	ErrMessage string // the scheduler's error message for a non-200 answer
	ErrReason  string
}

// BuildChatRequest makes the JSON body for a benchmark request.
func BuildChatRequest(profile string, r benchspec.Request) ChatRequest {
	body := map[string]any{
		"model": profile, "messages": r.ChatMessages(), "max_tokens": r.MaxTok(), "temperature": r.Temp(),
		"seed": r.SeedVal(), "cache_prompt": r.CachesPrompt(), "stream": r.Streams(),
	}
	if r.Streams() {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	b, _ := json.Marshal(body)
	return ChatRequest{Model: profile, Body: b, Stream: r.Streams()}
}

// HashMessage is the sha256 (hex) of the canonical JSON of a message object.
func HashMessage(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Head is the first n characters of s.
func Head(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---- HTTP implementation ----

// HTTPAPI is the API over HTTP on a base URL (the scheduler's own loopback address).
type HTTPAPI struct {
	Base   string
	Client *http.Client // nil: a client with no overall timeout (requests carry their own contexts)
}

func (h *HTTPAPI) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

type runCtxKey struct{}

// WithRun marks a context as belonging to a benchmark run: every request the
// HTTP API sends with it carries X-Bench-Run, so the scheduler tags the load
// in the history (data.bench_run) and does not audit it as a human action.
func WithRun(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runCtxKey{}, runID)
}

func (h *HTTPAPI) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.Base, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if id, _ := ctx.Value(runCtxKey{}).(string); id != "" {
		req.Header.Set("X-Bench-Run", id)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return h.client().Do(req)
}

func readLimited(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 4<<20))
	return b
}

type errBody struct {
	Error struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"error"`
}

// Plan implements API.
func (h *HTTPAPI) Plan(ctx context.Context, model string) (*PlanInfo, error) {
	b, _ := json.Marshal(map[string]any{"model": model})
	resp, err := h.do(ctx, "POST", "/api/plan", b, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/api/plan answered %d: %s", resp.StatusCode, Head(string(raw), 300))
	}
	var p struct {
		Action    string `json:"action"`
		Reason    string `json:"reason"`
		Nodes     []any  `json:"nodes"`
		Placement []struct {
			ID string `json:"id"`
		} `json:"placement"`
		LoadMode string `json:"load_mode"`
		Reload   []struct {
			ID        string   `json:"id"`
			Verdict   string   `json:"verdict"`
			Shared    bool     `json:"shared"`
			FileMiB   int      `json:"file_mib"`
			ModelMiB  int      `json:"model_mib"`
			MemoryMiB int      `json:"memory_mib"`
			MarginMiB int      `json:"margin_mib"`
			CachedPct *float64 `json:"cached_pct"`
		} `json:"reload_from_cache"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("/api/plan: %w", err)
	}
	info := &PlanInfo{Action: p.Action, Reason: p.Reason, Raw: raw, LoadMode: p.LoadMode}
	for _, r := range p.Reload {
		info.Reload = append(info.Reload, NodeReload{ID: r.ID, Node: r.ID, Verdict: r.Verdict, CachedPct: r.CachedPct, Shared: r.Shared,
			FileMiB: r.FileMiB, ModelMiB: r.ModelMiB, MemoryMiB: r.MemoryMiB, MarginMiB: r.MarginMiB})
	}
	for _, n := range p.Nodes {
		switch v := n.(type) {
		case string:
			info.Nodes = append(info.Nodes, v)
		case map[string]any:
			if id, ok := v["id"].(string); ok {
				info.Nodes = append(info.Nodes, id)
			}
		}
	}
	for _, n := range p.Placement {
		info.Nodes = append(info.Nodes, n.ID)
	}
	return info, nil
}

// Load implements API.
func (h *HTTPAPI) Load(ctx context.Context, model string) (*LoadAnswer, error) {
	b, _ := json.Marshal(map[string]any{"model": model})
	resp, err := h.do(ctx, "POST", "/api/load", b, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	out := &LoadAnswer{Status: resp.StatusCode}
	if resp.StatusCode >= 300 {
		var eb errBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Message != "" {
			out.Msg, out.Reason = eb.Error.Message, eb.Error.Reason
		} else {
			out.Msg = strings.TrimSpace(Head(string(raw), 1000))
		}
	}
	return out, nil
}

// Unload implements API.
func (h *HTTPAPI) Unload(ctx context.Context, model string, force bool) (*UnloadInfo, error) {
	b, _ := json.Marshal(map[string]any{"model": model, "force": force})
	resp, err := h.do(ctx, "POST", "/api/unload", b, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	var m struct {
		Msg   string `json:"msg"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &m)
	msg := m.Msg
	if msg == "" {
		msg = m.Error.Message
	}
	return &UnloadInfo{Status: resp.StatusCode, Msg: msg}, nil
}

type statusWire struct {
	Time      string `json:"time"`
	Instances []struct {
		Profile        string  `json:"profile"`
		State          string  `json:"state"`
		InFlight       int     `json:"in_flight"`
		Waiters        int     `json:"waiters"`
		LogFile        string  `json:"log_file"`
		Cmd            string  `json:"cmd"`
		ModelFile      string  `json:"model_file"`
		DeviceTotalMiB float64 `json:"device_total_mib"`
		LoadMode       string  `json:"load_mode"`
		Load           *struct {
			SessionID  string   `json:"session_id"`
			ElapsedS   float64  `json:"elapsed_s"`
			BytesTotal *int64   `json:"bytes_total"`
			BytesSent  *int64   `json:"bytes_sent"`
			Progress   *float64 `json:"progress"`
			EtaS       *float64 `json:"eta_s"`
		} `json:"load"`
		Nodes []struct {
			ID     string `json:"id"`
			Local  bool   `json:"local"`
			Memory []struct {
				TotalMiB float64 `json:"total_mib"`
			} `json:"memory"`
		} `json:"nodes"`
	} `json:"instances"`
	Queue       []json.RawMessage `json:"queue"`
	RecentLoads []struct {
		SessionID  string   `json:"session_id"`
		Model      string   `json:"model"`
		Outcome    string   `json:"outcome"`
		Reason     string   `json:"reason"`
		FinishedAt int64    `json:"finished_at"`
		LoadS      float64  `json:"load_s"`
		Workers    []string `json:"workers"`
	} `json:"recent_loads"`
	View struct {
		Nodes []struct {
			ID               string       `json:"id"`
			Hostname         string       `json:"hostname"`
			Local            bool         `json:"local"`
			RPCServerVersion string       `json:"rpc_server_version"`
			FreeMiB          *int64       `json:"free_mib"`
			Agent            *statusAgent `json:"agent"`
		} `json:"nodes"`
	} `json:"view"`
	LastRefusals []struct {
		Profile  string `json:"profile"`
		At       string `json:"at"`
		Status   int    `json:"status"`
		Reason   string `json:"reason"`
		Message  string `json:"message"`
		BenchRun string `json:"bench_run"`
	} `json:"last_refusals"`
	ConfigRevision json.RawMessage `json:"config_revision"`
	Config         struct {
		Revision json.RawMessage `json:"revision"`
	} `json:"config"`
}

// scalarText renders a JSON string or number as text ("" for anything else).
func scalarText(raw json.RawMessage) string {
	raw = bytesTrim(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func bytesTrim(b []byte) []byte { return bytes.TrimSpace(b) }

// Status implements API.
func (h *HTTPAPI) Status(ctx context.Context) (*StatusInfo, error) {
	resp, err := h.do(ctx, "GET", "/api/status", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/api/status answered %d", resp.StatusCode)
	}
	return ParseStatus(raw)
}

// ParseStatus reduces an /api/status document to the StatusInfo fields.
func ParseStatus(raw []byte) (*StatusInfo, error) {
	var w statusWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("/api/status: %w", err)
	}
	var envelope struct {
		Instances []struct {
			Load json.RawMessage `json:"load"`
		} `json:"instances"`
	}
	json.Unmarshal(raw, &envelope)
	out := &StatusInfo{QueueLen: len(w.Queue), Hostnames: map[string]string{}}
	if t, err := time.Parse(time.RFC3339Nano, w.Time); err == nil {
		out.Time = t
	}
	for i, in := range w.Instances {
		ii := InstanceInfo{Profile: in.Profile, State: in.State, InFlight: in.InFlight, Waiters: in.Waiters, LogFile: in.LogFile, DeviceTotalMiB: in.DeviceTotalMiB,
			Cmd: in.Cmd, ModelFile: in.ModelFile, LoadMode: in.LoadMode}
		if in.Load != nil {
			li := &LoadInfo{SessionID: in.Load.SessionID, ElapsedS: in.Load.ElapsedS, BytesTotal: in.Load.BytesTotal, BytesSent: in.Load.BytesSent, Progress: in.Load.Progress, EtaS: in.Load.EtaS}
			if i < len(envelope.Instances) {
				li.Raw = envelope.Instances[i].Load
			}
			ii.Load = li
		}
		for _, n := range in.Nodes {
			var mem float64
			for _, m := range n.Memory {
				mem += m.TotalMiB
			}
			ii.Nodes = append(ii.Nodes, InstanceNodeInfo{ID: n.ID, Local: n.Local, MemoryMiB: mem})
		}
		out.Instances = append(out.Instances, ii)
	}
	for _, r := range w.RecentLoads {
		out.RecentLoads = append(out.RecentLoads, RecentLoad{SessionID: r.SessionID, Model: r.Model, Outcome: r.Outcome, Reason: r.Reason, FinishedAt: r.FinishedAt, LoadS: r.LoadS, Workers: r.Workers})
	}
	for _, r := range w.LastRefusals {
		rf := Refusal{Profile: r.Profile, Status: r.Status, Reason: r.Reason, Message: r.Message, BenchRun: r.BenchRun}
		if t, err := time.Parse(time.RFC3339Nano, r.At); err == nil {
			rf.AtMS = t.UnixMilli()
		}
		out.LastRefusals = append(out.LastRefusals, rf)
	}
	for _, n := range w.View.Nodes {
		if n.Hostname != "" {
			out.Hostnames[n.ID] = n.Hostname
		}
		wi := WorkerInfo{ID: n.ID, Hostname: n.Hostname, Local: n.Local, RPCServerVersion: n.RPCServerVersion, AvailMiB: n.FreeMiB}
		if n.FreeMiB != nil {
			wi.AvailKind = MemKindSystem
			if n.Local {
				wi.AvailKind = MemKindGPU // the scheduler's own probe: its GPU's free memory
			}
		}
		if n.Agent != nil {
			if v, kind, note := agentAvail(n.Agent); v != nil {
				wi.AvailMiB, wi.AvailKind, wi.AvailNote = v, kind, note
			}
		}
		out.Workers = append(out.Workers, wi)
	}
	out.ConfigRevision = scalarText(w.ConfigRevision)
	if out.ConfigRevision == "" {
		out.ConfigRevision = scalarText(w.Config.Revision)
	}
	return out, nil
}

// LoadHistory implements API.
func (h *HTTPAPI) LoadHistory(ctx context.Context, sessionID string) (*HistoryLoad, error) {
	q := url.Values{"session_id": {sessionID}, "type": {"loadFinished"}, "limit": {"5"}}
	resp, err := h.do(ctx, "GET", "/api/history?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/api/history answered %d", resp.StatusCode)
	}
	var d struct {
		Events []struct {
			Data struct {
				LoadS       float64 `json:"load_s"`
				Bytes       float64 `json:"bytes"`
				AvgMBs      float64 `json:"avg_mb_s"`
				NodesDetail []struct {
					Node      string   `json:"node"`
					Bytes     *float64 `json:"bytes"`
					MemoryMiB *float64 `json:"memory_mib"`
				} `json:"nodes_detail"`
			} `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if len(d.Events) == 0 {
		return nil, nil
	}
	e := d.Events[0].Data
	out := &HistoryLoad{LoadS: e.LoadS, Bytes: int64(e.Bytes), AvgMBs: e.AvgMBs}
	for _, n := range e.NodesDetail {
		nl := NodeLoad{Node: n.Node, MemoryMiB: n.MemoryMiB}
		if n.Bytes != nil {
			b := int64(*n.Bytes)
			nl.Bytes = &b
		}
		out.Nodes = append(out.Nodes, nl)
	}
	return out, nil
}

// Refusals implements API.
func (h *HTTPAPI) Refusals(ctx context.Context, profile string, sinceMS int64, types ...string) ([]Refusal, error) {
	q := url.Values{"type": {strings.Join(types, ",")}, "profile": {profile}, "since": {strconv.FormatInt(sinceMS, 10)}, "limit": {"20"}}
	resp, err := h.do(ctx, "GET", "/api/history?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/api/history answered %d", resp.StatusCode)
	}
	var d struct {
		Events []struct {
			Ts      int64          `json:"ts"`
			Type    string         `json:"type"`
			Summary string         `json:"summary"`
			Data    map[string]any `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var out []Refusal
	for _, e := range d.Events {
		r := Refusal{AtMS: e.Ts, Type: e.Type, Message: strings.TrimSpace(e.Summary)}
		if v, ok := e.Data["message"].(string); ok && v != "" {
			r.Message = v
		}
		r.Reason, _ = e.Data["reason"].(string)
		r.BenchRun, _ = e.Data["bench_run"].(string)
		if v, ok := e.Data["status"].(float64); ok {
			r.Status = int(v)
		}
		out = append(out, r)
	}
	return out, nil
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		Message map[string]any `json:"message"`
	} `json:"choices"`
	Timings json.RawMessage `json:"timings"`
}

// Chat implements API.
func (h *HTTPAPI) Chat(ctx context.Context, runID string, req ChatRequest, onProgress func(ChatProgress)) (*ChatResponse, error) {
	start := time.Now()
	resp, err := h.do(ctx, "POST", "/v1/chat/completions", req.Body, map[string]string{"X-Bench-Run": runID})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &ChatResponse{Status: resp.StatusCode}
	if resp.StatusCode != 200 {
		raw := readLimited(resp.Body)
		var eb errBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Message != "" {
			out.ErrMessage, out.ErrReason = eb.Error.Message, eb.Error.Reason
		} else {
			out.ErrMessage = strings.TrimSpace(Head(string(raw), 1000))
		}
		out.WallMS = ms(time.Since(start))
		return out, nil
	}
	if !req.Stream {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		out.WallMS = ms(time.Since(start))
		if err != nil {
			return nil, err
		}
		var c chatChunk
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("the answer is not JSON: %w", err)
		}
		out.Timings = c.Timings
		if len(c.Choices) > 0 && c.Choices[0].Message != nil {
			out.Message, _ = json.Marshal(c.Choices[0].Message) // maps marshal with sorted keys: canonical
			if s, ok := c.Choices[0].Message["content"].(string); ok {
				out.Text = s
			}
		}
		return out, nil
	}
	// Streamed: server-sent events.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var text, reasoning strings.Builder
	chunks := 0
	var lastProgress time.Time
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var c chatChunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		if len(c.Timings) > 0 && string(c.Timings) != "null" {
			out.Timings = c.Timings
		}
		if len(c.Choices) == 0 {
			continue
		}
		d := c.Choices[0].Delta
		if d.Content != "" || d.ReasoningContent != "" {
			if out.TTFTMS == nil {
				v := ms(time.Since(start))
				out.TTFTMS = &v
			}
			chunks++
			text.WriteString(d.Content)
			reasoning.WriteString(d.ReasoningContent)
			if onProgress != nil && time.Since(lastProgress) >= 250*time.Millisecond {
				lastProgress = time.Now()
				onProgress(ChatProgress{Tokens: chunks, ElapsedMS: ms(time.Since(start))})
			}
		}
	}
	out.WallMS = ms(time.Since(start))
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if onProgress != nil {
		onProgress(ChatProgress{Tokens: chunks, ElapsedMS: out.WallMS})
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	out.Message, _ = json.Marshal(msg)
	out.Text = text.String()
	return out, nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
