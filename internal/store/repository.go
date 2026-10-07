package store

import (
	"context"
	"database/sql"
)

// released is every repository row the installation dropped (removed_at set)
// that no workspace holds any more.
const released = `SELECT id FROM repository WHERE removed_at IS NOT NULL
	AND NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.repository_id = repository.id)`

// DropReleasedRepositories deletes, in the caller's transaction, every
// repository row the installation dropped that no workspace holds any more,
// and the secret grants on each (§4: "a repository the catalog drops takes
// its grants with it; one that comes back is granted nothing").
//
// A dropped repository's row — and its grants — are kept only while a
// workspace holds it, because §12 keeps that workspace working on what it
// already has. The moment nothing holds it, it goes: the catalog calls this
// on every refresh, and a workspace's removal in the same transaction as its
// own row, so neither a delete nor a later refresh can leave a row behind
// whose grants a re-added repository would inherit.
//
// History is not touched: token_grant and secret_access name the repository
// and the workspace by value, with no foreign key, so "which workspaces ever
// held this secret?" outlives both.
//
// It returns how many grants it deleted, so a caller can tell the secrets
// store its decrypted snapshot is stale.
func DropReleasedRepositories(ctx context.Context, tx *sql.Tx) (grants int64, err error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM secret_grant WHERE repository_id IN (`+released+`)`)
	if err != nil {
		return 0, err
	}
	grants, err = res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repository WHERE id IN (`+released+`)`); err != nil {
		return 0, err
	}
	return grants, nil
}
