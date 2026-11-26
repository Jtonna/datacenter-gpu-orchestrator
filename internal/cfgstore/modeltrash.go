package cfgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// Trash record states (models slice 3). A delete writes the record first
// (pending), then the configuration revision that takes the file's
// profiles out, then renames the file into the trash, then marks the record
// trashed. A restore marks it restoring, renames the file back, adds the
// profiles back, then removes the record. A start finds any record left
// pending or restoring by a kill and repairs it from where the file is
// (api/trash.go, repairTrash).
const (
	TrashPending     = "pending"     // a delete is under way (or was killed)
	TrashTrashed     = "trashed"     // the file is in .trash/<id>/
	TrashRestoring   = "restoring"   // a restore is under way (or was killed)
	TrashInterrupted = "interrupted" // a start could not tell where the file is: Problem says what it found
	// TrashHeldBack: the file was restored (it is in the models folder) but
	// some of its profiles could not be added; Profiles holds only those,
	// Problem says why each was held back. A restore call adds them later.
	TrashHeldBack = "held_back"
)

// TrashRecord is one model file in the trash.
type TrashRecord struct {
	ID        string
	File      string
	SizeBytes int64
	DeletedAt int64 // unix ms
	PurgeAt   int64 // unix ms: deleted_at + model_trash_days (the purge itself is slice 4)
	State     string
	// Profiles are the configurations that named the file when it was
	// deleted, as they were; RestoreProfiles the ones a restore under way
	// adds back (ids may carry -restored).
	Profiles        map[string]*config.ProfileSpec
	RestoreProfiles map[string]*config.ProfileSpec
	Revision        int64 // the configuration revision that took the profiles out (0: none)
	Actor, Comment  string
	Problem         string
	UpdatedAt       int64
}

const trashCols = `id, file, size_bytes, deleted_at, purge_at, state, profiles, restore_profiles, revision, actor, comment, problem, updated_at`

func scanTrash(sc interface{ Scan(...any) error }) (*TrashRecord, error) {
	var r TrashRecord
	var profs, rprofs string
	if err := sc.Scan(&r.ID, &r.File, &r.SizeBytes, &r.DeletedAt, &r.PurgeAt, &r.State, &profs, &rprofs, &r.Revision, &r.Actor, &r.Comment, &r.Problem, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Profiles, r.RestoreProfiles = map[string]*config.ProfileSpec{}, map[string]*config.ProfileSpec{}
	if err := json.Unmarshal([]byte(profs), &r.Profiles); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rprofs), &r.RestoreProfiles); err != nil {
		return nil, err
	}
	return &r, nil
}

func profilesJSON(m map[string]*config.ProfileSpec) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// TrashInsert writes a new record.
func (s *Store) TrashInsert(r *TrashRecord) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO model_trash (`+trashCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.File, r.SizeBytes, r.DeletedAt, r.PurgeAt, r.State, profilesJSON(r.Profiles), profilesJSON(r.RestoreProfiles),
			r.Revision, r.Actor, r.Comment, r.Problem, r.UpdatedAt)
		return err
	})
}

// TrashGet reads one record; ErrNotFound when there is none.
func (s *Store) TrashGet(ctx context.Context, id string) (*TrashRecord, error) {
	r, err := scanTrash(s.db.DB().QueryRowContext(ctx, `SELECT `+trashCols+` FROM model_trash WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// TrashList is every record, newest first.
func (s *Store) TrashList(ctx context.Context) ([]*TrashRecord, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT `+trashCols+` FROM model_trash ORDER BY deleted_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TrashRecord
	for rows.Next() {
		r, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TrashSetState moves a record to state, with the revision (when not 0),
// the restore profiles (when not nil) and the problem text.
func (s *Store) TrashSetState(id, state string, revision int64, restore map[string]*config.ProfileSpec, problem string, at int64) error {
	q := `UPDATE model_trash SET state = ?, problem = ?, updated_at = ?`
	args := []any{state, problem, at}
	if revision != 0 {
		q += `, revision = ?`
		args = append(args, revision)
	}
	if restore != nil {
		q += `, restore_profiles = ?`
		args = append(args, profilesJSON(restore))
	}
	return s.execOne(q+` WHERE id = ?`, append(args, id)...)
}

// TrashHoldBack turns a record into a held-back one: the file is back in
// the folder, held are the profiles not added (they replace Profiles), why
// says why each was held back.
func (s *Store) TrashHoldBack(id string, held map[string]*config.ProfileSpec, why string, at int64) error {
	return s.execOne(`UPDATE model_trash SET state = ?, profiles = ?, restore_profiles = '{}', problem = ?, updated_at = ? WHERE id = ?`,
		TrashHeldBack, profilesJSON(held), why, at, id)
}

// TrashDelete removes a record (the file is back in the folder, or the
// delete never moved it).
func (s *Store) TrashDelete(id string) error {
	return s.execOne(`DELETE FROM model_trash WHERE id = ?`, id)
}
