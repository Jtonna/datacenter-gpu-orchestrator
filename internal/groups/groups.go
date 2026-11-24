// Package groups keeps the zone groups: named sets of zones (agent nodes, by
// id), each zone in at most one group. A group is a label for people and
// for actions on many zones at once (pause, unpause); it is never a
// placement rule (tiers are).
//
// The groups live in the scheduler database, in two tables versioned by
// their own ledger (group_migrations), as the start-up and learned-need
// tables are. An older image does not know the tables and leaves them
// alone, so going back is a plain image swap.
package groups

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DB is what the store needs from the scheduler database (bench.Store).
type DB interface {
	DB() *sql.DB
	Write(fn func(*sql.Tx) error) error
}

// migrations are applied in order, once each. Add to the end; never edit
// one that has shipped.
var migrations = []string{`
CREATE TABLE zone_groups (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX zone_groups_name ON zone_groups (name COLLATE NOCASE);
CREATE TABLE zone_group_members (
  node_id  TEXT PRIMARY KEY,
  group_id INTEGER NOT NULL,
  added_at INTEGER NOT NULL
);
CREATE INDEX zone_group_members_group ON zone_group_members (group_id);
`}

var (
	ErrNotFound = errors.New("no such group")
	ErrExists   = errors.New("a group with that name exists")
	ErrBadName  = errors.New("a group name is 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit")
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName says whether name may name a group.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Group is one group: its name, its members (node ids, sorted) and when it
// was made.
type Group struct {
	Name      string    `json:"name"`
	Members   []string  `json:"members"`
	CreatedAt time.Time `json:"created_at"`
}

type group struct {
	id      int64
	name    string
	created time.Time
}

// Store is the groups, in memory and (with a database) stored.
type Store struct {
	db  DB // nil: memory only
	now func() time.Time

	mu      sync.Mutex
	groups  []*group         // in the order they were made
	members map[string]int64 // node id -> group id
	nextID  int64
	rev     int64
}

// Memory is a store without a database: groups last for this process.
func Memory() *Store {
	return &Store{now: time.Now, members: map[string]int64{}, nextID: 1, rev: 1}
}

// Open applies the migrations and loads the groups.
func Open(db DB) (*Store, error) {
	err := db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS group_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM group_migrations`).Scan(&have); err != nil {
			return err
		}
		if have > len(migrations) {
			return fmt.Errorf("the group tables are at version %d but this build knows %d", have, len(migrations))
		}
		for i := have; i < len(migrations); i++ {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return fmt.Errorf("group migration %d: %w", i+1, err)
			}
			if _, err := tx.Exec(`INSERT INTO group_migrations (version, applied_at) VALUES (?,?)`, i+1, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s := Memory()
	s.db = db
	rows, err := db.DB().Query(`SELECT id, name, created_at FROM zone_groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		g := &group{}
		var ms int64
		if err := rows.Scan(&g.id, &g.name, &ms); err != nil {
			return nil, err
		}
		g.created = time.UnixMilli(ms)
		s.groups = append(s.groups, g)
		if g.id >= s.nextID {
			s.nextID = g.id + 1
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mrows, err := db.DB().Query(`SELECT node_id, group_id FROM zone_group_members`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var id string
		var gid int64
		if err := mrows.Scan(&id, &gid); err != nil {
			return nil, err
		}
		s.members[id] = gid
	}
	return s, mrows.Err()
}

func (s *Store) byNameLocked(name string) *group {
	for _, g := range s.groups {
		if strings.EqualFold(g.name, name) {
			return g
		}
	}
	return nil
}

func (s *Store) byIDLocked(id int64) *group {
	for _, g := range s.groups {
		if g.id == id {
			return g
		}
	}
	return nil
}

func (s *Store) write(fn func(tx *sql.Tx) error) error {
	if s.db == nil {
		return nil
	}
	return s.db.Write(fn)
}

// Revision changes with every change of the groups.
func (s *Store) Revision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

// List returns every group in the order they were made.
func (s *Store) List() []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Group, 0, len(s.groups))
	idx := map[int64]int{}
	for i, g := range s.groups {
		idx[g.id] = i
		out = append(out, Group{Name: g.name, Members: []string{}, CreatedAt: g.created})
	}
	for node, gid := range s.members {
		if i, ok := idx[gid]; ok {
			out[i].Members = append(out[i].Members, node)
		}
	}
	for i := range out {
		sort.Strings(out[i].Members)
	}
	return out
}

// Get returns one group.
func (s *Store) Get(name string) (Group, bool) {
	for _, g := range s.List() {
		if strings.EqualFold(g.Name, name) {
			return g, true
		}
	}
	return Group{}, false
}

// GroupOf names the group of a zone.
func (s *Store) GroupOf(node string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.byIDLocked(s.members[node]); g != nil {
		return g.name, true
	}
	return "", false
}

// Memberships maps every grouped zone to its group's name.
func (s *Store) Memberships() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for node, gid := range s.members {
		if g := s.byIDLocked(gid); g != nil {
			out[node] = g.name
		}
	}
	return out
}

// Create makes a group and moves the members given into it. It returns the
// zones moved out of another group (zone -> that group).
func (s *Store) Create(name string, members []string) (map[string]string, error) {
	if !ValidName(name) {
		return nil, ErrBadName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byNameLocked(name) != nil {
		return nil, ErrExists
	}
	now := s.now()
	g := &group{name: name, created: now}
	err := s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO zone_groups (name, created_at, updated_at) VALUES (?,?,?)`, name, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		g.id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return addLocked(tx, g.id, members, now)
	})
	if err != nil {
		return nil, err
	}
	if s.db == nil {
		g.id = s.nextID
	}
	if g.id >= s.nextID {
		s.nextID = g.id + 1
	}
	s.groups = append(s.groups, g)
	moved := s.moveLocked(g.id, members)
	s.rev++
	return moved, nil
}

func addLocked(tx *sql.Tx, gid int64, members []string, now time.Time) error {
	for _, m := range members {
		if _, err := tx.Exec(`INSERT INTO zone_group_members (node_id, group_id, added_at) VALUES (?,?,?)
			ON CONFLICT (node_id) DO UPDATE SET group_id = excluded.group_id, added_at = excluded.added_at`, m, gid, now.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// moveLocked puts members into gid in memory; it returns the zones that
// were in another group.
func (s *Store) moveLocked(gid int64, members []string) map[string]string {
	moved := map[string]string{}
	for _, m := range members {
		if old, ok := s.members[m]; ok && old != gid {
			if g := s.byIDLocked(old); g != nil {
				moved[m] = g.name
			}
		}
		s.members[m] = gid
	}
	return moved
}

// AddMembers moves zones into a group (out of the group they were in). It
// returns the zones moved out of another group.
func (s *Store) AddMembers(name string, members []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.byNameLocked(name)
	if g == nil {
		return nil, ErrNotFound
	}
	var change []string
	for _, m := range members {
		if s.members[m] != g.id {
			change = append(change, m)
		}
	}
	if len(change) == 0 {
		return map[string]string{}, nil
	}
	now := s.now()
	if err := s.write(func(tx *sql.Tx) error { return addLocked(tx, g.id, change, now) }); err != nil {
		return nil, err
	}
	moved := s.moveLocked(g.id, change)
	s.rev++
	return moved, nil
}

// RemoveMembers takes zones out of a group; it returns those that were in
// it (the others are left as they are).
func (s *Store) RemoveMembers(name string, members []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.byNameLocked(name)
	if g == nil {
		return nil, ErrNotFound
	}
	var out []string
	for _, m := range members {
		if s.members[m] == g.id {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{}, nil
	}
	err := s.write(func(tx *sql.Tx) error {
		for _, m := range out {
			if _, err := tx.Exec(`DELETE FROM zone_group_members WHERE node_id = ? AND group_id = ?`, m, g.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, m := range out {
		delete(s.members, m)
	}
	sort.Strings(out)
	s.rev++
	return out, nil
}

// Rename gives a group another name.
func (s *Store) Rename(name, to string) error {
	if !ValidName(to) {
		return ErrBadName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.byNameLocked(name)
	if g == nil {
		return ErrNotFound
	}
	if o := s.byNameLocked(to); o != nil && o != g {
		return ErrExists
	}
	if g.name == to {
		return nil
	}
	err := s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE zone_groups SET name = ?, updated_at = ? WHERE id = ?`, to, s.now().UnixMilli(), g.id)
		return err
	})
	if err != nil {
		return err
	}
	g.name = to
	s.rev++
	return nil
}

// Delete removes a group; its members are then in no group.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.byNameLocked(name)
	if g == nil {
		return ErrNotFound
	}
	err := s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM zone_group_members WHERE group_id = ?`, g.id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM zone_groups WHERE id = ?`, g.id)
		return err
	})
	if err != nil {
		return err
	}
	for m, gid := range s.members {
		if gid == g.id {
			delete(s.members, m)
		}
	}
	for i, x := range s.groups {
		if x == g {
			s.groups = append(s.groups[:i], s.groups[i+1:]...)
			break
		}
	}
	s.rev++
	return nil
}
