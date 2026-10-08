package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/krelinga/drydock/internal/subproc"
)

// Configuration is what Drydock reads from `devcontainer read-configuration`
// (design §6 step 3): enough to know the configuration can be built, and
// where the clone appears inside the container.
type Configuration struct {
	// WorkspaceFolder is the clone's path inside the container — measured
	// on CLI 0.89.0 as /workspaces/<basename of the host folder>, so
	// /workspaces/repo for every workspace unless the repository's own
	// config sets workspaceFolder. The probe (§6 step 7) runs git there.
	WorkspaceFolder string
	// ConfigFile is the file the CLI says it read. With --override-config
	// it still names the repository's default path, measured, so it says
	// where the config would be rather than where it came from.
	ConfigFile string
	// Own is the configuration itself, as the CLI resolved it (comments
	// stripped, ${localEnv:…} and ${localWorkspaceFolder} substituted), and
	// Merged is that configuration merged with what its Features and image
	// metadata declare (--include-merged-configuration). HostAccessOf
	// reads both; Merged is nil in a result read without the flag.
	Own, Merged map[string]json.RawMessage
}

// labelReadConfiguration is the id-label read-configuration is given, under
// the prefix: a label no container carries (ReadConfiguration).
const labelReadConfiguration = "read-configuration"

// ErrUnbuildable is a configuration that names nothing to build or run.
//
// Measured on CLI 0.89.0: a devcontainer.json that does not parse — a
// truncated `{"image": `, say — is not an error. read-configuration exits 0
// with a `configuration` holding nothing but configFilePath, and only `up`
// fails later with "No image information specified". So "the CLI accepted
// it" is not "it is a config"; Drydock checks for an image, a Dockerfile or
// a Compose file itself, and fails this step rather than the long one.
var ErrUnbuildable = errors.New("devcontainer read-configuration: the configuration names no image, Dockerfile or Compose file")

// ReadError is read-configuration exiting non-zero. Measured on CLI 0.89.0,
// a folder with no devcontainer.json exits 1 with an empty stdout and no
// message beyond the CLI's version banner — indistinguishable by output from
// any other failure. So the caller decides "no config" by looking for the
// file, never by reading this error; see provision's resolve step.
type ReadError struct {
	ExitCode int
	// Stderr is the CLI's log, which can quote the repository's config.
	// It is for the service log, never the event log.
	Stderr string
}

func (e *ReadError) Error() string {
	return fmt.Sprintf("devcontainer read-configuration exited %d: %s", e.ExitCode, e.Stderr)
}

// readResult is the shape read-configuration prints on stdout (0.89.0): one
// JSON object; logs go to stderr.
type readResult struct {
	Configuration map[string]json.RawMessage `json:"configuration"`
	Merged        map[string]json.RawMessage `json:"mergedConfiguration"`
	Workspace     *struct {
		WorkspaceFolder string `json:"workspaceFolder"`
	} `json:"workspace"`
}

// ReadConfiguration runs `devcontainer read-configuration` for a host folder,
// with overrideConfig as --override-config when it is not empty.
//
// It asks for the merged configuration — the folder's own plus what its
// Features and image metadata declare — because a Feature can ask for
// privileged and mounts as well as the repository can (HostAccessOf).
// And it passes an id-label no container carries: with none, the CLI looks
// for a container by its default labels, and from a container it found it
// would read the merged metadata off that container's labels rather than
// compute it from the configuration that the next `up` will use.
//
// Measured on CLI 0.89.0, read-configuration does not run initializeCommand,
// with or without the merged configuration: the recording of
// read-configuration-merged-hostile.json writes a host canary from
// initializeCommand and checks it is absent. It does fetch the Features'
// metadata and the image's, from their registries, so it needs the network.
func (m Manager) ReadConfiguration(ctx context.Context, folder, overrideConfig string) (Configuration, error) {
	if !strings.HasPrefix(folder, "/") {
		return Configuration{}, fmt.Errorf("container: folder %q must be absolute", folder)
	}
	args := []string{"read-configuration", "--workspace-folder", folder,
		"--include-merged-configuration", "--id-label", m.key(labelReadConfiguration) + "=none"}
	if overrideConfig != "" {
		if !strings.HasPrefix(overrideConfig, "/") {
			return Configuration{}, fmt.Errorf("container: override config %q must be absolute", overrideConfig)
		}
		args = append(args, "--override-config", overrideConfig)
	}
	// Through the guard, with no policy: read-configuration creates nothing
	// (measured: ps and inspect), and should a later CLI make it build, the
	// guard refuses rather than run that unchecked.
	dp, err := m.dockerPath(folder)
	if err != nil {
		return Configuration{}, err
	}
	var stdout, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "devcontainer", Args: withDockerPath(args, dp),
		Stdout: limit(&stdout, 4<<20), Stderr: limit(&stderr, 64<<10)})
	if res.Err != nil {
		return Configuration{}, fmt.Errorf("devcontainer read-configuration: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return Configuration{}, &ReadError{ExitCode: res.ExitCode, Stderr: strings.TrimSpace(stderr.String())}
	}
	return ParseConfiguration(stdout.Bytes())
}

// ParseConfiguration reads read-configuration's stdout. Exactly one JSON
// object, with a configuration and an absolute workspace folder; anything
// else is the contract moving, and an error rather than a guess.
func ParseConfiguration(b []byte) (Configuration, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return Configuration{}, errors.New("devcontainer read-configuration: empty result on stdout")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var r readResult
	if err := dec.Decode(&r); err != nil {
		return Configuration{}, fmt.Errorf("devcontainer read-configuration: result is not a JSON object: %w", err)
	}
	if dec.InputOffset() != int64(len(trimmed)) {
		return Configuration{}, errors.New("devcontainer read-configuration: trailing data after the result")
	}
	if r.Configuration == nil || r.Workspace == nil || !strings.HasPrefix(r.Workspace.WorkspaceFolder, "/") {
		return Configuration{}, errors.New("devcontainer read-configuration: the result lacks a configuration or an absolute workspace folder")
	}
	c := Configuration{WorkspaceFolder: r.Workspace.WorkspaceFolder, Own: r.Configuration, Merged: r.Merged}
	var path struct {
		FSPath string `json:"fsPath"`
	}
	if raw, ok := r.Configuration["configFilePath"]; ok {
		json.Unmarshal(raw, &path)
		c.ConfigFile = path.FSPath
	}
	if !buildable(r.Configuration) {
		return c, ErrUnbuildable
	}
	return c, nil
}

// buildable: an image, a Dockerfile (either spelling), or a Compose file.
func buildable(cfg map[string]json.RawMessage) bool {
	str := func(raw json.RawMessage) bool {
		var s string
		return json.Unmarshal(raw, &s) == nil && s != ""
	}
	if str(cfg["image"]) || str(cfg["dockerFile"]) {
		return true
	}
	if raw, ok := cfg["build"]; ok {
		var b struct {
			Dockerfile string `json:"dockerfile"`
		}
		if json.Unmarshal(raw, &b) == nil && b.Dockerfile != "" {
			return true
		}
	}
	if raw, ok := cfg["dockerComposeFile"]; ok {
		var list []string
		if str(raw) || (json.Unmarshal(raw, &list) == nil && len(list) > 0) {
			return true
		}
	}
	return false
}
