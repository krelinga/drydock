package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// checkedClock is the store's clock with a signal on its first Now. Approve
// reads the clock once, after its pre-transaction check of the request and
// before its transaction, so that signal is the moment the pre-check has
// passed. Nothing waits in Now: it only reports.
type checkedClock struct {
	sys.Clock
	once    sync.Once
	checked chan struct{}
}

func (c *checkedClock) Now() time.Time {
	c.once.Do(func() { close(c.checked) })
	return c.Clock.Now()
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
// pre-check is over, so Approve's transaction is the first to see it.
//
// The first case is the positive control: the same sequence with nothing
// changed approves. Mutation-checked: without the in-transaction hash check
// "the request is replaced" fails; without the baseline check "another
// approval lands" fails; without the state condition "the workspace moves
// on" fails.
func TestApproveRechecksInsideItsTransaction(t *testing.T) {
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
