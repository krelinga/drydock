package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// TestPasswdWhileServing is the reason passwd skips the instance lock: a
// running server holds it, and changing a password that may be compromised
// must not require downtime.
func TestPasswdWhileServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drydock.db")
	ctx := context.Background()

	running, err := store.Open(ctx, path) // stands in for a live `drydock serve`
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	svc := auth.New(running.DB, sys.Production())

	var out, errOut bytes.Buffer
	if rc := passwd([]string{"--db", path}, strings.NewReader("first long password\n"), &out, &errOut); rc != 0 {
		t.Fatalf("passwd while the store is locked = %d: %s", rc, errOut.String())
	}
	res, err := svc.SignIn(ctx, "192.0.2.1", "first long password", "")
	if err != nil {
		t.Fatalf("the running server cannot use the password passwd just set: %v", err)
	}

	// And it ends the running server's sessions, immediately.
	if rc := passwd([]string{"--db", path}, strings.NewReader("second long password\n"), &out, &errOut); rc != 0 {
		t.Fatalf("second passwd = %d: %s", rc, errOut.String())
	}
	if _, err := svc.Sessions.Lookup(ctx, res.Cookie); err == nil {
		t.Error("a session from before the password change still works")
	}
}

func TestPasswdKeepsSpacesAndRefusesShort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drydock.db")
	var out, errOut bytes.Buffer
	if rc := passwd([]string{"--db", path}, strings.NewReader("short\n"), &out, &errOut); rc == 0 {
		t.Error("a short password was accepted")
	}
	// A trailing space is part of the password; only the line ending is not.
	if rc := passwd([]string{"--db", path}, strings.NewReader("  spaced  password  \r\n"), &out, &errOut); rc != 0 {
		t.Fatalf("passwd = %d: %s", rc, errOut.String())
	}
	db, _ := store.OpenAdmin(context.Background(), path)
	defer db.Close()
	svc := auth.New(db.DB, sys.Production())
	if _, err := svc.SignIn(context.Background(), "192.0.2.1", "  spaced  password  ", ""); err != nil {
		t.Errorf("the exact password with its spaces does not work: %v", err)
	}
	if _, err := svc.SignIn(context.Background(), "192.0.2.2", "spaced  password", ""); err == nil {
		t.Error("a trimmed version of the password also works: whitespace was stripped")
	}
}

func TestPasswdNeverPrintsThePassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drydock.db")
	const canary = "canary-Zq81vPx3-password"
	var out, errOut bytes.Buffer
	if rc := passwd([]string{"--db", path}, strings.NewReader(canary+"\n"), &out, &errOut); rc != 0 {
		t.Fatalf("passwd = %d", rc)
	}
	if out.Len() == 0 {
		t.Fatal("control: passwd printed nothing, so the check below is vacuous")
	}
	if strings.Contains(out.String()+errOut.String(), canary) {
		t.Error("passwd echoed the password")
	}
}
