package api

import "context"

// The two hooks below exist only for cmd/devcluster, the development harness
// of the web UI. Production code does not use them.

// WithStatusSource returns a copy of o whose live channel (/ws) takes its
// status frames from f instead of the scheduler. GET /api/status is not
// affected.
func (o Options) WithStatusSource(f func(ctx context.Context) (any, error)) Options {
	o.statusSource = f
	return o
}

// StatusJSON converts a scheduler status struct (such as scheduler.Status)
// into the JSON value GET /api/status sends, with the same conversion code, so
// a scripted status cannot drift from the real shape.
func StatusJSON(v any) any { return jsonView(v) }
