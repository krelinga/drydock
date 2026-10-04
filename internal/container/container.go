// Package container is Drydock's only route to containers: `devcontainer up`
// to make one, and `docker` to find them again by label (design §6).
//
// Two rules from the design shape it. The `up` result is the contract — one
// JSON object on stdout, parsed by classify.ClassifyContainer — and nothing
// scrapes human-oriented output. And containers are found by label, never by
// name or a remembered id: Docker is the truth, the database is the cache, so
// every fact reconciliation needs to rebuild a workspace row is on the
// container as a label.
package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/subproc"
)

// Manager runs the devcontainer CLI and docker for one label prefix.
type Manager struct {
	Run subproc.Runner
	// LabelPrefix is config.LabelPrefix. Every label this manager writes or
	// filters on is under it, so a second Drydock with its own prefix — a
	// test run — never sees this one's containers.
	LabelPrefix string
}

// Label keys, under the prefix. Workspace is the id-label `up` matches on;
// the rest are what reconciliation needs to rebuild a row for an orphan.
const (
	LabelWorkspace    = "workspace"
	LabelRepositoryID = "repository-id"
	LabelRepo         = "repo"
	LabelBranch       = "branch"
)

func (m Manager) key(k string) string { return m.LabelPrefix + "." + k }

// UpSpec is one workspace's container.
type UpSpec struct {
	WorkspaceID  string
	RepositoryID int64
	FullName     string // owner/repo
	Branch       string
	// Folder is the clone on the host: /srv/drydock/ws/<id>/repo.
	Folder string
	// Rebuild passes --remove-existing-container. Without it, `up` with an
	// existing id-label reattaches and reports success even when the config
	// has changed (§6), so a rebuild that forgot it would silently not be one.
	Rebuild bool
}

var (
	workspaceIDPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`) // a ULID
	fullNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Args builds `devcontainer up`'s argv. Exported so the argv — a security
// surface, since it is assembled from workspace data — can be asserted on
// directly as well as through a fake binary (testing §5.3).
func (m Manager) Args(s UpSpec) ([]string, error) {
	if !workspaceIDPattern.MatchString(s.WorkspaceID) {
		return nil, fmt.Errorf("container: %q is not a workspace id", s.WorkspaceID)
	}
	if !fullNamePattern.MatchString(s.FullName) {
		return nil, fmt.Errorf("container: %q is not an owner/repo name", s.FullName)
	}
	if s.RepositoryID <= 0 || s.Branch == "" || !strings.HasPrefix(s.Folder, "/") {
		return nil, errors.New("container: a repository id, a branch, and an absolute folder are required")
	}
	args := []string{"up",
		"--workspace-folder", s.Folder,
		"--id-label", m.key(LabelWorkspace) + "=" + s.WorkspaceID,
		"--id-label", m.key(LabelRepositoryID) + "=" + strconv.FormatInt(s.RepositoryID, 10),
		"--id-label", m.key(LabelRepo) + "=" + s.FullName,
		"--id-label", m.key(LabelBranch) + "=" + s.Branch,
	}
	if s.Rebuild {
		args = append(args, "--remove-existing-container")
	}
	return args, nil
}

// Up brings a workspace's container up and returns the CLI's verdict. A
// failed `up` is a ContainerFailed result, not an error — and it may carry a
// ContainerID, because a failed postCreateCommand leaves the container
// running (§6). An error means the CLI could not be run or its result could
// not be read, which is the contract moving.
//
// stderr is the CLI's log and can quote the repository's own config and
// commands; it is returned for the caller to keep out of the event log.
func (m Manager) Up(ctx context.Context, s UpSpec) (classify.Container, []byte, error) {
	args, err := m.Args(s)
	if err != nil {
		return classify.Container{}, nil, err
	}
	var stdout, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "devcontainer", Args: args, Stdout: &stdout, Stderr: limit(&stderr, 1<<20)})
	if res.Err != nil {
		return classify.Container{}, stderr.Bytes(), fmt.Errorf("devcontainer up: %w", res.Err)
	}
	c, err := classify.ClassifyContainer(stdout.Bytes())
	if err != nil {
		return classify.Container{}, stderr.Bytes(), err
	}
	return c, stderr.Bytes(), nil
}

// Found is a container carrying this manager's workspace label.
type Found struct {
	ContainerID  string
	WorkspaceID  string
	RepositoryID int64 // 0 if the label is missing or malformed
	Repo         string
	Branch       string
	Running      bool
	Status       string // docker's State.Status: running, exited, created, …
}

// List finds every container, running or not, carrying this prefix's
// workspace label. Two calls: `docker ps` for the ids only, filtered by label
// on the daemon's side, then `docker inspect` for structured JSON. Nothing
// parses a table.
func (m Manager) List(ctx context.Context) ([]Found, error) {
	var ids, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:   []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=" + m.key(LabelWorkspace)},
		Stdout: &ids, Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker ps", res, &stderr); err != nil {
		return nil, err
	}
	list := strings.Fields(ids.String())
	if len(list) == 0 {
		return nil, nil
	}

	var out bytes.Buffer
	stderr.Reset()
	res = m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"inspect", "--type", "container"}, list...),
		Stdout: &out, Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker inspect", res, &stderr); err != nil {
		// A container removed between the two calls fails the inspect.
		// That is a race, not a fault, and the next reconcile sees the
		// world without it — but this one cannot trust a partial list.
		return nil, err
	}
	return m.parseInspect(out.Bytes())
}

type inspect struct {
	ID    string `json:"Id"`
	State struct {
		Status  string
		Running bool
	}
	Config struct {
		Labels map[string]string
	}
}

func (m Manager) parseInspect(b []byte) ([]Found, error) {
	var all []inspect
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var found []Found
	for _, c := range all {
		ws, ok := c.Config.Labels[m.key(LabelWorkspace)]
		if !ok || c.ID == "" {
			// The daemon filtered on this label; a result without it means
			// the contract moved, and acting on it would be guessing.
			return nil, fmt.Errorf("docker inspect: container %q lacks the %s label it was listed by", c.ID, m.key(LabelWorkspace))
		}
		repoID, _ := strconv.ParseInt(c.Config.Labels[m.key(LabelRepositoryID)], 10, 64)
		found = append(found, Found{
			ContainerID: c.ID, WorkspaceID: ws, RepositoryID: repoID,
			Repo: c.Config.Labels[m.key(LabelRepo)], Branch: c.Config.Labels[m.key(LabelBranch)],
			Running: c.State.Running, Status: c.State.Status,
		})
	}
	return found, nil
}

func failed(what string, res subproc.Result, stderr *bytes.Buffer) error {
	if res.Err != nil {
		return fmt.Errorf("%s: %w", what, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s: exit %d: %s", what, res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// limit caps a buffer, so a CLI that logs without end cannot exhaust memory.
func limit(b *bytes.Buffer, n int) *capped { return &capped{b: b, n: n} }

type capped struct {
	b *bytes.Buffer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil // never short: the child must not see a write error
}
