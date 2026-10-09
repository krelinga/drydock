package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlySSESubscribes: the event log is the record and the SSE feed, and
// nothing inside Drydock follows it. The one internal subscriber there was —
// the session supervisor's Watch, resuming servers on auth.identity — is what
// panicked in #54 (it subscribed after shutdown had closed the log), and it
// started servers beside the provisioner's jobs; a sign-in now reaches the
// supervisors through a direct call (identity.Watch.OnChange →
// provision.ResumeAwaitingLogin), as a job. So every call of Subscribe in a
// non-test file outside this package must be GET /api/events's, by file and
// enclosing function. A new one fails by name; the SSE one not being found
// fails too (the positive control: the walk read the module).
func TestOnlySSESubscribes(t *testing.T) {
	const want = "internal/api/events_routes.go: EventRoutes.stream"
	root := moduleRoot(t)
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", ".claude":
				if rel != "." {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/events/") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), rel, src, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				t := fd.Recv.List[0].Type
				if s, ok := t.(*ast.StarExpr); ok {
					t = s.X
				}
				if id, ok := t.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Subscribe" {
						found = append(found, rel+": "+name)
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
	sawSSE := false
	for _, c := range found {
		if c == want {
			sawSSE = true
			continue
		}
		t.Errorf("%s subscribes to the event log: only the SSE stream may (%s); have the producer call it instead", c, want)
	}
	if !sawSSE {
		t.Errorf("found no Subscribe in %s: the walk read nothing, or the stream moved (the positive control); found %v", want, found)
	}
}
