package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/subproc"
)

// Source is the two reads §7.3 names, kept apart so the watch can do what the
// classifier's rule order requires: the file first, and `auth status` only
// when the file holds live tokens, so a broken second read never hides the
// tombstone.
type Source interface {
	// Credentials returns $CLAUDE_CONFIG_DIR/.credentials.json from the
	// shared volume. nil with no error means the file — or the volume —
	// does not exist: no one has signed in. An error means it could not be
	// read, which is never the same thing.
	Credentials(ctx context.Context) ([]byte, error)
	// AuthStatus returns `claude auth status --json`'s stdout, run against
	// the shared volume.
	AuthStatus(ctx context.Context) ([]byte, error)
}

// ImageEnsurer is internal/claudeimage's Builder.
type ImageEnsurer interface {
	Ensure(ctx context.Context) (string, error)
}

// Problem names which read failed, for the sentence the operator sees. Never
// the subprocess's stderr: that is not Drydock's to publish, and it is the one
// place a credential byte could ride along.
type Problem string

const (
	ProblemDocker      Problem = "docker"         // the daemon did not answer
	ProblemImage       Problem = "image"          // the Claude image could not be built
	ProblemCredentials Problem = "credentials"    // the file could not be read, or makes no sense
	ProblemAuthStatus  Problem = "auth_status"    // claude auth status did not give a usable answer
	ProblemDisagree    Problem = "disagree"       // the file says live, Claude Code says signed out
	ProblemForeign     Problem = "foreign_volume" // a volume of the name exists, and this Drydock did not make it
	ProblemUnknown     Problem = "unknown"
)

// ReadError is a read that failed, with the problem it is, and a detail for
// the service log that carries no input bytes.
type ReadError struct {
	Problem Problem
	Detail  string
}

func (e *ReadError) Error() string { return "identity: " + string(e.Problem) + ": " + e.Detail }

// Mount is where the volume appears in the helper container, and what
// CLAUDE_CONFIG_DIR is set to there.
const Mount = "/claude"

// LabelIdentity is the label a helper carries, valued "1". Deliberately not
// <prefix>.workspace: reconciliation lists by that label, so a helper is never
// adopted or given a row (the cleanup helper's reasoning, internal/container).
const LabelIdentity = "identity"

// LabelVolume is the label, under the prefix, that §6 step 4 puts on the
// shared credential volume when it makes it — internal/container's
// LabelClaudeConfig itself, so the writer and the reader cannot drift. A volume of the
// configured name without it was made by something else — another Drydock
// with its own prefix, or a hand — and step 4 refuses to mount it, so the
// watch refuses to read it: a login no workspace runs on is not the fleet's.
const LabelVolume = container.LabelClaudeConfig

// maxRead bounds what Drydock reads from either helper. A credential file is a
// few hundred bytes; anything past this is not one.
const maxRead = 64 << 10

// credentialsScript is the one shell line the file read runs, inside the
// helper. It is a constant: nothing from configuration or the volume is
// interpolated. Exit 3 is "no file", which is the only way absent is ever
// reported; any other failure of cat is an error, never absent.
const credentialsScript = `f=` + Mount + `/.credentials.json; if [ -e "$f" ] || [ -L "$f" ]; then exec cat -- "$f"; fi; exit 3`

const exitAbsent = 3

var (
	imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	pinnedPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)
)

// DockerSource reads the volume through a short-lived container.
//
// Why a container, and not the volume's files from the host: a Docker
// volume's data lives under the daemon's own directory, root-owned and
// private to it, and its path there is the daemon's implementation detail —
// under DinD or rootless Docker it is not even on this filesystem. Drydock
// reaches it the way every workspace does, by mounting it. And `claude auth
// status` needs Claude Code, which the host need not have.
//
// The helper is as small as the job: the volume mounted read-only, so the
// watch can never write the credential, take the refresh lock, or touch the
// `.claude.json` the containers depend on; no network; a read-only root
// filesystem with a tmpfs for HOME; and of root's capabilities only
// DAC_READ_SEARCH, because the file is 0600 and owned by the containers' user
// — that capability reads past permissions and writes nothing.
//
// The file is read with FileImage — busybox, pinned by digest, the cleanup
// helper's image — and not with the Claude image, so blanked and absent can
// be told even when the Claude image cannot be built (its first build needs
// the network): the two verdicts that matter most depend on the least.
type DockerSource struct {
	Run         subproc.Runner
	Image       ImageEnsurer
	FileImage   string
	Volume      string
	LabelPrefix string
}

// Credentials implements Source.
func (d DockerSource) Credentials(ctx context.Context) ([]byte, error) {
	exists, err := d.volumeExists(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		// No volume: nothing has ever run that would create it, so no one
		// has signed in. Checked first because `docker run -v` would
		// create the volume as a side effect of looking at it.
		return nil, nil
	}
	args, err := d.RunArgs(d.FileImage, "sh", "-c", credentialsScript)
	if err != nil {
		return nil, err
	}
	out, code, err := d.run(ctx, args)
	if err != nil {
		return nil, err
	}
	switch code {
	case 0:
		// An empty file is a file: never nil, which would read as absent
		// (the classifier refuses an empty one).
		if out == nil {
			out = []byte{}
		}
		return out, nil
	case exitAbsent:
		return nil, nil
	}
	return nil, &ReadError{Problem: ProblemCredentials, Detail: fmt.Sprintf("reading the credential file exited %d", code)}
}

// AuthStatus implements Source. `auth status` exits 1 when it reports
// loggedIn:false, so 0 and 1 are both answers; the classifier decides what
// the bytes mean.
func (d DockerSource) AuthStatus(ctx context.Context) ([]byte, error) {
	img, err := d.Image.Ensure(ctx)
	if err != nil {
		return nil, &ReadError{Problem: ProblemImage, Detail: err.Error()}
	}
	args, err := d.RunArgs(img, "claude", "auth", "status", "--json")
	if err != nil {
		return nil, err
	}
	out, code, err := d.run(ctx, args)
	if err != nil {
		return nil, err
	}
	if code != 0 && code != 1 {
		return nil, &ReadError{Problem: ProblemAuthStatus, Detail: fmt.Sprintf("claude auth status exited %d", code)}
	}
	return out, nil
}

// RunArgs is the helper's `docker run` argv, exported so it can be asserted
// on directly: it mounts the login every workspace runs on.
func (d DockerSource) RunArgs(image string, entrypoint string, args ...string) ([]string, error) {
	if !config.ValidVolumeName(d.Volume) {
		return nil, fmt.Errorf("identity: %q is not a volume name", d.Volume)
	}
	if !imageIDPattern.MatchString(image) && !pinnedPattern.MatchString(image) {
		return nil, fmt.Errorf("identity: %q is neither an image id nor a reference pinned by digest", image)
	}
	if d.LabelPrefix == "" {
		return nil, errors.New("identity: no label prefix")
	}
	out := []string{"run", "--rm",
		"--label", d.LabelPrefix + "." + LabelIdentity + "=1",
		"--network", "none",
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=16m",
		"--cap-drop", "ALL", "--cap-add", "DAC_READ_SEARCH",
		"--security-opt", "no-new-privileges",
		"--user", "0:0",
		"--mount", "type=volume,source=" + d.Volume + ",target=" + Mount + ",readonly",
		// The whole environment, replaced: none of §2.1's variables can
		// reach it, and the auto-updater is off as in the Feature.
		"--env", "CLAUDE_CONFIG_DIR=" + Mount,
		"--env", "HOME=/tmp",
		"--env", "DISABLE_AUTOUPDATER=1",
		"--entrypoint", entrypoint,
		image,
	}
	return append(out, args...), nil
}

func (d DockerSource) volumeExists(ctx context.Context) (bool, error) {
	if !config.ValidVolumeName(d.Volume) {
		return false, fmt.Errorf("identity: %q is not a volume name", d.Volume)
	}
	// The name filter is a substring match, so the answer is the exact
	// line, compared here.
	out, code, err := d.run(ctx, []string{"volume", "ls", "--quiet", "--filter", "name=" + d.Volume})
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, &ReadError{Problem: ProblemDocker, Detail: fmt.Sprintf("docker volume ls exited %d", code)}
	}
	found := false
	for _, name := range strings.Fields(string(out)) {
		if name == d.Volume {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	return true, d.checkLabel(ctx)
}

// checkLabel refuses a volume this Drydock did not make (LabelVolume). Read
// as JSON, never as a table: a label's value can hold anything.
func (d DockerSource) checkLabel(ctx context.Context) error {
	if d.LabelPrefix == "" {
		return errors.New("identity: no label prefix")
	}
	out, code, err := d.run(ctx, []string{"volume", "inspect", "--format", "{{json .Labels}}", "--", d.Volume})
	if err != nil {
		return err
	}
	if code != 0 {
		return &ReadError{Problem: ProblemDocker, Detail: fmt.Sprintf("docker volume inspect exited %d", code)}
	}
	var labels map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(out), &labels); err != nil {
		return &ReadError{Problem: ProblemDocker, Detail: "docker volume inspect: the labels are not a JSON object"}
	}
	key := d.LabelPrefix + "." + LabelVolume
	if _, ok := labels[key]; !ok {
		return &ReadError{Problem: ProblemForeign, Detail: fmt.Sprintf("volume %q has no %s label", d.Volume, key)}
	}
	return nil
}

// run executes docker and returns stdout, capped. stderr is read and dropped:
// a failure is described by its exit code alone.
func (d DockerSource) run(ctx context.Context, args []string) ([]byte, int, error) {
	var out bytes.Buffer
	lim := &limited{buf: &out, max: maxRead}
	res := d.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: args, Stdout: lim, Stderr: discard{}})
	if res.Err != nil {
		return nil, 0, &ReadError{Problem: ProblemDocker, Detail: "docker could not be run: " + res.Err.Error()}
	}
	if lim.over {
		return nil, 0, &ReadError{Problem: ProblemCredentials, Detail: fmt.Sprintf("a read returned more than %d bytes", maxRead)}
	}
	return out.Bytes(), res.ExitCode, nil
}

type limited struct {
	buf  *bytes.Buffer
	max  int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		l.over = true
		return len(p), nil
	}
	return l.buf.Write(p)
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
