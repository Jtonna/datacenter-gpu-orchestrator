package cfgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Agent is one row of the agents table: an identity an AI agent presents with
// a key, so that what it does is attributed to it in the audit log. Only the
// SHA-256 of the key is stored. A revoked agent keeps its row, so old audit
// entries still resolve to a name.
type Agent struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	KeyHash       string `json:"-"`
	KeyPrefix     string `json:"key_prefix"`
	CreatedAt     int64  `json:"created_at"`
	CreatedSource string `json:"created_source"`
	CreatedActor  string `json:"created_actor"`
	RotatedAt     int64  `json:"rotated_at,omitempty"`
	LastUsedAt    int64  `json:"last_used_at,omitempty"`
	RevokedAt     int64  `json:"revoked_at,omitempty"`
	RequestCount  int64  `json:"request_count"`
}

const agentCols = `id, name, description, key_hash, key_prefix, created_at, created_source, created_actor, rotated_at, last_used_at, revoked_at, request_count`

func scanAgent(r interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	err := r.Scan(&a.ID, &a.Name, &a.Description, &a.KeyHash, &a.KeyPrefix, &a.CreatedAt, &a.CreatedSource, &a.CreatedActor, &a.RotatedAt, &a.LastUsedAt, &a.RevokedAt, &a.RequestCount)
	return a, err
}

// Agents lists every agent, revoked ones included, oldest first.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Agent returns one agent by id.
func (s *Store) Agent(ctx context.Context, id string) (*Agent, error) {
	a, err := scanAgent(s.db.DB().QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAgent inserts a new agent.
func (s *Store) CreateAgent(a Agent) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO agents (id, name, description, key_hash, key_prefix, created_at, created_source, created_actor) VALUES (?,?,?,?,?,?,?,?)`,
			a.ID, a.Name, a.Description, a.KeyHash, a.KeyPrefix, a.CreatedAt, a.CreatedSource, a.CreatedActor)
		return err
	})
}

func (s *Store) execOne(q string, args ...any) error {
	return s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpdateAgent renames or re-describes an agent.
func (s *Store) UpdateAgent(id, name, description string) error {
	return s.execOne(`UPDATE agents SET name = ?, description = ? WHERE id = ?`, name, description, id)
}

// RotateAgentKey replaces the key of an agent: the old key stops resolving.
func (s *Store) RotateAgentKey(id, hash, prefix string, at int64) error {
	return s.execOne(`UPDATE agents SET key_hash = ?, key_prefix = ?, rotated_at = ? WHERE id = ? AND revoked_at = 0`, hash, prefix, at, id)
}

// RevokeAgent marks an agent revoked. The row stays.
func (s *Store) RevokeAgent(id string, at int64) error {
	return s.execOne(`UPDATE agents SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, at, id)
}

// TouchAgent adds n requests and moves last_used_at forward.
func (s *Store) TouchAgent(id string, n, at int64) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE agents SET request_count = request_count + ?, last_used_at = MAX(last_used_at, ?) WHERE id = ?`, n, at, id)
		return err
	})
}

// AgentActions counts the audit entries of each agent id.
func (s *Store) AgentActions(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT agent_id, COUNT(*) FROM audit_log WHERE agent_id <> '' GROUP BY agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ---- model icons ----

// Icon is a small image the owner set for a model ("model:<key>") or for a
// whole family of models ("family:<brand>").
type Icon struct {
	Key         string
	ContentType string
	Data        []byte
	ETag        string
	UpdatedAt   int64
}

// Icon returns the stored icon under key.
func (s *Store) Icon(ctx context.Context, key string) (*Icon, error) {
	i := Icon{Key: key}
	err := s.db.DB().QueryRowContext(ctx, `SELECT content_type, data, etag, updated_at FROM model_icons WHERE key = ?`, key).Scan(&i.ContentType, &i.Data, &i.ETag, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// IconTags returns key -> etag of every stored icon (no image data).
func (s *Store) IconTags(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT key, etag FROM model_icons`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, e string
		if err := rows.Scan(&k, &e); err != nil {
			return nil, err
		}
		out[k] = e
	}
	return out, rows.Err()
}

// PutIcon stores (or replaces) an icon.
func (s *Store) PutIcon(i Icon) error {
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO model_icons (key, content_type, data, etag, updated_at) VALUES (?,?,?,?,?)
		  ON CONFLICT(key) DO UPDATE SET content_type = excluded.content_type, data = excluded.data, etag = excluded.etag, updated_at = excluded.updated_at`,
			i.Key, i.ContentType, i.Data, i.ETag, i.UpdatedAt)
		return err
	})
}

// DeleteIcon removes an icon; ErrNotFound when there was none.
func (s *Store) DeleteIcon(key string) error {
	return s.execOne(`DELETE FROM model_icons WHERE key = ?`, key)
}

// ---- model file metadata (so a restart does not read every GGUF header again) ----

// FileMeta fills into with the remembered metadata of a model file when its
// size and modification time are still the ones it was read at.
func (s *Store) FileMeta(path string, size, mtime int64, into any) bool {
	var meta string
	err := s.db.DB().QueryRow(`SELECT meta FROM model_file_meta WHERE path = ? AND size = ? AND mtime = ?`, path, size, mtime).Scan(&meta)
	return err == nil && json.Unmarshal([]byte(meta), into) == nil
}

// PutFileMeta remembers the metadata of a model file.
func (s *Store) PutFileMeta(path string, size, mtime int64, meta any) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO model_file_meta (path, size, mtime, meta, read_at) VALUES (?,?,?,?,?)
		  ON CONFLICT(path) DO UPDATE SET size = excluded.size, mtime = excluded.mtime, meta = excluded.meta, read_at = excluded.read_at`, path, size, mtime, string(b), now)
		return err
	})
}
