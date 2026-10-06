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
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/subproc"
)

// Manager runs the devcontainer CLI and docker for one label prefix.
type Manager struct {
	Run subproc.Runner
	// LabelPrefix is config.LabelPrefix. Every label this manager writes or
	// filters on is under it, so a second Drydock with its own prefix — a
	// test run — never sees this one's containers.
	LabelPrefix string
	// CleanupImage is config.CleanupImage: the digest-pinned image a delete
	// runs to remove what the drydock user cannot (cleanup.go).
	CleanupImage string
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
	// BrokerSocket is the workspace's token-broker socket on the host,
	// bind-mounted at BrokerMountPoint in this container and no other
	// (§6 step 5, §9.1). Empty mounts nothing.
	BrokerSocket string
	// ClaudeVolume is the shared Claude credential volume (§7.1), mounted at
	// ClaudeConfigMountPoint — the same volume in every workspace, which is
	// what makes one login serve them all. EnsureClaudeVolume makes it.
	// Empty mounts nothing, and the Feature then refuses the container.
	ClaudeVolume string
	// Features is --additional-features: feature reference → options. It
	// composes with what the repository declares rather than replacing it.
	Features map[string]map[string]any
	// RemoteEnv is --remote-env: set for the remote user's processes.
	RemoteEnv map[string]string
	// TempDir, when set, is the TMPDIR `up` runs with. It must be this
	// workspace's alone: CLI 0.89.0 stages each run's Features in
	// $TMPDIR/devcontainercli-<user>/container-features/<version>-<Date.now()>,
	// so two `up`s that start in the same millisecond share one folder and
	// build each other's Features — measured: of two workspaces created
	// together, one came up without a Feature its config declares.
	// Empty inherits Drydock's.
	TempDir string
	// Lockfile is what `up` does with the repository's
	// devcontainer-lock.json: LockfileHonour reads a committed one, and
	// rewrites it if it is stale, as VS Code would; LockfileIgnore (the zero
	// value), for a repository without one, never writes. See LockfileMode.
	Lockfile Lockfile
	// OverrideConfig is --override-config: a devcontainer.json outside the
	// clone, used in place of the repository's. Drydock writes one only for
	// a repository that has none (§6 step 3), so the repository is never
	// modified. Empty uses the repository's own.
	OverrideConfig string
}

// BrokerMountPoint is where a workspace's broker socket appears inside its
// container; the Feature's DRYDOCK_BROKER_SOCK points here.
const BrokerMountPoint = "/run/drydock/broker.sock"

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	// With no lockfile flag `up` may write devcontainer-lock.json into the
	// clone (measured on CLI 0.89.0); see Lockfile.
	lock, err := s.Lockfile.flag()
	if err != nil {
		return nil, err
	}
	if s.Lockfile == LockfileHonour && s.OverrideConfig != "" {
		// The override is Drydock's own config for a repository with none,
		// and with no flag the CLI fails writing a lockfile at the
		// repository's default path (ENOENT, measured) — there is nothing to
		// honour, and a lockfile beside the override is never read.
		return nil, errors.New("container: honouring a lockfile with an override config")
	}
	args := []string{"up", "--workspace-folder", s.Folder}
	if lock != "" {
		args = append(args, lock)
	}
	args = append(args,
		"--id-label", m.key(LabelWorkspace)+"="+s.WorkspaceID,
		"--id-label", m.key(LabelRepositoryID)+"="+strconv.FormatInt(s.RepositoryID, 10),
		"--id-label", m.key(LabelRepo)+"="+s.FullName,
		"--id-label", m.key(LabelBranch)+"="+s.Branch,
	)
	if s.BrokerSocket != "" {
		// --mount is comma-separated key=value pairs, so a comma or an equals
		// sign in the path would let it add mount options of its own.
		if !strings.HasPrefix(s.BrokerSocket, "/") || strings.ContainsAny(s.BrokerSocket, ",=\n") {
			return nil, fmt.Errorf("container: broker socket path %q must be absolute and free of ',' and '='", s.BrokerSocket)
		}
		args = append(args, "--mount", "type=bind,source="+s.BrokerSocket+",target="+BrokerMountPoint)
	}
	if s.ClaudeVolume != "" {
		if !config.ValidVolumeName(s.ClaudeVolume) {
			return nil, fmt.Errorf("container: %q is not a volume name", s.ClaudeVolume)
		}
		args = append(args, "--mount", "type=volume,source="+s.ClaudeVolume+",target="+ClaudeConfigMountPoint)
	}
	if len(s.Features) > 0 {
		b, err := json.Marshal(s.Features)
		if err != nil {
			return nil, fmt.Errorf("container: features: %w", err)
		}
		args = append(args, "--additional-features", string(b))
	}
	if s.OverrideConfig != "" {
		if !strings.HasPrefix(s.OverrideConfig, "/") {
			return nil, fmt.Errorf("container: override config %q must be absolute", s.OverrideConfig)
		}
		args = append(args, "--override-config", s.OverrideConfig)
	}
	env, err := remoteEnvArgs(s.RemoteEnv)
	if err != nil {
		return nil, err
	}
	args = append(args, env...)
	if s.Rebuild {
		args = append(args, "--remove-existing-container")
	}
	return args, nil
}

// Exec runs a command in a workspace's container as its remote user, found
// by the workspace's id-label (§6 step 7's probe, and the supervisor's
// start). argv is passed through; there is no shell here unless the caller
// names one.
//
// remoteEnv is passed again here because `up`'s --remote-env does not
// persist: measured on CLI 0.89.0, a variable given to `up` is absent from a
// later `exec`. Only devcontainer.json's own remoteEnv, and the Feature's
// containerEnv, carry over by themselves.
func (m Manager) Exec(ctx context.Context, workspaceID, folder string, remoteEnv map[string]string, argv []string, stdout, stderr io.Writer) (subproc.Result, error) {
	return m.ExecIn(ctx, ExecSpec{WorkspaceID: workspaceID, Folder: folder, RemoteEnv: remoteEnv,
		Argv: argv, Stdout: stdout, Stderr: stderr})
}

// ExecSpec is one `devcontainer exec`.
type ExecSpec struct {
	WorkspaceID string
	Folder      string // the clone on the host
	// OverrideConfig is the same --override-config `up` was given. exec
	// reads the configuration too (for the remote user and remoteEnv), and
	// measured on CLI 0.89.0 it needs the override as much as `up` does
	// when the repository has no devcontainer.json of its own.
	OverrideConfig string
	RemoteEnv      map[string]string
	Argv           []string
	Stdout, Stderr io.Writer
}

// ExecIn is Exec with every option.
func (m Manager) ExecIn(ctx context.Context, s ExecSpec) (subproc.Result, error) {
	if !workspaceIDPattern.MatchString(s.WorkspaceID) || !strings.HasPrefix(s.Folder, "/") || len(s.Argv) == 0 {
		return subproc.Result{}, errors.New("container: exec needs a workspace id, an absolute folder and a command")
	}
	args := []string{"exec", "--workspace-folder", s.Folder, "--id-label", m.key(LabelWorkspace) + "=" + s.WorkspaceID}
	if s.OverrideConfig != "" {
		if !strings.HasPrefix(s.OverrideConfig, "/") {
			return subproc.Result{}, fmt.Errorf("container: override config %q must be absolute", s.OverrideConfig)
		}
		args = append(args, "--override-config", s.OverrideConfig)
	}
	env, err := remoteEnvArgs(s.RemoteEnv)
	if err != nil {
		return subproc.Result{}, err
	}
	args = append(append(append(args, env...), "--"), s.Argv...)
	return m.Run.Run(ctx, subproc.Cmd{Name: "devcontainer", Args: args, Stdout: s.Stdout, Stderr: s.Stderr}), nil
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
	cmd := subproc.Cmd{Name: "devcontainer", Args: args, Stdout: &stdout, Stderr: limit(&stderr, 1<<20)}
	if s.TempDir != "" {
		if !strings.HasPrefix(s.TempDir, "/") {
			return classify.Container{}, nil, fmt.Errorf("container: temp dir %q must be absolute", s.TempDir)
		}
		cmd.Env = withTempDir(os.Environ(), s.TempDir)
	}
	res := m.Run.Run(ctx, cmd)
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

// remoteEnvArgs renders --remote-env flags in name order, so the argv is
// stable and a test can compare it.
func remoteEnvArgs(env map[string]string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("container: %q is not an environment variable name", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		args = append(args, "--remote-env", k+"="+env[k])
	}
	return args, nil
}

// withTempDir is env with TMPDIR set to dir, replacing any TMPDIR in it.
func withTempDir(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TMPDIR=") {
			out = append(out, kv)
		}
	}
	return append(out, "TMPDIR="+dir)
}
