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
	// lockPath is the file, when honoured.
	lockfile container.Lockfile
	lockPath string
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
	if err := r.recoverLockfile(w); err != nil {
		return err
	}
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
// writes. The path comes from what read-configuration says it read, and it
// must be inside the clone after symbolic links are resolved: Drydock saves
// and restores this file with its own uid, and so does the CLI write it.
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
	r.lockfile = mode
	if mode == container.LockfileHonour {
		r.lockPath = path
		return workspace.Note("The repository commits a devcontainer lockfile, so its pinned Feature versions are the ones installed.")
	}
	return nil
}

// credentialVolume is §6 step 4, and does nothing yet: the shared Claude
// credential volume (§7.1) arrives with Claude support in Phase 5. The step
// is kept, and says so, rather than being left out — so the pipeline the UI
// shows is the design's eight steps from the first workspace, and Phase 5
// fills a step in rather than adding one.
func (r *runState) credentialVolume(context.Context, workspace.Workspace) error {
	return workspace.Note("Nothing to do yet: the shared Claude credential volume arrives with Claude support.")
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
// Honouring a committed lockfile lets `up` rewrite it (container.Lockfile),
// so its bytes are saved outside the clone first and put back after, whatever
// the outcome; a restore that fails fails the step, and leaves the save for
// the next run or boot to finish (see lockfile.go).
func (r *runState) up(ctx context.Context, w workspace.Workspace) (err error) {
	fullName, err := r.fullName(ctx, w)
	if err != nil {
		return err
	}
	if err := r.recoverLockfile(w); err != nil {
		return err
	}
	lock := r.lockfile
	if lock == container.LockfileHonour {
		saved, serr := saveLockfile(r.dir(w), r.lockPath)
		if serr != nil {
			return workspace.Public("Drydock could not save the repository's devcontainer lockfile before devcontainer up.", serr)
		}
		if !saved {
			lock = container.LockfileIgnore // gone since resolve_config: nothing to honour
		} else {
			defer func() {
				if rerr := restoreLockfile(r.dir(w), w.HostPath); rerr != nil {
					r.p.logf("drydock: workspace %s: restoring the lockfile: %v", w.ID, rerr)
					if err == nil {
						err = workspace.Public("Drydock could not restore the repository's devcontainer lockfile after devcontainer up.", rerr)
					}
				}
			}()
		}
	}
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
		Features:       map[string]map[string]any{r.p.Feature: r.p.FeatureOptions},
		RemoteEnv:      r.remoteEnv(w, fullName),
		OverrideConfig: r.override,
		Lockfile:       lock,
		TempDir:        tmp,
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
	return nil
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
// Two checks, each through `devcontainer exec` as the remote user:
//
//   - drydock-probe, one PING over the broker socket — the Feature is
//     installed and the socket is mounted and answering;
//   - git -C <folder> remote -v — the clone is where the container's
//     workspace folder says, and its origin is the plain repository URL the
//     clone left (no credential, no helper: the Feature's is the only one).
//
// The design's third check, `claude --version`, belongs to Phase 5, which
// installs Claude Code; it is not faked here.
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
	return nil
}

func (r *runState) cloneURL(fullName string) string {
	base := r.p.Cloner.BaseURL
	if base == "" {
		base = clone.DefaultBaseURL
	}
	return strings.TrimSuffix(base, "/") + "/" + fullName + ".git"
}

// sessionServer is §6 step 8, and does nothing yet: the supervisor (§8) is
// Phase 5. Like the credential volume, the step stays and says so.
func (r *runState) sessionServer(context.Context, workspace.Workspace) error {
	return workspace.Note("Nothing to do yet: the Claude Code session server arrives with Claude support.")
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
