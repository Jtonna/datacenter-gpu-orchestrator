package startups

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DB is what the store needs from the scheduler database (bench.Store): the
// shared connection and its single writer. The start-up tables are
// versioned on their own, as the configuration tables are.
type DB interface {
	DB() *sql.DB
	Write(fn func(*sql.Tx) error) error
}

// migrations are applied in order, once each. Add to the end; never edit one
// that has shipped.
var migrations = []string{`
CREATE TABLE startups (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id        TEXT NOT NULL,
  agent_start_id TEXT NOT NULL,
  registered_at  INTEGER NOT NULL,
  kind           TEXT NOT NULL,
  record         TEXT NOT NULL CHECK (json_valid(record))
);
CREATE UNIQUE INDEX startups_start ON startups (node_id, agent_start_id);
CREATE INDEX startups_node ON startups (node_id, id);
`}

// Store keeps the start-up records in the scheduler database, at most
// KeepPerNode per node.
type Store struct {
	db DB
}

// Open applies the migrations and returns the store.
func Open(db DB) (*Store, error) {
	err := db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS startup_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM startup_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			return fmt.Errorf("the start-up tables are at version %d but this build knows %d", have, len(migrations))
		}
		for i := have; i < len(migrations); i++ {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return fmt.Errorf("start-up migration %d: %w", i+1, err)
			}
			if _, err := tx.Exec(`INSERT INTO startup_migrations (version, applied_at) VALUES (?,?)`, i+1, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Insert stores a new record, sets its ID and drops the node's oldest
// records beyond KeepPerNode.
func (s *Store) Insert(r *Record) error {
	return s.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO startups (node_id, agent_start_id, registered_at, kind, record) VALUES (?,?,?,?,'{}')`,
			r.NodeID, r.AgentStartID, r.RegisteredAt.UnixMilli(), r.Kind)
		if err != nil {
			return err
		}
		if r.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE startups SET record = ? WHERE id = ?`, string(b), r.ID); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM startups WHERE node_id = ? AND id NOT IN
			(SELECT id FROM startups WHERE node_id = ? ORDER BY id DESC LIMIT ?)`, r.NodeID, r.NodeID, KeepPerNode)
		return err
	})
}

// Update writes a stored record again.
func (s *Store) Update(r *Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE startups SET record = ?, kind = ? WHERE id = ?`, string(b), r.Kind, r.ID)
		return err
	})
}

func scanRecords(rows *sql.Rows, err error) ([]Record, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var id int64
		var doc string
		if err := rows.Scan(&id, &doc); err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			return nil, fmt.Errorf("start-up record %d: %w", id, err)
		}
		r.ID = id
		out = append(out, r)
	}
	return out, rows.Err()
}

// List returns a node's records, newest first; limit <= 0 means all kept.
func (s *Store) List(node string, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = KeepPerNode
	}
	return scanRecords(s.db.DB().Query(`SELECT id, record FROM startups WHERE node_id = ? ORDER BY id DESC LIMIT ?`, node, limit))
}

// All returns every record of every node, newest first.
func (s *Store) All() ([]Record, error) {
	return scanRecords(s.db.DB().Query(`SELECT id, record FROM startups ORDER BY id DESC`))
}

// Latest returns a node's newest record, nil when it has none.
func (s *Store) Latest(node string) (*Record, error) {
	rs, err := s.List(node, 1)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

// ByStart returns the record of one agent process, nil when there is none.
func (s *Store) ByStart(node, agentStartID string) (*Record, error) {
	rs, err := scanRecords(s.db.DB().Query(`SELECT id, record FROM startups WHERE node_id = ? AND agent_start_id = ?`, node, agentStartID))
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

// MemStore keeps the records in memory: for a scheduler without its
// database (records are lost at a restart) and for tests.
type MemStore struct {
	mu   sync.Mutex
	recs []Record
	next int64
	// Fail, when set, is returned by every write (tests).
	Fail error
}

// Insert implements the store.
func (m *MemStore) Insert(r *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for _, x := range m.recs {
		if x.NodeID == r.NodeID && x.AgentStartID == r.AgentStartID {
			return errors.New("startups: this agent start is already recorded")
		}
	}
	m.next++
	r.ID = m.next
	m.recs = append(m.recs, clone(*r))
	var keep []Record
	n := 0
	for i := len(m.recs) - 1; i >= 0; i-- {
		if m.recs[i].NodeID == r.NodeID {
			n++
			if n > KeepPerNode {
				continue
			}
		}
		keep = append(keep, m.recs[i])
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].ID < keep[j].ID })
	m.recs = keep
	return nil
}

// Update implements the store.
func (m *MemStore) Update(r *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for i := range m.recs {
		if m.recs[i].ID == r.ID {
			m.recs[i] = clone(*r)
			return nil
		}
	}
	return errors.New("startups: no such record")
}

// List implements the store.
func (m *MemStore) List(node string, limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = KeepPerNode
	}
	out := []Record{}
	for i := len(m.recs) - 1; i >= 0 && len(out) < limit; i-- {
		if m.recs[i].NodeID == node {
			out = append(out, clone(m.recs[i]))
		}
	}
	return out, nil
}

// All implements the store.
func (m *MemStore) All() ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Record{}
	for i := len(m.recs) - 1; i >= 0; i-- {
		out = append(out, clone(m.recs[i]))
	}
	return out, nil
}

// Latest implements the store.
func (m *MemStore) Latest(node string) (*Record, error) {
	rs, _ := m.List(node, 1)
	if len(rs) == 0 {
		return nil, nil
	}
	return &rs[0], nil
}

// ByStart implements the store.
func (m *MemStore) ByStart(node, agentStartID string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if r.NodeID == node && r.AgentStartID == agentStartID {
			c := clone(r)
			return &c, nil
		}
	}
	return nil, nil
}

// clone copies a record deeply enough that the copy shares no map or pointer.
// Snapshot returns a deep copy of r, for writing it while r keeps changing.
func (r *Record) Snapshot() Record { return clone(*r) }

func clone(r Record) Record {
	b, _ := json.Marshal(r)
	var c Record
	json.Unmarshal(b, &c)
	c.ID = r.ID
	return c
}
