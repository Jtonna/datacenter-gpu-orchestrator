package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// isDB reports a database file of the data folder: it is copied with SQLite
// itself, not byte by byte, and its -wal and -shm files are left behind.
func isDB(name string) bool { return strings.HasSuffix(name, ".db") }

func isDBSidecar(name string) bool {
	return strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") || strings.HasSuffix(name, ".db-journal")
}

// copyDB writes a consistent copy of the database src to dst with VACUUM
// INTO, from a read-only connection. Unlike a byte copy it is safe while a
// scheduler is writing src: the copy is one transaction's view, the write
// ahead log included. (The old gate scripts did the same with the SQLite
// backup call; a byte copy of a live database can be torn.)
//
// A database without a -wal file has no writer open on it (a scheduler in
// WAL mode keeps the file while it runs): it is copied byte by byte, so that
// not even an empty -wal file is made beside the source.
func copyDB(src, dst string) error {
	if _, err := os.Stat(src + "-wal"); errors.Is(err, fs.ErrNotExist) {
		return copyFile(src, dst)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(src)+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("copying the database %s: %w", src, err)
	}
	return nil
}

// dbCheck is what the gate reads of a database after a run: SQLite's
// integrity check, the memory-need tables, and every migrations ledger with
// the versions it holds (a way back must not leave a ledger the old build
// cannot read).
type dbCheck struct {
	File      string           `json:"file"`
	Integrity string           `json:"integrity"` // "ok", or the first problem
	Tables    []string         `json:"needs_tables,omitempty"`
	Ledgers   map[string][]int `json:"ledgers,omitempty"`
	Err       string           `json:"error,omitempty"`
}

func checkDB(path string) dbCheck {
	c := dbCheck{File: filepath.Base(path)}
	if _, err := os.Stat(path); err != nil {
		c.Err = err.Error()
		return c
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		c.Err = err.Error()
		return c
	}
	defer db.Close()
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&c.Integrity); err != nil {
		c.Err = err.Error()
		return c
	}
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		c.Err = err.Error()
		return c
	}
	var tables []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			tables = append(tables, n)
		}
	}
	rows.Close()
	for _, t := range tables {
		if strings.HasPrefix(t, "needs_") {
			c.Tables = append(c.Tables, t)
		}
		if !strings.Contains(t, "migrations") {
			continue
		}
		vr, err := db.Query(`SELECT version FROM "` + strings.ReplaceAll(t, `"`, `""`) + `" ORDER BY version`)
		if err != nil {
			continue
		}
		var vs []int
		for vr.Next() {
			var v int
			if vr.Scan(&v) == nil {
				vs = append(vs, v)
			}
		}
		vr.Close()
		if c.Ledgers == nil {
			c.Ledgers = map[string][]int{}
		}
		c.Ledgers[t] = vs
	}
	sort.Strings(c.Tables)
	return c
}

// checkDBs checks every database file directly in dir.
func checkDBs(dir string) []dbCheck {
	ents, _ := os.ReadDir(dir)
	var out []dbCheck
	for _, e := range ents {
		if !e.IsDir() && isDB(e.Name()) {
			out = append(out, checkDB(filepath.Join(dir, e.Name())))
		}
	}
	return out
}

func dbsOK(cs []dbCheck) bool {
	for _, c := range cs {
		if c.Err != "" || c.Integrity != "ok" {
			return false
		}
	}
	return true
}
