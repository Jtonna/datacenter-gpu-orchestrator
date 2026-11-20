package cfgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/benchspec"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// A database written by the build before agents existed (configuration
// tables at version 1, holding a configuration revision, audit rows and a
// benchmark) is migrated in place when this build opens it: nothing is lost,
// the old audit rows read back with an empty agent id, and the new tables are
// there. Opening it again changes nothing.
func TestMigrationInPlaceKeepsConfigurationBenchmarksAndAudit(t *testing.T) {
	dir := t.TempDir()
	db, err := bench.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// the tables as version 1 made them
	err = db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE cfg_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[0]); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO cfg_migrations (version, applied_at) VALUES (1, 1000)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO config_revisions (revision, ts, source, actor, comment, doc) VALUES (1, 2000, 'import', 'cli:jacob', 'first import', ?)`, mustDoc(t)); err != nil {
			return err
		}
		for _, target := range []string{"gemma:8k", "queue_timeout"} {
			if _, err := tx.Exec(`INSERT INTO audit_log (ts, source, actor, action, section, target, before, after, diff, revision, comment)
			  VALUES (2000, 'ui', 'jacob', 'update', 'profiles', ?, NULL, '{"ctx":8192}', '[]', 1, 'kept')`, target); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBenchmark(&benchspec.Benchmark{ID: "b1", Name: "Kept benchmark", Profile: "gemma:8k"}, "api", 3000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for pass := 0; pass < 2; pass++ { // the second open must be a no-op
		db, err = bench.OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := Open(db)
		if err != nil {
			t.Fatalf("pass %d: opening the version 1 database: %v", pass, err)
		}
		ctx := context.Background()
		var ver int
		if err := db.DB().QueryRow(`SELECT MAX(version) FROM cfg_migrations`).Scan(&ver); err != nil || ver != len(migrations) {
			t.Fatalf("pass %d: configuration tables at version %d (%v), want %d", pass, ver, err, len(migrations))
		}
		rev, err := cs.Latest(ctx)
		if err != nil || rev.Revision != 1 || rev.Actor != "cli:jacob" || rev.Comment != "first import" {
			t.Fatalf("pass %d: the configuration revision did not survive: %+v %v", pass, rev, err)
		}
		es, total, err := cs.Query(ctx, Filter{})
		if err != nil || total != 2+pass || len(es) != 2+pass {
			t.Fatalf("pass %d: audit rows %d (%v), want %d", pass, total, err, 2+pass)
		}
		old := es[len(es)-1]
		if old.Actor != "jacob" || old.Comment != "kept" || old.AgentID != "" || string(old.After) != `{"ctx":8192}` {
			t.Fatalf("pass %d: an old audit row changed: %+v", pass, old)
		}
		if b, err := db.GetBenchmark("b1"); err != nil || b.Spec == nil || b.Spec.Name != "Kept benchmark" {
			t.Fatalf("pass %d: the benchmark did not survive: %+v %v", pass, b, err)
		}
		if as, err := cs.Agents(ctx); err != nil || len(as) != pass {
			t.Fatalf("pass %d: agents %v %v", pass, as, err)
		}
		if pass == 0 {
			// the new parts work on the migrated database
			if err := cs.CreateAgent(Agent{ID: "ag_1", Name: "claude", KeyHash: "h", KeyPrefix: "lls_abc", CreatedAt: 5}); err != nil {
				t.Fatal(err)
			}
			if _, err := cs.Record(ctx, audit.Entry{Source: "mcp", Actor: "agent:claude", AgentID: "ag_1", Action: "update", Section: "settings", Target: "x"}); err != nil {
				t.Fatal(err)
			}
			if es, n, _ := cs.Query(ctx, Filter{Agent: "ag_1"}); n != 1 || es[0].AgentID != "ag_1" {
				t.Fatalf("filter by agent: %d %+v", n, es)
			}
			if err := cs.PutIcon(Icon{Key: "model:a.gguf", ContentType: "image/png", Data: []byte{1, 2}, ETag: "e"}); err != nil {
				t.Fatal(err)
			}
		}
		// the audit log is still append-only
		if _, err := db.DB().Exec(`UPDATE audit_log SET actor = 'x'`); err == nil {
			t.Fatalf("pass %d: the audit log accepted an update", pass)
		}
		db.Close()
	}
}

func mustDoc(t *testing.T) string {
	t.Helper()
	d := config.NewDocument()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
