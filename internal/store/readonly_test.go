package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenReadOnly is what lets the installer ask the database a question
// before it restarts anything: beside a running server (no lock), against a
// schema the reading binary may not know (no migration), and never writing.
func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	running, path := openTemp(t) // holds the instance lock, as a live server does
	if _, err := running.Exec(`INSERT INTO operator (id, password_hash) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := running.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly beside a running server, on a newer schema: %v", err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRow(`SELECT count(*) FROM operator`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("control: the read-only handle reads %d rows, %v; want 1", n, err)
	}
	if v, _ := ro.SchemaVersion(ctx); v != 999 {
		t.Errorf("user_version = %d through the read-only handle; it migrated, or read another file", v)
	}
	if _, err := ro.Exec(`DELETE FROM operator`); err == nil {
		t.Error("a write through the read-only handle succeeded")
	}
	if err := running.QueryRow(`SELECT count(*) FROM operator`).Scan(&n); err != nil || n != 1 {
		t.Errorf("operator rows after the refused write = %d, %v; want 1", n, err)
	}

	missing := filepath.Join(t.TempDir(), "none.db")
	if db, err := OpenReadOnly(ctx, missing); err == nil {
		db.Close()
		t.Error("OpenReadOnly opened a database that does not exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing database is %v; want os.ErrNotExist, which callers branch on", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Error("OpenReadOnly created the database it was asked to read")
	}
}

// TestOpenReadOnlyAfterACleanStop: a stopped server's clean close removes the
// -wal and -shm files, and the installer asks its question as the drydock
// user with the server stopped too.
func TestOpenReadOnlyAfterACleanStop(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drydock.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operator (id, password_hash) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRow(`SELECT count(*) FROM operator`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v; want 1", n, err)
	}
}
