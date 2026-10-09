// Package container is Drydock's only route to containers: `devcontainer up`
// to make one, and `docker` to find them again by label (design §6).
//
// Two rules from the design shape it. The `up` result is the contract — one
// JSON object on stdout, parsed by classify.ClassifyContainer — and nothing
// scrapes human-oriented output. And containers are found by label, never by
// name or a remembered id: Docker is the truth, the database is the cache, so
// every fact reconciliation needs to rebuild a workspace row is on the
// container as a label.
//
// # Rules and details
//
// `devcontainer up`'s argv is built and validated from workspace data and its
// result read by classify.ClassifyContainer. Containers are found with `docker
// ps -q --filter label=…` for ids and `docker inspect` for structured labels
// and state — never a table parse. The id-labels carry workspace id,
// repository id, repo and branch, so a row can be rebuilt from them.
//
// read-configuration is parsed too (one JSON object; an unparseable
// devcontainer.json exits 0, so "names no image" is checked here), run with
// --include-merged-configuration and an id-label no container carries, so the
// merged configuration is computed from what up will read, never from an old
// container's labels.
//
// HostAccessOf (policy.go) is the **host-access subset**: every field outside
// an allowlist, from the configuration and the merged one (a Feature's
// privileged is its own entry, source feature_or_image), as {field, source,
// value} with canonical JSON values and the clone's path written
// ${localWorkspaceFolder}, hashed by HashSettings (sha256: over the sorted
// entries) — initializeCommand, runArgs, appPort, workspaceMount, Compose,
// build.options, a bind mount or any volume not named with ${devcontainerId},
// privileged, hostRequirements.gpu, capAdd/securityOpt beyond the
// SYS_PTRACE/seccomp=unconfined debugger pair, a Dockerfile or build context
// outside the clone, a non-registry build.cacheFrom (by the guard's parser, so
// it can be approved), and **any field it does not name**. A config file that
// is a symlink or outside the clone is ErrConfigFileOutside, not a setting; a
// Dockerfile, context or bind source written inside the clone that is a link
// out of it is ErrPathEscapes — not approvable, since the guard refuses it
// even approved. DiffSettings is what the operator is shown, and Covered
// decides: every current entry within the approved set — the same canonical
// value, or for capAdd/securityOpt/mounts a list of approved elements; runArgs
// must equal, since its elements are argv and recombine. The hash is only the
// approval request's staleness check.
//
// **Every devcontainer invocation runs docker through the docker guard**
// (guard.go, Manager.Guard): --docker-path and --docker-compose-path name
// GuardDir(folder)/docker (<root>/<id>/.drydock/guard/, beside the clone), a
// link dockerguard.Guard.Prepare remakes before every call to the drydock
// binary, with real-docker beside it linking the docker the Manager itself
// runs; the pair goes among the CLI's options, before exec's --. Up refuses to
// run with no guard (ErrNoGuard) or a TMPDIR other than TempDirFor(folder)
// (<id>/.drydock/tmp), writes the guard's policy.json — the four id-labels,
// Drydock's two --mount values (ownMounts, the same strings Args passes), the
// clone, the TMPDIR, UpSpec.ConfigDir and UpSpec.Approved — removes it after,
// and reads the guard's refused.json before the CLI's result: a refusal is
// *GuardRefusal naming settings from the guard's closed set, never the CLI's
// prose. read-configuration and exec (and the session server's) get the guard
// with no policy, which passes everything they run. SweepPolicy removes a
// policy a killed up left.
//
// A repository's committed devcontainer-lock.json is **honoured**
// (LockfileHonour: no lockfile flag) and one without gets --no-lockfile, so
// none is created — never --frozen-lockfile, which refuses a lockfile a commit
// stale that VS Code would quietly rewrite. With no flag up leaves an in-sync
// lockfile byte for byte and rewrites a stale one to VS Code's bytes, **but
// only because Drydock's Feature has no dependsOn**: the CLI writes an
// injected Feature's dependencies into the lockfile (design §6, "The
// repository's lockfile"; the measured matrix is
// test/fixtures/devcontainer/lockfile-behaviour.txt). A lockfile with an entry
// for Drydock's own Feature, or one that is not a regular file inside the
// clone or does not parse, is refused.
//
// Each up gets its own TMPDIR: the CLI stages Features in a folder named by
// the millisecond, and concurrent creates shared one. --override-config goes
// to exec as well as up — exec reads the config too.
//
// The broker is mounted as the workspace's **directory**, UpSpec.BrokerDir at
// BrokerMountPoint (/run/drydock), so the socket is at
// BrokerSocketInContainer, /run/drydock/broker.sock — the path the Feature
// always used. Not read-only: the CLI's --mount regex takes type, source,
// target and external and nothing else (CLI 0.89.0 refuses ,readonly).
// LegacyBrokerMount (and Found.LegacyBrokerMount in List) reads docker
// inspect's Mounts for a bind at LegacyBrokerMountPoint — the socket file an
// earlier Drydock mounted, which only a rebuild replaces.
//
// Find/Stop/Remove act on the containers carrying one workspace's label (never
// a cached id); every id handed to docker stop/rm must be a full 64-hex id,
// after --; rm is --force --volumes, which takes anonymous volumes and never
// named ones.
//
// RemoveContents is the delete's **cleanup helper** (cleanup.go): docker run
// --rm of CleanupImage (configuration, busybox **pinned by digest** — Validate
// refuses a tag) as root with --network none, --read-only, every capability
// dropped but DAC_OVERRIDE and FOWNER, and one bind mount — <root>/<id> at /w,
// never a parent — running `find /w -mindepth 1 -delete`. It carries
// <prefix>.cleanup=<id>, never <prefix>.workspace, so reconciliation's listing
// never sees one; a stray left by an earlier attempt is removed by label
// before the next runs. Everything before the helper's own docker run that
// fails is ErrCleanupNotRun (an unpinned image also ErrCleanupImage), so the
// delete never says a helper was tried when none ran. ListHelpers is the boot
// sweep's listing: the bare cleanup label, and nothing that also carries the
// workspace label.
//
// BuiltImages(folder) are the names up gives the images it builds, measured on
// CLI 0.89.0: vsc-<basename>-<sha256 of --workspace-folder> and that with
// -features, -uid, -features-uid (nothing labels them, so the exact name is
// the handle); RemoveBuiltImages lists those by exact reference and docker
// image rms them — never --force, never a prune, so Docker itself refuses an
// image a container still uses.
//
// EnsureClaudeVolume (§6 step 4, and the login's first act, so the two never
// disagree) makes the shared credential volume local and labelled, refuses a
// foreign or non-local one, and then runs the **owner helper**
// (volumeowner.go: the pinned busybox as root with only CHOWN, FOWNER and
// DAC_OVERRIDE, --network none, the volume alone, label
// <prefix>.volume-owner): an *empty* volume, whoever owns it, is given to
// ClaudeUID (Drydock's own) 0700 with a marker directory .drydock-volume left
// in it, because Docker copies an image directory's owner into a volume
// whenever it is mounted while empty — and the Feature's /home/vscode/.claude
// carries a non-vscode remote user's *build-time* uid, which gave a fresh
// volume to the wrong uid (measured in test/container). A written volume
// another uid owns is ErrVolumeOwner naming it. The CLI's --mount cannot say
// volume-nocopy. feature/prepare-test-volumes.sh carries the same line for the
// Feature's tests, compared by a Go test.
//
// Address (address.go) is the preview proxy's resolution, per dial: docker ps
// for exactly one *running* container with the workspace's label (none is
// ErrNotRunning, two ErrAmbiguous), then docker inspect of it for an address
// on a Docker network — IPv4 first, networks by name, and never loopback,
// unspecified, link-local or multicast, never the network's gateway, never an
// address this host holds (LocalAddrs, nil = the kernel), and only on a
// network whose driver is bridge (one docker network inspect): macvlan/ipvlan
// with an approved --ip can name the host or the LAN (ErrNoAddress, as is
// --network=host); Confirm inspects the same container again after the connect
// (ErrMoved).
package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/dockerguard"
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
	// runs to remove what the drydock user cannot (cleanup.go), and the
	// image the credential volume's owner helper runs (volumeowner.go).
	CleanupImage string
	// ClaudeUID and ClaudeGID own the shared credential volume: Drydock's
	// own, which every workspace's remote user is given (volumeowner.go).
	ClaudeUID, ClaudeGID int
	// LocalAddrs lists the addresses this host holds, which Address never
	// returns as a container's (PF §10.6). Nil asks the kernel
	// (net.InterfaceAddrs), as production does; a test sets it.
	LocalAddrs func() ([]netip.Addr, error)
	// Guard is the docker guard every devcontainer invocation is given as
	// --docker-path (guard.go). Up refuses to run without one; read-
	// configuration and exec, which create nothing, run without it only
	// when it is nil.
	Guard *dockerguard.Guard
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
	// BrokerDir is the workspace's own token-broker directory on the host
	// (broker.SocketDir: <broker dir>/<id>, holding broker.sock and nothing
	// else), bind-mounted at BrokerMountPoint in this container and
	// no other (§6 step 5, §9.1). A directory rather than the socket file: a
	// file mount pins the socket's inode, which a Drydock restart replaces.
	// Empty mounts nothing.
	BrokerDir string
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
	// Approved is the repository's current host-access approval (§6), which
	// the docker guard holds every docker command of this up to, beside
	// Drydock's own flags. Empty approves nothing.
	Approved []HostSetting
	// ConfigDir is the directory of the configuration file up reads, which
	// an approved relative build context is resolved against.
	ConfigDir string
}

const (
	// BrokerMountPoint is where a workspace's broker directory is mounted
	// inside its container.
	BrokerMountPoint = "/run/drydock"
	// BrokerSocketInContainer is the socket inside it: the path the
	// Feature's DRYDOCK_BROKER_SOCK has always named, so the Feature is the
	// same whichever way the socket arrives.
	BrokerSocketInContainer = BrokerMountPoint + "/broker.sock"
	// LegacyBrokerMountPoint is the target of the file mount a Drydock
	// before the directory mount gave a container: the socket itself, whose
	// inode a restart replaces. A container carrying it has had no broker
	// since the restart that brought this version in, and only a rebuild
	// gives it the directory (Mounts.LegacyBroker).
	LegacyBrokerMountPoint = BrokerSocketInContainer
)

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
	if s.BrokerDir != "" {
		// --mount is comma-separated key=value pairs, so a comma or an equals
		// sign in the path would let it add mount options of its own.
		if !strings.HasPrefix(s.BrokerDir, "/") || strings.ContainsAny(s.BrokerDir, ",=\n") {
			return nil, fmt.Errorf("container: broker directory %q must be absolute and free of ',' and '='", s.BrokerDir)
		}
	}
	if s.ClaudeVolume != "" && !config.ValidVolumeName(s.ClaudeVolume) {
		return nil, fmt.Errorf("container: %q is not a volume name", s.ClaudeVolume)
	}
	args := []string{"up", "--workspace-folder", s.Folder}
	if lock != "" {
		args = append(args, lock)
	}
	for _, l := range m.idLabels(s) {
		args = append(args, "--id-label", l)
	}
	for _, mt := range m.ownMounts(s) {
		args = append(args, "--mount", mt)
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

// idLabels are up's id-labels, key=value: what finds the container again,
// and what reconciliation rebuilds a row from.
func (m Manager) idLabels(s UpSpec) []string {
	return []string{
		m.key(LabelWorkspace) + "=" + s.WorkspaceID,
		m.key(LabelRepositoryID) + "=" + strconv.FormatInt(s.RepositoryID, 10),
		m.key(LabelRepo) + "=" + s.FullName,
		m.key(LabelBranch) + "=" + s.Branch,
	}
}

// ownMounts are the --mount values Drydock gives up: the broker directory
// and the shared credential volume. Args validates both.
func (m Manager) ownMounts(s UpSpec) []string {
	var out []string
	if s.BrokerDir != "" {
		// Not read-only, though nothing in the container needs to write
		// here: the CLI's --mount takes type, source, target and external
		// and nothing else (CLI 0.89.0 refuses the argument). So root in the
		// container can write in this one directory, and the broker treats
		// what it finds there as the container's (broker.Open, Remove).
		out = append(out, "type=bind,source="+s.BrokerDir+",target="+BrokerMountPoint)
	}
	if s.ClaudeVolume != "" {
		out = append(out, "type=volume,source="+s.ClaudeVolume+",target="+ClaudeConfigMountPoint)
	}
	return out
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
	dp, err := m.dockerPath(s.Folder)
	if err != nil {
		return subproc.Result{}, err
	}
	return m.Run.Run(ctx, subproc.Cmd{Name: "devcontainer", Args: withDockerPath(args, dp), Stdout: s.Stdout, Stderr: s.Stderr}), nil
}

// UpStderrTail is how much of `up`'s stderr Up returns: the last MiB.
const UpStderrTail = 1 << 20

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
	// Every up runs through the guard, with a policy written for this run
	// alone and removed after it (guard.go). Without a guard, or without the
	// workspace's own TMPDIR — where the CLI writes what it builds from, and
	// the one place outside the clone the policy lets a build read — it
	// does not run.
	if m.Guard == nil {
		return classify.Container{}, nil, ErrNoGuard
	}
	if s.TempDir != TempDirFor(s.Folder) {
		return classify.Container{}, nil, fmt.Errorf("container: temp dir %q is not the workspace's own, %s", s.TempDir, TempDirFor(s.Folder))
	}
	dir := GuardDir(s.Folder)
	dp, err := m.dockerPath(s.Folder)
	if err != nil {
		return classify.Container{}, nil, err
	}
	if err := dockerguard.ClearRefusal(dir); err != nil {
		return classify.Container{}, nil, fmt.Errorf("container: docker guard: %w", err)
	}
	if err := dockerguard.WritePolicy(dir, m.guardPolicy(s)); err != nil {
		return classify.Container{}, nil, fmt.Errorf("container: docker guard: %w", err)
	}
	defer dockerguard.RemovePolicy(dir)
	var stdout bytes.Buffer
	// The tail, not the head: the lines that say why an up failed come last
	// (tail.go).
	stderr := newTail(UpStderrTail)
	cmd := subproc.Cmd{Name: "devcontainer", Args: withDockerPath(args, dp), Stdout: &stdout, Stderr: stderr,
		Env: withTempDir(os.Environ(), s.TempDir)}
	res := m.Run.Run(ctx, cmd)
	c, cerr := classify.ClassifyContainer(stdout.Bytes())
	// A refusal is read first: the CLI reports it only as a failed docker
	// command, in prose, and the guard's own record is the structured
	// answer. The refused command never reached docker, but an earlier one
	// may have made a container (a failed up can own one, §6), so an id the
	// result carries is kept.
	r, err := dockerguard.ReadRefusal(dir)
	if err != nil {
		return classify.Container{ContainerID: c.ContainerID}, stderr.Bytes(), fmt.Errorf("container: docker guard: reading its refusal: %w", err)
	}
	if r != nil {
		refusal := &GuardRefusal{Settings: r.Settings}
		if len(refusal.Settings) == 0 {
			refusal.Settings = []string{dockerguard.SettingCommand}
		}
		return classify.Container{ContainerID: c.ContainerID}, stderr.Bytes(), refusal
	}
	if res.Err != nil {
		return classify.Container{}, stderr.Bytes(), fmt.Errorf("devcontainer up: %w", res.Err)
	}
	if cerr != nil {
		return classify.Container{}, stderr.Bytes(), cerr
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
	// LegacyBrokerMount: the container has the broker socket bind-mounted
	// as a file, as a Drydock before the directory mount made it, and needs
	// a rebuild (Manager.LegacyBrokerMount).
	LegacyBrokerMount bool
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
	return m.inspect(ctx, strings.Fields(ids.String()))
}

// inspect is `docker inspect` of the given containers, parsed.
func (m Manager) inspect(ctx context.Context, list []string) ([]Found, error) {
	if len(list) == 0 {
		return nil, nil
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"inspect", "--type", "container"}, list...),
		Stdout: &out, Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker inspect", res, &stderr); err != nil {
		// A container removed between the two calls fails the inspect.
		// That is a race, not a fault, and the next reconcile sees the
		// world without it — but this one cannot trust a partial list.
		return nil, err
	}
	return m.parseInspect(out.Bytes())
}

// LegacyBrokerMount reports whether any container carrying the workspace's
// label was created with the broker socket mounted as a file
// (LegacyBrokerMountPoint) by a Drydock before the directory mount. Such a
// container lost its broker at the restart that brought this version in —
// its mount names a socket inode no process listens on — and keeps the
// mount until it is rebuilt: a plain start of it fails, since the file it
// names no longer exists. Read from docker inspect's Mounts, never guessed.
func (m Manager) LegacyBrokerMount(ctx context.Context, workspaceID string) (bool, error) {
	ids, err := m.Find(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	found, err := m.inspect(ctx, ids)
	if err != nil {
		return false, err
	}
	for _, f := range found {
		if f.LegacyBrokerMount {
			return true, nil
		}
	}
	return false, nil
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
	Mounts []struct {
		Type        string
		Destination string
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
		legacy := false
		for _, mt := range c.Mounts {
			legacy = legacy || (mt.Type == "bind" && mt.Destination == LegacyBrokerMountPoint)
		}
		found = append(found, Found{
			ContainerID: c.ID, WorkspaceID: ws, RepositoryID: repoID,
			Repo: c.Config.Labels[m.key(LabelRepo)], Branch: c.Config.Labels[m.key(LabelBranch)],
			Running: c.State.Running, Status: c.State.Status, LegacyBrokerMount: legacy,
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
