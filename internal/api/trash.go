package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelfiles"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/scheduler"
)

// Models slice 3: delete a model file to the trash, and restore it.
//
//	POST /api/models/delete/{key...}         {comment, dry_run?}       models
//	GET  /api/models/trash                                             observe
//	POST /api/models/trash/{id}/restore      {comment, rename_taken?}  models
//
// The file is renamed into <models>/.trash/<id>/ (the same file system: one
// atomic rename, never a copy) and back; nothing is ever removed here
// (removing for good is slice 4). Every profile that names the file leaves
// the configuration in one revision ("model deleted") and is kept with the
// trash record; a restore adds them back in one revision.
//
// Refused, with nothing changed:
//   - while any instance of the file is loaded, loading, draining or
//     stopping, or a request for one of its profiles is queued;
//   - while a benchmark run that touches the file is queued, running or paused;
//   - when the name is not one .gguf file in the models folder, or the file is
//     a link;
//   - when the models folder is read-only here (the first write, the trash
//     directory, fails before anything else is done).
//
// Crash safety: the record is written first (pending), then the revision,
// then the rename, then the record is marked trashed; a restore marks it
// restoring before the rename. A start repairs a record left pending or
// restoring from where the file is (repairTrash), and reports what it
// cannot tell.

// trashSummary is what the catalog's disk block shows of the trash.
type trashSummary struct {
	Bytes       int64
	Count       int
	NextPurgeAt int64 // unix ms, 0 = none
}

// trashState is the server's trash bookkeeping.
type trashState struct {
	mu      sync.Mutex
	sum     trashSummary
	repairs []string // what the last start repaired or could not
	op      sync.Mutex
	wr      writableRead // models_writable, read at most every writableEvery
}

// writableRead is the last answer of modelfiles.Writable for a folder.
type writableRead struct {
	dir      string
	at       time.Time
	writable bool
	ok       bool
}

// writableEvery is how often models_writable is read again: as often as the
// disk figures of the models folder (the diskstat reader's 30 s).
const writableEvery = 30 * time.Second

func (t *trashState) writable() writableRead {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.wr
}

func (t *trashState) setWritable(w writableRead) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wr = w
}

// modelsWritable says whether the models folder can be written here, read
// without writing anything (modelfiles.Writable: the mount's ro/rw from
// /proc/self/mountinfo, else access(W_OK)); ok is false when it cannot be
// told, and the field is then left out. The answer is kept for
// writableEvery: the first read is the first request after start.
func (s *Server) modelsWritable() (writable, ok bool) {
	dir := s.modelDir().Root
	if dir == "" {
		return false, false
	}
	if s.opts.modelsWritable != nil {
		return s.opts.modelsWritable(dir)
	}
	if w := s.trash.writable(); w.dir == dir && !w.at.IsZero() && time.Since(w.at) < writableEvery {
		return w.writable, w.ok
	}
	writable, ok = modelfiles.Writable(dir)
	s.trash.setWritable(writableRead{dir: dir, at: time.Now(), writable: writable, ok: ok})
	return writable, ok
}

func (t *trashState) summary() trashSummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sum
}

func (t *trashState) setSummary(sum trashSummary) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sum = sum
}

func (t *trashState) repairList() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.repairs...)
}

func (t *trashState) setRepairs(notes []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.repairs = notes
}

func (s *Server) modelDir() modelfiles.Dir {
	d := modelfiles.Dir{Ops: s.opts.trashOps}
	if c := s.conf(); c != nil {
		d.Root = c.ModelsDir
	}
	return d
}

// trashStop is the test hook that acts out a kill between two steps: true
// means "the process died here" (nothing after it runs, nothing is undone).
func (s *Server) trashStop(step string) bool {
	return s.opts.trashStop != nil && s.opts.trashStop(step)
}

func (s *Server) trashDays() int {
	if c := s.conf(); c != nil && c.ModelTrashDays > 0 {
		return c.ModelTrashDays
	}
	return config.DefaultModelTrashDays
}

func newTrashID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// trashRefuse answers a refusal with its reason and extra fields.
func trashRefuse(w http.ResponseWriter, status int, reason, msg string, extra map[string]any) {
	e := map[string]any{"message": msg, "type": errType(status), "param": nil, "code": status, "reason": reason}
	for k, v := range extra {
		e[k] = v
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func (s *Server) trashReady(w http.ResponseWriter) bool {
	if s.store == nil || s.opts.Config == nil {
		apiErr(w, http.StatusServiceUnavailable, "deleting and restoring model files need the database and the configuration service, which are not available")
		return false
	}
	if s.modelDir().Root == "" {
		apiErr(w, http.StatusServiceUnavailable, "no models folder is set")
		return false
	}
	return true
}

// readOnlyMsg is the refusal of a write to a read-only models folder.
func readOnlyMsg(err error) string {
	return fmt.Sprintf("the models folder is read-only here: nothing was changed (%v). Deleting and restoring model files need the scheduler's models folder mounted read-write.", modelfiles.ReadOnlyCause(err))
}

// docAt is the configuration document with the revision it is at.
func (s *Server) docAt() (*config.Document, int64) {
	sv := s.opts.Config
	for {
		rev := sv.Revision()
		d := sv.Document()
		if sv.Revision() == rev {
			return d, rev
		}
	}
}

// trashBusy says what keeps the file from being deleted now: instances,
// queued requests, benchmark runs.
func (s *Server) trashBusy(ctx context.Context, file string, profiles map[string]*config.ProfileSpec) ([]string, map[string]any, error) {
	var why []string
	extra := map[string]any{}
	if s.sched != nil {
		st, err := s.sched.Status(ctx)
		if err != nil {
			return nil, nil, err
		}
		var insts []map[string]any
		for _, i := range st.Instances {
			if _, mine := profiles[i.Profile]; mine || i.ModelFile == file {
				insts = append(insts, map[string]any{"profile": i.Profile, "state": i.State, "in_flight": i.InFlight})
				why = append(why, fmt.Sprintf("%s is %s", i.Profile, i.State))
			}
		}
		var queued []map[string]any
		for _, q := range st.Queue {
			if _, mine := profiles[q.Profile]; mine {
				queued = append(queued, map[string]any{"profile": q.Profile, "waiters": q.Waiters})
				why = append(why, fmt.Sprintf("%d request(s) for %s are queued", q.Waiters, q.Profile))
			}
		}
		if len(insts) > 0 {
			extra["instances"] = insts
		}
		if len(queued) > 0 {
			extra["queued"] = queued
		}
	}
	if s.opts.BenchStore != nil {
		runs, err := s.opts.BenchStore.ActiveRunsNaming(file, keysOfSpecs(profiles))
		if err != nil {
			return nil, nil, err
		}
		if len(runs) > 0 {
			extra["runs"] = runs
			why = append(why, fmt.Sprintf("benchmark run(s) %s use it", strings.Join(runs, ", ")))
		}
	}
	return why, extra, nil
}

// trashBusyNow is trashBusy without the benchmark runs: the instances and
// queued requests of the file now.
func (s *Server) trashBusyNow(ctx context.Context, file string, profiles map[string]*config.ProfileSpec) ([]string, map[string]any, error) {
	why, extra, err := s.trashBusy(ctx, file, profiles)
	if err != nil || extra["runs"] == nil {
		return why, extra, err
	}
	var keep []string
	for _, w := range why {
		if !strings.HasPrefix(w, "benchmark run") {
			keep = append(keep, w)
		}
	}
	return keep, extra, nil
}

// trashUnloadWait is how long a delete with interrupt waits for the file's
// instances to be gone: 5 s, the node force unload's bound.
func (s *Server) trashUnloadWait() time.Duration {
	if s.opts.trashUnloadWait > 0 {
		return s.opts.trashUnloadWait
	}
	return 5 * time.Second
}

// interruptFile force unloads every instance of the file (the existing
// forced unload: requests in flight are cut, a load in progress is stopped
// and its waiters answered 503 unloaded), cancels the queued requests of its
// profiles (scheduler CancelQueued, the same answer), then waits up to
// trashUnloadWait until no instance or queue entry of it is left. It
// returns what it interrupted and what is still there.
func (s *Server) interruptFile(ctx context.Context, file string, profiles map[string]*config.ProfileSpec) (map[string]any, []string) {
	insts := []map[string]any{}
	cut := 0
	var errs []string
	if st, err := s.sched.Status(ctx); err == nil {
		for _, i := range st.Instances {
			if _, mine := profiles[i.Profile]; !mine && i.ModelFile != file {
				continue
			}
			insts = append(insts, map[string]any{"profile": i.Profile, "state": i.State, "in_flight": i.InFlight, "waiters": i.Waiters})
			cut += i.InFlight + i.Waiters
			if err := s.sched.Unload(ctx, i.Profile, true); err != nil && !errors.Is(err, scheduler.ErrNotLoaded) {
				errs = append(errs, fmt.Sprintf("unload %s: %v", i.Profile, err))
			}
		}
	}
	cancelled, err := s.sched.CancelQueued(ctx, keysOfSpecs(profiles))
	if err != nil {
		errs = append(errs, fmt.Sprintf("queued requests: %v", err))
	}
	out := map[string]any{"instances": insts, "requests_cut": cut, "queued_cancelled": cancelled}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	deadline := time.Now().Add(s.trashUnloadWait())
	for {
		left, _, err := s.trashBusyNow(ctx, file, profiles)
		if err != nil {
			return out, []string{err.Error()}
		}
		if len(left) == 0 {
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, left
		}
		select {
		case <-ctx.Done():
			return out, append(left, ctx.Err().Error())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func keysOfSpecs(m map[string]*config.ProfileSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// trashedRecordOf is the newest record of file that is not restored.
func (s *Server) trashedRecordOf(ctx context.Context, file string) *cfgstore.TrashRecord {
	recs, _ := s.store.TrashList(ctx)
	for _, r := range recs {
		if r.File == file {
			return r
		}
	}
	return nil
}

func msText(ms int64) string { return time.UnixMilli(ms).UTC().Format("2 Jan 2006 15:04 UTC") }

// handleModelDelete is POST /api/models/delete/{key...}.
func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !s.trashReady(w) {
		return
	}
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if comment == "" {
		comment = audit.FromRequest(r).Comment
	}
	if comment == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	dry := truthy(r.URL.Query().Get("dry_run"))
	if v, ok := obj["dry_run"].(bool); ok {
		dry = dry || v
	}
	interrupt, _ := obj["interrupt"].(bool)
	if strings.HasPrefix(key, "command:") {
		trashRefuse(w, http.StatusBadRequest, "not_a_file", fmt.Sprintf("%s is a command profile: it has no model file to delete", key), nil)
		return
	}
	if err := modelfiles.ValidName(key); err != nil {
		trashRefuse(w, http.StatusBadRequest, "bad_name", err.Error(), nil)
		return
	}
	s.trash.op.Lock() // one delete or restore at a time
	defer s.trash.op.Unlock()
	ctx := r.Context()
	md := s.modelDir()
	st, err := md.Stat(key)
	switch {
	case errors.Is(err, modelfiles.ErrNotFound):
		if rec := s.trashedRecordOf(ctx, key); rec != nil {
			trashRefuse(w, http.StatusConflict, "in_trash", fmt.Sprintf("%s is in the trash since %s (trash id %s)", key, msText(rec.DeletedAt), rec.ID), map[string]any{"trash_id": rec.ID})
			return
		}
		trashRefuse(w, http.StatusNotFound, "not_found", fmt.Sprintf("no model file %s in the models folder", key), nil)
		return
	case errors.Is(err, modelfiles.ErrNotRegular):
		trashRefuse(w, http.StatusConflict, "not_regular", fmt.Sprintf("%v: Potluck deletes only plain files kept in the models folder", err), nil)
		return
	case err != nil:
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	doc, rev := s.docAt()
	profiles := cfgsvc.ProfilesOfFile(doc, key)
	why, extra, err := s.trashBusy(ctx, key, profiles)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	_, inRun := extra["runs"]
	switch {
	case len(why) > 0 && interrupt && inRun:
		trashRefuse(w, http.StatusConflict, "in_use", fmt.Sprintf("%s is in use and was not deleted: %s. interrupt does not stop benchmark runs: cancel them or wait for them first.", key, strings.Join(why, "; ")), extra)
		return
	case len(why) > 0 && !interrupt:
		trashRefuse(w, http.StatusConflict, "in_use", fmt.Sprintf("%s is in use and was not deleted: %s. Unload it (or wait for the runs) first, or delete with interrupt.", key, strings.Join(why, "; ")), extra)
		return
	}
	busy := len(why) > 0 // only instances and queued requests are left here: interrupt cuts them
	now := s.opts.Now().UnixMilli()
	days := s.trashDays()
	purgeAt := now + int64(days)*24*3600*1000
	ids := keysOfSpecs(profiles)
	figures := map[string]any{"file": key, "size_bytes": st.Size(), "profiles": ids, "trash_days": days, "purge_at": purgeAt, "purge_on": false}
	if busy {
		figures["would_interrupt"] = extra
	}
	if dry {
		figures["dry_run"] = true
		writeJSON(w, http.StatusOK, figures)
		return
	}

	// 1. the trash directory: the first write; a read-only folder stops here
	id := newTrashID()
	if err := md.PrepareTrash(id); err != nil {
		if modelfiles.IsReadOnly(err) {
			trashRefuse(w, http.StatusConflict, "read_only", readOnlyMsg(err), nil)
			return
		}
		apiErr(w, http.StatusInternalServerError, "the trash folder could not be made: %v", err)
		return
	}
	// 1b. interrupt: force unload the file's instances, cancel its queued
	// requests, and wait (bounded) until none is left; else nothing moves
	var interrupted map[string]any
	if busy {
		var left []string
		interrupted, left = s.interruptFile(ctx, key, profiles)
		if len(left) > 0 {
			md.RemoveEmptyTrashDir(id)
			trashRefuse(w, http.StatusConflict, "interrupt_incomplete", fmt.Sprintf("%s was not deleted: after the interrupt, within %s, still there: %s. Nothing was moved; the profiles are unchanged.",
				key, s.trashUnloadWait(), strings.Join(left, "; ")), map[string]any{"interrupted": interrupted, "still": left})
			return
		}
	}
	if s.trashStop("delete:after-interrupt") {
		apiErr(w, http.StatusInternalServerError, "stopped after the interrupt (test)")
		return
	}
	actor := audit.FromRequest(r).Actor
	rec := &cfgstore.TrashRecord{ID: id, File: key, SizeBytes: st.Size(), DeletedAt: now, PurgeAt: purgeAt, State: cfgstore.TrashPending,
		Profiles: profiles, Actor: actor, Comment: comment, UpdatedAt: now}
	// 2. the record (pending)
	if err := s.store.TrashInsert(rec); err != nil {
		md.RemoveEmptyTrashDir(id)
		apiErr(w, http.StatusInternalServerError, "the trash record could not be written; nothing was changed: %v", err)
		return
	}
	if s.trashStop("delete:after-record") {
		apiErr(w, http.StatusInternalServerError, "stopped after the record (test)")
		return
	}
	// 3. the profiles leave the configuration in one revision
	var res *cfgsvc.Result
	if len(ids) > 0 {
		o, ok := s.writeOpts(w, r, fmt.Sprintf("model deleted (%s): %s", key, comment))
		if !ok {
			s.undoDelete(rec, false)
			return
		}
		o.DryRun, o.Force, o.IfMatch = false, false, &rev
		res, err = s.opts.Config.RemoveProfiles(ctx, o, ids)
		if err != nil {
			s.undoDelete(rec, false)
			trashRefuse(w, http.StatusConflict, "config_refused", fmt.Sprintf("the profiles of %s could not be taken out of the configuration, so nothing was changed: %v", key, err), nil)
			return
		}
		s.store.TrashSetState(id, cfgstore.TrashPending, res.Revision, nil, "", now)
	}
	// 3b. something loaded or queued the file between the check and the
	// revision: undo the revision, nothing moves
	if again, _, err := s.trashBusyNow(ctx, key, profiles); err != nil || len(again) > 0 {
		s.undoDelete(rec, res != nil)
		msg := fmt.Sprintf("%s was not deleted: it was loaded again meanwhile (%s); the profiles were put back and nothing was moved", key, strings.Join(again, "; "))
		if err != nil {
			msg = fmt.Sprintf("%s was not deleted: its use could not be checked again (%v); the profiles were put back and nothing was moved", key, err)
		}
		trashRefuse(w, http.StatusConflict, "loaded_again", msg, nil)
		return
	}
	if s.trashStop("delete:after-config") {
		apiErr(w, http.StatusInternalServerError, "stopped after the configuration (test)")
		return
	}
	// 4. the rename
	if err := md.MoveToTrash(id, key); err != nil {
		s.undoDelete(rec, res != nil)
		if modelfiles.IsReadOnly(err) {
			trashRefuse(w, http.StatusConflict, "read_only", readOnlyMsg(err), nil)
			return
		}
		trashRefuse(w, http.StatusConflict, "move_failed", fmt.Sprintf("%s could not be moved to the trash; the profiles were put back: %v", key, err), nil)
		return
	}
	if s.trashStop("delete:after-rename") {
		apiErr(w, http.StatusInternalServerError, "stopped after the rename (test)")
		return
	}
	// 5. the record says trashed
	if err := s.store.TrashSetState(id, cfgstore.TrashTrashed, 0, nil, "", now); err != nil {
		s.log.Warn("model file moved to the trash, its record not completed: the next start completes it", "file", key, "trash_id", id, "err", err)
	}
	auditID := s.recordAuditWith(r, comment, audit.ActionDelete, audit.SectionModels, "model:"+key,
		map[string]any{"file": key, "size_bytes": st.Size(), "profiles": ids},
		map[string]any{"trash_id": id, "purge_at": purgeAt, "interrupted": interrupted})
	s.refreshTrash(ctx)
	s.catalogDirty(false)
	s.announce(r, audit.SectionModels, key, auditID)
	out := map[string]any{"trash_id": id, "file": key, "size_bytes": st.Size(), "deleted_at": now, "purge_at": purgeAt, "trash_days": days, "purge_on": false,
		"profiles_removed": ids, "revision": 0, "warnings": []string{}, "audit_id": auditID}
	if res != nil {
		out["revision"], out["warnings"] = res.Revision, res.Warnings
	}
	if interrupted != nil {
		out["interrupted"] = interrupted
	}
	writeJSON(w, http.StatusOK, out)
}

// undoDelete puts back what a delete did before its rename failed: the
// profiles (when the revision was written), the record, the empty trash
// directory. The file never moved.
func (s *Server) undoDelete(rec *cfgstore.TrashRecord, profiles bool) {
	ctx := context.Background()
	if profiles {
		if _, err := s.addMissingProfiles(ctx, rec.Profiles, fmt.Sprintf("model delete undone (%s): the file could not be moved", rec.File)); err != nil {
			s.log.Error("a model delete failed and its profiles could not be put back; the trash record keeps them", "file", rec.File, "trash_id", rec.ID, "err", err)
			s.store.TrashSetState(rec.ID, cfgstore.TrashInterrupted, 0, nil, "the delete failed before the file moved and the profiles could not be put back: "+err.Error(), s.opts.Now().UnixMilli())
			return
		}
	}
	s.store.TrashDelete(rec.ID)
	s.modelDir().RemoveEmptyTrashDir(rec.ID)
}

// addMissingProfiles adds the profiles of ps whose ids are not in the
// configuration now (in one revision); it returns the ids added.
func (s *Server) addMissingProfiles(ctx context.Context, ps map[string]*config.ProfileSpec, comment string) ([]string, error) {
	doc, _ := s.docAt()
	add := map[string]*config.ProfileSpec{}
	for id, p := range ps {
		if _, ok := doc.Profiles[id]; !ok {
			add[id] = p
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	o := cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceREST, Actor: "potluck (models trash)", Comment: comment}}
	if _, err := s.opts.Config.AddProfiles(ctx, o, add); err != nil {
		return nil, err
	}
	return keysOfSpecs(add), nil
}

// handleModelRestore is POST /api/models/trash/{id}/restore.
func (s *Server) handleModelRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.trashReady(w) {
		return
	}
	obj, comment, ok := bodyObject(w, r)
	if !ok {
		return
	}
	if comment == "" {
		comment = audit.FromRequest(r).Comment
	}
	if comment == "" {
		apiErr(w, http.StatusBadRequest, "a comment is required (body {\"comment\": \"...\"} or the X-Comment header)")
		return
	}
	renameTaken, _ := obj["rename_taken"].(bool)
	if !modelfiles.ValidID(id) {
		trashRefuse(w, http.StatusNotFound, "not_found", fmt.Sprintf("no trash item %s: see GET /api/models/trash", id), nil)
		return
	}
	s.trash.op.Lock()
	defer s.trash.op.Unlock()
	ctx := r.Context()
	rec, err := s.store.TrashGet(ctx, id)
	if errors.Is(err, cfgstore.ErrNotFound) {
		trashRefuse(w, http.StatusNotFound, "not_found", fmt.Sprintf("no trash item %s: see GET /api/models/trash", id), nil)
		return
	} else if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if rec.State != cfgstore.TrashTrashed {
		trashRefuse(w, http.StatusConflict, "not_restorable", fmt.Sprintf("trash item %s (%s) is %s, not in the trash: %s", id, rec.File, rec.State, rec.Problem), nil)
		return
	}
	if err := modelfiles.ValidName(rec.File); err != nil {
		trashRefuse(w, http.StatusConflict, "bad_name", err.Error(), nil)
		return
	}
	md := s.modelDir()
	if _, err := md.TrashStat(id, rec.File); err != nil {
		trashRefuse(w, http.StatusConflict, "missing_from_trash", fmt.Sprintf("%s is not in the trash folder (.trash/%s): %v", rec.File, id, err), nil)
		return
	}
	if _, err := md.Stat(rec.File); !errors.Is(err, modelfiles.ErrNotFound) {
		trashRefuse(w, http.StatusConflict, "file_exists", fmt.Sprintf("a file named %s is in the models folder again: move or delete it first, then restore", rec.File), nil)
		return
	}
	// the profiles: ids and aliases taken now
	doc, _ := s.docAt()
	add, renamed, dropped, taken := restorePlan(doc, rec.Profiles, renameTaken)
	if len(taken) > 0 {
		trashRefuse(w, http.StatusConflict, "profiles_taken", fmt.Sprintf("these names of %s's profiles are taken now: %s. Restore with rename_taken: true to add them as <id>-restored (taken aliases are left off)", rec.File, strings.Join(taken, ", ")), map[string]any{"taken": taken})
		return
	}
	now := s.opts.Now().UnixMilli()
	// 1. the record says restoring, with the profiles it will add
	if err := s.store.TrashSetState(id, cfgstore.TrashRestoring, 0, add, "", now); err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if s.trashStop("restore:after-record") {
		apiErr(w, http.StatusInternalServerError, "stopped after the record (test)")
		return
	}
	// 2. the rename back
	if err := md.RestoreFromTrash(id, rec.File); err != nil {
		s.store.TrashSetState(id, cfgstore.TrashTrashed, 0, map[string]*config.ProfileSpec{}, "", now)
		switch {
		case modelfiles.IsReadOnly(err):
			trashRefuse(w, http.StatusConflict, "read_only", readOnlyMsg(err), nil)
		case errors.Is(err, modelfiles.ErrExists):
			trashRefuse(w, http.StatusConflict, "file_exists", err.Error(), nil)
		default:
			apiErr(w, http.StatusInternalServerError, "%s could not be moved back: %v", rec.File, err)
		}
		return
	}
	if s.trashStop("restore:after-rename") {
		apiErr(w, http.StatusInternalServerError, "stopped after the rename (test)")
		return
	}
	// 3. the profiles come back in one revision
	var res *cfgsvc.Result
	if len(add) > 0 {
		o, ok := s.writeOpts(w, r, fmt.Sprintf("model restored (%s): %s", rec.File, comment))
		if !ok {
			s.undoRestore(rec)
			return
		}
		o.DryRun, o.Force, o.IfMatch = false, false, nil
		res, err = s.opts.Config.AddProfiles(ctx, o, add)
		if err != nil {
			s.undoRestore(rec)
			trashRefuse(w, http.StatusConflict, "config_refused", fmt.Sprintf("the profiles of %s could not be added back, so it stays in the trash: %v", rec.File, err), nil)
			return
		}
	}
	// 4. the record goes, then its empty directory
	if err := s.store.TrashDelete(id); err != nil {
		s.log.Warn("model file restored, its trash record not removed: the next start completes it", "file", rec.File, "trash_id", id, "err", err)
	}
	md.RemoveEmptyTrashDir(id)
	auditID := s.recordAuditWith(r, comment, audit.ActionCreate, audit.SectionModels, "model:"+rec.File,
		map[string]any{"trash_id": id, "deleted_at": rec.DeletedAt},
		map[string]any{"file": rec.File, "profiles": keysOfSpecs(add), "renamed": renamed, "aliases_dropped": dropped})
	s.refreshTrash(ctx)
	s.catalogDirty(false)
	s.announce(r, audit.SectionModels, rec.File, auditID)
	out := map[string]any{"file": rec.File, "trash_id": id, "profiles_restored": keysOfSpecs(add), "renamed": renamed, "aliases_dropped": dropped,
		"revision": 0, "audit_id": auditID}
	if res != nil {
		out["revision"] = res.Revision
	}
	writeJSON(w, http.StatusOK, out)
}

// undoRestore puts the file back in the trash after its profiles could not
// be added.
func (s *Server) undoRestore(rec *cfgstore.TrashRecord) {
	now := s.opts.Now().UnixMilli()
	if err := s.modelDir().MoveToTrash(rec.ID, rec.File); err != nil {
		s.log.Error("a restore failed after the file moved back and the file could not go back to the trash; the file stays in the models folder", "file", rec.File, "trash_id", rec.ID, "err", err)
		s.store.TrashSetState(rec.ID, cfgstore.TrashInterrupted, 0, nil, "a restore failed after the file was moved back; the file is in the models folder, its profiles were not added: "+err.Error(), now)
		return
	}
	s.store.TrashSetState(rec.ID, cfgstore.TrashTrashed, 0, map[string]*config.ProfileSpec{}, "", now)
}

// restorePlan is the profiles a restore adds: ids taken now are refused, or
// with renameTaken become <id>-restored; a taken alias is refused, or left
// off. taken lists what is in the way when not renaming.
func restorePlan(doc *config.Document, ps map[string]*config.ProfileSpec, renameTaken bool) (add map[string]*config.ProfileSpec, renamed map[string]string, dropped []string, taken []string) {
	used := map[string]bool{}
	for id, p := range doc.Profiles {
		used[id] = true
		if p != nil {
			for _, a := range p.Aliases {
				used[a] = true
			}
		}
	}
	add, renamed, dropped = map[string]*config.ProfileSpec{}, map[string]string{}, []string{}
	for _, id := range keysOfSpecs(ps) {
		c := *ps[id]
		nid := id
		if used[id] {
			if !renameTaken {
				taken = append(taken, "profile "+id)
			} else {
				nid = id + "-restored"
				if used[nid] {
					taken = append(taken, "profile "+nid)
				}
				renamed[id] = nid
			}
		}
		var keep []string
		for _, a := range c.Aliases {
			if used[a] {
				if !renameTaken {
					taken = append(taken, "alias "+a)
				} else {
					dropped = append(dropped, a)
				}
				continue
			}
			keep = append(keep, a)
		}
		c.Aliases = keep
		used[nid] = true
		add[nid] = &c
	}
	return add, renamed, dropped, taken
}

// handleTrashList is GET /api/models/trash.
func (s *Server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		apiErr(w, http.StatusServiceUnavailable, "the trash needs the database, which is not available")
		return
	}
	recs, err := s.store.TrashList(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	md := s.modelDir()
	items := []map[string]any{}
	sum := summarizeTrash(recs)
	for _, rec := range recs {
		profs := []map[string]any{}
		for _, id := range keysOfSpecs(rec.Profiles) {
			profs = append(profs, map[string]any{"id": id, "spec": rec.Profiles[id]})
		}
		_, ferr := md.TrashStat(rec.ID, rec.File)
		problem := rec.Problem
		if problem == "" && rec.State == cfgstore.TrashTrashed && ferr != nil {
			problem = fmt.Sprintf("the file is not in .trash/%s: %v", rec.ID, ferr)
		}
		items = append(items, map[string]any{"id": rec.ID, "file": rec.File, "size_bytes": rec.SizeBytes, "deleted_at": rec.DeletedAt,
			"purge_at": rec.PurgeAt, "state": rec.State, "profile_count": len(rec.Profiles), "profiles": profs, "revision": rec.Revision,
			"actor": rec.Actor, "comment": rec.Comment, "problem": nilIfEmpty(problem), "in_trash_folder": ferr == nil})
	}
	repairs := s.trash.repairList()
	out := map[string]any{"items": items, "count": sum.Count, "total_bytes": sum.Bytes, "next_purge_at": nilIfZero(sum.NextPurgeAt),
		"trash_days": s.trashDays(), "purge_on": false, "repairs_at_start": repairs}
	if wr, ok := s.modelsWritable(); ok {
		out["models_writable"] = wr
	}
	writeJSON(w, http.StatusOK, out)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// summarizeTrash counts the records whose file is in the trash.
func summarizeTrash(recs []*cfgstore.TrashRecord) trashSummary {
	var sum trashSummary
	for _, r := range recs {
		if r.State != cfgstore.TrashTrashed && r.State != cfgstore.TrashInterrupted {
			continue
		}
		sum.Bytes += r.SizeBytes
		sum.Count++
		if r.State == cfgstore.TrashTrashed && (sum.NextPurgeAt == 0 || r.PurgeAt < sum.NextPurgeAt) {
			sum.NextPurgeAt = r.PurgeAt
		}
	}
	return sum
}

// refreshTrash reads the trash summary for the catalog's disk block.
func (s *Server) refreshTrash(ctx context.Context) {
	if s.store == nil {
		return
	}
	recs, err := s.store.TrashList(ctx)
	if err != nil {
		s.log.Warn("the trash summary was not read", "err", err)
		return
	}
	s.trash.setSummary(summarizeTrash(recs))
}

// trashDisk adds the trash figures to the catalog's disk block.
func (s *Server) trashDisk(d map[string]any) {
	if s.store == nil {
		return
	}
	sum := s.trash.summary()
	d["trash_bytes"], d["trash_count"], d["next_purge_at"], d["trash_purge_on"] = sum.Bytes, sum.Count, nilIfZero(sum.NextPurgeAt), false
}

// RepairTrash is called once at start, after the configuration service
// applies its writes to the scheduler (main sets started first): a repair
// may add profiles back.
func (s *Server) RepairTrash(ctx context.Context) { s.repairTrash(ctx) }

// repairTrash runs at start: a record a kill left pending or restoring is
// finished or undone from where its file is; what it cannot tell is marked
// interrupted with what it found (GET /api/models/trash shows it), and no
// file is moved by the repair.
func (s *Server) repairTrash(ctx context.Context) {
	if s.store == nil {
		return
	}
	recs, err := s.store.TrashList(ctx)
	if err != nil {
		s.log.Warn("the trash records were not read at start", "err", err)
		return
	}
	md := s.modelDir()
	now := s.opts.Now().UnixMilli()
	var notes []string
	note := func(rec *cfgstore.TrashRecord, msg string, warn bool) {
		t := fmt.Sprintf("%s (trash id %s): %s", rec.File, rec.ID, msg)
		notes = append(notes, t)
		if warn {
			s.log.Warn("models trash at start: "+t, "trash_id", rec.ID)
		} else {
			s.log.Info("models trash at start: "+t, "trash_id", rec.ID)
		}
	}
	interrupted := func(rec *cfgstore.TrashRecord, msg string) {
		s.store.TrashSetState(rec.ID, cfgstore.TrashInterrupted, 0, nil, msg, now)
		note(rec, "needs a look: "+msg, true)
	}
	for _, rec := range recs {
		if rec.State != cfgstore.TrashPending && rec.State != cfgstore.TrashRestoring {
			continue
		}
		_, inErr := md.Stat(rec.File)
		_, trErr := md.TrashStat(rec.ID, rec.File)
		inFolder, inTrash := inErr == nil, trErr == nil
		where := fmt.Sprintf("in the folder: %v, in .trash/%s: %v", inFolder, rec.ID, inTrash)
		switch {
		case rec.State == cfgstore.TrashPending && inTrash && !inFolder:
			s.store.TrashSetState(rec.ID, cfgstore.TrashTrashed, 0, nil, "", now)
			note(rec, "a delete stopped after the move; its record is completed (in the trash)", false)
		case rec.State == cfgstore.TrashPending && inFolder && !inTrash:
			if s.opts.Config == nil {
				interrupted(rec, "a delete stopped before the move; the file is in the folder; its profiles could not be checked (no configuration service): "+where)
				continue
			}
			added, err := s.addMissingProfiles(ctx, rec.Profiles, fmt.Sprintf("model delete undone at start (%s): it stopped before the file moved", rec.File))
			if err != nil {
				interrupted(rec, "a delete stopped before the move; the file is in the folder; its profiles could not be put back ("+err.Error()+"); they are kept here")
				continue
			}
			s.store.TrashDelete(rec.ID)
			md.RemoveEmptyTrashDir(rec.ID)
			note(rec, fmt.Sprintf("a delete stopped before the move; undone: the file stayed in the folder, profiles put back: %v", added), false)
		case rec.State == cfgstore.TrashRestoring && inFolder && !inTrash:
			if s.opts.Config == nil {
				interrupted(rec, "a restore stopped after the move; the file is in the folder; its profiles could not be added (no configuration service)")
				continue
			}
			ps := rec.RestoreProfiles
			if len(ps) == 0 {
				ps = rec.Profiles
			}
			added, err := s.addMissingProfiles(ctx, ps, fmt.Sprintf("model restore completed at start (%s)", rec.File))
			if err != nil {
				interrupted(rec, "a restore stopped after the move; the file is in the folder; its profiles could not be added ("+err.Error()+"); they are kept here")
				continue
			}
			s.store.TrashDelete(rec.ID)
			md.RemoveEmptyTrashDir(rec.ID)
			note(rec, fmt.Sprintf("a restore stopped after the move; completed: profiles added: %v", added), false)
		case rec.State == cfgstore.TrashRestoring && inTrash && !inFolder:
			s.store.TrashSetState(rec.ID, cfgstore.TrashTrashed, 0, map[string]*config.ProfileSpec{}, "", now)
			note(rec, "a restore stopped before the move; it stays in the trash", false)
		default:
			interrupted(rec, "a "+map[bool]string{true: "delete", false: "restore"}[rec.State == cfgstore.TrashPending]+" stopped and the file is not in exactly one place ("+where+"); nothing was moved")
		}
	}
	s.trash.setRepairs(notes)
	s.refreshTrash(ctx)
}
