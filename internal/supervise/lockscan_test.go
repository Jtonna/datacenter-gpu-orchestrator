package supervise

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The lock scan (background-task isolation, review IB1): a task that
// recovers a panic must not leave a lock held, or the process hangs where it
// used to crash and come back. So in the scheduler binary every Lock or
// RLock is followed at once by its deferred release:
//
//	x.mu.Lock()
//	defer supervise.Unlocked("pool", x.mu.Unlock) // critical packages
//	defer x.mu.Unlock()                            // the others
//
// In a critical package (state that outlives a task and has no rebuild:
// pool, scheduler, needstore, history, instance, cfgsvc, bench) the release
// is supervise.Unlocked, which turns a panic inside the section into a
// fatal one (the state may be half-written; see Unlocked). A Lock in a
// function literal counts the same. Exceptions are listed in lockAllowlist
// with the reason.

// lockEnforce: see enforce in lint_test.go.
const lockEnforce = true

// criticalPkgs own shared state that a restarted task cannot rebuild.
var criticalPkgs = map[string]bool{
	"internal/pool": true, "internal/scheduler": true, "internal/needstore": true, "internal/history": true,
	"internal/instance": true, "internal/cfgsvc": true, "internal/bench": true,
}

// lockAllowlist is keyed by "kind file function"; kinds: manual (a Lock
// without a deferred release next to it), plain (defer x.Unlock() in a
// critical package).
var lockAllowlist = map[string]string{}

type lockFinding struct{ kind, key, pos string }

func lockScan(t *testing.T, root string) []lockFinding {
	t.Helper()
	var out []lockFinding
	fset := token.NewFileSet()
	src := func(e ast.Expr) string {
		var b strings.Builder
		printer.Fprint(&b, fset, e)
		return b.String()
	}
	for _, sr := range scanRoots {
		err := filepath.WalkDir(filepath.Join(root, sr), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if skipDir(rel) && rel != "internal/supervise" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || skipFile(rel) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			critical := criticalPkgs[filepath.ToSlash(filepath.Dir(rel))]
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := rel + " " + funcName(fd)
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					var list []ast.Stmt
					switch n := n.(type) {
					case *ast.BlockStmt:
						list = n.List
					case *ast.CaseClause:
						list = n.Body
					case *ast.CommClause:
						list = n.Body
					default:
						return true
					}
					for i, st := range list {
						x, lockName := lockCall(st)
						if x == nil {
							continue
						}
						pos := fset.Position(st.Pos()).String()
						want := map[string]string{"Lock": "Unlock", "RLock": "RUnlock"}[lockName]
						var next *ast.DeferStmt
						if i+1 < len(list) {
							next, _ = list[i+1].(*ast.DeferStmt)
						}
						switch {
						case next == nil:
							out = append(out, lockFinding{"manual", key, pos})
						case isUnlocked(next.Call, src(x)+"."+want, src):
							// supervise.Unlocked(name, x.Unlock)
						case isRelease(next.Call, src(x), want, src):
							if critical {
								out = append(out, lockFinding{"plain", key, pos})
							}
						default:
							out = append(out, lockFinding{"manual", key, pos})
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// lockCall: st is `x.Lock()` or `x.RLock()`.
func lockCall(st ast.Stmt) (ast.Expr, string) {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return nil, ""
	}
	c, ok := es.X.(*ast.CallExpr)
	if !ok || len(c.Args) != 0 {
		return nil, ""
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Lock" && sel.Sel.Name != "RLock") {
		return nil, ""
	}
	return sel.X, sel.Sel.Name
}

// isRelease: c is x.Unlock() (or RUnlock), or the pool's p.unlock() after
// p.mu.Lock().
func isRelease(c *ast.CallExpr, x, want string, src func(ast.Expr) string) bool {
	if len(c.Args) != 0 {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name == want && src(sel.X) == x {
		return true
	}
	return want == "Unlock" && sel.Sel.Name == "unlock" && src(sel.X)+".mu" == x
}

// isUnlocked: c is supervise.Unlocked(name, <release>) (or Unlocked inside
// package supervise) with release x.Unlock, or the pool's p.unlock.
func isUnlocked(c *ast.CallExpr, release string, src func(ast.Expr) string) bool {
	name := ""
	switch f := c.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && id.Name == "supervise" {
			name = f.Sel.Name
		}
	case *ast.Ident:
		name = f.Name
	}
	if name != "Unlocked" || len(c.Args) != 2 {
		return false
	}
	got := src(c.Args[1])
	if got == release {
		return true
	}
	return strings.HasSuffix(release, ".mu.Unlock") && got == strings.TrimSuffix(release, ".mu.Unlock")+".unlock"
}

func TestLockScan(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s", root)
	}
	used := map[string]bool{}
	var bad []string
	for _, f := range lockScan(t, root) {
		k := f.kind + " " + f.key
		if lockAllowlist[k] != "" {
			used[k] = true
			continue
		}
		bad = append(bad, k+"\t"+f.pos)
	}
	var stale []string
	for k := range lockAllowlist {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(bad)
	sort.Strings(stale)
	report := t.Logf
	if lockEnforce {
		report = t.Errorf
	}
	for _, b := range bad {
		report("lock without its deferred release: %s", b)
	}
	for _, s := range stale {
		report("lock allowlist entry matches nothing (remove it): %s", s)
	}
	t.Logf("%d lock sites, %d stale allowlist entries (enforce=%v)", len(bad), len(stale), lockEnforce)
}
