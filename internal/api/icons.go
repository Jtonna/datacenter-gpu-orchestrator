package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
)

// Model icons: a small image the owner sets for one model, or for every model
// of a family (?scope=family), kept in the database. The UI draws its own
// avatar when none is set.
//
//	PUT    /api/models/{key}/icon[?scope=family]   raw image body, Content-Type image/png|jpeg|webp|svg+xml
//	GET    /api/models/{key}/icon                  the model's icon, else its family's; 404 when neither is set
//	DELETE /api/models/{key}/icon[?scope=family]
//
// A change is recorded in the audit log (section "models").

// MaxIconBytes is the largest icon accepted.
const MaxIconBytes = 256 << 10

// iconPath splits "<key>/icon" (the wildcard of /api/models/{key...}).
func iconPath(p string) (key string, ok bool) {
	if strings.HasSuffix(p, "/icon") && len(p) > len("/icon") {
		return strings.TrimSuffix(p, "/icon"), true
	}
	return "", false
}

var svgBad = regexp.MustCompile(`(?i)<\s*script|<\s*foreignObject|<\s*iframe|<\s*embed|<\s*object|<!ENTITY|<!DOCTYPE[^>]*\[|\bon[a-z]+\s*=|javascript:|data:text/html|<\?xml-stylesheet`)
var svgRemote = regexp.MustCompile(`(?i)(href|src)\s*=\s*["']\s*(https?:)?//|url\(\s*["']?\s*(https?:)?//|@import`)

// sniffIcon decides what an uploaded image is from its bytes (the declared
// content type is not trusted) and refuses what is not a small PNG, JPEG, WebP
// or script-free SVG.
func sniffIcon(b []byte) (contentType string, err error) {
	switch {
	case len(b) == 0:
		return "", errors.New("the body is empty: send the image bytes")
	case len(b) > MaxIconBytes:
		return "", fmt.Errorf("the image is %d kB; an icon may be at most %d kB", len(b)>>10, MaxIconBytes>>10)
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", nil
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "image/jpeg", nil
	case len(b) > 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp", nil
	}
	// SVG: it must parse as XML with an <svg> root, and carry nothing that runs or loads
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	root := ""
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("the image is not a PNG, JPEG, WebP or well-formed SVG")
		}
		if se, ok := tok.(xml.StartElement); ok && root == "" {
			root = se.Name.Local
		}
	}
	if root != "svg" {
		return "", errors.New("the image is not a PNG, JPEG, WebP or SVG")
	}
	if m := svgBad.Find(b); m != nil {
		return "", fmt.Errorf("the SVG is refused because it contains %q: an icon must not carry scripts, event handlers or embedded documents", strings.TrimSpace(string(m)))
	}
	if svgRemote.Match(b) {
		return "", errors.New("the SVG is refused because it refers to another host: an icon must be self-contained")
	}
	return "image/svg+xml", nil
}

// iconTarget resolves the storage key of a request: the model's own icon, or
// with ?scope=family the icon of its family.
func (s *Server) iconTarget(w http.ResponseWriter, r *http.Request, key string) (storeKey, brand string, ok bool) {
	if s.store == nil {
		apiErr(w, http.StatusServiceUnavailable, "icons need the database, which is not available")
		return "", "", false
	}
	snap, err := s.catalogGet(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return "", "", false
	}
	raw, found := snap.raw[key]
	if !found {
		apiErr(w, http.StatusNotFound, "no model %s: see GET /api/models", key)
		return "", "", false
	}
	var e struct {
		Brand string `json:"brand"`
	}
	_ = json.Unmarshal(raw, &e)
	switch scope := r.URL.Query().Get("scope"); scope {
	case "", "model":
		return IconModelKey(key), e.Brand, true
	case "family":
		if e.Brand == "" || e.Brand == "other" || e.Brand == "command" {
			apiErr(w, http.StatusBadRequest, "this model belongs to no known family, so an icon can only be set for the model itself")
			return "", "", false
		}
		return IconFamilyKey(e.Brand), e.Brand, true
	default:
		apiErr(w, http.StatusBadRequest, "scope must be model or family, got %q", scope)
		return "", "", false
	}
}

func (s *Server) handleIconGet(w http.ResponseWriter, r *http.Request, key string) {
	if s.store == nil {
		apiErr(w, http.StatusNotFound, "no icon is set for %s", key)
		return
	}
	ic, err := s.store.Icon(r.Context(), IconModelKey(key))
	if errors.Is(err, cfgstore.ErrNotFound) {
		ic, err = s.store.Icon(r.Context(), IconFamilyKey(s.brandOfKey(r.Context(), key)))
	}
	if errors.Is(err, cfgstore.ErrNotFound) {
		apiErr(w, http.StatusNotFound, "no icon is set for %s", key)
		return
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h := w.Header()
	tag := `"` + ic.ETag + `"`
	h.Set("ETag", tag)
	// the UI asks with ?v=<etag>: that address never changes its content
	if r.URL.Query().Get("v") == ic.ETag {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ic.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	// an SVG opened on its own must not run or load anything either
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Write(ic.Data)
}

// brandOfKey is the family of a catalog key ("" when the catalog cannot say).
func (s *Server) brandOfKey(ctx context.Context, key string) string {
	snap, err := s.catalogGet(ctx)
	if err != nil {
		return ""
	}
	var e struct {
		Brand string `json:"brand"`
	}
	_ = json.Unmarshal(snap.raw[key], &e)
	return e.Brand
}

func (s *Server) handleIconPut(w http.ResponseWriter, r *http.Request) {
	key, ok := iconPath(r.PathValue("key"))
	if !ok {
		apiErr(w, http.StatusNotFound, "no such path: %s (a model's icon is at /api/models/<key>/icon)", r.URL.Path)
		return
	}
	storeKey, brand, ok := s.iconTarget(w, r, key)
	if !ok {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxIconBytes+1))
	if err != nil {
		apiErr(w, http.StatusRequestEntityTooLarge, "an icon may be at most %d kB", MaxIconBytes>>10)
		return
	}
	ct, err := sniffIcon(b)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	sum := sha256.Sum256(b)
	ic := cfgstore.Icon{Key: storeKey, ContentType: ct, Data: b, ETag: hex.EncodeToString(sum[:8]), UpdatedAt: time.Now().UnixMilli()}
	var before any
	if old, err := s.store.Icon(r.Context(), storeKey); err == nil {
		before = map[string]any{"content_type": old.ContentType, "bytes": len(old.Data), "etag": old.ETag}
	}
	if err := s.store.PutIcon(ic); err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	after := map[string]any{"content_type": ct, "bytes": len(b), "etag": ic.ETag}
	action := audit.ActionCreate
	if before != nil {
		action = audit.ActionUpdate
	}
	id := s.recordAudit(r, action, audit.SectionModels, iconAuditTarget(storeKey, key), before, after)
	s.catalogDirty(false)
	s.announce(r, audit.SectionModels, key, id)
	scope := "model"
	if strings.HasPrefix(storeKey, "family:") {
		scope = "family"
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "scope": scope, "family": brand, "icon": map[string]any{"etag": ic.ETag, "scope": scope, "content_type": ct, "bytes": len(b)}, "audit_id": id})
}

func (s *Server) handleIconDelete(w http.ResponseWriter, r *http.Request) {
	key, ok := iconPath(r.PathValue("key"))
	if !ok {
		apiErr(w, http.StatusNotFound, "no such path: %s (a model's icon is at /api/models/<key>/icon)", r.URL.Path)
		return
	}
	storeKey, _, ok := s.iconTarget(w, r, key)
	if !ok {
		return
	}
	old, err := s.store.Icon(r.Context(), storeKey)
	if errors.Is(err, cfgstore.ErrNotFound) {
		apiErr(w, http.StatusNotFound, "no icon is set there")
		return
	}
	if err == nil {
		err = s.store.DeleteIcon(storeKey)
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	id := s.recordAudit(r, audit.ActionDelete, audit.SectionModels, iconAuditTarget(storeKey, key), map[string]any{"content_type": old.ContentType, "bytes": len(old.Data), "etag": old.ETag}, nil)
	s.catalogDirty(false)
	s.announce(r, audit.SectionModels, key, id)
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "removed": true, "audit_id": id})
}

func iconAuditTarget(storeKey, key string) string {
	if strings.HasPrefix(storeKey, "family:") {
		return "icon of the " + strings.TrimPrefix(storeKey, "family:") + " family"
	}
	return "icon of " + key
}

// recordAudit writes one audit entry of a change made outside the
// configuration document (an icon, an agent) and returns its id (0 when the
// log is not available or the write failed: the change itself was made).
func (s *Server) recordAudit(r *http.Request, action, section, target string, before, after any) int64 {
	if s.opts.Audit == nil {
		return 0
	}
	e := audit.Entry{Action: action, Section: section, Target: target, Before: raw(before), After: raw(after), TS: time.Now().UnixMilli()}
	audit.FromRequest(r).Apply(&e)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := s.opts.Audit.Record(ctx, e)
	if err != nil {
		s.log.Warn("audit entry not recorded (the change itself was made)", "action", action, "target", target, "err", err)
		return 0
	}
	return id
}

// announce tells the live channel's config topic that something audited
// changed, so an open Audit or MCP screen refreshes (the revision is unchanged).
func (s *Server) announce(r *http.Request, section, target string, id int64) {
	o := audit.FromRequest(r)
	ids := []int64{}
	if id > 0 {
		ids = append(ids, id)
	}
	s.ws.publishConfig(map[string]any{"revision": s.configRevision(), "audit_entry_ids": ids, "section": section, "target": target, "source": o.Source, "actor": o.Actor})
}
