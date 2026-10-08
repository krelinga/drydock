package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/clone"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/dockerguard"
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
	// approved is the repository's host-access approval as step 3 read it,
	// and configDir the directory of the configuration it checked: up's
	// docker guard holds every docker command to them (design §6, "The
	// docker guard").
	approved  []container.HostSetting
	configDir string
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
//
// The configuration is then checked for host access (design §6, "What a
// configuration may ask of the host"): the clone is the container's to write,
// so the file read here may be the container's work, and `up` would act on
// it on the host. A refusal fails this step, before any `up`. And so that
// what is checked is what `up` reads, the workspace's containers are stopped
// first: a rebuild, or a start from failed, otherwise runs with the old
// container still up, free to rewrite the file between the check and `up`.
// Each of those runs passes --remove-existing-container, so the stop loses
// nothing; a start from stopped finds its container stopped already.
func (r *runState) resolveConfig(ctx context.Context, w workspace.Workspace) error {
	ids, err := r.p.Containers.Find(ctx, w.ID)
	if err == nil {
		err = r.p.Containers.Stop(ctx, ids)
	}
	if err != nil {
		return workspace.Public("Drydock could not stop the workspace's container before reading its configuration.", err)
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
		r.p.logTail(ctx, w.ID, "read-configuration", []byte(readErr.Stderr))
		return workspace.Public("devcontainer could not read the dev container configuration.", err)
	case err != nil:
		return workspace.Public("Drydock could not read devcontainer's answer about the configuration.", err)
	}
	// Drydock's own configuration, beside the clone, is checked too, but
	// its paths are Drydock's.
	ha, err := container.HostAccessOf(c, w.HostPath, r.override == "")
	switch {
	case errors.Is(err, container.ErrPathEscapes):
		return workspace.Public("devcontainer.json names a Dockerfile, build context or bind mount inside the clone that is a symbolic link leading outside it. Drydock does not run it, and it cannot be approved: replace the link with what it should hold.", err)
	case errors.Is(err, container.ErrConfigFileOutside):
		return workspace.Public("The repository's devcontainer.json is a symbolic link or not inside the clone, so Drydock does not read it.", err)
	case err != nil:
		return workspace.Public("Drydock could not check what the dev container configuration asks of the host.", err)
	}
	// The approval is read whether or not this configuration asks for
	// anything: up's docker guard is held to it, since what `up` asks
	// docker for may not be what read-configuration showed (§6, "The
	// docker guard").
	approved, ok, err := r.p.Workspaces.Approved(ctx, w.RepositoryID)
	if err != nil {
		return workspace.Public("Drydock could not read the repository's host-access approval.", err)
	}
	var granted []container.HostSetting
	if ok {
		if err := json.Unmarshal(approved.Settings, &granted); err != nil {
			return workspace.Public("Drydock could not read the repository's host-access approval.", err)
		}
	}
	r.approved = granted
	r.configDir = filepath.Dir(c.ConfigFile)
	if r.override != "" {
		r.configDir = filepath.Dir(r.override)
	}
	approvedNote := ""
	if !ha.Empty() {
		// Within what was approved — the same, or less — runs; anything
		// new or changed asks (design §6). The approval itself stays as it
		// was: running less does not narrow it.
		if !ok || !container.Covered(granted, ha.Settings) {
			return r.needsApproval(ha, approved, ok)
		}
		approvedNote = "It runs with host access the operator approved for this repository: " +
			strings.Join(container.FieldNames(ha.Settings), ", ") + "."
	}
	r.folder = c.WorkspaceFolder
	r.lockfile, r.lockPath = container.LockfileIgnore, ""
	if r.override != "" {
		return joinNotes("The repository has no devcontainer.json, so it gets Drydock's minimal configuration.", approvedNote)
	}
	err = r.resolveLockfile(w, c.ConfigFile)
	if n, ok := workspace.IsNote(err); ok || err == nil {
		return joinNotes(n, approvedNote)
	}
	return err
}

// needsApproval is step 3's stop for a host-access subset the operator has
// not approved for the repository (design §6): the request, and a sentence
// naming what is new or changed.
func (r *runState) needsApproval(ha container.HostAccess, approved workspace.Approval, had bool) error {
	var before []container.HostSetting
	if had {
		json.Unmarshal(approved.Settings, &before)
	}
	added, changed, removed := container.DiffSettings(before, ha.Settings)
	enc := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	settings := enc(ha.Settings)
	var asked []container.HostSetting
	asked = append(asked, added...)
	for _, c := range changed {
		asked = append(asked, container.HostSetting{Field: c.Field, Source: c.Source})
	}
	names := container.FieldNames(asked)
	if len(names) > 8 {
		names = append(names[:8], fmt.Sprintf("%d more", len(names)-8))
	}
	what := "This configuration asks for host access that has not been approved for this repository"
	if had {
		what = "This configuration's host access differs from what was approved for this repository"
	}
	sentence := what + ". Approve it to continue, or remove it from devcontainer.json."
	if len(names) > 0 {
		sentence = what + ": " + strings.Join(names, ", ") + ". Approve it to continue, or remove it from devcontainer.json."
	}
	return workspace.NeedsApproval(sentence, workspace.PendingApproval{
		Hash: ha.Hash, Settings: settings,
		Added: enc(added), Changed: enc(changed), Removed: enc(removed),
		RemoveExisting: r.removeExisting,
	})
}

func joinNotes(a, b string) error {
	switch {
	case a == "" && b == "":
		return nil
	case a == "":
		return workspace.Note(b)
	case b == "":
		return workspace.Note(a)
	}
	return workspace.Note(a + " " + b)
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
// start and rebuild and creates nothing once the volume is there; it never removes
// a volume, gives an empty one to Drydock's uid and marks it so no image's
// directory decides its owner, and refuses one this Drydock did not make,
// that is not a plain local volume, or that another uid has written to
// (container.EnsureClaudeVolume) — Claude Code's
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
	case errors.Is(err, container.ErrVolumeOwner):
		owner := "another uid"
		var oe *container.VolumeOwnerError
		if errors.As(err, &oe) {
			owner = oe.OwnerName()
		}
		return workspace.Public(fmt.Sprintf("The shared Claude credential volume %s holds files that belong to %s, not to Drydock's uid, so no workspace could read a login written there. Drydock does not re-own a login on its own; see the deployment runbook.", name, owner), err)
	case errors.Is(err, container.ErrVolumeNotLocal):
		return workspace.Public(fmt.Sprintf("The shared Claude credential volume %s is not a plain local Docker volume. It must be: Claude Code's refresh lock is not safe on a network filesystem.", name), err)
	case err != nil:
		return workspace.Public("Drydock could not create or check the shared Claude credential volume.", err)
	case created:
		return workspace.Note(fmt.Sprintf("Created the shared Claude credential volume %s.", name))
	}
	return workspace.Note(fmt.Sprintf("The shared Claude credential volume %s is there.", name))
}

// errLegacyMount is the journal's half of LegacyMountSentence.
var errLegacyMount = errors.New("the container has the broker socket bind-mounted as a file (" +
	container.LegacyBrokerMountPoint + "); only a rebuild replaces the mount")

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
	// A start reuses the existing container, mounts and all. One an earlier
	// Drydock made has the broker socket mounted as a file that no longer
	// exists, and docker would refuse it with a message about a bind source;
	// say what it is and what fixes it instead. A rebuild replaces the
	// container, so it needs no look.
	if !r.removeExisting && r.p.Broker != nil {
		legacy, err := r.p.Containers.LegacyBrokerMount(ctx, w.ID)
		if err != nil {
			return workspace.Public("Drydock could not inspect the workspace's existing container.", err)
		}
		if legacy {
			return workspace.Public(LegacyMountSentence, errLegacyMount)
		}
	}
	before := snapshotLockfile(r.lockPath)
	// A TMPDIR of the workspace's own, beside the clone: the CLI stages
	// Features under $TMPDIR in a folder named by the millisecond, which
	// concurrent creates otherwise share (container.UpSpec.TempDir).
	tmp := container.TempDirFor(w.HostPath)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return workspace.Public("Drydock could not create the workspace's temporary directory.", err)
	}
	defer os.RemoveAll(tmp)
	r.p.dropBuildLog(w.ID) // this run's build replaces the last one's
	res, stderr, err := r.p.Containers.Up(ctx, container.UpSpec{
		WorkspaceID: w.ID, RepositoryID: w.RepositoryID, FullName: fullName, Branch: w.Branch,
		Folder:         w.HostPath,
		BrokerDir:      r.p.Broker.SocketDir(w.ID),
		ClaudeVolume:   r.p.ClaudeVolume,
		Features:       map[string]map[string]any{r.p.Feature: r.p.FeatureOptions},
		RemoteEnv:      r.remoteEnv(w, fullName),
		OverrideConfig: r.override,
		Lockfile:       r.lockfile,
		TempDir:        tmp,
		Rebuild:        r.removeExisting,
		Approved:       r.approved,
		ConfigDir:      r.configDir,
	})
	if res.ContainerID != "" {
		if err := r.p.Workspaces.SetContainer(context.WithoutCancel(ctx), w.ID, res.ContainerID); err != nil {
			return workspace.Public("Drydock could not record the workspace's container.", err)
		}
	}
	var refused *container.GuardRefusal
	if errors.As(err, &refused) {
		r.p.logTail(w.ID, "devcontainer up", stderr)
		return workspace.Public(GuardRefusalSentence(refused.Settings), err)
	}
	if err != nil {
		r.p.logTail(ctx, w.ID, "devcontainer up", stderr)
		// A timeout or an unreadable result is a failed build too, and its
		// output is what says where it stopped.
		r.p.keepBuildLog(ctx, w.ID, stderr)
		return workspace.Public("Drydock could not run devcontainer up, or could not read its result. "+
			"Until Drydock restarts, the workspace page shows the output it printed.", err)
	}
	if res.Outcome != classify.ContainerRunning {
		r.p.logTail(ctx, w.ID, "devcontainer up", stderr)
		r.p.keepBuildLog(ctx, w.ID, stderr)
		// The CLI's message can quote the repository's own commands, so it
		// goes to the service log and the held build log, not into the
		// detail; the detail is §12's sentence for what the Feature's own
		// lines say happened (messages.go).
		return workspace.Public(upFailure(stderr),
			fmt.Errorf("devcontainer up: %s %s", res.Message, res.Description))
	}
	return lockfileChange(before, w.HostPath)
}

// GuardRefusalSentence is step 6's sentence when the docker guard refused
// a docker command of `up` (design §6, "The docker guard"): distinct from an
// up that failed, because nothing ran. It names the settings from the
// guard's closed set — never a value, which the configuration chose — and
// says why a start may not repeat it: step 3 reads the configuration again.
func GuardRefusalSentence(settings []string) string {
	var names []string
	for _, s := range settings {
		switch s {
		case dockerguard.SettingNoPolicy:
			return "Drydock's docker guard had no record of what this run may ask Docker for, so it created no container. Start it again; if it repeats, the service log says why."
		case dockerguard.SettingCommand:
			names = append(names, "a docker command Drydock does not recognise")
		case dockerguard.SettingExecOption:
			names = append(names, "a docker exec option Drydock does not recognise")
		case dockerguard.SettingStartUnread:
			names = append(names, "an existing container whose settings Drydock could not read")
		default:
			names = append(names, s)
		}
	}
	return "devcontainer up asked Docker for host access the operator has not approved for this repository: " +
		strings.Join(names, ", ") + ". Drydock refused it, and no container was created. A Feature or image tag may have " +
		"changed since the configuration was checked; starting again checks it again."
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
		r.p.logTail(ctx, w.ID, "probe", []byte(out))
		return workspace.Public(NoBrokerSentence, err)
	}
	out, err := exec("git", "-C", r.folder, "remote", "-v")
	if err != nil {
		r.p.logTail(ctx, w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: git found no clone at the workspace folder.", err)
	}
	want := "origin\t" + r.cloneURL(fullName) + " (fetch)"
	if !strings.Contains(out, want) {
		r.p.logTail(ctx, w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: the clone's origin is not the repository.",
			fmt.Errorf("git remote -v has no %q", want))
	}
	out, err = exec("claude", "--version")
	if err != nil {
		r.p.logTail(ctx, w.ID, "probe", []byte(out))
		return workspace.Public("The probe inside the container failed: Claude Code did not run.", err)
	}
	if v := r.p.ClaudeCodeVersion; v != "" && !strings.HasPrefix(out, v+" ") {
		r.p.logTail(ctx, w.ID, "probe", []byte(out))
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
