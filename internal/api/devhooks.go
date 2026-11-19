package api

import (
	"context"
	"encoding/json"
)

// The hooks below exist only for cmd/devcluster, the development harness
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

// WithModelFiles returns a copy of o whose model catalog (GET /api/models)
// takes the list of model files from f instead of scanning models_dir.
func (o Options) WithModelFiles(f func() []ModelFile) Options {
	o.modelFiles = f
	return o
}

// viaJSON turns a tagged struct into the plain JSON value a client would decode.
func viaJSON(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// ListModelFiles is the catalog's scan of a models directory: every *.gguf in
// dir plus the named files, each with its metadata. Not cached.
func ListModelFiles(dir string, named []string) []ModelFile {
	var c modelFileCache
	return c.list(dir, named)
}
