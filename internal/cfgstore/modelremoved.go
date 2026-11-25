package cfgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

// Removing a model file for good (models slice 4). The tombstones live in
// their own table, model_trash_removed (step 3 of model_migrations), never in
// model_trash: an image from before slice 4 lists every model_trash row as a
// trash item, so a tombstone there would show as a deleted file again. A
// removal is three steps:
//
//  1. RemoveBegin, one transaction: the trash record turns removing (an
//     older image lists it as such and refuses to restore it) and the
//     tombstone is written, state removing;
//  2. the file is erased (modelfiles.RemoveTrashed);
//  3. RemoveFinish, one transaction: the trash record goes and the
//     tombstone turns removed, with the time.
//
// RemoveAbort undoes step 1 when step 2 did not erase the file. A start
// finds a record left removing by a kill and finishes or aborts it from
// whether the file is still there (api/trashremove.go).
const (
	TrashRemoving  = "removing" // model_trash and the tombstone: a removal is under way (or was killed)
	RemovedRemoved = "removed"  // the tombstone: the file was erased
)

// Why a file was removed.
const (
	RemovedByOwner    = "owner"    // "Remove for good" (the API or MCP)
	RemovedByClearing = "clearing" // the timed clearing after model_trash_days
	// RemovedByRepair: a start completed a removal whose record was removing
	// with the file gone but no tombstone (not a state a kill produces: a
	// damaged or hand-edited database).
	RemovedByRepair = "repair"
)

// RemovedRecord is the tombstone of a model file removed for good. The
// saved profile specs are not kept: the file they ran is gone, so they
// cannot be restored, and the configuration history keeps them (the revision
// that took them out is Revision). Their ids are kept for the record.
type RemovedRecord struct {
	ID             string
	File           string
	SizeBytes      int64
	DeletedAt      int64 // unix ms
	PurgeAt        int64 // unix ms, the date it was due to be cleared
	RemovedAt      int64 // unix ms, 0 while removing
	State          string
	Reason         string
	ProfileIDs     []string
	Revision       int64
	DeletedBy      string
	DeletedComment string
	RemovedBy      string
	Comment        string
	UpdatedAt      int64
}

const removedCols = `id, file, size_bytes, deleted_at, purge_at, removed_at, state, reason, profile_ids, revision, deleted_by, deleted_comment, removed_by, comment, updated_at`

func scanRemoved(sc interface{ Scan(...any) error }) (*RemovedRecord, error) {
	var r RemovedRecord
	var ids string
	if err := sc.Scan(&r.ID, &r.File, &r.SizeBytes, &r.DeletedAt, &r.PurgeAt, &r.RemovedAt, &r.State, &r.Reason, &ids, &r.Revision,
		&r.DeletedBy, &r.DeletedComment, &r.RemovedBy, &r.Comment, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.ProfileIDs = []string{}
	if err := json.Unmarshal([]byte(ids), &r.ProfileIDs); err != nil {
		return nil, err
	}
	return &r, nil
}

// ErrNotTrashed: the record is not in state trashed (a removal starts only
// from there).
var ErrNotTrashed = errors.New("the trash record is not in the trash state")

// RemoveBegin is step 1: the trash record id (which must be trashed) turns
// removing and its tombstone is written, in one transaction. reason is
// RemovedByOwner or RemovedByClearing; by and comment sign the removal.
func (s *Store) RemoveBegin(id, reason, by, comment string, at int64) (*RemovedRecord, error) {
	var out *RemovedRecord
	err := s.db.Write(func(tx *sql.Tx) error {
		rec, err := scanTrash(tx.QueryRow(`SELECT `+trashCols+` FROM model_trash WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if rec.State != TrashTrashed {
			return ErrNotTrashed
		}
		ids := make([]string, 0, len(rec.Profiles))
		for k := range rec.Profiles {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		b, _ := json.Marshal(ids)
		if _, err := tx.Exec(`UPDATE model_trash SET state = ?, problem = '', updated_at = ? WHERE id = ?`, TrashRemoving, at, id); err != nil {
			return err
		}
		out = &RemovedRecord{ID: rec.ID, File: rec.File, SizeBytes: rec.SizeBytes, DeletedAt: rec.DeletedAt, PurgeAt: rec.PurgeAt, State: TrashRemoving,
			Reason: reason, ProfileIDs: ids, Revision: rec.Revision, DeletedBy: rec.Actor, DeletedComment: rec.Comment, RemovedBy: by, Comment: comment, UpdatedAt: at}
		_, err = tx.Exec(`INSERT INTO model_trash_removed (`+removedCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			out.ID, out.File, out.SizeBytes, out.DeletedAt, out.PurgeAt, 0, out.State, out.Reason, string(b), out.Revision,
			out.DeletedBy, out.DeletedComment, out.RemovedBy, out.Comment, at)
		return err
	})
	return out, err
}

// RemoveFinish is step 3: the file is gone; the trash record goes and the
// tombstone says removed, in one transaction. A record left removing without
// its tombstone (not a state a kill produces: step 1 writes both in one
// transaction; a damaged or hand-edited database) is completed too: the
// tombstone is written from the record, reason RemovedByRepair, signed
// potluck, so the record never stays removing. A tombstone already removed
// only has the record go. ErrNotFound: neither a tombstone removing nor a
// record removing.
func (s *Store) RemoveFinish(id string, at int64) error {
	return s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE model_trash_removed SET state = ?, removed_at = ?, updated_at = ? WHERE id = ? AND state = ?`, RemovedRemoved, at, at, id, TrashRemoving)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var state string
			err := tx.QueryRow(`SELECT state FROM model_trash_removed WHERE id = ?`, id).Scan(&state)
			switch {
			case errors.Is(err, sql.ErrNoRows): // no tombstone at all: write it from the record
				rec, err := scanTrash(tx.QueryRow(`SELECT `+trashCols+` FROM model_trash WHERE id = ? AND state = ?`, id, TrashRemoving))
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				if err != nil {
					return err
				}
				ids := make([]string, 0, len(rec.Profiles))
				for k := range rec.Profiles {
					ids = append(ids, k)
				}
				sort.Strings(ids)
				b, _ := json.Marshal(ids)
				if _, err := tx.Exec(`INSERT INTO model_trash_removed (`+removedCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					rec.ID, rec.File, rec.SizeBytes, rec.DeletedAt, rec.PurgeAt, at, RemovedRemoved, RemovedByRepair, string(b), rec.Revision,
					rec.Actor, rec.Comment, "potluck", "completed at start: the record was removing with the file gone and no tombstone", at); err != nil {
					return err
				}
			case err != nil:
				return err
			default: // a tombstone in another state (removed): only the record goes
				res, err := tx.Exec(`DELETE FROM model_trash WHERE id = ? AND state = ?`, id, TrashRemoving)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					return ErrNotFound
				}
				return nil
			}
		}
		_, err = tx.Exec(`DELETE FROM model_trash WHERE id = ? AND state = ?`, id, TrashRemoving)
		return err
	})
}

// RemoveAbort undoes step 1 (the file was not erased): the trash record is
// trashed again, with problem (empty: none), and the tombstone goes.
func (s *Store) RemoveAbort(id, problem string, at int64) error {
	return s.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM model_trash_removed WHERE id = ? AND state = ?`, id, TrashRemoving); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE model_trash SET state = ?, problem = ?, updated_at = ? WHERE id = ? AND state = ?`, TrashTrashed, problem, at, id, TrashRemoving)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RemovedGet reads one tombstone; ErrNotFound when there is none.
func (s *Store) RemovedGet(ctx context.Context, id string) (*RemovedRecord, error) {
	r, err := scanRemoved(s.db.DB().QueryRowContext(ctx, `SELECT `+removedCols+` FROM model_trash_removed WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RemovedList is every tombstone, the newest removal first (a removal under
// way, removed_at 0, first of all).
func (s *Store) RemovedList(ctx context.Context) ([]*RemovedRecord, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT `+removedCols+` FROM model_trash_removed ORDER BY removed_at = 0 DESC, removed_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RemovedRecord{}
	for rows.Next() {
		r, err := scanRemoved(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
