package store

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/schema.golden from the current migrations")

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drydock.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func pragma(t *testing.T, db *DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestOpenAppliesWALForeignKeysAndSchema(t *testing.T) {
	db, _ := openTemp(t)
	if got := pragma(t, db, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %q; want wal", got)
	}
	// foreign_keys is per-connection. Ask on several pooled connections at
	// once so a pragma applied to only one of them shows up as a failure.
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	conns := make([]interface{ Close() error }, 0, 4)
	for i := 0; i < 4; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Errorf("connection %d: foreign_keys = %d; want 1", i, fk)
		}
	}
	for _, c := range conns {
		c.Close()
	}
	if v, err := db.SchemaVersion(ctx); err != nil || v != len(migrations) {
		t.Errorf("schema version = %d, %v; want %d", v, err, len(migrations))
	}
}

// TestSecondInstanceIsRefused is §12's "two Drydocks on one host": refuse to
// start rather than run two supervisors against one container.
func TestSecondInstanceIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drydock.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err) // positive control: the lock is takeable at all
	}
	second, err := Open(ctx, path)
	if err == nil {
		second.Close()
		t.Fatal("second Open succeeded while the first held the lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("second Open error = %v; want ErrLocked", err)
	}

	// And it is released by Close, so a restart is not wedged by its own
	// previous run.
	first.Close()
	third, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	third.Close()
}

func TestReopenKeepsDataAndDoesNotRemigrate(t *testing.T) {
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

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err) // a re-run CREATE TABLE would fail here
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM operator`).Scan(&n); err != nil || n != 1 {
		t.Errorf("operator rows after reopen = %d, %v; want 1", n, err)
	}
}

// TestNewerSchemaIsRefused: an older binary must not run against a database a
// newer one has migrated, because it would write rows the newer code no longer
// expects.
func TestNewerSchemaIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drydock.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := Open(ctx, path); err == nil {
		db.Close()
		t.Fatal("Open accepted a schema newer than this binary knows")
	} else if !strings.Contains(err.Error(), "999") {
		t.Errorf("error does not name the version it refused: %v", err)
	}
}

type column struct{ table, name string }

func allColumns(t *testing.T, db *DB) []column {
	t.Helper()
	rows, err := db.Query(`
		SELECT m.name, p.name
		FROM sqlite_schema m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		ORDER BY m.name, p.cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// TestSchemaHasNothingToSteal is §4's four deliberate absences, asserted by
// walking every column of every table rather than by checking the four places
// they were meant to be absent from — so a column that reappears somewhere
// unexpected is caught too.
func TestSchemaHasNothingToSteal(t *testing.T) {
	db, _ := openTemp(t)
	cols := allColumns(t, db)
	if len(cols) < 40 {
		t.Fatalf("only %d columns found: the assertions below would pass vacuously", len(cols))
	}
	banned := map[string]string{
		"value":         "no plaintext secret value (§4); the secret is ciphertext+nonce",
		"token":         "no stored session or GitHub token (§4)",
		"github_token":  "tokens live in a bounded in-memory cache, never the database (§4)",
		"password":      "the operator password is stored only as an argon2id hash",
		"cookie":        "auth_session.id and preview_session.id are SHA-256s of their cookies, never the cookie",
		"preview_token": "the one-time preview token lives in memory only (PF §7)",
		"upstream_host": "the preview upstream is derived from the workspace, never stored (PF §5)",
		"secret_value":  "no plaintext secret value (§4)",
	}
	for _, c := range cols {
		if why, bad := banned[strings.ToLower(c.name)]; bad {
			t.Errorf("%s.%s exists: %s", c.table, c.name, why)
		}
	}

	// Positive control: the columns that *replace* those are present, so the
	// check above is looking at the real schema and not an empty one.
	want := []column{
		{"secret", "ciphertext"}, {"secret", "nonce"},
		{"operator", "password_hash"}, {"auth_session", "id"},
		{"token_grant", "permissions"},
		{"preview_session", "id"}, {"forwarded_port", "slug"},
	}
	have := map[column]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("expected column %s.%s is missing", w.table, w.name)
		}
	}
}

// TestEnumerationsAreEnforced is the CHECK-constraint tightening in migration
// 1, and in particular the one with teeth: supervisor.state has no 'failed',
// so a 409 wait cannot be stored as one.
func TestEnumerationsAreEnforced(t *testing.T) {
	db, _ := openTemp(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 1, 'o/r', 'main')`)
	mustExec(`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('w', 1, '/x', 'main', 'running')`)

	insertSupervisor := func(state string) error {
		_, err := db.Exec(`INSERT INTO supervisor (id, workspace_id, state, capacity) VALUES (?, 'w', ?, 4)`,
			"s-"+state, state)
		return err
	}
	if err := insertSupervisor("waiting_registration"); err != nil {
		t.Errorf("waiting_registration refused, but it is the state a 409 must be stored as: %v", err)
	}
	if err := insertSupervisor("failed"); err == nil {
		t.Error("supervisor.state accepted 'failed': a 409 wait could now be recorded as a failure")
	}

	if _, err := db.Exec(`INSERT INTO claude_identity (id, volume_name, state) VALUES (1, 'v', 'blanked')`); err != nil {
		t.Errorf("claude_identity refused 'blanked': %v", err)
	}
	if _, err := db.Exec(`UPDATE claude_identity SET state = 'signed_out' WHERE id = 1`); err == nil {
		t.Error("claude_identity.state accepted a value outside its five")
	}

	if _, err := db.Exec(`INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES ('a','A',x'00',x'00','   ')`); err == nil {
		t.Error("secret accepted a blank reach: §10.4 makes that field the control")
	}
	if _, err := db.Exec(`INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES ('b','B',x'00',x'00','reads staging data')`); err != nil {
		t.Errorf("secret refused a real reach: %v", err)
	}
}

// TestSchemaGolden pins the whole schema. Any change shows up as a diff in
// review rather than as a surprise in someone's database; regenerate with
// `go test ./internal/store -run Golden -update`.
func TestSchemaGolden(t *testing.T) {
	db, _ := openTemp(t)
	rows, err := db.Query(`SELECT type, name, sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, sql string
		if err := rows.Scan(&typ, &name, &sql); err != nil {
			t.Fatal(err)
		}
		b.WriteString("-- " + typ + " " + name + "\n" + sql + ";\n\n")
	}
	got := b.String()
	if got == "" {
		t.Fatal("empty schema")
	}
	golden := filepath.Join("testdata", "schema.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("schema differs from %s; if the change is intended, rerun with -update and review the diff", golden)
	}
}

// TestTransactionsTakeTheWriteLockAtBegin: a deferred transaction that reads
// and then writes cannot wait for a writer that committed in between — SQLite
// fails it at once with SQLITE_BUSY — so every check-then-write in Drydock
// (the duplicate-workspace check, the cap) relies on BEGIN holding the lock.
// A second transaction must wait at BEGIN, not proceed to its reads.
func TestTransactionsTakeTheWriteLockAtBegin(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	first, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	begun := make(chan error, 1)
	go func() {
		second, err := db.BeginTx(ctx, nil)
		if err == nil {
			// database/sql may defer BEGIN to the first statement; run one
			// so the lock is actually requested.
			_, err = second.ExecContext(ctx, `SELECT 1`)
			second.Rollback()
		}
		begun <- err
	}()
	select {
	case err := <-begun:
		t.Fatalf("a second transaction began while the first held the database (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	// Control: it begins as soon as the first ends, so the wait above was
	// the lock and not something else stuck.
	first.Rollback()
	select {
	case err := <-begun:
		if err != nil {
			t.Fatalf("the second transaction failed after the first ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control: the second transaction never began")
	}
}

func TestLabelPrefixIsClaimedOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drydock.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if err := db.ClaimLabelPrefix(ctx, "drydock", now); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := db.ClaimLabelPrefix(ctx, "drydock", now); err != nil {
		t.Errorf("the same prefix again: %v", err)
	}
	db.Close()

	// Across a restart, which is when a changed flag would bite.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimLabelPrefix(ctx, "drydock.other", now); err == nil {
		t.Error("a different prefix was accepted for a database that already has one")
	}
}

// TestMigrationSevenRetiresTheOldExpiring: before migration 7, 'expiring'
// meant "the access token ends within three days", which a real login's
// eight-hour token made true of every login (design §7.3). A row stored under
// that meaning must not reach the banner as the new "the login ends within
// three days", so the migration turns it into 'ok' and the boot check
// rewrites it. The control is a row in another state, which it leaves alone.
func TestMigrationSevenRetiresTheOldExpiring(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ stored, want string }{
		{"expiring", "ok"},
		{"expired", "expired"}, // control: only the old meaning is rewritten
		{"blanked", "blanked"},
	} {
		t.Run(c.stored, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "drydock.db")
			raw, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			for v := 0; v < 6; v++ {
				if _, err := raw.Exec(migrations[v]); err != nil {
					t.Fatalf("migration %d: %v", v+1, err)
				}
			}
			if _, err := raw.Exec(`PRAGMA user_version = 6`); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO claude_identity (id, volume_name, state, expires_at) VALUES (1, 'v', ?, '2026-10-08T20:00:00Z')`, c.stored); err != nil {
				t.Fatal(err)
			}
			raw.Close()

			db, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var state, expires string
			var login sql.NullString
			if err := db.QueryRow(`SELECT state, expires_at, login_expires_at FROM claude_identity WHERE id = 1`).Scan(&state, &expires, &login); err != nil {
				t.Fatal(err)
			}
			if state != c.want || expires != "2026-10-08T20:00:00Z" || login.Valid {
				t.Errorf("after migration: state %q, expires_at %q, login_expires_at %v; want %q, kept, NULL", state, expires, login, c.want)
			}
		})
	}
}

// TestPreviewTablesHoldTheirPromises is PF §5's schema, asserted on the
// database rather than on the code that writes it: a slug is a DNS label and
// never the installer's probe name; a retired row keeps its slug spent while a
// new live row for the same port is allowed and a second live one is not; and
// a preview session dies with the auth session that minted it.
func TestPreviewTablesHoldTheirPromises(t *testing.T) {
	db, _ := openTemp(t)
	port := func(id, ws string, n int, slug string) error {
		_, err := db.Exec(`INSERT INTO forwarded_port (id, workspace_id, container_port, slug) VALUES (?, ?, ?, ?)`, id, ws, n, slug)
		return err
	}
	if err := port("p1", "w", 5173, "myapp-5173-p2mq"); err != nil {
		t.Fatalf("control: a well-formed slug was refused: %v", err)
	}
	var hostHeader string
	if err := db.QueryRow(`SELECT host_header FROM forwarded_port WHERE id = 'p1'`).Scan(&hostHeader); err != nil || hostHeader != "localhost" {
		t.Errorf("host_header defaults to %q (%v); want localhost (PF §8.3)", hostHeader, err)
	}
	for i, bad := range []string{"drydock-check", "Upper-1-abcd", "a.b", "-lead", "trail-", "", strings.Repeat("a", 64), "sp ace"} {
		if err := port(fmt.Sprintf("bad%d", i), "w2", 3000+i, bad); err == nil {
			t.Errorf("slug %q was accepted", bad)
		}
	}
	if err := port("p2", "w", 5173, "myapp-5173-zzzz"); err == nil {
		t.Error("a second live row for (w, 5173) was accepted")
	}
	if _, err := db.Exec(`UPDATE forwarded_port SET retired_at = '2026-10-08T00:00:00Z' WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	if err := port("p3", "w", 5173, "myapp-5173-zzzz"); err != nil {
		t.Errorf("retiring did not free (w, 5173) for a new live row: %v", err)
	}
	if err := port("p4", "w9", 8080, "myapp-5173-p2mq"); err == nil {
		t.Error("a retired slug was reissued to another row")
	}

	if _, err := db.Exec(`INSERT INTO auth_session (id, created_at, last_seen_at, absolute_expires_at) VALUES ('a1', 'x', 'x', 'x'), ('a2', 'x', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO preview_session (id, auth_session_id, forwarded_port_id, preview_host, created_at, last_seen_at)
		VALUES ('s1', 'a1', 'p3', 'h', 'x', 'x'), ('s2', 'a2', 'p3', 'h', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO preview_session (id, auth_session_id, forwarded_port_id, preview_host, created_at, last_seen_at)
		VALUES ('s3', 'nobody', 'p3', 'h', 'x', 'x')`); err == nil {
		t.Error("a preview session was accepted for an auth session that does not exist")
	}
	if _, err := db.Exec(`DELETE FROM auth_session WHERE id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM preview_session WHERE id = 's1'`).Scan(&n)
	if n != 0 {
		t.Error("revoking the auth session left its preview session behind")
	}
	db.QueryRow(`SELECT count(*) FROM preview_session WHERE id = 's2'`).Scan(&n)
	if n != 1 {
		t.Error("control: another session's preview session went too")
	}
}
