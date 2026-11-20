package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
)

// recorderHook adapts an audit.Recorder to AuditHook: benchmark and suite
// changes and run start and cancel become audit entries of section "bench",
// with the source and actor decided by audit.FromRequest (ui when X-Source: ui,
// mcp through the MCP layer, else rest). Recording is best effort: a failure
// is returned to nobody and never fails the change that was already stored.
type recorderHook struct {
	rec audit.Recorder
	s   *Server
}

// RecorderHook returns the AuditHook that writes to rec.
func (s *Server) recorderHook(rec audit.Recorder) AuditHook { return recorderHook{rec: rec, s: s} }

func (h recorderHook) Record(r *http.Request, action, section, target string, before, after any) {
	e := audit.Entry{Action: action, Section: section, Target: target, Before: raw(before), After: raw(after)}
	audit.FromRequest(r).Apply(&e)
	if before != nil || after != nil {
		e.Diff = cfgsvc.Diff("", before, after)
	}
	e.TS = time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.rec.Record(ctx, e); err != nil {
		h.s.log.Warn("audit entry not recorded (the change itself was made)", "action", action, "target", target, "err", err)
	}
}
