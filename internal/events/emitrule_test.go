package events

import (
	"fmt"
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

// The row rule (the package comment): an event that describes a row change
// is written by Commit, in the transaction that changes the row, so commit
// order is publish order. Emit and Append are for events with no row. That
// cannot be checked by behaviour until two writers interleave (sup.write and
// the secrets writes did, latently, until R4(a)); what can be checked is
// where the bare calls are. So every non-test file is parsed, and each call
// of Emit, or of Append on an Events field, must be named in bare below, by
// file and enclosing function, with the reason it has no row. A new one fails
// by name; an entry nothing matches any more fails too.

// bareCall is one Emit or Append call: in which file, inside which top-level
// function (Type.Method; a closure counts as its enclosing function).
type bareCall struct {
	File, Func, Method string
}

func (c bareCall) String() string { return fmt.Sprintf("%s: %s: %s", c.File, c.Func, c.Method) }

type bareAllowance struct {
	bareCall
	N      int // how many times it may appear there; 0 means once
	Reason string
}

var bare = []bareAllowance{
	{bareCall{"internal/broker/broker.go", "Broker.token", "Emit"}, 0,
		"token.refused: a mint GitHub refused writes no row (token_grant is for tokens issued)"},
	{bareCall{"internal/catalog/catalog.go", "Catalog.run", "Emit"}, 0,
		"repo.refresh_failed: a failed refresh writes nothing; the failure is held in memory for GET"},
	{bareCall{"internal/identity/watch.go", "Watch.checked", "Emit"}, 0,
		"auth.identity_checked answers a Trigger with the stored view and describes no change; the verdict's own row change, when there is one, is auth.identity, in store's Commit"},
	{bareCall{"internal/identity/watch.go", "Watch.failed", "Append"}, 0,
		"the fallback when last_checked_at cannot be written in failed's Commit: the event alone, describing no row change, because a Trigger is owed an answer"},
	{bareCall{"internal/login/login.go", "Manager.emit", "Emit"}, 0,
		"auth.login: the handshake's phases are held in memory, never a row"},
	{bareCall{"internal/provision/lifecycle.go", "Provisioner.SweepHelpers", "Emit"}, 0,
		"container.helpers_swept: Docker containers removed, no row"},
	{bareCall{"internal/provision/lifecycle.go", "Provisioner.emit", "Emit"}, 0,
		"container.unpaused and container.repaused: a container's pause state, which Docker holds, no row"},
	{bareCall{"internal/provision/lifecycle.go", "Provisioner.subSteps", "Emit"}, 0,
		"workspace.action: the event is the record (the view's last_action reads it), no row"},
	{bareCall{"internal/reconcile/reconcile.go", "Reconciler.apply", "Emit"}, 0,
		"container.unclaimed: a container left alone, no row"},
	{bareCall{"internal/secrets/store.go", "Store.noteLocked", "Emit"}, 2,
		"secret.undeliverable and secret.deliverable: the verdict of the in-memory snapshot, no row"},
	{bareCall{"internal/server/server.go", "Server.Serve", "Emit"}, 0,
		"system.reconcile: a boot warning, no row"},
	{bareCall{"internal/workspace/job.go", "Store.EndJob", "Append"}, 0,
		"workspace.job, the job's end, when its last commit did not carry it: the event is the record, no row"},
	{bareCall{"internal/workspace/steps.go", "Store.stepEvent", "Append"}, 0,
		"workspace.step: the event is the record (the views read steps from the log), no row"},
}

// scanBare returns the bare calls in one file's source.
func scanBare(rel string, src []byte) ([]bareCall, error) {
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, 0)
	if err != nil {
		return nil, err
	}
	var out []bareCall
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
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
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Emit":
				out = append(out, bareCall{rel, name, "Emit"})
			case "Append":
				// append is lower case; Append on anything named Events is
				// this package's.
				if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "Events" {
					out = append(out, bareCall{rel, name, "Append"})
				}
			}
			return true
		})
	}
	return out, nil
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = up
	}
}

// TestRowRule is the rule over the whole module.
func TestRowRule(t *testing.T) {
	root := moduleRoot(t)
	var found []bareCall
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
		// This package defines them.
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/events/") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		calls, err := scanBare(rel, src)
		if err != nil {
			return err
		}
		found = append(found, calls...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("found no call at all: the walk read nothing (the positive control)")
	}
	count := map[bareCall]int{}
	for _, c := range found {
		count[c]++
	}
	allow := map[bareCall]bareAllowance{}
	for _, a := range bare {
		if _, dup := allow[a.bareCall]; dup {
			t.Errorf("allowlist names %s twice", a.bareCall)
		}
		if strings.TrimSpace(a.Reason) == "" {
			t.Errorf("allowlist entry %s has no reason", a.bareCall)
		}
		allow[a.bareCall] = a
	}
	var bad []string
	for c, n := range count {
		a, ok := allow[c]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s (×%d) is not allowed: an event that describes a row change goes through events.Commit with the row; a bare call needs an allowlist entry saying why it has no row", c, n))
			continue
		}
		max := a.N
		if max == 0 {
			max = 1
		}
		if n > max {
			bad = append(bad, fmt.Sprintf("%s appears %d times; the allowlist allows %d", c, n, max))
		}
	}
	for c := range allow {
		if count[c] == 0 {
			bad = append(bad, fmt.Sprintf("allowlist entry %s matches nothing: remove it", c))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

// TestRowRuleScanner is the scanner's own control: what it must see and what
// it must not.
func TestRowRuleScanner(t *testing.T) {
	src := `package p
func (s *Store) put() {
	s.Events.Emit(ctx, "", Info, "k", "m", nil)
	func() { s.log.Emit(ctx, "", Info, "k", "m", nil) }()
	s.Events.Append(ctx, e)
	s.Events.Commit(ctx, fn)
	xs = append(xs, 1)
	b.Append(x)
}
func free() { l.Emit() }
`
	got, err := scanBare("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []bareCall{
		{"p.go", "Store.put", "Emit"}, {"p.go", "Store.put", "Emit"}, {"p.go", "Store.put", "Append"},
		{"p.go", "free", "Emit"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("scanned %v, want %v", got, want)
	}
}
