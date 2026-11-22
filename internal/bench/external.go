package bench

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
)

// External result sets are benchmark results produced outside the bench
// runner (by a driver script, by hand) and stored so that the Benchmarks page
// can show them beside the built-in suites. They are documents: tables of
// numbers and strings with column definitions, a note and text attachments.
// Nothing is executed and no profile has to exist.

// SourceExternal is the source of every external result set.
const SourceExternal = "external"

// Limits of one external result set.
const (
	MaxExternalBytes       = 2 << 20 // the request body
	MaxExternalTables      = 50
	MaxExternalRows        = 2000 // per table
	MaxExternalColumns     = 60   // per table
	MaxExternalAttachments = 20
	MaxExternalAttachBytes = 512 << 10 // per attachment text
	maxExternalTitle       = 200
)

// ExternalColumn defines one column of a table. HigherIsBetter is left out
// when the column has no better direction (a name, a verdict).
type ExternalColumn struct {
	Key            string `json:"key"`
	Label          string `json:"label,omitempty"`
	Unit           string `json:"unit,omitempty"`
	HigherIsBetter *bool  `json:"higher_is_better,omitempty"`
}

// ExternalTable is one table: ordered columns and rows. A row maps column
// keys to cells (a number, a string or null). The keys "model" and "profile"
// may appear in a row without being columns: they name what the row measured
// so that the UI can link it.
type ExternalTable struct {
	Title       string           `json:"title"`
	Description string           `json:"description,omitempty"`
	Columns     []ExternalColumn `json:"columns"`
	Rows        []map[string]any `json:"rows"`
}

// ExternalAttachment is a named text kept with the set (a task statement, a rubric).
type ExternalAttachment struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// ExternalSet is one stored external result set.
type ExternalSet struct {
	ID          string               `json:"id"`
	Title       string               `json:"title"`
	Description string               `json:"description,omitempty"`
	Source      string               `json:"source"`
	Note        string               `json:"note,omitempty"`
	CreatedAt   int64                `json:"created_at"`
	UpdatedAt   int64                `json:"updated_at"`
	Tables      []ExternalTable      `json:"tables"`
	Attachments []ExternalAttachment `json:"attachments,omitempty"`
}

// ExternalTableInfo is a table without its rows (for listings).
type ExternalTableInfo struct {
	Title    string           `json:"title"`
	Columns  []ExternalColumn `json:"columns"`
	RowCount int              `json:"row_count"`
}

// ExternalAttachmentInfo is an attachment without its text.
type ExternalAttachmentInfo struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// ExternalSummary is an external result set without rows or attachment texts.
type ExternalSummary struct {
	ID          string                   `json:"id"`
	Title       string                   `json:"title"`
	Description string                   `json:"description,omitempty"`
	Source      string                   `json:"source"`
	Note        string                   `json:"note,omitempty"`
	CreatedAt   int64                    `json:"created_at"`
	UpdatedAt   int64                    `json:"updated_at"`
	Tables      []ExternalTableInfo      `json:"tables"`
	Attachments []ExternalAttachmentInfo `json:"attachments"`
	Models      []string                 `json:"models,omitempty"`
}

// Summary is the set without its rows and attachment texts.
func (x *ExternalSet) Summary() ExternalSummary {
	out := ExternalSummary{ID: x.ID, Title: x.Title, Description: x.Description, Source: x.Source, Note: x.Note, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		Tables: []ExternalTableInfo{}, Attachments: []ExternalAttachmentInfo{}}
	seen := map[string]bool{}
	for _, t := range x.Tables {
		out.Tables = append(out.Tables, ExternalTableInfo{Title: t.Title, Columns: t.Columns, RowCount: len(t.Rows)})
		for _, r := range t.Rows {
			if m, ok := r["model"].(string); ok && m != "" && !seen[m] {
				seen[m] = true
				out.Models = append(out.Models, m)
			}
		}
	}
	for _, a := range x.Attachments {
		out.Attachments = append(out.Attachments, ExternalAttachmentInfo{Name: a.Name, Bytes: len(a.Text)})
	}
	return out
}

var columnKeyRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// externalIDRe is the id rule of benchmarks and suites.
var externalIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ParseExternalJSON decodes a set strictly (unknown fields are refused).
// Call Validate next.
func ParseExternalJSON(data []byte) (*ExternalSet, error) {
	var x ExternalSet
	if err := benchspec.DecodeStrict(data, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks the id, the titles, the limits and every cell. It wraps
// ErrInvalid.
func (x *ExternalSet) Validate() error {
	if !externalIDRe.MatchString(x.ID) {
		return invalidf("id %q must be 1 to 64 lower-case letters, digits, dots, dashes or underscores, starting with a letter or digit", x.ID)
	}
	if strings.TrimSpace(x.Title) == "" || len(x.Title) > maxExternalTitle {
		return invalidf("title is required and at most %d characters", maxExternalTitle)
	}
	if x.Source != "" && x.Source != SourceExternal {
		return invalidf("source is always %q (or left out), got %q", SourceExternal, x.Source)
	}
	if len(x.Tables) == 0 {
		return invalidf("at least one table is required")
	}
	if len(x.Tables) > MaxExternalTables {
		return invalidf("%d tables: at most %d", len(x.Tables), MaxExternalTables)
	}
	for ti, t := range x.Tables {
		where := fmt.Sprintf("tables[%d]", ti)
		if strings.TrimSpace(t.Title) == "" || len(t.Title) > maxExternalTitle {
			return invalidf("%s: title is required and at most %d characters", where, maxExternalTitle)
		}
		if len(t.Columns) == 0 {
			return invalidf("%s: at least one column is required", where)
		}
		if len(t.Columns) > MaxExternalColumns {
			return invalidf("%s: %d columns: at most %d", where, len(t.Columns), MaxExternalColumns)
		}
		if len(t.Rows) > MaxExternalRows {
			return invalidf("%s: %d rows: at most %d", where, len(t.Rows), MaxExternalRows)
		}
		keys := map[string]bool{}
		for ci, c := range t.Columns {
			if !columnKeyRe.MatchString(c.Key) {
				return invalidf("%s.columns[%d]: key %q must be 1 to 64 letters, digits, dots, dashes or underscores", where, ci, c.Key)
			}
			if keys[c.Key] {
				return invalidf("%s.columns[%d]: key %q appears twice", where, ci, c.Key)
			}
			keys[c.Key] = true
		}
		for ri, r := range t.Rows {
			for k, v := range r {
				if !keys[k] && k != "model" && k != "profile" {
					return invalidf("%s.rows[%d]: %q is not a column of the table", where, ri, k)
				}
				switch c := v.(type) {
				case nil, string:
				case float64:
					if math.IsNaN(c) || math.IsInf(c, 0) {
						return invalidf("%s.rows[%d].%s: not a finite number", where, ri, k)
					}
				default:
					return invalidf("%s.rows[%d].%s: a cell is a number, a string or null", where, ri, k)
				}
				if (k == "model" || k == "profile") && v != nil {
					if _, ok := v.(string); !ok {
						return invalidf("%s.rows[%d].%s: must be a string", where, ri, k)
					}
				}
			}
		}
	}
	if len(x.Attachments) > MaxExternalAttachments {
		return invalidf("%d attachments: at most %d", len(x.Attachments), MaxExternalAttachments)
	}
	names := map[string]bool{}
	for i, a := range x.Attachments {
		if strings.TrimSpace(a.Name) == "" || len(a.Name) > maxExternalTitle {
			return invalidf("attachments[%d]: name is required and at most %d characters", i, maxExternalTitle)
		}
		if names[a.Name] {
			return invalidf("attachments[%d]: name %q appears twice", i, a.Name)
		}
		names[a.Name] = true
		if len(a.Text) > MaxExternalAttachBytes {
			return invalidf("attachments[%d] (%s): %d bytes of text: at most %d", i, a.Name, len(a.Text), MaxExternalAttachBytes)
		}
	}
	return nil
}

// SaveExternal inserts or replaces a set. created_at is kept from the stored
// set when there is one; updated_at is now. x.Source, CreatedAt and UpdatedAt
// are set on x.
func (s *Store) SaveExternal(x *ExternalSet, now int64) error {
	x.Source = SourceExternal
	return s.write(func(tx *sql.Tx) error {
		var created int64
		err := tx.QueryRow(`SELECT created_at FROM external_sets WHERE id=?`, x.ID).Scan(&created)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = now
		case err != nil:
			return err
		}
		x.CreatedAt, x.UpdatedAt = created, now
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO external_sets (id, title, doc, created_at, updated_at) VALUES (?,?,?,?,?)
		  ON CONFLICT(id) DO UPDATE SET title=excluded.title, doc=excluded.doc, updated_at=excluded.updated_at`, x.ID, x.Title, string(b), created, now)
		return err
	})
}

// GetExternal returns one set or ErrNotFound.
func (s *Store) GetExternal(id string) (*ExternalSet, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM external_sets WHERE id=?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var x ExternalSet
	if err := json.Unmarshal([]byte(doc), &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// ListExternal returns every set without rows or attachment texts, newest first.
func (s *Store) ListExternal() ([]ExternalSummary, error) {
	rows, err := s.db.Query(`SELECT doc FROM external_sets ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExternalSummary{}
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var x ExternalSet
		if err := json.Unmarshal([]byte(doc), &x); err != nil {
			return nil, err
		}
		out = append(out, x.Summary())
	}
	return out, rows.Err()
}

// DeleteExternal removes a set; ErrNotFound when there is none.
func (s *Store) DeleteExternal(id string) error {
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM external_sets WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
