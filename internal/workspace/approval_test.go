package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// checkedClock is the store's clock with a signal on its first Now. Approve
// reads the clock once, after its pre-transaction check of the request and
// before its transaction, so that signal is the moment the pre-check has
// passed — which approveNowLine proves from the source and the recorded
// caller proves at run time. Nothing waits in Now: it only reports.
type checkedClock struct {
	sys.Clock
	once    sync.Once
	checked chan struct{}
	caller  string // the first Now's caller, as "function:line"
}

func (c *checkedClock) Now() time.Time {
	pc, _, line, _ := runtime.Caller(1)
	c.once.Do(func() {
		c.caller = fmt.Sprintf("%s:%d", runtime.FuncForPC(pc).Name(), line)
		close(c.checked)
	})
	return c.Clock.Now()
}

// approveNowLine is the line of Approve's one clock read, after proving from
// approval.go that the read is where the race test needs it: a statement of
// Approve's own body (not inside the Commit closure, nor any other function
// literal), the only Clock.Now in Approve, and after the pre-check — the
// top-level if comparing hash with p.Hash, itself after the s.Pending call.
// If the read moved before the pre-check, the test's commit could land
// before the pre-check ran, the pre-check would refuse, and the
// in-transaction check would never be exercised while the test still
// passed; this refuses that arrangement by name.
func approveNowLine(t *testing.T) int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "approval.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "Approve" && f.Recv != nil {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("approval.go has no (*Store).Approve")
	}
	isSel := func(n ast.Node, recv, name string) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		return recv == "" || types.ExprString(sel.X) == recv
	}
	var nows []*ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if isSel(n, "s.Env.Clock", "Now") {
			nows = append(nows, n.(*ast.CallExpr))
		}
		return true
	})
	if len(nows) != 1 {
		t.Fatalf("Approve reads the clock %d times; the race test relies on exactly one read", len(nows))
	}
	pending, precheck, now := token.NoPos, token.NoPos, token.NoPos
	for _, st := range fn.Body.List {
		// has walks the statement but not into function literals.
		has := func(pred func(ast.Node) bool) bool {
			found := false
			ast.Inspect(st, func(n ast.Node) bool {
				if _, lit := n.(*ast.FuncLit); lit {
					return false
				}
				found = found || pred(n)
				return !found
			})
			return found
		}
		if pending == token.NoPos && has(func(n ast.Node) bool { return isSel(n, "s", "Pending") }) {
			pending = st.Pos()
		}
		if is, ok := st.(*ast.IfStmt); ok && precheck == token.NoPos && pending != token.NoPos &&
			strings.Contains(types.ExprString(is.Cond), "hash != p.Hash") {
			precheck = st.End()
		}
		if has(func(n ast.Node) bool { return n == nows[0] }) {
			now = st.Pos()
		}
	}
	switch {
	case pending == token.NoPos || precheck == token.NoPos:
		t.Fatal("Approve has no s.Pending call followed by a top-level `hash != p.Hash` pre-check")
	case now == token.NoPos:
		t.Fatal("Approve's clock read is inside a function literal (the Commit closure?), not its own body")
	case now < precheck:
		t.Fatalf("Approve reads the clock (line %d) before its pre-check ends (line %d): the race test's signal "+
			"would no longer mean the pre-check passed", fset.Position(now).Line, fset.Position(precheck).Line)
	}
	return fset.Position(nows[0].Pos()).Line
}

// TestApproveRechecksInsideItsTransaction is the race between Approve's
// check of the request, which runs before its transaction, and something
// that changes the request or the repository's approval before that
// transaction takes the write lock: a new run replacing the request, a
// second approval landing, the workspace moving on. The pre-check catches
// every serial case, so only an interleaving shows whether the transaction
// checks again.
//
// The interleaving is made deterministic with the database's own write
// lock, not a timing window: a second connection opens an IMMEDIATE
// transaction and makes the change in it, uncommitted. Approve's pre-check
// reads the last committed state (WAL), so it sees the request as it was
// and passes; its transaction then waits at BEGIN IMMEDIATE behind that
// lock. The test commits the change only once the clock has said the
// pre-check is over, so Approve's transaction is the first to see it. That
// signal means "the pre-check passed" only while Approve's one clock read
// follows it: approveNowLine checks that in the source, and each run checks
// that the first Now came from that line of Approve, so a refactor that
// moves the read fails here by name instead of quietly letting the commit
// land before the pre-check (which would then refuse, and pass the test
// without the in-transaction checks ever running).
//
// The first case is the positive control: the same sequence with nothing
// changed approves. Mutation-checked: without the in-transaction hash check
// "the request is replaced" fails; without the baseline check "another
// approval lands" fails; without the state condition "the workspace moves
// on" fails; with the clock read moved above the pre-check, every case fails.
func TestApproveRechecksInsideItsTransaction(t *testing.T) {
	nowLine := approveNowLine(t)
	wantCaller := fmt.Sprintf("github.com/krelinga/drydock/internal/workspace.(*Store).Approve:%d", nowLine)
	const otherHash = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tc := range []struct {
		name string
		// change runs in the lock holder's transaction, before Approve's
		// pre-check; it is committed only after the pre-check.
		change func(ctx context.Context, tx *sql.Tx, w Workspace) error
		want   error
	}{
		{"positive control: nothing changes", nil, nil},
		{"the request is replaced", func(ctx context.Context, tx *sql.Tx, w Workspace) error {
			// A new run stopped at a different subset, against the same
			// baseline (none): only the hash tells them apart.
			b, err := json.Marshal(PendingApproval{Hash: otherHash, Settings: json.RawMessage(`[]`),
				Added: json.RawMessage(`[]`), Changed: json.RawMessage(`[]`), Removed: json.RawMessage(`[]`)})
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE workspace SET pending_approval = ? WHERE id = ?`, string(b), w.ID)
			return err
		}, ErrApprovalStale},
		{"another approval lands", func(ctx context.Context, tx *sql.Tx, w Workspace) error {
			// What a second Approve of the same request writes: the request
			// stays on the row (only a move clears it), so its hash still
			// matches, and the repository's approval is no longer the
			// baseline the request was shown against.
			_, err := tx.ExecContext(ctx, `INSERT INTO config_approval
				(repository_id, hash, settings, workspace_id, approved_by, approved_at)
				VALUES (?, ?, '[]', ?, 'session-other', '2026-10-04T12:00:00Z')`, w.RepositoryID, otherHash, w.ID)
			return err
		}, ErrApprovalStale},
		{"the workspace moves on", func(ctx context.Context, tx *sql.Tx, w Workspace) error {
			_, err := tx.ExecContext(ctx, `UPDATE workspace SET state = ? WHERE id = ?`, string(Building), w.ID)
			return err
		}, ErrNoApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := newFixture(t)
			w := f.create(t, 1)
			for _, to := range []State{Cloning, Building} {
				if _, err := f.store.Move(ctx, w.ID, to, ""); err != nil {
					t.Fatal(err)
				}
			}
			const hash = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			req := PendingApproval{Hash: hash, Settings: json.RawMessage(`[{"field":"privileged"}]`),
				Added: json.RawMessage(`[]`), Changed: json.RawMessage(`[]`), Removed: json.RawMessage(`[]`)}
			if err := f.store.awaitApproval(ctx, w.ID, "needs approval", req); err != nil {
				t.Fatal(err)
			}
			clock := &checkedClock{Clock: f.store.Env.Clock, checked: make(chan struct{})}
			f.store.Env.Clock = clock

			// The lock holder: a connection of its own, an IMMEDIATE
			// transaction (the store's DSN), the change made and held.
			conn, err := f.store.DB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if tc.change != nil {
				if err := tc.change(ctx, tx, w); err != nil {
					t.Fatal(err)
				}
			}

			done := make(chan error, 1)
			go func() { _, err := f.store.Approve(ctx, w.ID, hash, "session-x"); done <- err }()
			select {
			case <-clock.checked:
			case err := <-done:
				t.Fatalf("Approve returned %v before its pre-check passed: the race was not set up", err)
			case <-ctx.Done():
				t.Fatal("Approve never passed its pre-check")
			}
			if clock.caller != wantCaller {
				t.Fatalf("the clock's first read came from %s, not %s: the signal does not mark the pre-check's end",
					clock.caller, wantCaller)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var got error
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("Approve never returned once the lock was released")
			}

			var mine int
			if err := f.store.DB.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM config_approval WHERE approved_by = 'session-x'`).Scan(&mine); err != nil {
				t.Fatal(err)
			}
			all, err := f.events.Since(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			approvedEvents := 0
			for _, e := range all {
				if e.Kind == events.KindConfigApproved {
					approvedEvents++
				}
			}
			if tc.want == nil {
				if got != nil || mine != 1 || approvedEvents != 1 {
					t.Fatalf("Approve: %v, %d approval rows, %d config.approved events; want it approved once",
						got, mine, approvedEvents)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("Approve: %v; want %v", got, tc.want)
			}
			if mine != 0 || approvedEvents != 0 {
				t.Fatalf("a refused Approve recorded %d approval rows and %d config.approved events", mine, approvedEvents)
			}
		})
	}
}
