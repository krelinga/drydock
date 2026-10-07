package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

func runCount(t *testing.T, path string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	rc := countSecrets([]string{"--db", path}, &out, &errOut)
	return out.String(), errOut.String(), rc
}

func putSecret(t *testing.T, db *store.DB, name string) {
	t.Helper()
	raw := make([]byte, secrets.KeySize)
	rand.Read(raw)
	key, err := secrets.NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := &secrets.Store{DB: db.DB, Key: key, Env: sys.Production()}
	if _, err := s.Put(context.Background(), name, "a-value", "reaches a scratch database", ""); err != nil {
		t.Fatal(err)
	}
}

// TestCountSecretsWhileServing: the installer asks with the server running, so
// the count must ignore the instance lock — and its zero is only believable
// beside the same command answering one once a secret is stored.
func TestCountSecretsWhileServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drydock.db")
	running, err := store.Open(context.Background(), path) // stands in for a live `drydock serve`
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	if out, errOut, rc := runCount(t, path); rc != 0 || out != "0\n" {
		t.Fatalf("an empty store: %q, rc %d, %s; want \"0\\n\"", out, rc, errOut)
	}
	putSecret(t, running, "TEST_KEY")
	if out, errOut, rc := runCount(t, path); rc != 0 || out != "1\n" {
		t.Fatalf("control: one stored secret counts as %q, rc %d, %s; want \"1\\n\"", out, rc, errOut)
	}
	putSecret(t, running, "OTHER_KEY")
	if out, _, _ := runCount(t, path); out != "2\n" {
		t.Errorf("two stored secrets count as %q", out)
	}
}

// TestCountSecretsWritesNothing: no migration, no file created. A newer binary
// migrating the schema under an older running server is what would leave a
// rolled-back upgrade unable to start.
func TestCountSecretsWritesNothing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.db")
	if out, errOut, rc := runCount(t, missing); rc != 0 || out != "0\n" {
		t.Errorf("a database that does not exist yet: %q, rc %d, %s; want 0", out, rc, errOut)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Error("count-secrets created the database it was asked about")
	}

	// A database from before the secret table: WAL mode, user_version 0 and
	// one unrelated table, with a row in it. A migration would add the secret
	// table and set user_version.
	pre := filepath.Join(dir, "pre.db")
	raw, err := sql.Open("sqlite", "file:"+pre)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`PRAGMA journal_mode = WAL`, `CREATE TABLE legacy_note (id INTEGER PRIMARY KEY, body TEXT)`,
		`INSERT INTO legacy_note VALUES (1, 'x')`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	preSum := fileSum(t, pre)
	if out, errOut, rc := runCount(t, pre); rc != 0 || out != "0\n" {
		t.Errorf("a database with no secret table: %q, rc %d, %s; want 0", out, rc, errOut)
	}
	if fileSum(t, pre) != preSum {
		t.Error("count-secrets changed a database with no secret table: it migrated")
	}
	check, err := sql.Open("sqlite", "file:"+pre+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var version, tables, rows int
	check.QueryRow(`PRAGMA user_version`).Scan(&version)
	check.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'secret'`).Scan(&tables)
	check.QueryRow(`SELECT count(*) FROM legacy_note`).Scan(&rows)
	check.Close()
	if rows != 1 {
		t.Fatalf("control: the older database reads %d rows of its table; want 1", rows)
	}
	if version != 0 || tables != 0 {
		t.Errorf("after the question: user_version %d, secret tables %d; want 0 and 0", version, tables)
	}
	sidecarsHoldNothing(t, pre)

	// And a current database's bytes are unchanged by the question, with a
	// secret in it (the control: the question did read it).
	cur := filepath.Join(dir, "current.db")
	db, err := store.Open(context.Background(), cur)
	if err != nil {
		t.Fatal(err)
	}
	putSecret(t, db, "TEST_KEY")
	db.Close()
	curSum := fileSum(t, cur)
	if out, _, rc := runCount(t, cur); rc != 0 || out != "1\n" {
		t.Errorf("control: the current database answers %q, rc %d; want 1", out, rc)
	}
	if fileSum(t, cur) != curSum {
		t.Error("count-secrets changed the database file")
	}
	sidecarsHoldNothing(t, cur)
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// sidecarsHoldNothing: reading a cleanly closed WAL database makes SQLite
// create its -shm index and an empty -wal (see countSecrets). Nothing may be
// written into the -wal: that would be a write to the database.
func sidecarsHoldNothing(t *testing.T, path string) {
	t.Helper()
	if st, err := os.Stat(path + "-wal"); err == nil && st.Size() != 0 {
		t.Errorf("%s-wal holds %d bytes after the question; want none", filepath.Base(path), st.Size())
	}
}

// TestCountSecretsFailsClosed: the answer decides whether a key is replaced,
// so a database it cannot read is an error, never a zero.
func TestCountSecretsFailsClosed(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, rc := runCount(t, junk)
	if rc == 0 || out != "" {
		t.Errorf("a file that is not a database: %q, rc %d; want an error and nothing on stdout", out, rc)
	}
	if !strings.Contains(errOut, "count-secrets") {
		t.Errorf("stderr = %q; want it to say what failed", errOut)
	}
	if out, _, rc := runCount(t, dir); rc == 0 || out != "" {
		t.Errorf("a directory: %q, rc %d; want an error", out, rc)
	}

	// An empty file is no database Drydock left (it migrates on open), so
	// it is not "no secrets" — beside the control that the same path, once
	// a database, answers.
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, rc = runCount(t, empty)
	if rc == 0 || out != "" {
		t.Errorf("an empty file: %q, rc %d; want an error and nothing on stdout", out, rc)
	}
	if !strings.Contains(errOut, "empty file") {
		t.Errorf("stderr = %q; want it to say the file is empty", errOut)
	}
	if st, _ := os.Stat(empty); st.Size() != 0 {
		t.Error("count-secrets wrote to the empty file")
	}
	os.Remove(empty)
	db, err := store.Open(context.Background(), empty)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if out, errOut, rc := runCount(t, empty); rc != 0 || out != "0\n" {
		t.Errorf("control: a database at that path answers %q, rc %d, %s; want 0", out, rc, errOut)
	}
	var o, e bytes.Buffer
	if rc := countSecrets([]string{"--db", junk, "extra"}, &o, &e); rc != 2 {
		t.Errorf("a stray argument: rc %d; want 2", rc)
	}
}
