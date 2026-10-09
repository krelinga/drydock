package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/ephemeral"
	"github.com/krelinga/drydock/internal/subproc"
)

// Who owns the shared credential volume (design §7.1, "Who owns it").
//
// Drydock's own uid does, and Drydock sets it before anything else touches the
// volume. Every workspace's remote user has that uid — the dev container CLI,
// run by Drydock, updates the remote user's uid to its own — and the login
// handshake writes the credential as it (internal/login), so the 0600 files
// on the volume are readable by every workspace that can run a session at all.
//
// It used to be the first workspace's. Docker copies the image's directory —
// its owner and mode — into an empty volume each time one is mounted over it,
// and the Feature's /home/vscode/.claude carries the remote user's
// *build-time* uid, which the CLI's uid update does not change unless the
// remote user's home is /home/vscode. So the first workspace whose remote
// user was not vscode (node, say) gave the empty volume uid 1000, and every
// later create failed the Feature's preflight and every login was refused,
// until someone removed the volume by hand. Two things close that:
//
//   - The owner helper gives an empty volume to Drydock's uid, whatever owns
//     it now: an empty volume holds no login anyone could lose.
//   - It leaves a marker directory in it, so the volume is never empty again
//     and Docker never copies an image's owner into it. (The CLI's --mount
//     takes no volume-nocopy: it accepts only type, source, target and
//     external — read off CLI 0.89.0's own pattern.)
//
// A volume that holds anything but the marker and belongs to another uid is
// refused with both uids named, and nothing is changed: its files were
// written by that uid, and re-owning a login is not a repair Drydock makes on
// its own.

// VolumeMarker is the directory the owner helper leaves in the volume.
const VolumeMarker = ".drydock-volume"

// ownerMount is where the owner helper sees the volume.
const ownerMount = "/claude"

// ownerScript is the one shell line the owner helper runs, as root with
// CHOWN, FOWNER and DAC_OVERRIDE only — the last because the volume's root is
// 0700 and another uid's, and root needs it to list and to make the marker
// there. Constant: the uid and gid arrive as validated integers in the
// environment. Exit 4 prints the owner: a volume with a login another uid
// wrote. The marker is made with mkdir, which never follows a symlink a
// workspace might have put at its name, and a concurrent helper's marker is
// as good as this one's.
const ownerScript = `d=` + ownerMount + `; m=$d/` + VolumeMarker + `; ` +
	`o=$(stat -c %u "$d") || exit 5; ` +
	`if [ "$o" != "$DRYDOCK_UID" ]; then ` +
	`if [ -n "$(ls -A "$d" | grep -vxF ` + VolumeMarker + `)" ]; then echo "$o"; exit 4; fi; ` +
	`chown "$DRYDOCK_UID:$DRYDOCK_GID" "$d" && chmod 0700 "$d" || exit 5; fi; ` +
	`if [ ! -e "$m" ] && [ ! -L "$m" ]; then mkdir -m 0700 "$m" 2>/dev/null && chown -h "$DRYDOCK_UID:$DRYDOCK_GID" "$m"; fi; ` +
	`if [ -e "$m" ] || [ -L "$m" ]; then exit 0; fi; exit 5`

const exitForeignOwner = 4

// ErrVolumeOwner: the volume holds files and belongs to a uid that is not
// Drydock's. The error is a *VolumeOwnerError.
var ErrVolumeOwner = errors.New("container: the credential volume belongs to another uid")

// VolumeOwnerError says whose the volume is.
type VolumeOwnerError struct {
	// Owner is the volume's owner as the helper printed it: a uid, or ""
	// when it printed something that is not one.
	Owner string
	// UID is Drydock's.
	UID int
}

func (e *VolumeOwnerError) Error() string {
	return fmt.Sprintf("%v: it belongs to %s, and Drydock runs as uid %d", ErrVolumeOwner, e.OwnerName(), e.UID)
}

func (e *VolumeOwnerError) Unwrap() error { return ErrVolumeOwner }

// OwnerName is "uid N", or "another uid".
func (e *VolumeOwnerError) OwnerName() string {
	if e.Owner == "" {
		return "another uid"
	}
	return "uid " + e.Owner
}

// OwnerArgs is the owner helper's argv, exported so it can be asserted on.
func (m Manager) OwnerArgs(name string) ([]string, error) {
	if !config.ValidVolumeName(name) {
		return nil, fmt.Errorf("container: %q is not a volume name", name)
	}
	if m.LabelPrefix == "" {
		return nil, errors.New("container: no label prefix")
	}
	// Root would own a credential no workspace's user could read, and the
	// Feature refuses a root remote user.
	if m.ClaudeUID <= 0 || m.ClaudeGID < 0 {
		return nil, fmt.Errorf("container: uid %d gid %d cannot own the shared credential volume: Drydock must not run as root", m.ClaudeUID, m.ClaudeGID)
	}
	if !ValidCleanupImage(m.CleanupImage) {
		return nil, fmt.Errorf("%w: %q", ErrCleanupImage, m.CleanupImage)
	}
	label, err := ephemeral.Label(m.LabelPrefix, ephemeral.VolumeOwner, name)
	if err != nil {
		return nil, err
	}
	return []string{"run", "--rm",
		// Its own label, never the workspace's, so reconciliation never
		// lists one; removed by it however the run ends, and by boot's
		// sweep (internal/ephemeral).
		"--label", label,
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE",
		"--security-opt", "no-new-privileges",
		"--user", "0:0",
		"--mount", "type=volume,source=" + name + ",target=" + ownerMount,
		"--env", "DRYDOCK_UID=" + strconv.Itoa(m.ClaudeUID),
		"--env", "DRYDOCK_GID=" + strconv.Itoa(m.ClaudeGID),
		"--entrypoint", "sh",
		m.CleanupImage, "-c", ownerScript,
	}, nil
}

// LabelVolumeOwner is the label the owner helper carries, valued with the
// volume's name: ephemeral.VolumeOwner's. Not LabelWorkspace (reconciliation
// lists by that) and not LabelCleanup (that one's value is a workspace id).
// Two creates can run the helper for one volume at once; the label is
// removed by the last of them to end (ephemeral.Helper.End).
const LabelVolumeOwner = string(ephemeral.VolumeOwner)

// ensureOwner runs the owner helper over the volume.
func (m Manager) ensureOwner(ctx context.Context, name string) error {
	args, err := m.OwnerArgs(name)
	if err != nil {
		return err
	}
	var out, stderr bytes.Buffer
	res, err := m.helper(ephemeral.VolumeOwner, name).Run(ctx, subproc.Cmd{Name: "docker", Args: args,
		Stdout: limit(&out, 64), Stderr: limit(&stderr, 64<<10)})
	if err != nil {
		return fmt.Errorf("docker run (volume owner): %w", err)
	}
	if res.Err != nil {
		return fmt.Errorf("docker run (volume owner): %w", res.Err)
	}
	switch res.ExitCode {
	case 0:
		return nil
	case exitForeignOwner:
		owner := strings.TrimSpace(out.String())
		if _, err := strconv.Atoi(owner); err != nil {
			owner = ""
		}
		return &VolumeOwnerError{Owner: owner, UID: m.ClaudeUID}
	}
	return failed("docker run (volume owner)", res, &stderr)
}
