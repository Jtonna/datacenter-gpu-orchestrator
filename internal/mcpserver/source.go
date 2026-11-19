package mcpserver

import "context"

// SourceFromContext tells the audit log whether a request reached a REST
// handler through the MCP server, and on whose behalf.
//
// The marker is an unexported context key set only by this package on the
// in-process requests it builds for tool calls. A context value cannot travel
// over the network and no header is ever consulted, so a client of the public
// REST API cannot forge it. actor is the MCP client's name and version from
// the initialize handshake (clientInfo), or "<actor argument> (via <client>)"
// when a write tool was given its optional actor argument.
//
// Merge wiring (one line, in cmd/llm-scheduler/main.go where mcpserver.New is
// called): set Options.Bridge to
//
//	func(ctx context.Context, actor string) context.Context { return audit.WithSource(ctx, "mcp", actor) }
//
// Bridge runs on every in-process request, so the audit package needs no
// import of this one. Code that prefers to ask directly can call this function
// from its handler instead.
func SourceFromContext(ctx context.Context) (isMCP bool, actor string) {
	s, ok := ctx.Value(sourceKey{}).(sourceVal)
	return ok, s.actor
}

type sourceKey struct{}
type sourceVal struct{ actor string }

func withSource(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, sourceKey{}, sourceVal{actor})
}
