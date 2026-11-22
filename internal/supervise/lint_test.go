package supervise

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The source scan: every goroutine the scheduler process starts is either
// supervised (supervise.Go, or `go supervise.Once(...)`) or named in the
// allowlist below with the reason it needs nothing. It also lists
// time.AfterFunc / Clock.AfterFunc with a function literal, recover() outside
// this package, and any reference to a *ForTest function from production
// code.
//
// enforce is false while the adoption is incremental (step 1 of the
// background-task isolation): the test prints what it finds and fails only
// on a *ForTest reference.
const enforce = false

// allowlist is keyed by "file (receiver) function" (stable across line
// moves); the value is why that site needs no supervision.
var allowlist = map[string]string{}

// scanRoots are the trees the scheduler binary is built from (relative to
// the module root). internal/agent is the node agent, except the two
// packages the scheduler runs for itself.
var scanRoots = []string{"cmd/llm-scheduler", "internal"}

func skipDir(rel string) bool {
	rel = filepath.ToSlash(rel)
	switch rel {
	case "internal/agent", "internal/agent/logship", "internal/agent/filecache":
		return false // files directly in internal/agent are skipped by skipFile
	case "internal/testutil", "internal/e2e", "internal/supervise":
		return true
	}
	return strings.HasPrefix(rel, "internal/agent/")
}

func skipFile(rel string) bool {
	return filepath.ToSlash(filepath.Dir(rel)) == "internal/agent"
}

type finding struct{ key, kind, pos string }

func scan(t *testing.T, root string) []finding {
	t.Helper()
	var out []finding
	fset := token.NewFileSet()
	for _, sr := range scanRoots {
		err := filepath.WalkDir(filepath.Join(root, sr), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if skipDir(rel) {
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
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := rel + " " + funcName(fd)
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if n == nil {
						return false
					}
					pos := fset.Position(n.Pos()).String()
					switch n := n.(type) {
					case *ast.GoStmt:
						if !supervisedCall(n.Call) {
							out = append(out, finding{key, "go", pos})
						}
					case *ast.CallExpr:
						if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "AfterFunc" && len(n.Args) == 2 {
							if _, lit := n.Args[1].(*ast.FuncLit); lit {
								out = append(out, finding{key, "afterfunc", pos})
							}
						}
						if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "recover" && len(n.Args) == 0 {
							out = append(out, finding{key, "recover", pos})
						}
					case *ast.SelectorExpr:
						if strings.HasSuffix(n.Sel.Name, "ForTest") {
							if x, ok := n.X.(*ast.Ident); ok && x.Name == "supervise" {
								out = append(out, finding{key, "fortest", pos})
							}
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

// funcName is "(*T).M", "(T).M" or "F".
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	var b strings.Builder
	b.WriteString("(")
	typ := fd.Recv.List[0].Type
	if st, ok := typ.(*ast.StarExpr); ok {
		b.WriteString("*")
		typ = st.X
	}
	if ix, ok := typ.(*ast.IndexExpr); ok {
		typ = ix.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		b.WriteString(id.Name)
	}
	b.WriteString(").")
	b.WriteString(fd.Name.Name)
	return b.String()
}

// supervisedCall is `supervise.Once(...)` (or Go).
func supervisedCall(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "supervise" && (sel.Sel.Name == "Once" || sel.Sel.Name == "Go")
}

func TestSourceScan(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s", root)
	}
	found := scan(t, root)
	used := map[string]bool{}
	var bad []string
	for _, f := range found {
		if f.kind == "fortest" {
			t.Errorf("%s: a *ForTest function of supervise is referenced from production code (%s)", f.pos, f.key)
			continue
		}
		if _, ok := allowlist[f.key]; ok {
			used[f.key] = true
			continue
		}
		bad = append(bad, f.kind+"\t"+f.key+"\t"+f.pos)
	}
	var stale []string
	for k := range allowlist {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(bad)
	sort.Strings(stale)
	report := t.Logf
	if enforce {
		report = t.Errorf
	}
	for _, b := range bad {
		report("unsupervised and not allowlisted: %s", b)
	}
	for _, s := range stale {
		report("allowlist entry matches nothing (remove it): %s", s)
	}
	t.Logf("%d unsupervised sites, %d stale allowlist entries (enforce=%v)", len(bad), len(stale), enforce)
}
