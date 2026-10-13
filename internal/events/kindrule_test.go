package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The kind registry (kinds.go) is only a registry if nothing spells a kind
// anywhere else, and only honest if everything in it is emitted. Neither is
// something the compiler checks: Go converts an untyped string literal to
// Kind silently, so typing Emit's parameter alone would let
// Emit(…, "token.refused2", …) through. So TestKindRegistry typechecks every
// non-test package of the module with go/types and holds four rules:
//
//  1. Every constant expression of type Kind traces to an identifier declared
//     in kinds.go — a literal, a Kind("…") conversion, or a constant declared
//     elsewhere (even a registry constant plus a suffix) fails by file and
//     line, and so does declaring a Kind constant outside kinds.go.
//  2. A conversion to Kind from a non-constant fails, except the one scan of
//     a stored row (kindFromRow below): a kind from a variable cannot be
//     checked.
//  3. Every registered kind is emitted: it reaches NewEvent's or Emit's kind,
//     or an Event's Kind field, through parameters, local and package
//     variables, closures and single-result helpers.
//  4. Every registered kind has a scope in Scopes, and every key of Scopes is
//     registered.

// kindFromRow is the one place a Kind may be made from a string: the log's
// scan of a stored row, whose kind was a registered constant when written.
var kindFromRow = struct{ File, Func string }{"internal/events/events.go", "scan"}

const modulePath = "github.com/krelinga/drydock"

// checkedPkg is one of the module's packages, typechecked from its non-test
// source.
type checkedPkg struct {
	Path  string
	Files []*ast.File
	Rel   []string // each file, relative to the module root
	Info  *types.Info
	Pkg   *types.Package
}

type listedPkg struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	CgoFiles   []string
	Module     *struct{ Path string }
}

// loadModule typechecks every package of the module from source, importing
// everything else from the export data `go list -export` names. The module's
// own packages import each other from source, so an object is one object
// across the whole module (a parameter of a helper in one package is the same
// *types.Var at a call site in another).
func loadModule(t *testing.T, root string) (*token.FileSet, []*checkedPkg) {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,Export,GoFiles,CgoFiles,Module", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var listed []listedPkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, p)
	}
	exports := map[string]string{}
	for _, p := range listed {
		exports[p.ImportPath] = p.Export
	}
	fset := token.NewFileSet()
	gc := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	mine := map[string]*types.Package{}
	imp := importerFunc(func(path string) (*types.Package, error) {
		if p, ok := mine[path]; ok {
			return p, nil
		}
		return gc.Import(path)
	})
	var pkgs []*checkedPkg
	for _, p := range listed { // -deps lists every package after its dependencies
		if p.Module == nil || p.Module.Path != modulePath {
			continue
		}
		if len(p.CgoFiles) > 0 {
			t.Fatalf("%s has cgo files, which this loader does not typecheck", p.ImportPath)
		}
		cp := &checkedPkg{Path: p.ImportPath}
		for _, name := range p.GoFiles {
			full := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, full)
			cp.Files = append(cp.Files, f)
			cp.Rel = append(cp.Rel, filepath.ToSlash(rel))
		}
		cp.Info = &types.Info{
			Types:      map[ast.Expr]types.TypeAndValue{},
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", runtime.GOARCH)}
		cp.Pkg, err = conf.Check(p.ImportPath, fset, cp.Files, cp.Info)
		if err != nil {
			t.Fatalf("typecheck %s: %v", p.ImportPath, err)
		}
		mine[p.ImportPath] = cp.Pkg
		pkgs = append(pkgs, cp)
	}
	return fset, pkgs
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// kindReport is what the analysis finds; TestKindRegistry turns it into
// failures, and a mutation shows up here first.
type kindReport struct {
	Registered map[string]*types.Const   // name → constant, from kinds.go
	Used       map[*types.Const][]string // registered constant → where it reaches an event
	Violations []string                  // rules 1 and 2, by file:line
}

func analyzeKinds(fset *token.FileSet, pkgs []*checkedPkg) (kindReport, error) {
	r := kindReport{Registered: map[string]*types.Const{}, Used: map[*types.Const][]string{}}
	var events *checkedPkg
	for _, p := range pkgs {
		if p.Path == modulePath+"/internal/events" {
			events = p
		}
	}
	if events == nil {
		return r, errors.New("internal/events was not loaded")
	}
	kindTN, _ := events.Pkg.Scope().Lookup("Kind").(*types.TypeName)
	eventTN, _ := events.Pkg.Scope().Lookup("Event").(*types.TypeName)
	if kindTN == nil || eventTN == nil {
		return r, errors.New("internal/events declares no Kind or no Event")
	}
	isKind := func(t types.Type) bool { return t != nil && types.Identical(t, kindTN.Type()) }
	isEvent := func(t types.Type) bool { return t != nil && types.Identical(t, eventTN.Type()) }
	const kindsFile = "internal/events/kinds.go"

	// The registry: every Kind constant kinds.go declares.
	registered := map[*types.Const]bool{}
	for i, f := range events.Files {
		if events.Rel[i] != kindsFile {
			continue
		}
		for id, obj := range events.Info.Defs {
			c, ok := obj.(*types.Const)
			if !ok || !isKind(c.Type()) || fset.File(id.Pos()) != fset.File(f.Pos()) {
				continue
			}
			r.Registered[c.Name()] = c
			registered[c] = true
		}
	}
	at := func(n ast.Node) string {
		p := fset.Position(n.Pos())
		for _, cp := range pkgs {
			for i, f := range cp.Files {
				if fset.File(f.Pos()) == fset.File(n.Pos()) {
					return fmt.Sprintf("%s:%d", cp.Rel[i], p.Line)
				}
			}
		}
		return p.String()
	}
	// objOf is the object an identifier or a qualified identifier names.
	objOf := func(info *types.Info, e ast.Expr) types.Object {
		switch e := ast.Unparen(e).(type) {
		case *ast.Ident:
			if o := info.Uses[e]; o != nil {
				return o
			}
			return info.Defs[e]
		case *ast.SelectorExpr:
			if s := info.Selections[e]; s != nil {
				return s.Obj()
			}
			return info.Uses[e.Sel]
		}
		return nil
	}

	// What the flow analysis needs, over the whole module: where each
	// variable is assigned, each function's parameters and returns, and
	// every call by its callee.
	type param struct {
		fn  types.Object // *types.Func, or the *types.Var a closure is assigned to
		idx int
	}
	type site struct {
		info *types.Info
		e    ast.Expr
	}
	assigns := map[*types.Var][]site{}
	paramOf := map[*types.Var]param{}
	returns := map[types.Object][]site{}
	calls := map[types.Object][]site{} // callee → each call, as the CallExpr
	var work []site                    // expressions whose value becomes an event's kind

	for _, cp := range pkgs {
		info := cp.Info
		for i, f := range cp.Files {
			rel := cp.Rel[i]
			// Rule 1's second half: no Kind constant declared outside kinds.go.
			if rel != kindsFile {
				for id, obj := range info.Defs {
					if c, ok := obj.(*types.Const); ok && isKind(c.Type()) && fset.File(id.Pos()) == fset.File(f.Pos()) {
						r.Violations = append(r.Violations, fmt.Sprintf("%s: the Kind constant %s is declared outside kinds.go: register it there, and use the registry's", at(id), c.Name()))
					}
				}
			}
			curFunc := ""
			var walk func(n ast.Node, fnObj types.Object) // fnObj: the function whose returns this body's return statements give
			// sigParams records a function's parameters as belonging to it.
			sigParams := func(fn types.Object, sig *types.Signature) {
				for j := 0; j < sig.Params().Len(); j++ {
					paramOf[sig.Params().At(j)] = param{fn, j}
				}
			}
			walk = func(n ast.Node, fnObj types.Object) {
				ast.Inspect(n, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.FuncLit:
						// A closure assigned to a variable is that variable's;
						// one that is not is followed only into its body.
						walk(n.Body, nil)
						return false
					case *ast.AssignStmt:
						if len(n.Lhs) == len(n.Rhs) {
							for j, l := range n.Lhs {
								rhs := n.Rhs[j]
								if v, ok := objOf(info, l).(*types.Var); ok {
									assigns[v] = append(assigns[v], site{info, rhs})
									if fl, ok := rhs.(*ast.FuncLit); ok {
										sigParams(v, info.Types[fl].Type.(*types.Signature))
										walk(fl.Body, v)
										continue
									}
								}
								walk(rhs, fnObj)
								if sel, ok := ast.Unparen(l).(*ast.SelectorExpr); ok && sel.Sel.Name == "Kind" {
									if s := info.Selections[sel]; s != nil && s.Kind() == types.FieldVal && isEvent(derefType(s.Recv())) {
										work = append(work, site{info, rhs})
									}
								}
							}
							return false
						}
					case *ast.ValueSpec:
						for j, name := range n.Names {
							if j < len(n.Values) {
								if v, ok := info.Defs[name].(*types.Var); ok {
									assigns[v] = append(assigns[v], site{info, n.Values[j]})
									if fl, ok := n.Values[j].(*ast.FuncLit); ok {
										sigParams(v, info.Types[fl].Type.(*types.Signature))
										walk(fl.Body, v)
										continue
									}
								}
								walk(n.Values[j], fnObj)
							}
						}
						return false
					case *ast.ReturnStmt:
						if fnObj != nil && len(n.Results) == 1 {
							returns[fnObj] = append(returns[fnObj], site{info, n.Results[0]})
						}
					case *ast.CompositeLit:
						if isEvent(info.Types[n].Type) {
							for _, el := range n.Elts {
								if kv, ok := el.(*ast.KeyValueExpr); ok {
									if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Kind" {
										work = append(work, site{info, kv.Value})
									}
								}
							}
						}
					case *ast.CallExpr:
						if tv, ok := info.Types[n.Fun]; ok && tv.IsType() {
							// Rule 2: a conversion to Kind from a non-constant.
							if isKind(tv.Type) && len(n.Args) == 1 && info.Types[n.Args[0]].Value == nil {
								here := struct{ File, Func string }{rel, curFunc}
								if here != kindFromRow {
									r.Violations = append(r.Violations, fmt.Sprintf("%s: a Kind converted from a non-constant: a kind from a variable cannot be checked against the registry", at(n)))
								}
							}
							return true
						}
						if callee := objOf(info, n.Fun); callee != nil {
							if fn, ok := callee.(*types.Func); ok {
								callee = fn.Origin()
							}
							calls[callee] = append(calls[callee], site{info, n})
						}
					}
					return true
				})
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					fn, _ := info.Defs[d.Name].(*types.Func)
					if fn == nil || d.Body == nil {
						continue
					}
					sigParams(fn, fn.Type().(*types.Signature))
					curFunc = d.Name.Name
					walk(d.Body, fn)
				case *ast.GenDecl:
					curFunc = ""
					walk(d, nil)
				}
			}
			// Rule 1: every constant expression of type Kind names a
			// registered constant.
			if rel == kindsFile {
				continue
			}
			for e, tv := range info.Types {
				if tv.Value == nil || !isKind(tv.Type) || fset.File(e.Pos()) != fset.File(f.Pos()) {
					continue
				}
				if c, ok := objOf(info, e).(*types.Const); ok && registered[c] {
					continue
				}
				if _, ok := e.(*ast.ParenExpr); ok {
					continue // its operand is checked on its own
				}
				r.Violations = append(r.Violations, fmt.Sprintf("%s: the kind %s is spelled outside kinds.go: use a registered constant (events.Kind…)", at(e), tv.Value.ExactString()))
			}
		}
	}
	// Rule 3: follow every event kind back to the constants that reach it.
	sinkVar := map[*types.Var]bool{}
	sinkFn := map[types.Object]bool{}
	for len(work) > 0 {
		s := work[len(work)-1]
		work = work[:len(work)-1]
		e := ast.Unparen(s.e)
		if call, ok := e.(*ast.CallExpr); ok {
			if callee := objOf(s.info, call.Fun); callee != nil {
				if fn, ok := callee.(*types.Func); ok {
					callee = fn.Origin()
				}
				if !sinkFn[callee] {
					sinkFn[callee] = true
					work = append(work, returns[callee]...)
				}
			}
			continue
		}
		switch o := objOf(s.info, e).(type) {
		case *types.Const:
			if registered[o] {
				r.Used[o] = append(r.Used[o], at(e))
			}
		case *types.Var:
			if sinkVar[o] {
				continue
			}
			sinkVar[o] = true
			work = append(work, assigns[o]...)
			if p, ok := paramOf[o]; ok {
				for _, c := range calls[p.fn] {
					call := c.e.(*ast.CallExpr)
					if p.idx < len(call.Args) && call.Ellipsis == token.NoPos {
						work = append(work, site{c.info, call.Args[p.idx]})
					}
				}
			}
		}
	}
	sort.Strings(r.Violations)
	r.Violations = slices.Compact(r.Violations) // Kind("…") is reported once, not for the call and its literal
	return r, nil
}

func derefType(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// TestKindRegistry holds the module to the registry (the rules above).
func TestKindRegistry(t *testing.T) {
	fset, pkgs := loadModule(t, moduleRoot(t))
	r, err := analyzeKinds(fset, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	// The control: the registry is read, and the flow analysis finds the
	// one use everything depends on, workspace.Move's.
	if len(r.Registered) == 0 {
		t.Fatal("kinds.go registers no kind: the scan read nothing (the positive control)")
	}
	ws := r.Registered["KindWorkspaceState"]
	found := false
	for _, where := range r.Used[ws] {
		if strings.HasPrefix(where, "internal/workspace/") {
			found = true
		}
	}
	if ws == nil || !found {
		t.Fatalf("found no use of KindWorkspaceState in internal/workspace (the positive control); uses: %v", r.Used[ws])
	}

	for _, v := range r.Violations {
		t.Error(v)
	}
	values := map[Kind]string{}
	for name, c := range r.Registered {
		if len(r.Used[c]) == 0 {
			t.Errorf("kinds.go: %s is registered but nothing emits it: remove it, so the registry promises only what the server sends", name)
		}
		k := Kind(constant.StringVal(c.Val()))
		if other, dup := values[k]; dup {
			t.Errorf("kinds.go: %s and %s are both %q", name, other, k)
		}
		values[k] = name
		if _, ok := Scopes[k]; !ok {
			t.Errorf("kinds.go: %s (%q) has no scope in Scopes", name, k)
		}
	}
	for k, s := range Scopes {
		if _, ok := values[k]; !ok {
			t.Errorf("kinds.go: Scopes names %q, which is not a registered kind", k)
		}
		if s != ScopeRead && s != ScopeSession {
			t.Errorf("kinds.go: %q has the unknown scope %q", k, s)
		}
	}
}
