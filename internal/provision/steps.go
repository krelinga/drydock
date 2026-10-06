package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/clone"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/workspace"
)

// runState carries what one step learns for a later one: resolve_config
// decides the override config and the in-container folder, and up and
// verify use them. A start re-runs resolve_config, so nothing here has to
// survive a run.
type runState struct {
	p *Provisioner
	// override is the --override-config path, empty when the repository
	// has its own devcontainer.json.
	override string
	// folder is the clone's path inside the container.
	folder string
	// lockfile is what up does with the repository's devcontainer-lock.json:
	// honoured when the repository commits one, ignored when it does not.
	// lockPath is where up reads and writes that file, for any repository
	// with its own devcontainer.json; "" with Drydock's minimal config.
	lockfile container.Lockfile
	lockPath string
	// removeExisting passes --remove-existing-container to up: a rebuild,
	// or a start from failed.
	removeExisting bool
}

// dir is the workspace's directory: /srv/drydock/ws/<id>.
func (r *runState) dir(w workspace.Workspace) string {
	return filepath.Join(r.p.Workspaces.Root, w.ID)
}

// allocate is §6 step 1's directory. The row already exists (Create made it
// under the cap check); this makes /srv/drydock/ws/<id>/, and refuses one that
// is already there, since a fresh id's directory existing means something
// Drydock does not understand is at that path.
func (r *runState) allocate(_ context.Context, w workspace.Workspace) error {
	dir := r.dir(w)
	if w.HostPath != filepath.Join(dir, "repo") {
		return workspace.Public("The workspace's clone path is not under the workspace root.",
			fmt.Errorf("host_path %q, root %q", w.HostPath, r.p.Workspaces.Root))
	}
	if err := os.MkdirAll(r.p.Workspaces.Root, 0o700); err != nil {
		return workspace.Public("Drydock could not create the workspace root.", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return workspace.Public("Drydock could not create the workspace directory.", err)
	}
	return nil
}

// resolveConfig is §6 step 3. A repository with no devcontainer.json is not a
// disqualification: Drydock writes its minimal config to
// /srv/drydock/ws/<id>/.drydock/devcontainer.json — beside the clone, never in
// it — and passes it as --override-config, here and to up and exec.
//
// "Has no devcontainer.json" is decided by looking for the two files the CLI
// reads by default, not by read-configuration failing: measured on 0.89.0, a
// folder with none exits 1 with an empty stdout and no message, which is what
// any other failure looks like too. A config only in a subfolder of
// .devcontainer/ is one the CLI would not pick without --config, so it gets
// the minimal config as well, and the step says so.
func (r *runState) resolveConfig(ctx context.Context, w workspace.Workspace) error {
	has := false
	for _, rel := range []string{".devcontainer/devcontainer.json", ".devcontainer.json"} {
		ok, err := exists(filepath.Join(w.HostPath, rel))
		if err != nil {
			return workspace.Public("Drydock could not read the clone.", err)
		}
		has = has || ok
	}
	r.override = ""
	if !has {
		r.override = filepath.Join(r.dir(w), ".drydock", "devcontainer.json")
		if err := os.MkdirAll(filepath.Dir(r.override), 0o700); err != nil {
			return workspace.Public("Drydock could not write its minimal dev container configuration.", err)
		}
		cfg := r.p.Config
		if cfg == nil {
			cfg = DefaultConfig()
		}
		if err := os.WriteFile(r.override, cfg, 0o600); err != nil {
			return workspace.Public("Drydock could not write its minimal dev container configuration.", err)
		}
	}
	c, err := r.p.Containers.ReadConfiguration(ctx, w.HostPath, r.override)
	var readErr *container.ReadError
	switch {
	case errors.Is(err, container.ErrUnbuildable):
		return workspace.Public("The repository's devcontainer.json names no image, Dockerfile or Compose file; it may not parse.", err)
	case errors.As(err, &readErr):
		r.p.logTail(w.ID, "read-configuration", []byte(readErr.Stderr))
		return workspace.Public("devcontainer could not read the dev container configuration.", err)
	case err != nil:
		return workspace.Public("Drydock could not read devcontainer's answer about the configuration.", err)
	}
	r.folder = c.WorkspaceFolder
	r.lockfile, r.lockPath = container.LockfileIgnore, ""
	if r.override != "" {
		return workspace.Note("The repository has no devcontainer.json, so it gets Drydock's minimal configuration.")
	}
	return r.resolveLockfile(w, c.ConfigFile)
}

// resolveLockfile decides how up treats the repository's
// devcontainer-lock.json (design §6, "The repository's lockfile"). A committed lockfile
// is honoured — its pinned Feature versions are what the container gets, as
// in VS Code — and a repository without one gets --no-lockfile, which never
// writes, so no lockfile appears in a clone that had none. The path comes
// from what read-configuration says it read, and it must be inside the clone
// after symbolic links are resolved: Drydock reads this file with its own
// uid, and the CLI writes it with the same.
func (r *runState) resolveLockfile(w workspace.Workspace, configFile string) error {
	if !strings.HasPrefix(configFile, w.HostPath+"/") {
		return workspace.Public("devcontainer reported a configuration file outside the clone.",
			fmt.Errorf("config file %q, clone %q", configFile, w.HostPath))
	}
	path := container.LockfilePath(configFile)
	clone, err1 := filepath.EvalSymlinks(w.HostPath)
	dir, err2 := filepath.EvalSymlinks(filepath.Dir(path))
	if err := errors.Join(err1, err2); err != nil || !strings.HasPrefix(dir+"/", clone+"/") {
		return workspace.Public("The repository's dev container configuration is not inside the clone.",
			fmt.Errorf("lockfile directory %q resolves to %q, clone %q: %v", filepath.Dir(path), dir, clone, err))
	}
	mode, err := container.LockfileMode(configFile, []string{r.p.Feature})
	switch {
	case errors.Is(err, container.ErrLockfilePinsInjected):
		return workspace.Public("The repository's devcontainer lockfile pins Drydock's own Feature, which only Drydock's configuration may pin. Remove that entry and commit the lockfile.", err)
	case errors.Is(err, container.ErrLockfileUnreadable):
		return workspace.Public("The repository's devcontainer lockfile is not a lockfile the dev container CLI could use.", err)
	case err != nil:
		return workspace.Public("Drydock could not read the repository's devcontainer lockfile.", err)
	}
	r.lockfile, r.lockPath = mode, path
	if mode == container.LockfileHonour {
		return workspace.Note("The repository commits a devcontainer lockfile, so its pinned Feature versions are the ones installed.")
	}
	return nil
}

// credentialVolume is §6 step 4: the shared Claude credential volume (§7.1),
// made if it is absent — local driver, labelled with this Drydock's prefix —
// and checked either way. Every workspace mounts the same one at its
// CLAUDE_CONFIG_DIR, so one login serves them all. It runs on every create,
// start and rebuild and is a no-op once the volume is there; it never removes
// or changes a volume, and refuses one this Drydock did not make or that is
// not a plain local volume (container.EnsureClaudeVolume) — Claude Code's
// refresh lock inside it needs mkdir to be atomic, which NFS and CIFS do not
// give (Spike 00).
func (r *runState) credentialVolume(ctx context.Context, _ workspace.Workspace) error {
	name := r.p.ClaudeVolume
	if name == "" {
		return workspace.Public("No shared Claude credential volume is configured.",
			errors.New("provision: ClaudeVolume is empty"))
	}
	created, err := r.p.Containers.EnsureClaudeVolume(ctx, name)
	switch {
	case errors.Is(err, container.ErrForeignVolume):
		return workspace.Public(fmt.Sprintf("A Docker volume named %s exists but was not made by this Drydock, so it is not used as the shared Claude credential volume.", name), err)
	case errors.Is(err, container.ErrVolumeNotLocal):
		return workspace.Public(fmt.Sprintf("The shared Claude credential volume %s is not a plain local Docker volume. It must be: Claude Code's refresh lock is not safe on a network filesystem.", name), err)
	case err != nil:
		return workspace.Public("Drydock could not create or check the shared Claude credential volume.", err)
	case created:
		return workspace.Note(fmt.Sprintf("Created the shared Claude credential volume %s.", name))
	}
	return workspace.Note(fmt.Sprintf("The shared Claude credential volume %s is there.", name))
}

// brokerSocket is §6 step 5: the workspace's token-broker socket.
func (r *runState) brokerSocket(ctx context.Context, w workspace.Workspace) error {
	if err := r.p.Broker.Open(ctx, w.ID); err != nil {
		return workspace.Public("Drydock could not open the workspace's GitHub access socket.", err)
	}
	return nil
}

// up is §6 step 6. A failed up can still own a container — a failing
// postCreateCommand leaves it created and running (§6) — so the id is
// recorded whatever the outcome, for teardown and reconciliation to find.
//
// Honouring a committed lockfile lets `up` rewrite it when it is stale
// (container.Lockfile). The rewrite is left in the clone, and the step names
// the file so it does not surprise anyone at commit time (see lockfile.go).
func (r *runState) up(ctx context.Context, w workspace.Workspace) error {
	fullName, err := r.fullName(ctx, w)
	if err != nil {
		return err
	}
	before := snapshotLockfile(r.lockPath)
	// A TMPDIR of the workspace's own, beside the clone: the CLI stages
	// Features under $TMPDIR in a folder named by the millisecond, which
	// concurrent creates otherwise share (container.UpSpec.TempDir).
	tmp := filepath.Join(r.dir(w), ".drydock", "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return workspace.Public("Drydock could not create the workspace's temporary directory.", err)
	}
	defer os.RemoveAll(tmp)
	res, stderr, err := r.p.Containers.Up(ctx, container.UpSpec{
		WorkspaceID: w.ID, RepositoryID: w.RepositoryID, FullName: fullName, Branch: w.Branch,
		Folder:         w.HostPath,
		BrokerSocket:   r.p.Broker.SocketPath(w.ID),
		ClaudeVolume:   r.p.ClaudeVolume,
		Features:       map[string]map[string]any{r.p.Feature: r.p.FeatureOptions},
		RemoteEnv:      r.remoteEnv(w, fullName),
		OverrideConfig: r.override,
		Lockfile:       r.lockfile,
		TempDir:        tmp,
		Rebuild:        r.removeExisting,
	})
	if res.ContainerID != "" {
		if err := r.p.Workspaces.SetContainer(context.WithoutCancel(ctx), w.ID, res.ContainerID); err != nil {
			return workspace.Public("Drydock could not record the workspace's container.", err)
		}
	}
	if err != nil {
		r.p.logTail(w.ID, "devcontainer up", stderr)
		return workspace.Public("Drydock could not run devcontainer up, or could not read its result.", err)
	}
	if res.Outcome != classify.ContainerRunning {
		r.p.logTail(w.ID, "devcontainer up", stderr)
		// The CLI's message can quote the repository's own commands, so it
		// goes to the service log with the rest, not into the detail.
		return workspace.Public("devcontainer up did not bring the container up; the service log has its output.",
			fmt.Errorf("devcontainer up: %s %s", res.Message, res.Description))
	}
	return lockfileChange(before, w.HostPath)
}

func (r *runState) fullName(ctx context.Context, w workspace.Workspace) (string, error) {
	var name string
	if err := r.p.Workspaces.DB.QueryRowContext(ctx, `SELECT full_name FROM repository WHERE id = ?`,
		w.RepositoryID).Scan(&name); err != nil {
		return "", workspace.Public("Drydock could not read the repository's record.", err)
	}
	return name, nil
}

// remoteEnv is DRYDOCK_WORKSPACE and DRYDOCK_REPO (§6 step 6) plus anything
// configured. Passed to exec as well as up: up's --remote-env does not reach
// a later exec (§6, measured).
func (r *runState) remoteEnv(w workspace.Workspace, fullName string) map[string]string {
	env := map[string]string{"DRYDOCK_WORKSPACE": w.ID, "DRYDOCK_REPO": fullName}
	for k, v := range r.p.RemoteEnv {
		env[k] = v
	}
	return env
}

// verify is §6 step 7: a green probe is what moves the workspace to running.
// Three checks, each through `devcontainer exec` as the remote user:
//
//   - drydock-probe, one PING over the broker socket — the Feature is
//     installed and the socket is mounted and answering;
//   - git -C <folder> remote -v — the clone is where the container's
//     workspace folder says, and its origin is the plain repository URL the
//     clone left (no credential, no helper: the Feature's is the only one);
//   - claude --version — Claude Code runs, and is the version this Drydock's
//     classifiers were recorded against. A repository's image can carry a
//     Claude Code of its own; the Feature puts its pinned one first on PATH,
//     and this is where a container in which that did not hold is refused,
//     rather than a session server scraped with the wrong version's patterns.
func (r *runState) verify(ctx context.Context, w workspace.Workspace) error {
	fullName, err := r.fullName(ctx, w)
	if err != nil {
		return err
	}
	exec := func(argv ...string) (string, error) {
		out := &limitWriter{n: 64 << 10}
		res, err := r.p.Containers.ExecIn(ctx, container.ExecSpec{
			WorkspaceID: w.ID, Folder: w.HostPath, OverrideConfig: r.override,
			RemoteEnv: r.remoteEnv(w, fullName), Argv: argv, Stdout: out, Stderr: out,
		})
		switch {
		case err != nil:
			return "", err
		case res.Err != nil:
			return string(out.b), res.Err
		case res.ExitCode != 0:
			return string(out.b), fmt.Errorf("%s exited %d", argv[0], res.ExitCode)
		}
		return string(out.b), nil
	}
	if out, err := exec("drydock-probe"); err != nil {
		r.p.logTail(w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: the GitHub access socket did not answer.", err)
	}
	out, err := exec("git", "-C", r.folder, "remote", "-v")
	if err != nil {
		r.p.logTail(w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: git found no clone at the workspace folder.", err)
	}
	want := "origin\t" + r.cloneURL(fullName) + " (fetch)"
	if !strings.Contains(out, want) {
		r.p.logTail(w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: the clone's origin is not the repository.",
			fmt.Errorf("git remote -v has no %q", want))
	}
	out, err = exec("claude", "--version")
	if err != nil {
		r.p.logTail(w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: Claude Code did not run.", err)
	}
	if v := r.p.ClaudeCodeVersion; v != "" && !strings.HasPrefix(out, v+" ") {
		r.p.logTail(w.ID, "probe", []byte(out))
		return workspace.Public(fmt.Sprintf("The probe inside the container failed: Claude Code is not version %s, the one Drydock was built for.", v),
			fmt.Errorf("claude --version said %q", strings.TrimSpace(out)))
	}
	return nil
}

func (r *runState) cloneURL(fullName string) string {
	base := r.p.Cloner.BaseURL
	if base == "" {
		base = clone.DefaultBaseURL
	}
	return strings.TrimSuffix(base, "/") + "/" + fullName + ".git"
}

// sessionServer is §6 step 8: the workspace is handed to the session
// supervisor (§8), which starts `claude remote-control` in the container and
// reports its own states — starting, serving, waiting, awaiting a login — as
// supervisor.state events. The step does not wait for serving: a server that
// never serves is the supervisor's to describe, and the container is fine,
// so even a failure here leaves the workspace running (§6).
func (r *runState) sessionServer(ctx context.Context, w workspace.Workspace) error {
	if r.p.StartSupervisor == nil {
		return workspace.Note("No session supervisor is configured, so no session server was started.")
	}
	if err := r.p.StartSupervisor(ctx, w); err != nil {
		return workspace.Public("Drydock could not hand the workspace to the session supervisor.", err)
	}
	return workspace.Note("Handed to the session supervisor, which starts the Claude Code session server.")
}

// SessionSpec is how the supervisor execs into a workspace's container: the
// same folder and override config `up` and the probe used, and the remote env
// passed again on the exec, since `up`'s does not carry over (§6, measured) —
// plus the session name prefix, so the Claude app's session list reads
// `myrepo-graceful-unicorn` rather than a container hostname (§8). The
// override is decided as step 3 decides it: by looking for the repository's
// own devcontainer.json in the clone.
func (p *Provisioner) SessionSpec(ctx context.Context, id string) (container.SessionSpec, error) {
	w, err := p.Workspaces.Get(ctx, id)
	if err != nil {
		return container.SessionSpec{}, err
	}
	r := &runState{p: p}
	fullName, err := r.fullName(ctx, w)
	if err != nil {
		return container.SessionSpec{}, err
	}
	has := false
	for _, rel := range []string{".devcontainer/devcontainer.json", ".devcontainer.json"} {
		ok, err := exists(filepath.Join(w.HostPath, rel))
		if err != nil {
			return container.SessionSpec{}, err
		}
		has = has || ok
	}
	override := ""
	if !has {
		override = filepath.Join(r.dir(w), ".drydock", "devcontainer.json")
	}
	env := r.remoteEnv(w, fullName)
	env["CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX"] = fullName[strings.LastIndex(fullName, "/")+1:]
	return container.SessionSpec{WorkspaceID: w.ID, Folder: w.HostPath, OverrideConfig: override, RemoteEnv: env}, nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
