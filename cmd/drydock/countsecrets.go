package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/store"
)

// countSecrets prints how many secrets the database holds, as a bare number,
// and nothing else on stdout. It is the installer's question before it puts a
// different secrets master key in place (design §10.2): every stored secret is
// sealed under the installed key, so a new one is safe only when there are
// none.
//
// It is built to be asked of a database another binary owns. No instance lock,
// like passwd, because the server is usually running. Unlike passwd, no
// migration and no write of any kind: the installer asks before it restarts
// anything, so the running server — or the one a failed upgrade rolls back to —
// may be older than this binary, and a migrated schema would refuse it. A
// database that does not exist yet holds no secrets, and is not created; one
// from before secrets existed has no secret table, and holds none either. Any
// other failure is an error, never a zero: the answer decides whether a key is
// replaced, so it fails closed.
func countSecrets(args []string, stdout, stderr io.Writer) int {
	dbPath := config.Default().DatabasePath
	fs := flag.NewFlagSet("count-secrets", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&dbPath, "db", dbPath, "SQLite database path (opened read-only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "drydock count-secrets: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	n, err := storedSecrets(context.Background(), dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "drydock count-secrets: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, n)
	return 0
}

func storedSecrets(ctx context.Context, path string) (int, error) {
	db, err := store.OpenReadOnly(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'secret'`).Scan(&tables); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	if tables == 0 {
		return 0, nil
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM secret`).Scan(&n); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return n, nil
}
