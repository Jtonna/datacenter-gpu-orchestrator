package cfgstore

import (
	"database/sql"
	"fmt"
)

// Key access (expiry, permissions, the key's last four characters) lives in
// its own table with its own version ledger, not in new columns of agents
// and not as a cfg_migrations step: an older image refuses to open a
// database whose cfg_migrations is ahead of it, and it never reads these
// two tables, so going back to it stays a plain image swap. An agent with
// no access row is a key made before permissions existed (or by an older
// image): full permissions, no expiry.
var accessMigrations = []string{`
CREATE TABLE agent_access (
  agent_id    TEXT PRIMARY KEY,
  key_hash    TEXT NOT NULL,
  key_last4   TEXT NOT NULL,
  expires_at  INTEGER NOT NULL DEFAULT 0,
  permissions TEXT NOT NULL CHECK (json_valid(permissions)),
  updated_at  INTEGER NOT NULL,
  warned      TEXT NOT NULL DEFAULT ''
);
`}

// AccessVersion is the version of the access tables this build writes.
func AccessVersion() int { return len(accessMigrations) }

func migrateAccess(tx *sql.Tx, now int64) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS access_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM access_migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(accessMigrations) {
		// A newer image wrote them: keys still resolve here (as identity
		// with the permissions this build can read), nothing is lost.
		return nil
	}
	for i := have; i < len(accessMigrations); i++ {
		if _, err := tx.Exec(accessMigrations[i]); err != nil {
			return fmt.Errorf("access migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO access_migrations (version, applied_at) VALUES (?,?)`, i+1, now); err != nil {
			return err
		}
	}
	return nil
}

// AgentAccess is an agent's row of agent_access.
type AgentAccess struct {
	// KeyHash is the hash of the key KeyLast4 belongs to: when an older
	// image rotated the key, it differs from the agent's and the hint falls
	// back to the key prefix.
	KeyHash   string
	KeyLast4  string
	ExpiresAt int64  // unix ms; 0 never
	Grants    string // JSON {observe, zones, models, settings}
	UpdatedAt int64
	// Warned lists the expiry events already sent for this key and expiry
	// ("7d", "1d", "expired", comma separated); cleared when the expiry or
	// the key changes.
	Warned string
}

const accessCols = `x.key_hash, x.key_last4, x.expires_at, x.permissions, x.updated_at, x.warned`

func accessJoin() string {
	return `SELECT ` + agentColsA + `, ` + accessCols + ` FROM agents a LEFT JOIN agent_access x ON x.agent_id = a.id`
}

const agentColsA = `a.id, a.name, a.description, a.key_hash, a.key_prefix, a.created_at, a.created_source, a.created_actor, a.rotated_at, a.last_used_at, a.revoked_at, a.request_count`

func scanAgentAccess(r interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var kh, l4, perms, warned sql.NullString
	var exp, upd sql.NullInt64
	err := r.Scan(&a.ID, &a.Name, &a.Description, &a.KeyHash, &a.KeyPrefix, &a.CreatedAt, &a.CreatedSource, &a.CreatedActor, &a.RotatedAt, &a.LastUsedAt, &a.RevokedAt, &a.RequestCount,
		&kh, &l4, &exp, &perms, &upd, &warned)
	if err == nil && perms.Valid {
		a.Access = &AgentAccess{KeyHash: kh.String, KeyLast4: l4.String, ExpiresAt: exp.Int64, Grants: perms.String, UpdatedAt: upd.Int64, Warned: warned.String}
	}
	return a, err
}

// CreateAgentAccess inserts a new agent and its access row in one write.
func (s *Store) CreateAgentAccess(a Agent, x AgentAccess) error {
	return s.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO agents (id, name, description, key_hash, key_prefix, created_at, created_source, created_actor) VALUES (?,?,?,?,?,?,?,?)`,
			a.ID, a.Name, a.Description, a.KeyHash, a.KeyPrefix, a.CreatedAt, a.CreatedSource, a.CreatedActor); err != nil {
			return err
		}
		return putAccess(tx, a.ID, x)
	})
}

func putAccess(tx *sql.Tx, id string, x AgentAccess) error {
	_, err := tx.Exec(`INSERT INTO agent_access (agent_id, key_hash, key_last4, expires_at, permissions, updated_at, warned) VALUES (?,?,?,?,?,?,?)
  ON CONFLICT(agent_id) DO UPDATE SET key_hash = excluded.key_hash, key_last4 = excluded.key_last4, expires_at = excluded.expires_at,
  permissions = excluded.permissions, updated_at = excluded.updated_at, warned = excluded.warned`,
		id, x.KeyHash, x.KeyLast4, x.ExpiresAt, x.Grants, x.UpdatedAt, x.Warned)
	return err
}

// UpdateAgentAccess changes an agent that is not revoked: its name and
// description, and its access row (written whole). ErrNotFound when there
// is no such agent or it is revoked.
func (s *Store) UpdateAgentAccess(id, name, description string, x AgentAccess) error {
	return s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE agents SET name = ?, description = ? WHERE id = ? AND revoked_at = 0`, name, description, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return putAccess(tx, id, x)
	})
}

// RotateAgentAccess replaces the key of an agent that is not revoked, and
// the hint of its access row.
func (s *Store) RotateAgentAccess(id, hash, prefix string, at int64, x AgentAccess) error {
	return s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE agents SET key_hash = ?, key_prefix = ?, rotated_at = ? WHERE id = ? AND revoked_at = 0`, hash, prefix, at, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return putAccess(tx, id, x)
	})
}

// MarkAgentWarned records which expiry events were sent for an agent.
func (s *Store) MarkAgentWarned(id, warned string) error {
	return s.execOne(`UPDATE agent_access SET warned = ? WHERE agent_id = ?`, warned, id)
}
