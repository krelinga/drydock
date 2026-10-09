// Package clone is design §6 step 2: the host-side clone of a workspace's
// repository into /srv/drydock/ws/<id>/repo, before `devcontainer up`.
//
// The whole package is about one property: the installation token that
// authorizes the clone must leave no trace. Not in any process's argv, not in
// .git/config or anywhere else under the workspace, not in an event, not in
// an error string. Each of those is a place a token-in-the-URL clone (the
// obvious implementation, and the one earlier drafts of §6 described) leaks
// it.
//
// # Rules and details
//
// A contents:read token for the one repository reaches git only through its
// environment: a GIT_CONFIG_COUNT credential helper that prints it from an
// environment variable. It is never in argv, a URL or .git/config. git runs
// with global and system config at /dev/null and GIT_CEILING_DIRECTORIES above
// the workspace, so an enclosing repo's http.extraheader cannot win. It never
// deletes a directory already at the clone path.
//
// Tested with a GIT_TRACE wrapper that records every child process's argv (a
// wrapper around git sees only git's own argv; GIT_TRACE also records the
// credential helper git starts), plus a canary sweep of the tree and the
// database.
package clone

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/workspace"
)

// DefaultBaseURL is where repositories are cloned from.
const DefaultBaseURL = "https://github.com"

// tokenEnv is the variable the credential helper reads the token from.
const tokenEnv = "DRYDOCK_CLONE_TOKEN"

// Permissions is what a clone token may do: read the repository, and the
// metadata every installation token carries. Nothing that could push — the
// clone is Drydock's, not the agent's, and the agent's writes go through the
// broker's own scope (§9.3).
func Permissions() map[string]string {
	return map[string]string{"contents": github.Read, "metadata": github.Read}
}

// Cloner clones workspaces. Step is its workspace.StepFunc.
type Cloner struct {
	DB     *sql.DB
	GitHub *github.Client
	Runner subproc.Runner
	// BaseURL is DefaultBaseURL unless a test points it at a fake's git
	// remote. No trailing slash.
	BaseURL string
	// Path is the PATH git runs with. Empty means the process's own. It is
	// the only variable passed through: the child's environment is built,
	// never inherited (subproc.Cmd.Env).
	Path string
}

// repository is what the clone reads about a workspace's repository. The
// broker reads the same row per request; this is the clone's own copy of
// that read because the clone wants the name and the broker does not.
type repository struct {
	installationID int64
	fullName       string
	removed        bool
}

func (c *Cloner) repository(ctx context.Context, id int64) (repository, error) {
	var r repository
	err := c.DB.QueryRowContext(ctx, `
		SELECT installation_id, full_name, removed_at IS NOT NULL
		FROM repository WHERE id = ?`, id).Scan(&r.installationID, &r.fullName, &r.removed)
	return r, err
}

// fullName is GitHub's owner/name. Checked because it becomes part of a URL
// and a path segment of argv.
var fullName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

// Step clones w's repository to w.HostPath on w.Branch.
//
// Every error it returns is a workspace.Public whose sentence is Drydock's
// own: the wrapped error may carry git's stderr, which names the URL, and
// the step runner keeps everything but the sentence out of the event log.
// git's stderr is also scrubbed of the token before it is wrapped, so even
// the private half is safe to log — defence in depth, since the helper
// design means git never had the token in a URL to print.
func (c *Cloner) Step(ctx context.Context, w workspace.Workspace) error {
	repo, err := c.repository(ctx, w.RepositoryID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return workspace.Public("The repository is not in Drydock's catalog.", err)
	case err != nil:
		return workspace.Public("Drydock could not read the repository's record.", err)
	case repo.removed:
		return workspace.Public("The repository is no longer covered by the GitHub App installation.", errors.New("repository removed"))
	case !fullName.MatchString(repo.fullName) || strings.Contains(repo.fullName, ".."):
		return workspace.Public("The repository's name is not one Drydock can clone.", fmt.Errorf("full_name %q", repo.fullName))
	}
	if !ValidBranch(w.Branch) {
		return workspace.Public("The branch name is not one Drydock can clone.", fmt.Errorf("branch %q", w.Branch))
	}
	if w.HostPath == "" || !filepath.IsAbs(w.HostPath) {
		return workspace.Public("The workspace has no clone path.", fmt.Errorf("host_path %q", w.HostPath))
	}
	if _, err := os.Lstat(w.HostPath); err == nil {
		// A retried clone starts clean, but never by deleting: the
		// directory may be someone's work, and removing it is Delete's
		// job alone.
		return workspace.Public("The clone directory already exists.", fmt.Errorf("%s exists", w.HostPath))
	}
	parent := filepath.Dir(w.HostPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return workspace.Public("Drydock could not create the workspace directory.", err)
	}

	tok, err := c.GitHub.InstallationToken(ctx, github.TokenRequest{
		InstallationID: repo.installationID,
		Permissions:    Permissions(),
		RepositoryIDs:  []int64{w.RepositoryID}, // exactly this workspace's one repository
	})
	if err != nil {
		// APIError text names the method, path and GitHub's message; the
		// token travels in a header and appears in none of them.
		return workspace.Public("GitHub refused a token to clone the repository.", err)
	}

	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/")
	url := base + "/" + repo.fullName + ".git"

	var stderr bytes.Buffer
	res := c.Runner.Run(ctx, subproc.Cmd{
		Name: "git",
		// --branch=<b> in one word and `--` before the operands, so a
		// branch or path can never be read as an option. No
		// --single-branch: the agent fetches other branches through the
		// Feature's helper later, and a narrowed refspec would surprise
		// it. No --recurse-submodules: a submodule may name another
		// host, and this credential is for exactly one repository.
		Args:   []string{"clone", "--quiet", "--no-tags", "--branch=" + w.Branch, "--", url, w.HostPath},
		Dir:    parent,
		Env:    c.env(base, parent, tok.Value()),
		Stderr: &stderr,
	})
	if res.Err != nil || res.ExitCode != 0 {
		msg := strings.ReplaceAll(stderr.String(), tok.Value(), "[redacted]")
		cause := fmt.Errorf("git clone exited %d: %s", res.ExitCode, strings.TrimSpace(msg))
		if res.Err != nil {
			cause = fmt.Errorf("git clone: %w", res.Err)
		}
		return workspace.Public("git could not clone the repository.", cause)
	}
	return nil
}

// env is git's whole environment. Nothing is inherited.
//
// The credential reaches git as a credential helper set through
// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n — command-line-scope
// configuration carried in the environment — and the helper is an inline
// shell function that prints the token from tokenEnv. Why this, rather than
// the alternatives:
//
//   - A token in the URL, or `-c http.extraheader=…`, puts it in argv, and
//     /proc/<pid>/cmdline is readable by every user on the host. A URL
//     token is also what git writes into .git/config as the remote.
//   - The environment, by contrast, is /proc/<pid>/environ, readable only
//     by the same uid (ptrace access), which is Drydock itself.
//   - GIT_ASKPASS would work too, but needs an executable on disk to name,
//     and a file Drydock writes is one more thing that must be created,
//     guarded and removed. The inline helper needs nothing: git runs it
//     with `sh -c`, so the helper's own argv carries the literal text
//     "$DRYDOCK_CLONE_TOKEN" — the shell expands it, and printf is a
//     builtin, so the value is never in any process's argv.
//   - Configuration from the environment is never written to .git/config,
//     so after the clone no helper is left configured and the remote is
//     the plain URL: inside the container the Feature's helper (§9.2) is
//     the only one git consults.
//
// The helper is keyed to the base URL, so a redirect to another host gets
// nothing, and an empty `credential.helper` first resets the list.
//
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are /dev/null so no operator or
// image configuration joins in, and GIT_CEILING_DIRECTORIES stops git
// searching above the workspace directory for a repository whose config it
// would read: an `http.extraheader` in an enclosing repository's config is
// sent ahead of the helper's credential and wins.
func (c *Cloner) env(base, dir, token string) []string {
	path := c.Path
	if path == "" {
		path = os.Getenv("PATH")
	}
	helper := `!f() { test "$1" = get && printf 'username=x-access-token\npassword=%s\n' "$` + tokenEnv + `"; }; f`
	cfg := [][2]string{
		{"credential.helper", ""},
		{"credential." + base + ".helper", helper},
		{"credential." + base + ".username", "x-access-token"},
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + dir,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir),
		"LC_ALL=C",
		tokenEnv + "=" + token,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)),
	}
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// ValidBranch is a conservative subset of git's ref-name rules: enough for
// every branch a person names, and nothing that could be an option or walk
// out of refs/heads.
func ValidBranch(b string) bool {
	if b == "" || len(b) > 255 || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") || strings.HasSuffix(b, ".") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") {
		return false
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}
