package bench

import "database/sql"

// DB is the shared SQLite connection pool (reads). Other packages keep their own tables in the same file.
func (s *Store) DB() *sql.DB { return s.db }

// Write runs fn in a transaction on the single writer goroutine and waits for it.
func (s *Store) Write(fn func(*sql.Tx) error) error { return s.write(fn) }
