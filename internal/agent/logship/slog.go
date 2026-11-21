package logship

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"unicode"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agentproto"
)

// Handler is the agent's slog handler with the shipper added: every record
// is printed by the inner handler, as before, and copied into the ring as a
// line of source agent. Copying never waits (Shipper.Add) and never logs, so
// it cannot recurse. The record's own time (the node's wall clock) is not
// used: the line gets Potluck's time through the anchor.
type Handler struct {
	inner  slog.Handler
	ship   *Shipper
	key    Key
	attrs  string // attributes of WithAttrs, already formatted
	prefix string // group prefix, "a.b."
}

// NewHandler wraps inner; the lines go into the ring under AgentKey.
func NewHandler(inner slog.Handler, ship *Shipper) *Handler {
	return NewHandlerKey(inner, ship, AgentKey)
}

// NewHandlerKey wraps inner; the lines go into the ring under k (Potluck's
// own lines use source scheduler).
func NewHandlerKey(inner slog.Handler, ship *Shipper, k Key) *Handler {
	return &Handler{inner: inner, ship: ship, key: k}
}

// AgentKey is the ring key of the agent's own lines.
var AgentKey = Key{Source: agentproto.LogSourceAgent}

// Enabled implements slog.Handler: the same lines as the printed log.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, h.prefix, a)
		return true
	})
	h.ship.Add(h.key, Line{Mono: h.ship.Now(), Msg: b.String(), Level: levelWord(r.Level)})
	return err
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		writeAttr(&b, h.prefix, a)
	}
	return &Handler{inner: h.inner.WithAttrs(as), ship: h.ship, key: h.key, attrs: b.String(), prefix: h.prefix}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{inner: h.inner.WithGroup(name), ship: h.ship, key: h.key, attrs: h.attrs, prefix: h.prefix + name + "."}
}

func levelWord(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	}
	return "error"
}

// writeAttr writes " key=value" as the text handler does.
func writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			writeAttr(b, p, g)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(quoteIf(prefix + a.Key))
	b.WriteByte('=')
	b.WriteString(quoteIf(a.Value.String()))
}

func quoteIf(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == '"' || r == '=' || !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
}
