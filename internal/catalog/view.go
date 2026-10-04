package catalog

import (
	"context"
	"database/sql"
	"time"

	"github.com/krelinga/drydock/internal/github"
)

// View is GET /api/repos.
type View struct {
	// RefreshedAt is when the list last came from GitHub; nil before the
	// first refresh, which the UI shows as "loading" rather than "empty".
	RefreshedAt   *time.Time         `json:"refreshed_at"`
	Installations []InstallationView `json:"installations"`
	Repos         []RepoView         `json:"repos"`
}

// InstallationView carries the settings link the UI gives when a repository
// the operator expected is missing (§9.4, frontend §4.5 #8).
type InstallationView struct {
	ID          int64  `json:"id"`
	Account     string `json:"account"`
	SettingsURL string `json:"settings_url"`
}

// RepoView is one repository, joined with its workspace if it has one.
type RepoView struct {
	ID             int64  `json:"id"`
	InstallationID int64  `json:"installation_id"`
	FullName       string `json:"full_name"`
	DefaultBranch  string `json:"default_branch"`
	Private        bool   `json:"private"`
	Archived       bool   `json:"archived"`
	// HasDevcontainer is null when unknown — a probe that failed or has not
	// run. Never false-by-default: "no dev container" is a badge, and showing
	// it for a repository nobody checked would be inventing it.
	HasDevcontainer *bool      `json:"has_devcontainer"`
	PushedAt        *time.Time `json:"pushed_at"`
	// Removed: the installation no longer covers this repository, but a
	// workspace still holds it (§12).
	Removed   bool           `json:"removed"`
	Workspace *WorkspaceView `json:"workspace"`
}

// WorkspaceView is the join the home list needs: which workspace, in which
// state. Everything else about it comes from the workspace's own route.
type WorkspaceView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// List reads the cache. Removed repositories appear only while a workspace
// holds them, which is the only reason they are kept.
func (c *Catalog) List(ctx context.Context) (View, error) {
	v := View{Installations: []InstallationView{}, Repos: []RepoView{}}

	irows, err := c.DB.QueryContext(ctx, `SELECT id, account, account_type, refreshed_at FROM installation ORDER BY id`)
	if err != nil {
		return View{}, err
	}
	defer irows.Close()
	for irows.Next() {
		var in github.Installation
		var refreshed string
		if err := irows.Scan(&in.ID, &in.Account, &in.AccountType, &refreshed); err != nil {
			return View{}, err
		}
		v.Installations = append(v.Installations, InstallationView{ID: in.ID, Account: in.Account, SettingsURL: in.SettingsURL()})
		if t, err := time.Parse(time.RFC3339Nano, refreshed); err == nil && (v.RefreshedAt == nil || t.After(*v.RefreshedAt)) {
			v.RefreshedAt = &t
		}
	}
	if err := irows.Err(); err != nil {
		return View{}, err
	}

	// The workspace join takes the newest workspace per repository that is
	// not being deleted; Create allows at most one occupying workspace per
	// repository, so "newest" only matters among failed and stopped ones.
	rows, err := c.DB.QueryContext(ctx, `
		SELECT r.id, r.installation_id, r.full_name, r.default_branch, coalesce(r.private, 0),
		       coalesce(r.archived, 0), r.has_devcontainer, coalesce(r.pushed_at, ''),
		       r.removed_at IS NOT NULL, coalesce(w.id, ''), coalesce(w.state, '')
		FROM repository r
		LEFT JOIN workspace w ON w.id = (
		  SELECT id FROM workspace WHERE repository_id = r.id AND state != 'deleting' ORDER BY id DESC LIMIT 1)
		WHERE r.removed_at IS NULL OR w.id IS NOT NULL
		ORDER BY r.pushed_at DESC, r.full_name`)
	if err != nil {
		return View{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r RepoView
		var has sql.NullBool
		var pushed, wsID, wsState string
		if err := rows.Scan(&r.ID, &r.InstallationID, &r.FullName, &r.DefaultBranch, &r.Private, &r.Archived,
			&has, &pushed, &r.Removed, &wsID, &wsState); err != nil {
			return View{}, err
		}
		if has.Valid {
			b := has.Bool
			r.HasDevcontainer = &b
		}
		if t, err := time.Parse(time.RFC3339Nano, pushed); err == nil {
			r.PushedAt = &t
		}
		if wsID != "" {
			r.Workspace = &WorkspaceView{ID: wsID, State: wsState}
		}
		v.Repos = append(v.Repos, r)
	}
	return v, rows.Err()
}
