// Package mcpserver is the Model Context Protocol surface of the scheduler:
// the same binary and port, served at /mcp over Streamable HTTP (design 9.5).
//
// One implementation, two transports: every tool is a thin wrapper that builds
// an HTTP request and runs it IN-PROCESS through the scheduler's own REST
// handler (the inner http.Handler), so a tool can never drift from REST. See
// SourceFromContext for how the audit log learns a request came from here.
//
// The protocol is hand-rolled (JSON-RPC 2.0: initialize, ping, tools/*,
// resources/*) so the binary gains no dependency.
package mcpserver
