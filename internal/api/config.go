package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/audit"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/cfgstore"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/cfgsvc"
	"github.com/Jtonna/ps5-ai-datacenter-load-balancer/internal/config"
)

// AuditStore is the audit log: it records and answers queries.
type AuditStore interface {
	audit.Recorder
	Query(ctx context.Context, f cfgstore.Filter) ([]audit.Entry, int, error)
	GetEntry(ctx context.Context, id int64) (*audit.Entry, error)
}

// configRoutes registers the configuration and audit endpoints. Everything
// here is a thin transport over cfgsvc (the MCP tools call the same service).
func (s *Server) configRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/config", s.cfgGet)
	m.HandleFunc("GET /api/config/schema", s.cfgSchema)
	m.HandleFunc("GET /api/config/profiles", s.cfgProfiles)
	m.HandleFunc("POST /api/config/profiles", s.cfgProfileCreate)
	m.HandleFunc("GET /api/config/profiles/{id...}", s.cfgProfileGet)
	m.HandleFunc("PUT /api/config/profiles/{id...}", s.cfgProfilePut)
	m.HandleFunc("DELETE /api/config/profiles/{id...}", s.cfgProfileDelete)
	m.HandleFunc("GET /api/config/tiers", s.cfgSectionGet("tiers"))
	m.HandleFunc("PUT /api/config/tiers", s.cfgTiersPut)
	m.HandleFunc("GET /api/config/settings", s.cfgSectionGet("settings"))
	m.HandleFunc("PATCH /api/config/settings", s.cfgPatch(func(sv *cfgsvc.Service) patchFn { return sv.PatchSettings }))
	m.HandleFunc("GET /api/config/exclude-nodes", s.cfgSectionGet("exclude_nodes"))
	m.HandleFunc("PUT /api/config/exclude-nodes", s.cfgExcludePut)
	m.HandleFunc("GET /api/config/node-overrides", s.cfgSectionGet("node_overrides"))
	m.HandleFunc("PUT /api/config/node-overrides", s.cfgOverridesPut)
	m.HandleFunc("GET /api/config/recovery", s.cfgSectionGet("recovery"))
	m.HandleFunc("PUT /api/config/recovery", s.cfgPatch(func(sv *cfgsvc.Service) patchFn { return sv.ReplaceRecovery }))
	m.HandleFunc("PATCH /api/config/recovery", s.cfgPatch(func(sv *cfgsvc.Service) patchFn { return sv.PatchRecovery }))
	m.HandleFunc("GET /api/config/fleet", s.cfgSectionGet("fleet"))
	m.HandleFunc("PUT /api/config/fleet", s.cfgPatch(func(sv *cfgsvc.Service) patchFn { return sv.ReplaceFleet }))
	m.HandleFunc("PATCH /api/config/fleet", s.cfgPatch(func(sv *cfgsvc.Service) patchFn { return sv.PatchFleet }))
	m.HandleFunc("GET /api/config/revisions", s.cfgRevisions)
	m.HandleFunc("GET /api/config/revisions/{n}", s.cfgRevisionGet)
	m.HandleFunc("POST /api/config/revisions/{n}/rollback", s.cfgRollback)
	m.HandleFunc("GET /api/config/export", s.cfgExport)
	m.HandleFunc("POST /api/config/import", s.cfgImport)
	m.HandleFunc("GET /api/audit", s.auditList)
	m.HandleFunc("GET /api/audit/{id}", s.auditGet)
}

type patchFn = func(ctx context.Context, o cfgsvc.WriteOpts, obj map[string]any) (*cfgsvc.Result, error)

func (s *Server) svc(w http.ResponseWriter) *cfgsvc.Service {
	if s.opts.Config == nil {
		apiErr(w, http.StatusServiceUnavailable, "the configuration service is not available")
		return nil
	}
	return s.opts.Config
}

// cfgErr maps a service error to an answer.
func (s *Server) cfgErr(w http.ResponseWriter, err error) {
	var ve *cfgsvc.ValidationError
	var st *cfgstore.ErrStale
	var re *cfgsvc.RefError
	body := func(status int, typ string, extra map[string]any) {
		e := map[string]any{"message": err.Error(), "type": typ}
		for k, v := range extra {
			e[k] = v
		}
		writeJSON(w, status, map[string]any{"error": e})
	}
	switch {
	case errors.As(err, &ve):
		body(http.StatusBadRequest, "invalid_config", map[string]any{"messages": ve.Messages})
	case errors.As(err, &st):
		body(http.StatusConflict, "stale_revision", map[string]any{"current_revision": st.Current})
	case errors.As(err, &re):
		body(http.StatusConflict, "profile_referenced", map[string]any{"warnings": re.Warnings})
	case errors.Is(err, cfgsvc.ErrExists), errors.Is(err, cfgstore.ErrNotEmpty):
		body(http.StatusConflict, "conflict", nil)
	case errors.Is(err, cfgsvc.ErrNotFound), errors.Is(err, cfgstore.ErrNotFound):
		body(http.StatusNotFound, "not_found", nil)
	case errors.Is(err, cfgsvc.ErrNoStore):
		body(http.StatusServiceUnavailable, "database_unavailable", nil)
	default:
		body(http.StatusInternalServerError, "server_error", nil)
	}
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// writeOpts reads ?dry_run, ?force, If-Match and the origin (source, actor, comment).
func (s *Server) writeOpts(w http.ResponseWriter, r *http.Request, bodyComment string) (cfgsvc.WriteOpts, bool) {
	o := cfgsvc.WriteOpts{Origin: audit.FromRequest(r), DryRun: truthy(r.URL.Query().Get("dry_run")), Force: truthy(r.URL.Query().Get("force"))}
	if bodyComment != "" {
		o.Origin.Comment = bodyComment
	}
	if v := strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("If-Match")), "W/"), `"`); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			apiErr(w, http.StatusBadRequest, "If-Match must be a revision number, got %q", v)
			return o, false
		}
		o.IfMatch = &n
	}
	return o, true
}

// bodyObject reads a JSON object and takes its "comment" field out.
func bodyObject(w http.ResponseWriter, r *http.Request) (map[string]any, string, bool) {
	b, ok := readBody(w, r)
	if !ok {
		return nil, "", false
	}
	obj := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(b))
		if err := dec.Decode(&obj); err != nil {
			apiErr(w, http.StatusBadRequest, "the body must be a JSON object: %v", err)
			return nil, "", false
		}
	}
	c, _ := obj["comment"].(string)
	delete(obj, "comment")
	return obj, strings.TrimSpace(c), true
}

func (s *Server) respondWrite(w http.ResponseWriter, status int, res *cfgsvc.Result, extra map[string]any) {
	out := map[string]any{}
	b, _ := json.Marshal(res)
	_ = json.Unmarshal(b, &out)
	out["audit_ids"] = res.AuditEntryIDs // same list, under the shorter name too
	for k, v := range extra {
		out[k] = v
	}
	if res.DryRun {
		status = http.StatusOK
	}
	w.Header().Set("X-Config-Revision", strconv.FormatInt(res.Revision, 10))
	writeJSON(w, status, out)
}

func (s *Server) cfgGet(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	writeJSON(w, http.StatusOK, sv.View())
}

func (s *Server) cfgSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"fields": config.Schema()})
}

// cfgSectionGet answers GET of one section as {"revision": N, "<section>": ...}.
func (s *Server) cfgSectionGet(section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sv := s.svc(w)
		if sv == nil {
			return
		}
		v := sv.View()
		out := map[string]any{"revision": v.Revision}
		switch section {
		case "tiers":
			out["tiers"] = v.Tiers
		case "settings":
			out["settings"], out["settings_set"] = v.Settings, v.SettingsSet
		case "exclude_nodes":
			out["exclude_nodes"] = v.ExcludeNodes
		case "node_overrides":
			out["node_overrides"] = v.NodeOverrides
		case "recovery":
			out["recovery"] = v.Recovery
		case "fleet":
			out["fleet"] = v.Fleet
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) cfgProfiles(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	v := sv.View()
	writeJSON(w, http.StatusOK, map[string]any{"revision": v.Revision, "profiles": v.Profiles})
}

func (s *Server) cfgProfileGet(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	v := sv.View()
	id := r.PathValue("id")
	p, ok := v.Profiles[id]
	if !ok {
		s.cfgErr(w, cfgsvc.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": v.Revision, "id": id, "profile": p})
}

// profileBody reads a profile; for a create the id is the "id" field or the path.
func profileBody(w http.ResponseWriter, r *http.Request) (id string, spec *config.ProfileSpec, comment string, ok bool) {
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if v, has := obj["id"]; has {
		id, _ = v.(string)
		delete(obj, "id")
	}
	if inner, has := obj["profile"].(map[string]any); has && len(obj) == 1 {
		obj = inner
	}
	b, _ := json.Marshal(obj)
	spec, err := cfgsvc.ParseProfile(b)
	if err != nil {
		var ve *cfgsvc.ValidationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": ve.Error(), "type": "invalid_config", "messages": ve.Messages}})
		}
		return "", nil, "", false
	}
	return id, spec, comment, true
}

func (s *Server) cfgProfileCreate(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	id, spec, comment, ok := profileBody(w, r)
	if !ok {
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.CreateProfile(r.Context(), o, id, spec)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusCreated, res, map[string]any{"id": id})
}

func (s *Server) cfgProfilePut(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	_, spec, comment, ok := profileBody(w, r)
	if !ok {
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.UpdateProfile(r.Context(), o, r.PathValue("id"), spec)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, map[string]any{"id": r.PathValue("id")})
}

func (s *Server) cfgProfileDelete(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	o, ok := s.writeOpts(w, r, "")
	if !ok {
		return
	}
	res, err := sv.DeleteProfile(r.Context(), o, r.PathValue("id"))
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, map[string]any{"id": r.PathValue("id")})
}

// listBody reads either a bare JSON array or {"<key>": [...], "comment": "..."}.
func listBody(w http.ResponseWriter, r *http.Request, key string, into any) (comment string, ok bool) {
	b, ok := readBody(w, r)
	if !ok {
		return "", false
	}
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			apiErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
			return "", false
		}
		if c, has := m["comment"]; has {
			_ = json.Unmarshal(c, &comment)
		}
		inner, has := m[key]
		if !has && key == "node_overrides" { // a bare map of node to override is accepted too
			delete(m, "comment")
			inner, has = json.RawMessage(nil), true
			inner, _ = json.Marshal(m)
		}
		if !has {
			apiErr(w, http.StatusBadRequest, "the object must have a %q field", key)
			return "", false
		}
		b = inner
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid %s: %v", key, err)
		return "", false
	}
	return strings.TrimSpace(comment), true
}

func (s *Server) cfgTiersPut(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	var tiers []config.TierSpec
	comment, ok := listBody(w, r, "tiers", &tiers)
	if !ok {
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.SetTiers(r.Context(), o, tiers)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, nil)
}

func (s *Server) cfgExcludePut(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	var list []string
	comment, ok := listBody(w, r, "exclude_nodes", &list)
	if !ok {
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.SetExcludeNodes(r.Context(), o, list)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, nil)
}

func (s *Server) cfgOverridesPut(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	var m map[string]config.NodeOverrideSpec
	comment, ok := listBody(w, r, "node_overrides", &m)
	if !ok {
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.SetNodeOverrides(r.Context(), o, m)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, nil)
}

// cfgPatch serves the endpoints whose body is an object of key to value.
func (s *Server) cfgPatch(pick func(*cfgsvc.Service) patchFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sv := s.svc(w)
		if sv == nil {
			return
		}
		obj, comment, ok := bodyObject(w, r)
		if !ok {
			return
		}
		o, ok := s.writeOpts(w, r, comment)
		if !ok {
			return
		}
		res, err := pick(sv)(r.Context(), o, obj)
		if err != nil {
			s.cfgErr(w, err)
			return
		}
		s.respondWrite(w, http.StatusOK, res, nil)
	}
}

func (s *Server) cfgRevisions(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	if sv.Store() == nil {
		s.cfgErr(w, cfgsvc.ErrNoStore)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	revs, err := sv.Store().Revisions(r.Context(), limit)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(revs))
	for _, rv := range revs {
		out = append(out, map[string]any{"revision": rv.Revision, "ts": rv.TS, "source": rv.Source, "actor": rv.Actor, "comment": rv.Comment})
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out, "current": sv.Revision()})
}

func revisionNumber(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || n < 1 {
		apiErr(w, http.StatusBadRequest, "the revision must be a positive number")
		return 0, false
	}
	return n, true
}

func (s *Server) cfgRevisionGet(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	n, ok := revisionNumber(w, r)
	if !ok {
		return
	}
	v, rev, err := sv.RevisionView(r.Context(), n)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev.Revision, "ts": rev.TS, "source": rev.Source, "actor": rev.Actor,
		"comment": rev.Comment, "config": v, "document": rev.Doc})
}

func (s *Server) cfgRollback(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	n, ok := revisionNumber(w, r)
	if !ok {
		return
	}
	var comment string
	if b, ok2 := readBody(w, r); !ok2 {
		return
	} else if len(bytes.TrimSpace(b)) > 0 {
		var m struct {
			Comment string `json:"comment"`
		}
		_ = json.Unmarshal(b, &m)
		comment = strings.TrimSpace(m.Comment)
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.Rollback(r.Context(), o, n)
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, map[string]any{"rolled_back_to": n})
}

// ExportFormat marks an export document.
const ExportFormat = "llm-scheduler-config/1"

func (s *Server) cfgExport(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	b, _ := json.Marshal(sv.Document())
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	out["format"], out["revision"], out["exported_at"] = ExportFormat, sv.Revision(), time.Now().UnixMilli()
	w.Header().Set("Content-Disposition", `attachment; filename="llm-scheduler-config.json"`)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) cfgImport(w http.ResponseWriter, r *http.Request) {
	sv := s.svc(w)
	if sv == nil {
		return
	}
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if f, has := obj["format"].(string); has && f != ExportFormat {
		apiErr(w, http.StatusBadRequest, "unknown export format %q (this scheduler reads %s)", f, ExportFormat)
		return
	}
	for _, k := range []string{"format", "revision", "exported_at"} {
		delete(obj, k)
	}
	b, _ := json.Marshal(obj)
	doc, err := config.ParseDocument(b)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	o, ok := s.writeOpts(w, r, comment)
	if !ok {
		return
	}
	res, err := sv.Import(r.Context(), o, doc, truthy(r.URL.Query().Get("replace")))
	if err != nil {
		s.cfgErr(w, err)
		return
	}
	s.respondWrite(w, http.StatusOK, res, nil)
}

func (s *Server) auditStore(w http.ResponseWriter) AuditStore {
	if s.opts.Audit == nil {
		apiErr(w, http.StatusServiceUnavailable, "the audit log is not available (no database)")
		return nil
	}
	return s.opts.Audit
}

// timeParam accepts unix milliseconds or an RFC 3339 time.
func timeParam(v string) (int64, bool) {
	if v == "" {
		return 0, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	st := s.auditStore(w)
	if st == nil {
		return
	}
	q := r.URL.Query()
	since, ok1 := timeParam(q.Get("since"))
	until, ok2 := timeParam(q.Get("until"))
	if !ok1 || !ok2 {
		apiErr(w, http.StatusBadRequest, "since and until are unix milliseconds or RFC 3339 times")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := cfgstore.Filter{Since: since, Until: until, Source: q.Get("source"), Actor: q.Get("actor"), Section: q.Get("section"),
		Target: q.Get("target"), Action: q.Get("action"), Query: q.Get("q"), Limit: limit, Ascending: strings.EqualFold(q.Get("order"), "asc")}
	es, total, err := st.Query(r.Context(), f)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": es, "total_matching": total})
}

func (s *Server) auditGet(w http.ResponseWriter, r *http.Request) {
	st := s.auditStore(w)
	if st == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "the audit id must be a number")
		return
	}
	e, err := st.GetEntry(r.Context(), id)
	if errors.Is(err, cfgstore.ErrNotFound) {
		apiErr(w, http.StatusNotFound, "no audit entry %d", id)
		return
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ---- runtime audit (load and unload) ----

// auditRuntime records a load or unload asked for over REST or MCP. It is
// best effort and never delays or fails the action: entries go through a
// bounded queue to one writer goroutine; when the queue is full or the
// database is down the entry is dropped with a warning.
func (s *Server) auditRuntime(r *http.Request, action, target string, after map[string]any) {
	if s.opts.Audit == nil || s.auditQ == nil {
		return
	}
	e := audit.Entry{Action: action, Section: audit.SectionRuntime, Target: target, Before: json.RawMessage("null"), After: raw(after)}
	audit.FromRequest(r).Apply(&e)
	if run := benchRunHeader(r.Header.Get("X-Bench-Run")); run != "" {
		// A load or unload made by a benchmark run (the runner calls the API on
		// loopback) is the system working, not a person: it stays in the log,
		// as source "system", so it is visible but never mistaken for a human
		// action. The history record of the load carries data.bench_run too.
		e.Source, e.Actor = audit.SourceSystem, "bench run "+run
		merged := map[string]any{"bench_run": run}
		for k, v := range after {
			merged[k] = v
		}
		e.After = raw(merged)
	}
	e.TS = time.Now().UnixMilli()
	select {
	case s.auditQ <- e:
	default:
		s.log.Warn("audit queue full; a runtime action was not recorded", "action", action, "target", target)
	}
}

func raw(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) startAudit() {
	if s.opts.Audit == nil {
		return
	}
	s.auditQ = make(chan audit.Entry, 256)
	s.auditQuit = make(chan struct{})
	s.auditDone = make(chan struct{})
	rec := func(e audit.Entry) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.opts.Audit.Record(ctx, e); err != nil {
			s.log.Warn("audit entry not recorded (model serving is not affected)", "err", err)
		}
	}
	go func() {
		defer close(s.auditDone)
		for {
			select {
			case e := <-s.auditQ:
				rec(e)
			case <-s.auditQuit:
				for {
					select {
					case e := <-s.auditQ:
						rec(e)
					default:
						return
					}
				}
			}
		}
	}()
}

func (s *Server) stopAudit() {
	s.auditOnce.Do(func() {
		if s.auditQ != nil {
			close(s.auditQuit)
			<-s.auditDone
		}
	})
}

// configRevision is the revision in force (0 when nothing is configured).
func (s *Server) configRevision() int64 { return s.conf().Revision }

// PublishConfig announces a configuration change on the WebSocket config topic.
func (s *Server) PublishConfig(ev cfgsvc.ChangeEvent) {
	ids := ev.EntryIDs
	if ids == nil {
		ids = []int64{}
	}
	s.ws.publishConfig(map[string]any{"revision": ev.Revision, "audit_entry_ids": ids, "section": ev.Section, "target": ev.Target, "source": ev.Source, "actor": ev.Actor})
}
