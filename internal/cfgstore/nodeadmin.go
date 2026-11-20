package cfgstore

import (
	"database/sql"
	"fmt"
)

// NodeAdmin is the operator state of one agent node (agent contract 7.2 and
// 10): paused, and removed until re-admitted. It survives restarts.
type NodeAdmin struct {
	Paused  bool
	Removed bool
}

// NodeAdmins returns every node with operator state.
func (s *Store) NodeAdmins() (map[string]NodeAdmin, error) {
	rows, err := s.db.DB().Query(`SELECT id, paused, removed FROM node_admin`)
	if err != nil {
		return nil, fmt.Errorf("cfgstore: node admin: %w", err)
	}
	defer rows.Close()
	out := map[string]NodeAdmin{}
	for rows.Next() {
		var id string
		var p, r int
		if err := rows.Scan(&id, &p, &r); err != nil {
			return nil, err
		}
		out[id] = NodeAdmin{Paused: p != 0, Removed: r != 0}
	}
	return out, rows.Err()
}

// SetNodeAdmin stores a node's operator state; the zero value deletes it.
func (s *Store) SetNodeAdmin(id string, a NodeAdmin) error {
	return s.db.Write(func(tx *sql.Tx) error {
		if !a.Paused && !a.Removed {
			_, err := tx.Exec(`DELETE FROM node_admin WHERE id = ?`, id)
			return err
		}
		_, err := tx.Exec(`INSERT INTO node_admin (id, paused, removed, updated_at) VALUES (?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET paused = excluded.paused, removed = excluded.removed, updated_at = excluded.updated_at`,
			id, b2i(a.Paused), b2i(a.Removed), s.now().UnixMilli())
		return err
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
