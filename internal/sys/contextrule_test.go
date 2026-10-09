package sys

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The context rule (CLAUDE.md, *Working conventions*), as a meta-test over the source:
//
//  1. A request's context is for the synchronous part of a handler only.
//  2. Background work runs under its component's context.
//  3. Bookkeeping or cleanup after a cancellation uses sys.Cleanup.
//  4. Shared, joinable work never runs under a caller's context.
//
// None of those can be checked by a test of behaviour until the bug they
// prevent happens (#83, #85, #94's probe removal). What can be checked is the
// shape every one of those bugs had: a context made from nothing, or from a
// parent with its cancellation removed, or a timer on the wall clock, which a
// test cannot move. So every non-test file is parsed, and each reference to
// one of banned below — a call or a function value — must be in an exempt
// directory or named in allowed, by file and enclosing function, with a
// reason. A new one fails by name; an entry nothing matches any more fails
// too, so the list only shrinks honestly.

// banned are the references the rule watches, by import path and name.
var banned = map[string]map[string]bool{
	"context": {"Background": true, "TODO": true, "WithoutCancel": true},
	"time": {
		"After": true, "AfterFunc": true, "Now": true, "Since": true, "Until": true,
		"Sleep": true, "NewTimer": true, "NewTicker": true, "Tick": true,
	},
}

// exempt reports whether a module-relative, slash-separated path is outside
// the rule: the entry points, this package (the seams themselves), the test
// helper packages (any directory under internal/ named *test, and what is
// beneath it), and the docker guard, which is a separate process with no
// Drydock lifecycle to join.
func exempt(rel string) bool {
	if strings.HasPrefix(rel, "cmd/") || strings.HasPrefix(rel, "internal/sys/") ||
		strings.HasPrefix(rel, "internal/dockerguard/") {
		return true
	}
	dirs := strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/")
	for _, d := range dirs[1:] {
		if strings.HasSuffix(d, "test") {
			return true
		}
	}
	return false
}

// use is one reference: in which file, inside which top-level function
// (Type.Method for a method; a closure counts as its enclosing function),
// to what (context.Background, time.After, …).
type use struct {
	File, Func, Ref string
}

func (u use) String() string { return fmt.Sprintf("%s: %s: %s", u.File, u.Func, u.Ref) }

// allowance is an allowed use and why. N is how many times it may appear in
// that function (0 means once), so a second copy beside an allowed one is
// still new.
type allowance struct {
	use
	N      int
	Reason string
}

const (
	whyRoot    = "component root; R1 replaces with Group"
	whyShutWin = "Shutdown's wait bound on the wall clock; R1 moves it to the injected clock"
)

// allowed is every use the rule tolerates today. Remove an entry when its use
// goes; add one only with a reason a reviewer can check against the rule.
var allowed = []allowance{
	// Component roots: the context every background job of the component
	// runs under, ended by its Shutdown.
	{use{"internal/catalog/catalog.go", "Catalog.init", "context.Background"}, 0, whyRoot},
	{use{"internal/identity/watch.go", "Watch.init", "context.Background"}, 0, whyRoot},
	{use{"internal/login/login.go", "Manager.init", "context.Background"}, 0, whyRoot},
	{use{"internal/provision/provision.go", "Provisioner.launch", "context.Background"}, 0, whyRoot},
	{use{"internal/supervisor/supervisor.go", "Manager.launchLocked", "context.Background"}, 0, whyRoot},

	// Shutdown: bounds that start once the component's context has ended.
	{use{"internal/catalog/catalog.go", "Catalog.Shutdown", "time.After"}, 0, whyShutWin},
	{use{"internal/identity/watch.go", "Watch.Shutdown", "time.After"}, 0, whyShutWin},
	{use{"internal/login/login.go", "Manager.Shutdown", "time.After"}, 0, whyShutWin},
	{use{"internal/provision/provision.go", "Provisioner.Shutdown", "time.After"}, 0, whyShutWin},
	{use{"internal/server/server.go", "Server.Serve", "context.Background"}, 0,
		"the HTTP servers' graceful-shutdown bound, after the serving context has ended; R1 moves it"},

	// Processes whose lifetime is not a context's.
	{use{"internal/login/login.go", "StartProc", "context.Background"}, 0,
		"a login process is ended by the session (kill), never by a context SIGTERMing it"},
	{use{"internal/supervisor/run.go", "sup.runOnce", "context.Background"}, 0,
		"the session server's process: Drydock's shutdown must leave it running, and a stop signals it in the container"},
	{use{"internal/login/login.go", "Proc.kill", "time.After"}, 0,
		"the wait after SIGKILL for a real process to be reaped: wall time"},

	// Bookkeeping and cleanup owed after a cancellation: rule 3's cases from
	// before sys.Cleanup. The unbounded ones write to the local database; the
	// rest are in code #96 is changing (provision, supervisor), or would put
	// a timer on a fake clock whose Waiting a test counts.
	{use{"internal/login/login.go", "session.finish", "context.Background"}, 2,
		"removing the login container (a wall-clock bound; a fake-clock one would join the login tests' Waiting) and announcing the end, owed after the session's context ended"},
	{use{"internal/login/login.go", "Manager.emit", "context.Background"}, 0,
		"an announcement owed after its context ended; the event log ends with the store"},
	{use{"internal/login/login.go", "session.succeed", "time.After"}, 0,
		"Settle, the wall-clock grace for the real process to exit by itself after a success"},
	{use{"internal/identity/watch.go", "Watch.end", "context.WithoutCancel"}, 0,
		"answers to a finished check, written whoever stopped waiting; a local event write"},
	{use{"internal/provision/provision.go", "Provisioner.run", "context.WithoutCancel"}, 0,
		"book: a run's step events and its move to failed, owed after cancellation"},
	{use{"internal/provision/lifecycle.go", "Provisioner.stopJob", "context.WithoutCancel"}, 0,
		"book: the stop's own record, owed after cancellation"},
	{use{"internal/provision/lifecycle.go", "Provisioner.deleteJob", "context.WithoutCancel"}, 0,
		"book: the delete's own record, owed after cancellation"},
	{use{"internal/provision/lifecycle.go", "Provisioner.subSteps", "context.WithoutCancel"}, 0,
		"book: each sub-step's workspace.action event, owed after cancellation"},
	{use{"internal/provision/steps.go", "runState.up", "context.WithoutCancel"}, 0,
		"records the container up created, even when up's context then ended"},
	{use{"internal/provision/messages.go", "Provisioner.keepBuildLog", "context.WithoutCancel"}, 0,
		"the secret values a cancelled build's log is redacted with"},
	{use{"internal/provision/provision.go", "Provisioner.logTail", "context.WithoutCancel"}, 0,
		"the secret values a cancelled step's log tail is redacted with"},
	{use{"internal/supervisor/supervisor.go", "Manager.Restart", "context.WithoutCancel"}, 0,
		"the degraded answer to a restart whose start failed, owed after the caller's context ended"},
	{use{"internal/supervisor/supervisor.go", "Manager.Stop", "context.WithoutCancel"}, 0,
		"book: the stop's state record, owed after cancellation"},
	{use{"internal/supervisor/run.go", "sup.runOnce", "context.WithoutCancel"}, 0,
		"stopping a hung server, which must finish after the run that found it has ended"},
	{use{"internal/supervisor/supervisor.go", "Manager.redactValues", "context.Background"}, 0,
		"the ring outlives a run, so a flush after a cancelled run must still be masked; a wall-clock bound"},
	{use{"internal/login/docker.go", "DockerLauncher.Remove", "time.Now"}, 2,
		"RemoveSettle's deadline for a container the real daemon is still creating"},
	{use{"internal/login/docker.go", "DockerLauncher.Remove", "time.After"}, 0,
		"RemoveSettle's poll interval against the real daemon"},

	// Per-connection bounds where no request context exists.
	{use{"internal/broker/broker.go", "Broker.handle", "context.Background"}, 0,
		"a broker connection's own requestTimeout: a raw socket carries no request context"},
	{use{"internal/broker/broker.go", "Broker.handle", "time.Now"}, 0,
		"the socket's SetDeadline, which the kernel measures in wall time"},
	{use{"internal/preview/proxy.go", "upgrade.stillHolds", "context.Background"}, 0,
		"re-asking the gate mid-WebSocket, bounded on its own: the upgrade's request context is gone"},
}

// moduleRoot finds go.mod above the test's directory.
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
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

// scan returns every banned reference in one file's source. A dot import of
// context or time is reported as a use of its own, since it would hide every
// reference from this check.
func scan(rel string, src []byte) ([]use, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	// The local name each watched package is known by in this file.
	names := map[string]string{}
	var uses []use
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || banned[p] == nil {
			continue
		}
		local := p
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch local {
		case "_":
			continue
		case ".":
			uses = append(uses, use{rel, "<imports>", "dot import of " + p})
			continue
		}
		names[local] = p
	}
	if len(names) == 0 {
		return uses, nil
	}
	for _, d := range f.Decls {
		fn := "<package>"
		if fd, ok := d.(*ast.FuncDecl); ok {
			fn = funcName(fd)
		}
		// Names a function declares itself (parameters, locals) shadow an
		// import; a reference through one is not to the package. Resolution
		// is skipped above, so this is a conservative per-function check.
		shadow := declared(d)
		ast.Inspect(d, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || shadow[id.Name] {
				return true
			}
			p, ok := names[id.Name]
			if ok && banned[p][sel.Sel.Name] {
				uses = append(uses, use{rel, fn, p + "." + sel.Sel.Name})
			}
			return true
		})
	}
	return uses, nil
}

// declared is every name a declaration binds inside itself.
func declared(d ast.Decl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Field:
			for _, id := range n.Names {
				out[id.Name] = true
			}
		case *ast.ValueSpec:
			if _, top := d.(*ast.GenDecl); !top {
				for _, id := range n.Names {
					out[id.Name] = true
				}
			}
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, e := range n.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		}
		return true
	})
	return out
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fd.Name.Name
		}
		return "?." + fd.Name.Name
	}
}

// TestContextRule is the rule over the whole module.
func TestContextRule(t *testing.T) {
	root := moduleRoot(t)
	var found []use
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
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || exempt(rel) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		uses, err := scan(rel, src)
		if err != nil {
			return err
		}
		found = append(found, uses...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("found no use at all: the walk read nothing (the positive control)")
	}

	count := map[use]int{}
	for _, u := range found {
		count[u]++
	}
	allow := map[use]allowance{}
	for _, a := range allowed {
		if _, dup := allow[a.use]; dup {
			t.Errorf("allowlist names %s twice", a.use)
		}
		if strings.TrimSpace(a.Reason) == "" {
			t.Errorf("allowlist entry %s has no reason", a.use)
		}
		allow[a.use] = a
	}
	var bad []string
	for u, n := range count {
		a, ok := allow[u]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s (×%d) is not allowed: see the context rule in CLAUDE.md — use the component's context, sys.Cleanup or a sys.Clock, or add an allowlist entry with a reason", u, n))
			continue
		}
		max := a.N
		if max == 0 {
			max = 1
		}
		if n > max {
			bad = append(bad, fmt.Sprintf("%s appears %d times; the allowlist allows %d", u, n, max))
		}
	}
	for u := range allow {
		if count[u] == 0 {
			bad = append(bad, fmt.Sprintf("allowlist entry %s matches nothing: remove it", u))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

// TestContextRuleScanner is the scanner's own control: what it must see and
// what it must not.
func TestContextRuleScanner(t *testing.T) {
	src := `package p

import (
	ctx "context"
	tm "time"
)

var at = tm.Now

type T struct{}

func (t *T) M() { _ = ctx.Background() }

func F() {
	go func() { <-tm.After(1) }()
	f := ctx.WithoutCancel
	_ = f
}

func G(ctx struct{ Background func() }) { ctx.Background() }

func H() {
	tm := struct{ Sleep func(int) }{}
	tm.Sleep(1)
}
`
	got, err := scan("x/p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, u := range got {
		s = append(s, u.String())
	}
	want := []string{
		"x/p.go: <package>: time.Now",
		"x/p.go: T.M: context.Background",
		"x/p.go: F: time.After",
		"x/p.go: F: context.WithoutCancel",
	}
	if strings.Join(s, "\n") != strings.Join(want, "\n") {
		t.Fatalf("scan =\n%s\nwant\n%s", strings.Join(s, "\n"), strings.Join(want, "\n"))
	}

	dot, err := scan("x/d.go", []byte("package p\nimport . \"context\"\nfunc F() { _ = Background() }\n"))
	if err != nil || len(dot) != 1 || dot[0].Ref != "dot import of context" {
		t.Fatalf("a dot import must be reported: %v %v", dot, err)
	}

	for rel, want := range map[string]bool{
		"cmd/drydock/main.go":                  true,
		"internal/sys/sys.go":                  true,
		"internal/dockerguard/logprobe.go":     true,
		"internal/claudetest/fakeclaude/x.go":  true,
		"internal/github/githubtest/fake.go":   true,
		"internal/login/logintest/launcher.go": true,
		"internal/login/login.go":              false,
		"internal/systest.go":                  false,
	} {
		if exempt(rel) != want {
			t.Errorf("exempt(%q) = %v; want %v", rel, !want, want)
		}
	}
}
