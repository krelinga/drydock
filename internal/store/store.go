// Package store owns the SQLite file: opening it, holding the single-instance
// lock, and migrating the schema in design §4.
//
// The database is a cache of Docker and of GitHub, not a source of truth for
// anything a container is doing (design §6: "Docker is the truth, the database
// is the cache"). It is also, deliberately, a file with nothing worth stealing
// in it — no plaintext secret, no session token, no GitHub token (§4's four
// absences). Both properties are tested here rather than left to review.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure Go: no cgo, so the binary stays static
)

// DB is the opened store plus the lock that makes it this process's.
type DB struct {
	*sql.DB
	lock *os.File
}

// ErrLocked is returned by Open when another process already holds the store.
var ErrLocked = errors.New("another Drydock is running against this database")

// Open takes the single-instance lock, opens the database in WAL mode, and
// brings the schema up to date.
//
// The lock is taken *before* the database is opened, so a second Drydock fails
// before it can touch the file. Note what the lock does not cover (§13.5,
// testing §5.4): a second instance with its *own* database file is not stopped
// by this — and if it shares the workspace label prefix it will adopt and can
// delete the first instance's containers. That is the label prefix's job, not
// this lock's.
func Open(ctx context.Context, path string) (*DB, error) {
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}

	// Pragmas in the DSN apply to every pooled connection, which matters for
	// foreign_keys in particular: set once with Exec it would hold on one
	// connection and silently not on the rest.
	db, err := sql.Open("sqlite", "file:"+path+"?"+pragmas().Encode())
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &DB{DB: db, lock: lock}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// OpenAdmin opens the database WITHOUT the single-instance lock, for
// short-lived administrative commands — `drydock passwd` — that must work while
// the server runs. Changing a password that may be compromised should not
// require taking Drydock down first.
//
// The lock exists to stop a second *server*, which would run a second
// supervisor against the same containers. A one-shot write is not that, and
// SQLite's WAL mode serialises it against the running server's writes. Never
// use this to serve.
func OpenAdmin(ctx context.Context, path string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?"+pragmas().Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &DB{DB: db}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database and then the lock, in that order, so the lock
// is never released while this process might still write.
func (s *DB) Close() error {
	err := s.DB.Close()
	if s.lock != nil {
		s.lock.Close() // closing the descriptor drops the flock
		s.lock = nil
	}
	return err
}

// acquireLock takes an exclusive, non-blocking flock on a sidecar file.
//
// A sidecar rather than the database file itself: SQLite manages its own
// locks on the database with fcntl, and an flock on the same file is a second,
// independent lock family whose interaction is easy to get wrong. The sidecar
// has one job and nothing else touches it.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock %s is held)", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// migrate applies every migration newer than the file's user_version, each in
// its own transaction.
func (s *DB) migrate(ctx context.Context) error {
	var have int
	if err := s.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if have > len(migrations) {
		return fmt.Errorf("database schema is version %d but this binary knows only %d: refusing to run an older Drydock against a newer database", have, len(migrations))
	}
	for v := have; v < len(migrations); v++ {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		// PRAGMA does not take a bound parameter.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: set version: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: commit: %w", v+1, err)
		}
	}
	return nil
}

// SchemaVersion reports the applied migration count.
func (s *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// pragmas go in the DSN, not an Exec after opening: foreign_keys is
// per-connection, and set once it would hold on one pooled connection and
// silently not on the others.
func pragmas() url.Values {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// Every transaction takes the write lock at BEGIN. A deferred one that
	// reads, then writes, cannot wait for a writer that committed in between:
	// SQLite fails it with SQLITE_BUSY at once, busy_timeout or not. Drydock's
	// transactions are check-then-write — the duplicate-workspace check, the
	// cap — so each one must hold the lock across its check.
	q.Set("_txlock", "immediate")
	return q
}

// ClaimLabelPrefix records prefix as this database's label prefix on first
// run, and on every later run refuses a different one. Adoption and deletion
// are label-driven (§6), so the prefix decides whose containers this instance
// may touch; a changed prefix would orphan every container the database
// knows and could adopt another instance's. Changing it is a migration, not a
// flag.
func (s *DB) ClaimLabelPrefix(ctx context.Context, prefix string, now time.Time) error {
	if _, err := s.ExecContext(ctx,
		`INSERT INTO instance (id, label_prefix, created_at) VALUES (1, ?, ?) ON CONFLICT(id) DO NOTHING`,
		prefix, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var recorded string
	if err := s.QueryRowContext(ctx, `SELECT label_prefix FROM instance WHERE id = 1`).Scan(&recorded); err != nil {
		return err
	}
	if recorded != prefix {
		return fmt.Errorf("this database belongs to label prefix %q, not %q: its workspaces' containers carry %q labels, and starting under another prefix would orphan them and could adopt another instance's", recorded, prefix, recorded)
	}
	return nil
}
