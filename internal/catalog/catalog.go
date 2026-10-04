// Package catalog is the repository list (design §5's GET /api/repos): a
// cached copy of every repository the GitHub App is installed on, refreshed
// on demand and every 15 minutes (§3.1).
//
// It is a cache and never an authority. A repository the installation stops
// covering disappears from the list — unless a workspace still holds it, in
// which case the row stays, marked removed, because the working tree may
// hold unpushed work (§12). Adding a repository is a GitHub-side action;
// Drydock picks it up on the next refresh.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/sys"
)

// DefaultInterval is the periodic refresh (§3.1).
const DefaultInterval = 15 * time.Minute

// Event kinds. The frontend's catalog store refetches on repo.* (frontend §4.1).
const (
	KindRefreshed     = "repo.refreshed"      // data: {count, added, removed}
	KindRefreshFailed = "repo.refresh_failed" // data: {}
)

// Catalog refreshes and reads the repository cache.
type Catalog struct {
	DB       *sql.DB
	GitHub   *github.Client
	Events   *events.Log
	Clock    sys.Clock
	Interval time.Duration

	// one refresh at a time: a manual refresh during the periodic one joins
	// it rather than racing it.
	mu      sync.Mutex
	running bool
	done    chan struct{}
}

// Result summarises one refresh.
type Result struct {
	Count, Added, Removed int
}

// Refresh re-reads every installation's repositories and rewrites the cache.
// Concurrent callers share one refresh.
func (c *Catalog) Refresh(ctx context.Context) (Result, error) {
	c.mu.Lock()
	if c.running {
		done := c.done
		c.mu.Unlock()
		select {
		case <-done:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	c.running, c.done = true, make(chan struct{})
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		close(c.done)
		c.mu.Unlock()
	}()

	res, err := c.refresh(ctx)
	if err != nil {
		// GitHub's message names the problem ("Bad credentials", "Not
		// Found") and carries no credential; the token is a header and
		// github.Token prints as [redacted] besides.
		c.Events.Emit(ctx, "", events.Warn, KindRefreshFailed,
			"Could not refresh the repository list from GitHub: "+publicReason(err), map[string]any{})
		return Result{}, err
	}
	_, err = c.Events.Emit(ctx, "", events.Info, KindRefreshed,
		fmt.Sprintf("Repository list refreshed: %d repositories.", res.Count),
		map[string]any{"count": res.Count, "added": res.Added, "removed": res.Removed})
	return res, err
}

// Trigger starts a refresh in the background and returns at once: POST
// /api/repos/refresh answers 202 and the client follows the event stream.
func (c *Catalog) Trigger() {
	go c.Refresh(context.Background())
}

// Run refreshes now and then every Interval until ctx ends. Failures are
// reported through the event log and tried again at the next interval.
func (c *Catalog) Run(ctx context.Context, logf func(string, ...any)) {
	every := c.Interval
	if every <= 0 {
		every = DefaultInterval
	}
	for {
		if _, err := c.Refresh(ctx); err != nil && ctx.Err() == nil && logf != nil {
			logf("drydock: catalog refresh: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.Clock.After(every):
		}
	}
}

type known struct {
	pushedAt, branch string
	hasDevcontainer  sql.NullBool
}

func (c *Catalog) refresh(ctx context.Context) (Result, error) {
	installs, err := c.GitHub.Installations(ctx)
	if err != nil {
		return Result{}, err
	}
	cached, err := c.cached(ctx)
	if err != nil {
		return Result{}, err
	}

	type row struct {
		repo            github.Repository
		installation    int64
		hasDevcontainer sql.NullBool
	}
	var rows []row
	for _, in := range installs {
		// Listing needs nothing beyond metadata; the probe needs contents.
		// Two tokens, each asking only for what its call needs (§9.3).
		listTok, err := c.GitHub.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID,
			Permissions: map[string]string{"metadata": github.Read}})
		if err != nil {
			return Result{}, err
		}
		repos, err := c.GitHub.Repositories(ctx, listTok)
		if err != nil {
			return Result{}, err
		}
		var probeTok github.Token
		for _, r := range repos {
			pushed := ts(r.PushedAt)
			k, seen := cached[r.ID]
			has := k.hasDevcontainer
			// Probe only what may have changed: a new repository, a push, a
			// changed default branch, or a probe that failed last time.
			if !seen || k.pushedAt != pushed || k.branch != r.DefaultBranch || !has.Valid {
				if probeTok.Value() == "" {
					if probeTok, err = c.GitHub.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID,
						Permissions: map[string]string{"contents": github.Read}}); err != nil {
						return Result{}, err
					}
				}
				has = c.probe(ctx, probeTok, r)
			}
			rows = append(rows, row{repo: r, installation: in.ID, hasDevcontainer: has})
		}
	}

	now := ts(c.Clock.Now())
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM installation`); err != nil {
		return Result{}, err
	}
	for _, in := range installs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO installation (id, account, account_type, refreshed_at) VALUES (?, ?, ?, ?)`,
			in.ID, in.Account, in.AccountType, now); err != nil {
			return Result{}, err
		}
	}
	var res Result
	listed := map[int64]bool{}
	for _, r := range rows {
		listed[r.repo.ID] = true
		if _, ok := cached[r.repo.ID]; !ok {
			res.Added++
		}
		var has any
		if r.hasDevcontainer.Valid {
			has = r.hasDevcontainer.Bool
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repository (id, installation_id, full_name, default_branch, has_devcontainer,
			                        private, archived, pushed_at, refreshed_at, removed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(id) DO UPDATE SET
			  installation_id = excluded.installation_id, full_name = excluded.full_name,
			  default_branch = excluded.default_branch, has_devcontainer = excluded.has_devcontainer,
			  private = excluded.private, archived = excluded.archived, pushed_at = excluded.pushed_at,
			  refreshed_at = excluded.refreshed_at, removed_at = NULL`,
			r.repo.ID, r.installation, r.repo.FullName, r.repo.DefaultBranch, has,
			r.repo.Private, r.repo.Archived, ts(r.repo.PushedAt), now); err != nil {
			return Result{}, err
		}
	}
	for id := range cached {
		if listed[id] {
			continue
		}
		// Kept, and marked, while a workspace holds it; otherwise gone.
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace WHERE repository_id = ?`, id).Scan(&held); err != nil {
			return Result{}, err
		}
		if held > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE repository SET removed_at = coalesce(removed_at, ?) WHERE id = ?`, now, id); err != nil {
				return Result{}, err
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM repository WHERE id = ?`, id); err != nil {
			return Result{}, err
		}
		res.Removed++
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	res.Count = len(rows)
	return res, nil
}

// cached reads the rows a refresh compares against: every repository still
// in an installation as of the last refresh. Already-removed rows are left
// out, so a repository removed twice is counted once.
func (c *Catalog) cached(ctx context.Context) (map[int64]known, error) {
	rows, err := c.DB.QueryContext(ctx,
		`SELECT id, coalesce(pushed_at,''), default_branch, has_devcontainer FROM repository WHERE removed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]known{}
	for rows.Next() {
		var id int64
		var k known
		if err := rows.Scan(&id, &k.pushedAt, &k.branch, &k.hasDevcontainer); err != nil {
			return nil, err
		}
		out[id] = k
	}
	return out, rows.Err()
}

// probe reports whether a repository declares a dev container in any of the
// places the spec allows: .devcontainer/devcontainer.json, .devcontainer.json,
// or .devcontainer/<folder>/devcontainer.json. A repository without one is
// listed and clonable all the same — the result is a badge, not a filter
// (§6 step 3). An error leaves the answer unknown, to be asked again.
func (c *Catalog) probe(ctx context.Context, tok github.Token, r github.Repository) sql.NullBool {
	found := func(ok bool) sql.NullBool { return sql.NullBool{Bool: ok, Valid: true} }
	dir, err := c.GitHub.Contents(ctx, tok, r.FullName, ".devcontainer", r.DefaultBranch)
	switch {
	case err == nil:
		var subdirs []string
		for _, e := range dir {
			if e.Name == "devcontainer.json" && e.Type == "file" {
				return found(true)
			}
			if e.Type == "dir" {
				subdirs = append(subdirs, e.Name)
			}
		}
		for i, d := range subdirs {
			if i == 10 { // a bound, not a spec rule: ten configurations is plenty to badge
				break
			}
			_, err := c.GitHub.Contents(ctx, tok, r.FullName, ".devcontainer/"+d+"/devcontainer.json", r.DefaultBranch)
			if err == nil {
				return found(true)
			}
			if !github.IsNotFound(err) {
				return sql.NullBool{}
			}
		}
	case !github.IsNotFound(err):
		return sql.NullBool{}
	}
	_, err = c.GitHub.Contents(ctx, tok, r.FullName, ".devcontainer.json", r.DefaultBranch)
	switch {
	case err == nil:
		return found(true)
	case github.IsNotFound(err):
		return found(false)
	}
	return sql.NullBool{}
}

func publicReason(err error) string {
	var ae *github.APIError
	if errors.As(err, &ae) {
		return fmt.Sprintf("GitHub answered %d (%s).", ae.Status, ae.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the request timed out."
	}
	return "GitHub could not be reached."
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
