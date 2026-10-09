// Package catalog is the repository list (design §5's GET /api/repos): a
// cached copy of every repository the GitHub App is installed on, refreshed
// on demand and every 15 minutes (§3.1).
//
// It is a cache and never an authority. A repository the installation stops
// covering disappears from the list — unless a workspace still holds it, in
// which case the row stays, marked removed, because the working tree may
// hold unpushed work (§12). Adding a repository is a GitHub-side action;
// Drydock picks it up on the next refresh.
//
// # Rules and details
//
// Refreshed at boot, every 15 minutes and on demand, one at a time, by one
// worker (life.Coalescer) that Start runs under a life.Group Serve owns:
// Serve's shutdown stops the group, which ends a refresh running and starts no
// other, and waits for it before the database closes. A request (Trigger,
// behind POST /api/repos/refresh, or Refresh) is answered only by a refresh
// that begins after it — never by one already running, which may have listed
// before the click or already emitted the event the button settles on — so
// one made during a refresh gets one more after it, with its own event, and
// any number of them share that one. The period restarts after every
// refresh.
//
// A listing token asks for metadata only and a probe token for contents read.
// A repo is probed for devcontainer.json only when it was pushed to, and a
// failed probe is null, never false.
//
// A repo dropped from the installation is deleted unless a workspace holds it,
// and its secret grants with it. One a workspace holds keeps its row and
// grants while that workspace lives (§12: the broker keeps serving it what it
// had) and loses both when it is released: store.DropReleasedRepositories runs
// in workspace.Remove's transaction and on every refresh (which sweeps rows an
// older release left), so a re-added repository is granted nothing. **Deleting
// grants outside internal/secrets must call secrets.Store.Invalidate** (the
// GrantsDropped hooks on Catalog and workspace.Store), or the broker serves
// them from its snapshot to the re-added repository. token_grant and
// secret_access are never touched.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/store"
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
	// GrantsDropped is called after a refresh commits that deleted secret
	// grants, so the secrets store rebuilds the snapshot the broker serves
	// from (secrets.Store.Invalidate). Without it a re-added repository
	// would be served the grants this refresh deleted, from memory.
	GrantsDropped func()
	// Logf is the service log; nil is standard error.
	Logf func(string, ...any)

	// w runs every refresh, one at a time, on the goroutine Start gives it:
	// the periodic ones and every one asked for (life.Coalescer).
	w life.Coalescer[Result]

	mu sync.Mutex
	// ending, if set, runs as a refresh ends: after its event, before the
	// worker records which requests it answered. A test seam for the window
	// between the two.
	ending func()
	// failure is the last refresh's failure, nil once one succeeds. In
	// memory only: it is what a reloaded page reads instead of the event it
	// missed, and a restarted server refreshes at once anyway.
	failure *RefreshError
}

// Result summarises one refresh.
type Result struct {
	Count, Added, Removed int
}

// Start runs the catalog's refreshes under g until g stops: one now, then
// every Interval, and one for each Trigger or Refresh. g's Stop ends a
// refresh running and starts no other; g's Wait waits for it, which is what
// keeps a refresh from outliving the database it writes to.
func (c *Catalog) Start(g *life.Group) error { return c.start(g, true) }

// start is Start; boot false leaves out the refresh at once, for a test that
// counts what each of its own refreshes does.
func (c *Catalog) start(g *life.Group, boot bool) error {
	c.w.Work = c.run
	c.w.Clock = c.Clock
	c.w.Interval = c.Interval
	if c.w.Interval <= 0 {
		c.w.Interval = DefaultInterval
	}
	if err := c.w.Start(g, "refresh"); err != nil {
		return err
	}
	if boot {
		c.w.Trigger()
	}
	return nil
}

// Refresh asks for a refresh that begins after this call and waits for it:
// never one already running, which may have listed before the call. Callers
// at the same moment can share one. It is life.ErrNotStarted before Start
// and life.ErrStopping once the catalog's group is stopping; a caller whose
// ctx ends stops waiting but does not cancel the refresh, which is shared.
func (c *Catalog) Refresh(ctx context.Context) (Result, error) {
	return c.w.TriggerAndWait(ctx)
}

// Trigger asks for a refresh in the background and returns at once: POST
// /api/repos/refresh answers 202 and the client follows the event stream,
// where the refresh it asked for — one that begins after the call, with an
// event of its own — settles it. A Trigger during a refresh gets one more
// after it, and any number of them share that one. Once the catalog's group
// is stopping it starts nothing.
func (c *Catalog) Trigger() { c.w.Trigger() }

// run is one refresh, on the worker: it writes the cache, records the
// outcome for List and says so on the stream.
func (c *Catalog) run(ctx context.Context) (Result, error) {
	defer func() {
		c.mu.Lock()
		ending := c.ending
		c.mu.Unlock()
		if ending != nil {
			ending()
		}
	}()
	res, err := c.refresh(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logf("drydock: catalog refresh: %v", err)
		}
		// GitHub's message names the problem ("Bad credentials", "Not
		// Found") and carries no credential; the token is a header and
		// github.Token prints as [redacted] besides.
		msg := "Could not refresh the repository list from GitHub: " + publicReason(err)
		c.mu.Lock()
		c.failure = &RefreshError{At: c.Clock.Now().UTC(), Message: msg}
		c.mu.Unlock()
		c.Events.Emit(ctx, "", events.Warn, KindRefreshFailed, msg, map[string]any{})
		return Result{}, err
	}
	c.mu.Lock()
	c.failure = nil
	c.mu.Unlock()
	_, err = c.Events.Emit(ctx, "", events.Info, KindRefreshed,
		fmt.Sprintf("Repository list refreshed: %d repositories.", res.Count),
		map[string]any{"count": res.Count, "added": res.Added, "removed": res.Removed})
	return res, err
}

func (c *Catalog) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
		return
	}
	fmt.Fprintf(os.Stderr, f+"\n", a...)
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
		// Marked removed. Kept, with its grants, while a workspace holds it
		// (§12); otherwise deleted with them, below.
		if _, err := tx.ExecContext(ctx,
			`UPDATE repository SET removed_at = coalesce(removed_at, ?) WHERE id = ?`, now, id); err != nil {
			return Result{}, err
		}
		res.Removed++
	}
	// Every removed row nothing holds goes now, with its secret grants —
	// the ones just marked, and any marked earlier whose workspace has
	// since gone (a delete takes its row too, but a row left by an older
	// release, or by a delete cut off before its transaction, is swept
	// here). A repository that comes back is then granted nothing: default
	// deny (§10.1) is the right state for one the operator last saw leave.
	_, dropped, err := store.DropReleasedRepositories(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	if dropped > 0 && c.GrantsDropped != nil {
		c.GrantsDropped()
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
