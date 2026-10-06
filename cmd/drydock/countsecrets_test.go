package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	// A database from before the secret table: user_version 0 and one
	// unrelated table. A migration would add the secret table.
	old := filepath.Join(dir, "old.db")
	db, err := store.OpenAdmin(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	pre := filepath.Join(dir, "pre.db")
	if err := os.WriteFile(pre, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, rc := runCount(t, pre); rc != 0 || out != "0\n" {
		t.Errorf("a database with no secret table: %q, rc %d, %s; want 0", out, rc, errOut)
	}
	if st, _ := os.Stat(pre); st.Size() != 0 {
		t.Error("count-secrets wrote to a database with no schema: it migrated")
	}

	// And a full database's bytes are unchanged by the question.
	before := sha256.Sum256(raw)
	if out, _, rc := runCount(t, old); rc != 0 || out != "0\n" {
		t.Errorf("control: the migrated database answers %q, rc %d", out, rc)
	}
	raw, _ = os.ReadFile(old)
	if sha256.Sum256(raw) != before {
		t.Error("count-secrets changed the database file")
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
	var o, e bytes.Buffer
	if rc := countSecrets([]string{"--db", junk, "extra"}, &o, &e); rc != 2 {
		t.Errorf("a stray argument: rc %d; want 2", rc)
	}
}
