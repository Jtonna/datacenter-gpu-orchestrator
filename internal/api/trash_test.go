package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/history"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/modelfiles"
)

// trashEnv is cfgEnv on a real, throwaway models folder: the configuration
// checks that files exist (os.Stat), the API moves files in that folder.
type trashEnv struct {
	*env
	svc   *cfgsvc.Service
	cs    *cfgstore.Store
	bs    *bench.Store
	dir   string
	stop  string                 // the step trashStop "kills" at
	ro    bool                   // act out a read-only folder
	outer string                 // a folder outside the models folder (must stay untouched)
	hook  func(step string) bool // set before newTrashEnvWith returns: runs at every step
	hist  *history.Store
}

func newTrashEnv(t *testing.T) *trashEnv { t.Helper(); return newTrashEnvWith(t, nil) }

func newTrashEnvWith(t *testing.T, hook func(te *trashEnv, step string) bool) *trashEnv {
	t.Helper()
	te := &trashEnv{dir: t.TempDir(), outer: t.TempDir(), hist: history.Open(history.Options{Dir: t.TempDir()})}
	t.Cleanup(te.hist.Close)
	if hook != nil {
		te.hook = func(step string) bool { return hook(te, step) }
	}
	st, err := bench.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	te.bs = st
	cs, err := cfgstore.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	te.cs = cs
	var e *env
	svc := cfgsvc.New(context.Background(), cfgsvc.Options{Store: cs, Boot: config.Bootstrap{ModelsDir: te.dir},
		Apply:    func(c *config.Config) { e.sched.ApplyConfig(c) },
		OnChange: func(ev cfgsvc.ChangeEvent) { e.api.PublishConfig(ev) }})
	te.svc = svc
	rofs := func(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: syscall.EROFS} }
	ops := &modelfiles.Ops{
		Mkdir: func(p string, m fs.FileMode) error {
			if te.ro {
				return rofs("mkdir", p)
			}
			return os.Mkdir(p, m)
		},
		Rename: func(a, b string) error {
			if te.ro {
				return rofs("rename", a)
			}
			return os.Rename(a, b)
		},
		Access: func(p string) error {
			if te.ro {
				return rofs("access", p)
			}
			return nil
		},
	}
	e = newEnv(t, okUpstream(), func(o *Options) {
		o.Config, o.Audit, o.BenchStore = svc, cs, st
		o.trashOps = ops
		o.trashStop = func(step string) bool { return te.stop != "" && step == te.stop }
		o.trashUnloadWait = time.Second
		o.History = te.hist
		o.trashClearEvery = -1 // tests run clearTrash themselves
		if te.hook != nil {
			o.trashStop = func(step string) bool { return te.hook(step) || (te.stop != "" && step == te.stop) }
		}
	})
	te.env = e
	return te
}

func (te *trashEnv) file(name, body string) {
	te.t.Helper()
	if err := os.WriteFile(filepath.Join(te.dir, name), []byte(body), 0o644); err != nil {
		te.t.Fatal(err)
	}
}

func (te *trashEnv) has(name string) bool {
	_, err := os.Lstat(filepath.Join(te.dir, name))
	return err == nil
}

func (te *trashEnv) profileIDs() []string {
	d := te.svc.Document()
	return keysOfSpecs(d.Profiles)
}

// restart acts out the next start: a new API server on the same database,
// configuration and models folder, then RepairTrash as main runs it.
func (te *trashEnv) restart() *Server {
	te.t.Helper()
	s2 := New(te.cfg, te.sched, Options{Config: te.svc, Audit: te.cs, BenchStore: te.bs, Logger: te.api.log, Now: te.api.opts.Now, History: te.hist, trashClearEvery: -1})
	te.t.Cleanup(s2.Close)
	s2.RepairTrash(context.Background()) // as main does once the server is up
	return s2
}

var trashUI = map[string]string{"X-Source": "ui", "X-Actor": "alex"}

func (te *trashEnv) setup() {
	te.t.Helper()
	te.file("a.gguf", "GGUF-a")
	te.file("b.gguf", "GGUF-b")
	for _, p := range []string{
		`{"id":"a1","model":"a.gguf","ctx":2048,"needs_mib":100,"aliases":["ay"]}`,
		`{"id":"a2","model":"a.gguf","ctx":8192,"needs_mib":200}`,
		`{"id":"b1","model":"b.gguf","ctx":2048,"needs_mib":100}`,
	} {
		if st, m := te.cdo("POST", "/api/config/profiles", p, nil); st != 201 && st != 200 {
			te.t.Fatalf("profile %s: %d %v", p, st, m)
		}
	}
}

func catalogKeys(t *testing.T, e *env) map[string]map[string]any {
	t.Helper()
	_, m := e.cdo("GET", "/api/models", "", nil)
	out := map[string]map[string]any{}
	for _, x := range m["models"].([]any) {
		out[x.(map[string]any)["key"].(string)] = x.(map[string]any)
	}
	out["__disk"] = m["disk"].(map[string]any)
	return out
}

// Delete moves the file into .trash/<id>/ and takes its profiles out in one
// revision; the catalog no longer lists it, the trash list does; restore
// renames it back and adds the profiles in one revision; both are audited
// in section models with the comment and actor.
func TestModelTrash_DeleteRestoreRoundTrip(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	if c := catalogKeys(t, te.env); c["a.gguf"] == nil {
		t.Fatalf("a.gguf not in the catalog: %v", c)
	}
	rev0 := te.svc.Revision()

	// refused: no comment, not a .gguf, a path, a command key, an unknown file
	if st, m := te.cdo("POST", "/api/models/delete/a.gguf", ``, trashUI); st != 400 {
		t.Fatalf("no comment: %d %v", st, m)
	}
	for _, bad := range []string{"..%2Fx.gguf", "%2Fetc%2Fx.gguf", ".trash", "a.txt", "sub/a.gguf"} {
		if st, m := te.cdo("POST", "/api/models/delete/"+bad, `{"comment":"x"}`, trashUI); st != 400 && st != 404 {
			t.Errorf("%s: %d %v", bad, st, m)
		}
	}
	if st, m := te.cdo("POST", "/api/models/delete/command:x", `{"comment":"x"}`, trashUI); st != 400 || errOf(m)["reason"] != "not_a_file" {
		t.Fatalf("command key: %d %v", st, m)
	}
	if st, m := te.cdo("POST", "/api/models/delete/nope.gguf", `{"comment":"x"}`, trashUI); st != 404 {
		t.Fatalf("unknown: %d %v", st, m)
	}
	// dry run: the figures, nothing changes
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","dry_run":true}`, trashUI)
	if st != 200 || m["dry_run"] != true || m["size_bytes"] != float64(6) || len(m["profiles"].([]any)) != 2 || m["trash_days"] != float64(30) {
		t.Fatalf("dry run: %d %v", st, m)
	}
	if !te.has("a.gguf") || te.svc.Revision() != rev0 {
		t.Fatal("the dry run changed something")
	}

	st, m = te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"not used any more"}`, trashUI)
	if st != 200 || m["file"] != "a.gguf" || len(m["profiles_removed"].([]any)) != 2 || m["revision"] != float64(rev0+1) {
		t.Fatalf("delete: %d %v", st, m)
	}
	id := m["trash_id"].(string)
	if te.has("a.gguf") || !te.has(filepath.Join(".trash", id, "a.gguf")) {
		t.Fatal("the file is not where it should be")
	}
	if b, _ := os.ReadFile(filepath.Join(te.dir, ".trash", id, "a.gguf")); string(b) != "GGUF-a" {
		t.Fatalf("trash bytes %q", b)
	}
	if ids := te.profileIDs(); strings.Join(ids, ",") != "b1" {
		t.Fatalf("profiles after delete: %v", ids)
	}
	c := catalogKeys(t, te.env)
	if c["a.gguf"] != nil {
		t.Fatalf("a deleted file is in the catalog: %v", c["a.gguf"])
	}
	for k := range c {
		if strings.Contains(k, ".trash") {
			t.Fatalf("the trash folder shows in the catalog: %s", k)
		}
	}
	if d := c["__disk"]; d["trash_bytes"] != float64(6) || d["trash_count"] != float64(1) || d["next_purge_at"] == nil || d["trash_purge_on"] != false {
		t.Fatalf("disk block: %v", d)
	}
	// deleting it again names the trash
	if st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI); st != 409 || errOf(m)["reason"] != "in_trash" ||
		!strings.Contains(errOf(m)["message"].(string), "in the trash since") {
		t.Fatalf("delete again: %d %v", st, m)
	}
	// the trash list (observe)
	st, m = te.cdo("GET", "/api/models/trash", "", nil)
	items := m["items"].([]any)
	if st != 200 || len(items) != 1 || m["total_bytes"] != float64(6) || m["purge_on"] != false {
		t.Fatalf("trash list: %d %v", st, m)
	}
	it := items[0].(map[string]any)
	if it["id"] != id || it["state"] != "trashed" || it["profile_count"] != float64(2) || it["actor"] != "alex" || it["comment"] != "not used any more" || it["in_trash_folder"] != true {
		t.Fatalf("trash item: %v", it)
	}

	// a write that adds a profile on the trashed file is refused with the trash named
	st, m = te.cdo("POST", "/api/config/profiles", `{"id":"a3","model":"a.gguf","ctx":2048,"needs_mib":100}`, nil)
	if st < 400 || !strings.Contains(strings.ToLower(jsonText(m)), "in the trash since") {
		t.Fatalf("profile on a trashed file: %d %v", st, m)
	}
	// so is a rollback to the revision from before the delete
	st, m = te.cdo("POST", "/api/config/revisions/"+itoa(rev0)+"/rollback", `{"comment":"back"}`, nil)
	if st < 400 || !strings.Contains(jsonText(m), "in the trash since") || !strings.Contains(jsonText(m), id) {
		t.Fatalf("rollback to before the delete: %d %v", st, m)
	}

	// restore: refused while a file of that name is back; then fine
	te.file("a.gguf", "other")
	if st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI); st != 409 || errOf(m)["reason"] != "file_exists" {
		t.Fatalf("restore over a file: %d %v", st, m)
	}
	os.Rename(filepath.Join(te.dir, "a.gguf"), filepath.Join(te.outer, "a.gguf"))
	if st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", ``, trashUI); st != 400 {
		t.Fatalf("restore without comment: %d %v", st, m)
	}
	if st, _ := te.cdo("POST", "/api/models/trash/0123456789ab/restore", `{"comment":"x"}`, trashUI); st != 404 {
		t.Fatalf("unknown id: %d", st)
	}
	if st, _ := te.cdo("POST", "/api/models/trash/..%2F..%2Fx/restore", `{"comment":"x"}`, trashUI); st != 404 {
		t.Fatalf("bad id: %d", st)
	}
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"needed again"}`, trashUI)
	if st != 200 || len(m["profiles_restored"].([]any)) != 2 {
		t.Fatalf("restore: %d %v", st, m)
	}
	if b, _ := os.ReadFile(filepath.Join(te.dir, "a.gguf")); string(b) != "GGUF-a" {
		t.Fatalf("restored bytes %q", b)
	}
	if te.has(filepath.Join(".trash", id)) {
		t.Fatal("the empty trash folder stayed")
	}
	if ids := te.profileIDs(); strings.Join(ids, ",") != "a1,a2,b1" {
		t.Fatalf("profiles after restore: %v", ids)
	}
	if p := te.svc.Document().Profiles["a1"]; p.Aliases[0] != "ay" || *p.Ctx != 2048 {
		t.Fatalf("a1 came back as %+v", p)
	}
	if c := catalogKeys(t, te.env); c["a.gguf"] == nil || c["__disk"]["trash_count"] != float64(0) {
		t.Fatalf("catalog after restore: %v", c["__disk"])
	}
	es := auditOf(t, te.env, "section=models")
	if len(es) != 2 || es[1]["action"] != "delete" || es[1]["target"] != "model:a.gguf" || es[1]["comment"] != "not used any more" || es[1]["actor"] != "alex" ||
		es[0]["action"] != "create" || es[0]["comment"] != "needed again" {
		t.Fatalf("audit: %v", es)
	}
	if ents, _ := os.ReadDir(te.outer); len(ents) != 1 {
		t.Fatalf("outside the models folder: %v", ents)
	}
}

// Restore when a profile id or alias is taken now: refused with the list;
// with rename_taken the ids get -restored and taken aliases are left off.
func TestModelTrash_RestoreTakenNames(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	_, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	id := m["trash_id"].(string)
	te.file("c.gguf", "c")
	te.cdo("POST", "/api/config/profiles", `{"id":"a1","model":"c.gguf","ctx":2048,"needs_mib":100,"aliases":["ay"]}`, nil)
	st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "profiles_taken" || len(errOf(m)["taken"].([]any)) != 2 {
		t.Fatalf("taken: %d %v", st, m)
	}
	if !te.has(filepath.Join(".trash", id, "a.gguf")) {
		t.Fatal("a refused restore moved the file")
	}
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x","rename_taken":true}`, trashUI)
	if st != 200 || m["renamed"].(map[string]any)["a1"] != "a1-restored" || m["aliases_dropped"].([]any)[0] != "ay" {
		t.Fatalf("rename_taken: %d %v", st, m)
	}
	if ids := strings.Join(te.profileIDs(), ","); ids != "a1,a1-restored,a2,b1" {
		t.Fatalf("profiles: %s", ids)
	}
}

// A delete is refused while the file is loaded; nothing moves.
func TestModelTrash_RefusedWhileLoadedOrInARun(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	resp := te.post("/v1/chat/completions", `{"model":"a1","messages":[{"role":"user","content":"hi"}]}`)
	body(t, resp)
	eventually(t, "a1 loaded", func() bool {
		st, _ := te.sched.Status(context.Background())
		return len(st.Instances) > 0
	})
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "in_use" || errOf(m)["instances"] == nil || !strings.Contains(errOf(m)["message"].(string), "a1 is") {
		t.Fatalf("loaded: %d %v", st, m)
	}
	if !te.has("a.gguf") || len(te.profileIDs()) != 3 {
		t.Fatal("a refused delete changed something")
	}
	// a benchmark run that names b1 blocks b.gguf
	if _, err := te.bs.DB().Exec(`INSERT INTO runs (id, suite_id, suite_name, state, queued_at, started_at, finished_at, version, suite) VALUES ('r1','s','S','queued',1,0,0,'v','{"steps":[{"profile":"b1"}]}')`); err != nil {
		t.Fatal(err)
	}
	st, m = te.cdo("POST", "/api/models/delete/b.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "in_use" || errOf(m)["runs"].([]any)[0] != "r1" {
		t.Fatalf("in a run: %d %v", st, m)
	}
	if !te.has("b.gguf") {
		t.Fatal("moved")
	}
}

// A read-only models folder (as on live today): a clear refusal, nothing
// changed (file, profiles, trash records, configuration revision); a
// restore on a folder that turned read-only is refused the same way.
func TestModelTrash_ReadOnlyFolder(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	rev := te.svc.Revision()
	te.ro = true
	st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "read_only" || !readOnlyOnce(errOf(m)["message"].(string), "mkdir") {
		t.Fatalf("read-only delete: %d %v", st, m)
	}
	if !te.has("a.gguf") || te.has(".trash") || te.svc.Revision() != rev || len(te.profileIDs()) != 3 {
		t.Fatal("a read-only refusal changed something")
	}
	if l, _ := te.cs.TrashList(context.Background()); len(l) != 0 {
		t.Fatalf("records: %v", l)
	}
	// the trash folder exists already (made while writable): the rename is what fails, and is undone
	te.ro = false
	os.MkdirAll(filepath.Join(te.dir, ".trash"), 0o755)
	te.api.opts.trashOps.Mkdir = os.Mkdir
	te.ro = true
	st, m = te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "read_only" || !readOnlyOnce(errOf(m)["message"].(string), "rename") {
		t.Fatalf("read-only rename: %d %v", st, m)
	}
	if !te.has("a.gguf") || len(te.profileIDs()) != 3 {
		t.Fatalf("after the failed rename: file %v profiles %v", te.has("a.gguf"), te.profileIDs())
	}
	if l, _ := te.cs.TrashList(context.Background()); len(l) != 0 {
		t.Fatalf("records: %v", l)
	}
	if ents, _ := os.ReadDir(filepath.Join(te.dir, ".trash")); len(ents) != 0 {
		t.Fatalf("trash folders left: %v", ents)
	}
	// restore on a folder that became read-only
	te.ro = false
	_, m = te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
	id := m["trash_id"].(string)
	te.ro = true
	st, m = te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI)
	if st != 409 || errOf(m)["reason"] != "read_only" || !readOnlyOnce(errOf(m)["message"].(string), "rename") {
		t.Fatalf("read-only restore: %d %v", st, m)
	}
	if r, _ := te.cs.TrashGet(context.Background(), id); r.State != cfgstore.TrashTrashed || !te.has(filepath.Join(".trash", id, "a.gguf")) {
		t.Fatalf("after the refused restore: %+v", r)
	}
}

// A kill between the steps of a delete or a restore, in every order: the
// next start repairs it from where the file is.
func TestModelTrash_KillAndRepairAtStart(t *testing.T) {
	ctx := context.Background()
	t.Run("delete killed after the record, before the revision", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		te.stop = "delete:after-record"
		te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
		te.stop = ""
		s2 := te.restart()
		if l, _ := te.cs.TrashList(ctx); len(l) != 0 || !te.has("a.gguf") || len(te.profileIDs()) != 3 {
			t.Fatalf("records %v file %v profiles %v", l, te.has("a.gguf"), te.profileIDs())
		}
		if len(s2.trash.repairList()) != 1 || !strings.Contains(s2.trash.repairList()[0], "undone") {
			t.Fatalf("repairs: %v", s2.trash.repairList())
		}
	})
	t.Run("delete killed after the revision, before the rename", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		te.stop = "delete:after-config"
		te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
		te.stop = ""
		if len(te.profileIDs()) != 1 {
			t.Fatalf("the kill came after the revision: %v", te.profileIDs())
		}
		te.restart()
		if l, _ := te.cs.TrashList(ctx); len(l) != 0 || !te.has("a.gguf") || strings.Join(te.profileIDs(), ",") != "a1,a2,b1" {
			t.Fatalf("records %v file %v profiles %v", l, te.has("a.gguf"), te.profileIDs())
		}
		if p := te.svc.Document().Profiles["a1"]; p.Aliases[0] != "ay" {
			t.Fatalf("a1 came back as %+v", p)
		}
		if ents, _ := os.ReadDir(filepath.Join(te.dir, ".trash")); len(ents) != 0 {
			t.Fatalf("trash folders left: %v", ents)
		}
	})
	t.Run("delete killed after the rename, before the record says trashed", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		te.stop = "delete:after-rename"
		te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
		te.stop = ""
		l, _ := te.cs.TrashList(ctx)
		if len(l) != 1 || l[0].State != cfgstore.TrashPending {
			t.Fatalf("before the start: %+v", l)
		}
		s2 := te.restart()
		l, _ = te.cs.TrashList(ctx)
		if len(l) != 1 || l[0].State != cfgstore.TrashTrashed || te.has("a.gguf") || len(te.profileIDs()) != 1 {
			t.Fatalf("after the start: %+v", l)
		}
		if s2.trash.summary().Count != 1 || !strings.Contains(s2.trash.repairList()[0], "completed") {
			t.Fatalf("summary %+v repairs %v", s2.trash.summary(), s2.trash.repairList())
		}
	})
	t.Run("restore killed after the record, before the rename", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		_, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
		id := m["trash_id"].(string)
		te.stop = "restore:after-record"
		te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI)
		te.stop = ""
		te.restart()
		if r, _ := te.cs.TrashGet(ctx, id); r.State != cfgstore.TrashTrashed || te.has("a.gguf") || len(te.profileIDs()) != 1 {
			t.Fatalf("after the start: %+v", r)
		}
		// and it restores normally afterwards
		if st, m := te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI); st != 200 {
			t.Fatalf("restore after the repair: %d %v", st, m)
		}
	})
	t.Run("restore killed after the rename, before the profiles", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		_, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
		id := m["trash_id"].(string)
		te.stop = "restore:after-rename"
		te.cdo("POST", "/api/models/trash/"+id+"/restore", `{"comment":"x"}`, trashUI)
		te.stop = ""
		te.restart()
		if _, err := te.cs.TrashGet(ctx, id); err == nil || !te.has("a.gguf") || strings.Join(te.profileIDs(), ",") != "a1,a2,b1" {
			t.Fatalf("after the start: err %v file %v profiles %v", err, te.has("a.gguf"), te.profileIDs())
		}
	})
	t.Run("the file in neither place: reported, nothing moved", func(t *testing.T) {
		te := newTrashEnv(t)
		te.setup()
		te.stop = "delete:after-rename"
		_, _ = te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI)
		te.stop = ""
		l, _ := te.cs.TrashList(ctx)
		os.Rename(filepath.Join(te.dir, ".trash", l[0].ID, "a.gguf"), filepath.Join(te.outer, "a.gguf")) // someone took it away
		s2 := te.restart()
		r, _ := te.cs.TrashGet(ctx, l[0].ID)
		if r.State != cfgstore.TrashInterrupted || !strings.Contains(r.Problem, "not in exactly one place") || !strings.Contains(s2.trash.repairList()[0], "needs a look") {
			t.Fatalf("after the start: %+v %v", r, s2.trash.repairList())
		}
		if len(r.Profiles) != 2 {
			t.Fatal("the saved profiles were lost")
		}
	})
}

func jsonText(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// Ruling 2 (step A ships with the folder read-only): a start on a
// read-only folder with no trash records writes nothing at WARN or ERROR
// for the trash and makes no directory; the trash list is empty; the
// catalog's disk block says an empty trash; delete and restore refuse
// with read_only or not_found and change nothing.
func TestModelTrash_ReadOnlyStartIsQuiet(t *testing.T) {
	te := newTrashEnv(t)
	te.setup()
	te.ro = true
	before := len(te.logs.String())
	s2 := te.restart()
	logged := te.logs.String()[before:]
	for _, l := range strings.Split(logged, "\n") {
		if (strings.Contains(l, "level=WARN") || strings.Contains(l, "level=ERROR")) && strings.Contains(strings.ToLower(l), "trash") {
			t.Errorf("start logged: %s", l)
		}
	}
	if len(s2.trash.repairList()) != 0 || te.has(".trash") {
		t.Fatalf("repairs %v, .trash made %v", s2.trash.repairList(), te.has(".trash"))
	}
	st, m := te.cdo("GET", "/api/models/trash", "", nil)
	if st != 200 || len(m["items"].([]any)) != 0 || m["count"] != float64(0) || m["total_bytes"] != float64(0) || m["next_purge_at"] != nil {
		t.Fatalf("trash list: %d %v", st, m)
	}
	if d := catalogKeys(t, te.env)["__disk"]; d["trash_count"] != float64(0) || d["trash_bytes"] != float64(0) || d["next_purge_at"] != nil {
		t.Fatalf("disk: %v", d)
	}
	if st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x","dry_run":true}`, trashUI); st != 200 || m["dry_run"] != true {
		t.Fatalf("dry run on a read-only folder: %d %v", st, m)
	}
	if st, m := te.cdo("POST", "/api/models/delete/a.gguf", `{"comment":"x"}`, trashUI); st != 409 || errOf(m)["reason"] != "read_only" {
		t.Fatalf("delete: %d %v", st, m)
	}
	if st, m := te.cdo("POST", "/api/models/trash/0123456789ab/restore", `{"comment":"x"}`, trashUI); st != 404 {
		t.Fatalf("restore: %d %v", st, m)
	}
	if te.has(".trash") || !te.has("a.gguf") || len(te.profileIDs()) != 3 {
		t.Fatal("something changed")
	}
}

// readOnlyOnce: the refusal says its first clause once, then the OS error
// of op, then the sentence on the mount.
func readOnlyOnce(msg, op string) bool {
	const head = "the models folder is read-only here: nothing was changed ("
	const tail = "). Deleting and restoring model files need the scheduler's models folder mounted read-write."
	return strings.HasPrefix(msg, head+op+" ") && strings.HasSuffix(msg, "read-only file system"+tail) &&
		strings.Count(msg, "read-only here") == 1
}
