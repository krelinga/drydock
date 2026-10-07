package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/subproc"
)

// ClaudeConfigMountPoint is where the shared Claude credential volume (design
// §7.1) appears in every workspace's container. It is the Feature's
// CLAUDE_CONFIG_DIR, which a Feature's containerEnv cannot take from an
// option, so it is a constant paired with the Feature's as BrokerMountPoint
// is with DRYDOCK_BROKER_SOCK. The Feature's postCreateCommand refuses a
// container in which this is not the one mount at CLAUDE_CONFIG_DIR, so the
// two cannot drift apart quietly.
const ClaudeConfigMountPoint = "/home/vscode/.claude"

// LabelClaudeConfig marks, under the prefix, the shared credential volume this
// Drydock made. The identity watch reads only a volume carrying it
// (identity.LabelVolume is this constant). A volume of that name without it was made by something else —
// another Drydock with its own prefix, or a hand — and is refused rather than
// shared: every workspace's login lives in it.
const LabelClaudeConfig = "claude-config"

var (
	// ErrForeignVolume: a volume of the configured name exists, and this
	// Drydock did not make it.
	ErrForeignVolume = errors.New("container: the credential volume exists but was not made by this Drydock")
	// ErrVolumeNotLocal: the volume is not a plain local Docker volume. Its
	// driver is not "local", or it has driver options, which on the local
	// driver mean it mounts something else — an NFS or CIFS export, say.
	// Claude Code's refresh lock is a mkdir(2) lock inside this volume, and
	// a network filesystem does not make mkdir atomic (Spike 00).
	ErrVolumeNotLocal = errors.New("container: the credential volume is not a plain local Docker volume")
)

// EnsureClaudeVolume makes the shared credential volume if it is absent —
// local driver, labelled with this prefix — and checks the one it finds or
// makes: refused if foreign, or not local. Then it gives an empty volume to
// Drydock's uid and marks it, so no image's directory ever decides its owner
// (volumeowner.go), and refuses one another uid has written to
// (ErrVolumeOwner). created says whether this call made it. Safe to run concurrently and repeatedly: `docker volume create` of
// a name that exists is a no-op, and the check after it reads what is there.
//
// The volume is never removed by Drydock (Remove leaves named volumes): it
// holds the one login every workspace shares.
func (m Manager) EnsureClaudeVolume(ctx context.Context, name string) (created bool, err error) {
	if !config.ValidVolumeName(name) {
		return false, fmt.Errorf("container: %q is not a volume name", name)
	}
	exists, err := m.volumeExists(ctx, name)
	if err != nil {
		return false, err
	}
	if !exists {
		var stderr bytes.Buffer
		res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
			Args: []string{"volume", "create", "--driver", "local",
				"--label", m.key(LabelClaudeConfig) + "=true", "--", name},
			Stdout: limit(&bytes.Buffer{}, 64<<10), Stderr: limit(&stderr, 64<<10)})
		if err := failed("docker volume create", res, &stderr); err != nil {
			return false, err
		}
		created = true
	}
	if err := m.checkClaudeVolume(ctx, name); err != nil {
		return created, err
	}
	return created, m.ensureOwner(ctx, name)
}

// volumeExists lists volume names — one per line, no table — and compares
// them exactly: docker's name filter matches substrings.
func (m Manager) volumeExists(ctx context.Context, name string) (bool, error) {
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:   []string{"volume", "ls", "--quiet", "--filter", "name=" + name},
		Stdout: limit(&out, 1<<20), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker volume ls", res, &stderr); err != nil {
		return false, err
	}
	for _, n := range strings.Fields(out.String()) {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

type volumeInspect struct {
	Name    string
	Driver  string
	Labels  map[string]string
	Options map[string]string
}

func (m Manager) checkClaudeVolume(ctx context.Context, name string) error {
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: []string{"volume", "inspect", "--", name},
		Stdout: limit(&out, 1<<20), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker volume inspect", res, &stderr); err != nil {
		return err
	}
	var vs []volumeInspect
	if err := json.Unmarshal(out.Bytes(), &vs); err != nil {
		return fmt.Errorf("docker volume inspect: %w", err)
	}
	if len(vs) != 1 || vs[0].Name != name {
		return fmt.Errorf("docker volume inspect: asked for %q, got %d volumes", name, len(vs))
	}
	v := vs[0]
	if _, ok := v.Labels[m.key(LabelClaudeConfig)]; !ok {
		return fmt.Errorf("%w: %q has no %s label", ErrForeignVolume, name, m.key(LabelClaudeConfig))
	}
	if v.Driver != "local" {
		return fmt.Errorf("%w: %q uses the %q driver", ErrVolumeNotLocal, name, v.Driver)
	}
	if len(v.Options) > 0 {
		keys := make([]string, 0, len(v.Options))
		for k := range v.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("%w: %q has driver options %v (type %q)", ErrVolumeNotLocal, name, keys, v.Options["type"])
	}
	return nil
}
