package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const extSample = `{"id":"coding-1","title":"Coding quality","description":"six tasks","note":"run by a driver script",
 "tables":[{"title":"Tasks","columns":[{"key":"model","label":"Model"},{"key":"passed","label":"Passed","unit":"tests","higher_is_better":true},{"key":"end","label":"Ended"}],
   "rows":[{"model":"Model A","profile":"pa","passed":3,"end":"stop"},{"model":"Model B","passed":null,"end":"length"}]},
  {"title":"Totals","columns":[{"key":"n"}],"rows":[]}],
 "attachments":[{"name":"t1.md","text":"Write a function."}]}`

func parseExt(t *testing.T, s string) *ExternalSet {
	t.Helper()
	x, err := ParseExternalJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestExternalSetRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	x := parseExt(t, extSample)
	if err := x.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveExternal(x, 100); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetExternal("coding-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceExternal || got.CreatedAt != 100 || got.UpdatedAt != 100 || got.Note != "run by a driver script" || len(got.Tables) != 2 ||
		len(got.Tables[0].Rows) != 2 || got.Tables[0].Rows[0]["passed"] != 3.0 || got.Tables[0].Rows[1]["passed"] != nil ||
		got.Tables[0].Rows[0]["profile"] != "pa" || *got.Tables[0].Columns[1].HigherIsBetter != true || got.Tables[0].Columns[2].HigherIsBetter != nil ||
		got.Attachments[0].Text != "Write a function." {
		t.Fatalf("%+v", got)
	}
	// replacing keeps created_at
	x.Title = "Renamed"
	if err := s.SaveExternal(x, 200); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetExternal("coding-1")
	if got.Title != "Renamed" || got.CreatedAt != 100 || got.UpdatedAt != 200 {
		t.Fatalf("%+v", got)
	}
	// the listing leaves rows and attachment texts out
	list, err := s.ListExternal()
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	sum := list[0]
	if sum.Tables[0].RowCount != 2 || sum.Tables[1].RowCount != 0 || sum.Attachments[0].Bytes != len("Write a function.") ||
		len(sum.Models) != 2 || sum.Models[0] != "Model A" {
		t.Fatalf("%+v", sum)
	}
	if b := jsonText(t, list); strings.Contains(b, `"rows"`) || strings.Contains(b, "Write a function.") {
		t.Fatalf("listing carries rows or texts: %s", b)
	}
	if err := s.DeleteExternal("coding-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExternal("coding-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.DeleteExternal("coding-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if list, _ := s.ListExternal(); list == nil || len(list) != 0 {
		t.Fatalf("empty listing must be [] not nil: %v", list)
	}
}

// A database written before external sets existed opens unchanged and gains the table.
func TestExternalMigrationFromOlderDatabase(t *testing.T) {
	dir := t.TempDir()
	old, err := openStore(dir, len(migrations)-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`INSERT INTO runs (id, suite_id, suite_name, state, queued_at, suite) VALUES ('r-old','s','S','completed',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := old.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='external_sets'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old schema already has the table: %d %v", n, err)
	}
	old.Close()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.SchemaVersion(); v != len(migrations) {
		t.Fatalf("version %d", v)
	}
	if _, err := s.GetRun("r-old"); err != nil {
		t.Fatalf("old run lost: %v", err)
	}
	if list, err := s.ListExternal(); err != nil || len(list) != 0 {
		t.Fatalf("%v %v", list, err)
	}
	x := parseExt(t, extSample)
	if err := s.SaveExternal(x, 5); err != nil {
		t.Fatal(err)
	}
}

func extWith(tables, rows, cols int) *ExternalSet {
	x := &ExternalSet{ID: "x", Title: "X"}
	for i := 0; i < tables; i++ {
		t := ExternalTable{Title: fmt.Sprintf("t%d", i)}
		for c := 0; c < cols; c++ {
			t.Columns = append(t.Columns, ExternalColumn{Key: fmt.Sprintf("c%d", c)})
		}
		for r := 0; r < rows; r++ {
			t.Rows = append(t.Rows, map[string]any{"c0": float64(r)})
		}
		x.Tables = append(x.Tables, t)
	}
	return x
}

func TestExternalValidationLimits(t *testing.T) {
	ok := []*ExternalSet{extWith(MaxExternalTables, 1, 1), extWith(1, MaxExternalRows, 1), extWith(1, 1, MaxExternalColumns)}
	for i, x := range ok {
		if err := x.Validate(); err != nil {
			t.Errorf("at the limit %d: %v", i, err)
		}
	}
	att := func(n, size int) *ExternalSet {
		x := extWith(1, 1, 1)
		for i := 0; i < n; i++ {
			x.Attachments = append(x.Attachments, ExternalAttachment{Name: fmt.Sprintf("a%d", i), Text: strings.Repeat("x", size)})
		}
		return x
	}
	if err := att(MaxExternalAttachments, MaxExternalAttachBytes).Validate(); err != nil {
		t.Errorf("attachments at the limit: %v", err)
	}
	mod := func(f func(*ExternalSet)) *ExternalSet { x := extWith(1, 1, 2); f(x); return x }
	for name, x := range map[string]*ExternalSet{
		"tables":          extWith(MaxExternalTables+1, 1, 1),
		"rows":            extWith(1, MaxExternalRows+1, 1),
		"columns":         extWith(1, 1, MaxExternalColumns+1),
		"no table":        extWith(0, 0, 0),
		"no column":       extWith(1, 0, 0),
		"attachments":     att(MaxExternalAttachments+1, 1),
		"attachment size": att(1, MaxExternalAttachBytes+1),
		"bad id":          mod(func(x *ExternalSet) { x.ID = "A B" }),
		"no title":        mod(func(x *ExternalSet) { x.Title = " " }),
		"other source":    mod(func(x *ExternalSet) { x.Source = "api" }),
		"table title":     mod(func(x *ExternalSet) { x.Tables[0].Title = "" }),
		"column key":      mod(func(x *ExternalSet) { x.Tables[0].Columns[1].Key = "a b" }),
		"duplicate key":   mod(func(x *ExternalSet) { x.Tables[0].Columns[1].Key = "c0" }),
		"unknown cell":    mod(func(x *ExternalSet) { x.Tables[0].Rows[0]["zz"] = 1.0 }),
		"bool cell":       mod(func(x *ExternalSet) { x.Tables[0].Rows[0]["c1"] = true }),
		"object cell":     mod(func(x *ExternalSet) { x.Tables[0].Rows[0]["c1"] = map[string]any{} }),
		"model number":    mod(func(x *ExternalSet) { x.Tables[0].Rows[0]["model"] = 3.0 }),
		"attachment name": mod(func(x *ExternalSet) { x.Attachments = []ExternalAttachment{{Name: "", Text: "t"}} }),
		"same attachment": mod(func(x *ExternalSet) {
			x.Attachments = []ExternalAttachment{{Name: "a", Text: "t"}, {Name: "a", Text: "u"}}
		}),
	} {
		if err := x.Validate(); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseExternalJSON([]byte(`{"id":"x","title":"X","tables":[],"nope":1}`)); err == nil {
		t.Error("unknown field accepted")
	}
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
