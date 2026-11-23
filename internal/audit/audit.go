// Package audit defines the audit log entry, the Recorder every writer
// records to, and how the origin of a request (source and actor) is decided.
package audit

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// Sources of a change.
const (
	SourceUI     = "ui"
	SourceREST   = "rest"
	SourceMCP    = "mcp"
	SourceImport = "import"
	SourceSystem = "system"
)

// Actions of an entry.
const (
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionRollback = "rollback"
	ActionImport   = "import"
	ActionLoad     = "load"
	ActionUnload   = "unload"
	// ActionForgetNeed drops a profile's learned memory need (runtime).
	ActionForgetNeed = "forget_need"
)

// Sections of an entry. Configuration sections are the keys of the configuration document.
const (
	SectionBench   = "bench"
	SectionRuntime = "runtime"
	SectionAgents  = "agents" // agent identities (keys that name an AI agent in this log)
	SectionModels  = "models" // per-model settings that are not configuration (the icon)
)

// DefaultActor is used when a request names no actor.
const DefaultActor = "anonymous"

// DiffItem is one changed path.
type DiffItem struct {
	Path string `json:"path"`
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

// Entry is one audit log row. ID and TS are set when it is recorded.
type Entry struct {
	ID         int64           `json:"id"`
	TS         int64           `json:"ts"` // unix ms
	Source     string          `json:"source"`
	Actor      string          `json:"actor"`
	RemoteAddr string          `json:"remote_addr"`
	UserAgent  string          `json:"user_agent"`
	Action     string          `json:"action"`
	Section    string          `json:"section"`
	Target     string          `json:"target"`
	Before     json.RawMessage `json:"before"`
	After      json.RawMessage `json:"after"`
	Diff       []DiffItem      `json:"diff"`
	Revision   int64           `json:"revision"` // 0 when no configuration revision was created
	Comment    string          `json:"comment"`
	// AgentID is the id of the agent whose key came with the request ("" when
	// none did); Actor is then "agent:<name>".
	AgentID string `json:"agent_id,omitempty"`
}

// Recorder appends an entry to the audit log and returns its id.
type Recorder interface {
	Record(ctx context.Context, e Entry) (id int64, err error)
}

// Origin says who made a change and from where.
type Origin struct {
	Source     string
	Actor      string
	RemoteAddr string
	UserAgent  string
	Comment    string
	AgentID    string
}

// Apply copies the origin onto an entry.
func (o Origin) Apply(e *Entry) {
	e.Source, e.Actor, e.RemoteAddr, e.UserAgent = o.Source, o.Actor, o.RemoteAddr, o.UserAgent
	if e.AgentID == "" {
		e.AgentID = o.AgentID
	}
	if e.Comment == "" {
		e.Comment = o.Comment
	}
}

type ctxKey struct{}

type ctxVal struct{ source, actor string }

// WithSource marks a context as coming from a trusted in-process caller (the
// MCP layer dispatching a tool call, the import command). Only code in this
// process can set it; an external client cannot claim it with a header.
func WithSource(ctx context.Context, source, actor string) context.Context {
	return context.WithValue(ctx, ctxKey{}, ctxVal{source, actor})
}

type defaultUserKey struct{}

// WithDefaultUser names the default user for a request: the actor of an
// entry whose request names no actor (no X-Actor, no access key). The
// server sets it from the default_user_name setting; without it the actor
// is DefaultActor.
func WithDefaultUser(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, defaultUserKey{}, name)
}

type agentKey struct{}

type agentVal struct{ id, name string }

// AgentPrefix starts the actor of an entry made with an agent key.
const AgentPrefix = "agent:"

// WithAgent marks a context as acting for a named agent: the server sets it
// after it resolved the key a request carried (it is identity for this log,
// not access control). FromRequest then reports the actor "agent:<name>".
func WithAgent(ctx context.Context, id, name string) context.Context {
	return context.WithValue(ctx, agentKey{}, agentVal{id, name})
}

// AgentFrom returns the agent a context acts for.
func AgentFrom(ctx context.Context) (id, name string, ok bool) {
	v, ok := ctx.Value(agentKey{}).(agentVal)
	return v.id, v.name, ok
}

// FromRequest decides the origin of a request. The source is "mcp" or
// "import" only when an in-process caller marked the request context with
// WithSource; from outside, only the header X-Source: ui is honoured, anything
// else is "rest". The actor is the X-Actor header (or the one WithSource carried), default "anonymous".
func FromRequest(r *http.Request) Origin {
	actor := DefaultActor
	if n, ok := r.Context().Value(defaultUserKey{}).(string); ok && n != "" {
		actor = n
	}
	o := Origin{Source: SourceREST, Actor: actor, UserAgent: clip(r.UserAgent(), 300), Comment: clip(strings.TrimSpace(r.Header.Get("X-Comment")), 2000)}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		o.RemoteAddr = host
	} else {
		o.RemoteAddr = r.RemoteAddr
	}
	if a := clip(strings.TrimSpace(r.Header.Get("X-Actor")), 200); a != "" {
		o.Actor = a
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Source")), "ui") {
		o.Source = SourceUI
	}
	if v, ok := r.Context().Value(ctxKey{}).(ctxVal); ok {
		o.Source = v.source
		if v.actor != "" {
			o.Actor = v.actor
		}
	}
	// A request that carried a valid agent key is recorded under that agent's
	// name, whatever X-Actor says; the source stays what it was (mcp or rest).
	if id, name, ok := AgentFrom(r.Context()); ok {
		if v, isMCP := r.Context().Value(ctxKey{}).(ctxVal); isMCP && v.actor != "" && o.UserAgent == "" {
			o.UserAgent = clip("mcp client: "+v.actor, 300)
		}
		o.Actor, o.AgentID = AgentPrefix+name, id
	}
	return o
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
