// Package docs embeds the documents the binary serves itself.
package docs

import _ "embed"

// BenchmarkGuide is docs/benchmark-guide.md, served as an MCP resource.
//
//go:embed benchmark-guide.md
var BenchmarkGuide string
