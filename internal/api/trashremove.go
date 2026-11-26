package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelfiles"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/supervise"
)

// Models slice 4: remove a model file from the trash for good, and the
// timed clearing.
//
//	POST /api/models/trash/{id}/remove   {comment, dry_run?}   models
//
// The only file Potluck ever erases is <models>/.trash/<id>/<file>, through
// modelfiles.RemoveTrashed. Only a record in state trashed whose file is in
// its trash folder, a plain regular file of the size recorded at the delete,
// is removed. The order: the trash record turns removing and its tombstone
// is written (one transaction), the file is erased, then the record goes
// and the tombstone says removed (one transaction). A start finishes or
// undoes a removal a kill cut, from whether the file is still there.
//
// The clearing (setting model_trash_purge, off by default) runs at start
// and once an hour: it removes up to trashClearPerPass files whose clearing
// date has passed, each with a history event and an audit entry signed
// potluck. It does nothing, quietly, while the models folder is read-only.
//
// A file's clearing date is the later of its purge_at (set at the delete)
// and its deleted_at plus model_trash_days now: raising the setting keeps
// every file longer, lowering it never clears a file sooner than the date it
// was given (clearAt).

const (
	trashClearEvery   = time.Hour
	trashClearPerPass = 3
	trashClearActor   = "potluck"
)

func (s *Server) trashPurgeOn() bool {
	c := s.conf()
	return c != nil && c.ModelTrashPurge
}

// clearAt is when rec may be cleared, with model_trash_days at days.
func clearAt(rec *cfgstore.TrashRecord, days int) int64 {
	at := rec.DeletedAt + int64(days)*dayMS
	if rec.PurgeAt > at {
		return rec.PurgeAt
	}
	return at
}

// removeRefusal is why a removal did not happen (nothing was erased).
type removeRefusal struct {
	status        int
	reason, msg   string
	extra         map[string]any
	readOnly      bool
	problemStored bool // the record now carries the problem
}

// removeTrashed erases rec's file and leaves its tombstone. rec must be
// trashed. dry: every check, nothing written (a read-only folder is then
// reported, not refused: the answer says read_only). The caller holds
// s.trash.op.
func (s *Server) removeTrashed(ctx context.Context, rec *cfgstore.TrashRecord, reason, by, comment string, dry bool) (*cfgstore.RemovedRecord, bool, *removeRefusal) {
	md := s.modelDir()
	if err := modelfiles.ValidName(rec.File); err != nil {
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "bad_name", msg: err.Error()}
	}
	st, err := md.TrashStat(rec.ID, rec.File)
	switch {
	case errors.Is(err, modelfiles.ErrNotFound):
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "missing_from_trash",
			msg: fmt.Sprintf("%s is not in its trash folder (.trash/%s): there is nothing to erase, and nothing was changed (%v)", rec.File, rec.ID, err)}
	case errors.Is(err, modelfiles.ErrUnsafe), errors.Is(err, modelfiles.ErrNotRegular):
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "not_regular",
			msg: fmt.Sprintf("%s was not removed: %v. Potluck erases only a plain file in a plain trash folder; nothing was changed", rec.File, err)}
	case err != nil:
		return nil, false, &removeRefusal{status: http.StatusInternalServerError, reason: "stat_failed", msg: fmt.Sprintf("%s could not be checked: %v", rec.File, err)}
	case st.Size() != rec.SizeBytes:
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "changed",
			msg: fmt.Sprintf("%s in .trash/%s is %d bytes, %d were deleted: it is not the file that was deleted, so it was not removed", rec.File, rec.ID, st.Size(), rec.SizeBytes)}
	}
	if ids := s.profilesInTrashFolder(rec.ID, rec.File); len(ids) > 0 {
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "in_use_by_profile", extra: map[string]any{"profiles": ids},
			msg: fmt.Sprintf("%s (trash id %s) was not removed: profile(s) %s name a file in its trash folder (.trash/%s/). Change or delete them first; nothing was changed", rec.File, rec.ID, strings.Join(ids, ", "), rec.ID)}
	}
	readOnly := false
	if err := md.TrashWritable(rec.ID); err != nil {
		if !modelfiles.IsReadOnly(err) {
			return nil, false, &removeRefusal{status: http.StatusConflict, reason: "not_regular", msg: fmt.Sprintf("%s was not removed: %v", rec.File, err)}
		}
		if !dry {
			return nil, false, &removeRefusal{status: http.StatusConflict, reason: "read_only", msg: readOnlyMsg(err), readOnly: true}
		}
		readOnly = true
	}
	if dry {
		return nil, readOnly, nil
	}
	if s.trashStop("remove:before-record") {
		return nil, false, &removeRefusal{status: http.StatusInternalServerError, reason: "stopped", msg: "stopped before the record (test)"}
	}
	now := s.opts.Now().UnixMilli()
	// 1. the record says removing; the tombstone is written
	tomb, err := s.store.RemoveBegin(rec.ID, reason, by, comment, now)
	if errors.Is(err, cfgstore.ErrNotTrashed) || errors.Is(err, cfgstore.ErrNotFound) {
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "not_removable", msg: fmt.Sprintf("trash item %s (%s) changed meanwhile and was not removed", rec.ID, rec.File)}
	} else if err != nil {
		return nil, false, &removeRefusal{status: http.StatusInternalServerError, reason: "record_failed", msg: fmt.Sprintf("the removal could not be recorded, so nothing was erased: %v", err)}
	}
	if s.trashStop("remove:after-record") {
		return nil, false, &removeRefusal{status: http.StatusInternalServerError, reason: "stopped", msg: "stopped after the record (test)"}
	}
	// 2. the file is erased
	if err := md.RemoveTrashed(rec.ID, rec.File, rec.SizeBytes); err != nil {
		ro := modelfiles.IsReadOnly(err)
		problem := ""
		if !ro {
			problem = "a removal for good failed and nothing was erased: " + err.Error()
		}
		if aerr := s.store.RemoveAbort(rec.ID, problem, s.opts.Now().UnixMilli()); aerr != nil {
			s.log.Warn("a removal failed and its record was not put back; the next start settles it", "file", rec.File, "trash_id", rec.ID, "err", aerr)
		}
		if ro {
			return nil, false, &removeRefusal{status: http.StatusConflict, reason: "read_only", msg: readOnlyMsg(err), readOnly: true}
		}
		return nil, false, &removeRefusal{status: http.StatusConflict, reason: "remove_failed", problemStored: problem != "",
			msg: fmt.Sprintf("%s was not removed, nothing was erased: %v", rec.File, err)}
	}
	if s.trashStop("remove:after-unlink") {
		return nil, false, &removeRefusal{status: http.StatusInternalServerError, reason: "stopped", msg: "stopped after the file was erased (test)"}
	}
	// 3. the record goes; the tombstone says removed
	now = s.opts.Now().UnixMilli()
	if err := s.store.RemoveFinish(rec.ID, now); err != nil {
		s.log.Warn("model file erased, its record not completed: the next start completes it", "file", rec.File, "trash_id", rec.ID, "err", err)
	}
	if s.trashStop("remove:after-finish") {
		return nil, false, &removeRefusal{status: http.StatusInternalServerError, reason: "stopped", msg: "stopped after the record was completed (test)"}
	}
	tomb.State, tomb.RemovedAt, tomb.UpdatedAt = cfgstore.RemovedRemoved, now, now
	md.RemoveEmptyTrashDir(rec.ID)
	s.recordRemoved(tomb)
	s.refreshTrash(ctx)
	s.catalogDirty(false)
	return tomb, false, nil
}

// recordRemoved writes the history event of a removal.
func (s *Server) recordRemoved(t *cfgstore.RemovedRecord) { s.recordRemovedEvent(t, false) }

// recordRemovedEvent writes the modelRemoved event of a removal; atStart:
// written by a start for a removal whose event a kill may have lost.
func (s *Server) recordRemovedEvent(t *cfgstore.RemovedRecord, atStart bool) {
	if s.hist == nil {
		return
	}
	how := "removed for good from the models trash by " + t.RemovedBy
	if t.Reason == cfgstore.RemovedByClearing {
		how = fmt.Sprintf("cleared from the models trash (in it since %s; model_trash_days)", msText(t.DeletedAt))
	}
	sum := fmt.Sprintf("%s was %s: %.1f GB freed. It and %s cannot be restored.", t.File, how, float64(t.SizeBytes)/1e9, itsProfiles(len(t.ProfileIDs)))
	data := map[string]any{"file": t.File, "trash_id": t.ID, "size_bytes": t.SizeBytes, "reason": t.Reason, "profiles": t.ProfileIDs,
		"deleted_at": t.DeletedAt, "removed_by": t.RemovedBy}
	if atStart {
		sum += fmt.Sprintf(" (Written at start for a removal made at %s by %s.)", msText(t.RemovedAt), t.RemovedBy)
		data["written_at_start"] = true
	}
	s.hist.Record(history.Event{Ts: t.RemovedAt, Type: history.TModelRemoved, Severity: history.Info, ModelFile: t.File, Nodes: []string{}, Summary: sum, Data: data})
}

// markAudited sets a tombstone's audited mark once its audit entry is
// written (auditID > 0; its event came before it). Unset, the next start
// writes what is missing (auditMissing).
func (s *Server) markAudited(t *cfgstore.RemovedRecord, auditID int64) {
	if auditID <= 0 || s.store == nil {
		return
	}
	if err := s.store.RemovedSetAudited(t.ID); err != nil {
		s.log.Warn("a removal's audit entry is written, its mark is not: the next start sets it", "file", t.File, "trash_id", t.ID, "err", err)
	}
}

// auditMissing runs at start: an erased model file never lacks its audit
// entry. For each removed tombstone without the audited mark (a kill came
// between the removal and the mark) it writes the modelRemoved event and
// the audit entry, signed potluck, saying they were written at start for a
// removal made at <time> by <who>, then sets the mark. Each is looked up
// first by the tombstone's id (the event's trash_id; the audit entry's
// target and trash_id), so a kill between writing them and the mark never
// gives two.
func (s *Server) auditMissing(ctx context.Context, note func(*cfgstore.TrashRecord, string, bool)) {
	if s.store == nil || s.opts.Audit == nil {
		return
	}
	tombs, err := s.store.RemovedUnaudited(ctx)
	if err != nil {
		s.log.Warn("the removed model files were not checked for their audit entries at start", "err", err)
		return
	}
	for _, t := range tombs {
		wrote := []string{}
		if s.hist != nil && !s.removedEventKept(t.ID) {
			s.recordRemovedEvent(t, true)
			s.hist.Flush()
			wrote = append(wrote, "history event")
		}
		target := "model:" + t.File
		_, n, err := s.opts.Audit.Query(ctx, cfgstore.Filter{Section: audit.SectionModels, Action: audit.ActionRemove, Target: target, Query: `"trash_id":"` + t.ID + `"`, Limit: 1})
		if err != nil {
			s.log.Warn("a removal's audit entry was not looked up at start; tried again at the next start", "file", t.File, "trash_id", t.ID, "err", err)
			continue
		}
		id := int64(1)
		if n == 0 {
			before, after := removedAudit(t)
			id = s.recordSystemAudit(fmt.Sprintf("written at start for a removal made at %s by %s (%s: %s)", msText(t.RemovedAt), t.RemovedBy, t.Reason, t.Comment),
				audit.ActionRemove, audit.SectionModels, target, before, after)
			wrote = append(wrote, "audit entry")
		}
		if s.trashStop("audit-at-start:after-audit") {
			return
		}
		s.markAudited(t, id)
		if len(wrote) > 0 {
			note(&cfgstore.TrashRecord{ID: t.ID, File: t.File}, "removed for good at "+msText(t.RemovedAt)+" by "+t.RemovedBy+"; its "+strings.Join(wrote, " and ")+" written at start", false)
		}
	}
}

// removedEventKept: the history keeps a modelRemoved event of trash item id.
func (s *Server) removedEventKept(id string) bool {
	found := false
	s.hist.ScanType(history.TModelRemoved, func(e history.Event) {
		if v, _ := e.Data["trash_id"].(string); v == id {
			found = true
		}
	})
	return found
}

// removedAudit is the audit entry of a removal: before, what was in the
// trash; after, the tombstone.
func removedAudit(t *cfgstore.RemovedRecord) (before, after map[string]any) {
	return map[string]any{"file": t.File, "trash_id": t.ID, "size_bytes": t.SizeBytes, "deleted_at": t.DeletedAt, "deleted_by": t.DeletedBy, "profiles": t.ProfileIDs},
		map[string]any{"removed": true, "removed_at": t.RemovedAt, "reason": t.Reason}
}

// recordSystemAudit writes an audit entry signed potluck (source system):
// the clearing and the start's repair.
func (s *Server) recordSystemAudit(comment, action, section, target string, before, after any) int64 {
	if s.opts.Audit == nil {
		return 0
	}
	e := audit.Entry{Action: action, Section: section, Target: target, Before: raw(before), After: raw(after), TS: time.Now().UnixMilli(), Comment: comment,
		Source: audit.SourceSystem, Actor: trashClearActor}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := s.opts.Audit.Record(ctx, e)
	if err != nil {
		s.log.Warn("audit entry not recorded (the change itself was made)", "action", action, "target", target, "err", err)
		return 0
	}
	ids := []int64{}
	if id > 0 {
		ids = append(ids, id)
	}
	s.ws.publishConfig(map[string]any{"revision": s.configRevision(), "audit_entry_ids": ids, "section": section, "target": target, "source": audit.SourceSystem, "actor": trashClearActor})
	return id
}

func (rf *removeRefusal) write(w http.ResponseWriter) {
	trashRefuse(w, rf.status, rf.reason, rf.msg, rf.extra)
}

// handleTrashRemove is POST /api/models/trash/{id}/remove.
func (s *Server) handleTrashRemove(w http.ResponseWriter, r *http.Request) {
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
	dry := truthy(r.URL.Query().Get("dry_run"))
	if v, ok := obj["dry_run"].(bool); ok {
		dry = dry || v
	}
	if !modelfiles.ValidID(id) {
		trashRefuse(w, http.StatusNotFound, "not_found", fmt.Sprintf("no trash item %s: see GET /api/models/trash", id), nil)
		return
	}
	s.trash.op.Lock() // one delete, restore or removal at a time
	defer s.trash.op.Unlock()
	ctx := r.Context()
	rec, err := s.store.TrashGet(ctx, id)
	if errors.Is(err, cfgstore.ErrNotFound) {
		if t, terr := s.store.RemovedGet(ctx, id); terr == nil && t.State == cfgstore.RemovedRemoved {
			trashRefuse(w, http.StatusGone, "already_removed", fmt.Sprintf("%s (trash id %s) was removed for good on %s by %s", t.File, id, msText(t.RemovedAt), t.RemovedBy), nil)
			return
		}
		trashRefuse(w, http.StatusNotFound, "not_found", fmt.Sprintf("no trash item %s: see GET /api/models/trash", id), nil)
		return
	} else if err != nil {
		apiErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	md := s.modelDir()
	switch rec.State {
	case cfgstore.TrashTrashed:
	case cfgstore.TrashInterrupted:
		_, inErr := md.Stat(rec.File)
		_, trErr := md.TrashStat(rec.ID, rec.File)
		advice := "Look at the models folder by hand. "
		switch {
		case inErr == nil && trErr != nil:
			advice += "The file is in the models folder: it is an ordinary model file again; to get rid of it, delete it from the models page and remove it from the trash then."
		case trErr == nil && inErr != nil:
			advice += fmt.Sprintf("The file is in .trash/%s/ and nowhere else, but the record is not sure how it got there, so Potluck does not erase it; restore needs the record settled first.", rec.ID)
		default:
			advice += "The file is not in exactly one place; nothing will be erased until that is settled by hand."
		}
		trashRefuse(w, http.StatusConflict, "needs_a_look", fmt.Sprintf("%s (trash id %s) was not removed: its record needs a look (%s). %s Nothing was changed.", rec.File, id, rec.Problem, advice),
			map[string]any{"problem": rec.Problem, "in_folder": inErr == nil, "in_trash_folder": trErr == nil})
		return
	default:
		trashRefuse(w, http.StatusConflict, "busy", fmt.Sprintf("%s (trash id %s) is %s: a delete, restore or removal of it is under way or was cut by a stop (the next start settles it); nothing was changed", rec.File, id, rec.State), nil)
		return
	}
	now := s.opts.Now().UnixMilli()
	at := clearAt(rec, s.trashDays())
	ids := keysOfSpecs(rec.Profiles)
	daysEarly := int64(0)
	if at > now {
		daysEarly = (at - now + dayMS - 1) / dayMS
	}
	figures := map[string]any{"file": rec.File, "trash_id": id, "size_bytes": rec.SizeBytes, "profiles": ids, "deleted_at": rec.DeletedAt, "purge_at": at,
		"days_early": daysEarly, "cannot_undo": true, "purge_on": s.trashPurgeOn(),
		"message": fmt.Sprintf("This cannot be undone. %s and %s are erased now, %d days before they would have been. %.1f GB is freed.", rec.File, itsProfiles(len(ids)), daysEarly, float64(rec.SizeBytes)/1e9)}
	if dry {
		_, ro, rf := s.removeTrashed(ctx, rec, cfgstore.RemovedByOwner, audit.FromRequest(r).Actor, comment, true)
		if rf != nil {
			rf.write(w)
			return
		}
		figures["dry_run"], figures["read_only"] = true, ro
		writeJSON(w, http.StatusOK, figures)
		return
	}
	tomb, _, rf := s.removeTrashed(ctx, rec, cfgstore.RemovedByOwner, audit.FromRequest(r).Actor, comment, false)
	if rf != nil {
		rf.write(w)
		return
	}
	before, after := removedAudit(tomb)
	auditID := s.recordAuditWith(r, comment, audit.ActionRemove, audit.SectionModels, "model:"+rec.File, before, after)
	if s.trashStop("remove:after-audit") {
		trashRefuse(w, http.StatusInternalServerError, "stopped", "stopped after the audit entry (test)", nil)
		return
	}
	s.markAudited(tomb, auditID)
	s.announce(r, audit.SectionModels, rec.File, auditID)
	writeJSON(w, http.StatusOK, map[string]any{"file": rec.File, "trash_id": id, "removed_at": tomb.RemovedAt, "freed_bytes": tomb.SizeBytes,
		"profiles_removed": tomb.ProfileIDs, "reason": tomb.Reason, "cannot_undo": true, "audit_id": auditID})
}

// startTrashClearing starts the clearing task once (RepairTrash calls it,
// after the start's repair).
func (s *Server) startTrashClearing() {
	if s.store == nil || s.opts.trashClearEvery < 0 { // < 0: off (tests)
		return
	}
	every := orDur(s.opts.trashClearEvery, trashClearEvery)
	s.bgWG.Add(1)
	supervise.Go(s.bg, supervise.Spec{Name: "api.trashclear", Policy: supervise.Restart, Done: s.bgWG.Done}, func(ctx context.Context) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.clearTrash(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// clearTrash is one pass of the clearing: the files whose clearing date has
// passed, the soonest first, at most trashClearPerPass. It returns the files
// it removed. Off (model_trash_purge false), or on a read-only folder, it
// does nothing (the read-only case is logged once, not each hour).
func (s *Server) clearTrash(ctx context.Context) []string {
	removed := []string{}
	md := s.modelDir()
	if !s.trashPurgeOn() || s.store == nil || md.Root == "" {
		return removed
	}
	recs, err := s.store.TrashList(ctx)
	if err != nil {
		s.log.Warn("the models trash clearing could not read the trash", "err", err)
		return removed
	}
	now, days := s.opts.Now().UnixMilli(), s.trashDays()
	var due []*cfgstore.TrashRecord
	for _, r := range recs {
		if r.State == cfgstore.TrashTrashed && now >= clearAt(r, days) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return removed
	}
	if err := md.TrashWritable(""); err != nil {
		if s.trash.noteClearBlocked(err.Error()) {
			s.log.Info("the models trash clearing waits: the models folder cannot be written here", "due", len(due), "err", modelfiles.ReadOnlyCause(err))
		}
		return removed
	}
	s.trash.noteClearBlocked("")
	sort.SliceStable(due, func(i, j int) bool { return clearAt(due[i], days) < clearAt(due[j], days) })
	for _, r := range due {
		if len(removed) >= trashClearPerPass || ctx.Err() != nil {
			break
		}
		if file := s.clearOne(ctx, r.ID); file != "" {
			removed = append(removed, file)
		}
	}
	return removed
}

// clearOne removes one due file under the trash lock, after reading its
// record again; "" when it did not.
func (s *Server) clearOne(ctx context.Context, id string) string {
	s.trash.op.Lock()
	defer s.trash.op.Unlock()
	rec, err := s.store.TrashGet(ctx, id)
	if err != nil || rec.State != cfgstore.TrashTrashed || s.opts.Now().UnixMilli() < clearAt(rec, s.trashDays()) || !s.trashPurgeOn() {
		return ""
	}
	days := s.trashDays()
	comment := fmt.Sprintf("cleared after %d days in the trash (model_trash_days %d, model_trash_purge on)", (s.opts.Now().UnixMilli()-rec.DeletedAt)/dayMS, days)
	tomb, _, rf := s.removeTrashed(ctx, rec, cfgstore.RemovedByClearing, trashClearActor, comment, false)
	if rf != nil {
		if rf.readOnly {
			s.trash.noteClearBlocked(rf.msg)
			return ""
		}
		if s.trash.noteClearFailed(id) {
			s.log.Warn("the models trash clearing did not remove a file; it tries again each hour (logged once)", "file", rec.File, "trash_id", id, "reason", rf.reason, "msg", rf.msg)
		}
		return ""
	}
	before, after := removedAudit(tomb)
	s.markAudited(tomb, s.recordSystemAudit(comment, audit.ActionRemove, audit.SectionModels, "model:"+rec.File, before, after))
	s.log.Info("models trash: cleared", "file", rec.File, "trash_id", id, "size_bytes", rec.SizeBytes)
	return rec.File
}

// repairRemoving settles a removal a kill cut (the record is removing):
// the file still in the trash: undone (the record is trashed again; the
// clearing, if on, tries again); the file gone: completed (the record goes,
// the tombstone says removed, with its history event and audit entry signed
// potluck); anything else: the record is marked interrupted with what was
// found. It never erases a file.
func (s *Server) repairRemoving(ctx context.Context, rec *cfgstore.TrashRecord, now int64, note func(*cfgstore.TrashRecord, string, bool)) {
	md := s.modelDir()
	_, inErr := md.Stat(rec.File)
	_, trErr := md.TrashStat(rec.ID, rec.File)
	switch {
	case trErr == nil:
		if err := s.store.RemoveAbort(rec.ID, "", now); err != nil {
			note(rec, "a removal stopped before the file was erased; its record could not be put back: "+err.Error(), true)
			return
		}
		note(rec, "a removal stopped before the file was erased; it stays in the trash", false)
	case errors.Is(trErr, modelfiles.ErrNotFound) && errors.Is(inErr, modelfiles.ErrNotFound):
		if err := s.store.RemoveFinish(rec.ID, now); err != nil {
			note(rec, "a removal stopped after the file was erased; its record could not be completed: "+err.Error(), true)
			return
		}
		md.RemoveEmptyTrashDir(rec.ID)
		if t, err := s.store.RemovedGet(ctx, rec.ID); err == nil {
			s.recordRemoved(t)
			before, after := removedAudit(t)
			s.markAudited(t, s.recordSystemAudit(fmt.Sprintf("completed at start (removal by %s: %s)", t.RemovedBy, t.Comment), audit.ActionRemove, audit.SectionModels, "model:"+t.File, before, after))
		}
		note(rec, "a removal stopped after the file was erased; completed (removed for good)", false)
	default:
		where := fmt.Sprintf("in the folder: %v, in .trash/%s: %v (%v)", inErr == nil, rec.ID, trErr == nil, trErr)
		msg := "a removal stopped and the file is not where it can be settled (" + where + "); nothing was erased"
		if err := s.store.RemoveAbort(rec.ID, msg, now); err == nil {
			s.store.TrashSetState(rec.ID, cfgstore.TrashInterrupted, 0, nil, msg, now)
		}
		note(rec, "needs a look: "+msg, true)
	}
}

// noteClearBlocked records why the clearing waits ("" once it can run);
// true when that changed, so it is logged once.
func (t *trashState) noteClearBlocked(why string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := t.clearBlocked != why
	t.clearBlocked = why
	return changed && why != ""
}

// noteClearFailed is true the first time the clearing fails on id.
func (t *trashState) noteClearFailed(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.clearFailed == nil {
		t.clearFailed = map[string]bool{}
	}
	first := !t.clearFailed[id]
	t.clearFailed[id] = true
	return first
}

// itsProfiles is "its 1 profile" or "its N profiles" (the removal sentences).
func itsProfiles(n int) string {
	if n == 1 {
		return "its 1 profile"
	}
	return fmt.Sprintf("its %d profiles", n)
}
