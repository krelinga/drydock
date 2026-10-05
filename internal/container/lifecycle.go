package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/krelinga/drydock/internal/subproc"
)

// containerID is a full container id as `docker ps --no-trunc` prints it.
// Every id handed to stop or rm is checked against it, so nothing that came
// back from docker can turn into an option or a second argument.
var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Find returns the id of every container, running or not, carrying this
// prefix's workspace label with this workspace's id — found by label on the
// daemon's side, never from a remembered id (§6: Docker is the truth). A
// workspace can have more than one: a failed `up` leaves its container
// behind, and a rebuild that failed half-way can leave two.
func (m Manager) Find(ctx context.Context, workspaceID string) ([]string, error) {
	if !workspaceIDPattern.MatchString(workspaceID) {
		return nil, fmt.Errorf("container: %q is not a workspace id", workspaceID)
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args: []string{"ps", "--all", "--quiet", "--no-trunc",
			"--filter", "label=" + m.key(LabelWorkspace) + "=" + workspaceID},
		Stdout: limit(&out, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker ps", res, &stderr); err != nil {
		return nil, err
	}
	ids := strings.Fields(out.String())
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("docker ps: %q is not a container id", id)
		}
	}
	return ids, nil
}

// Stop stops containers with `docker stop`: SIGTERM, then SIGKILL after
// docker's grace period. A container already stopped is not an error. The
// container, its filesystem and the clone's bind mount all survive.
func (m Manager) Stop(ctx context.Context, ids []string) error {
	return m.each(ctx, "stop", []string{"stop"}, ids)
}

// Remove removes containers with `docker rm --force --volumes`: running or
// not, and with their anonymous volumes, which nothing else will ever name
// again. Named volumes are never removed — the shared Claude credential
// volume (§7.1, EnsureClaudeVolume) is one, and it is every workspace's.
func (m Manager) Remove(ctx context.Context, ids []string) error {
	return m.each(ctx, "rm", []string{"rm", "--force", "--volumes"}, ids)
}

func (m Manager) each(ctx context.Context, what string, args, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return fmt.Errorf("container: %q is not a container id", id)
		}
	}
	var stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append(append(args, "--"), ids...),
		Stdout: limit(&bytes.Buffer{}, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker "+what, res, &stderr); err != nil {
		return err
	}
	return nil
}

// ErrStillThere is a container that survived a remove: listed by label again
// after `docker rm` said it had gone.
var ErrStillThere = errors.New("container: a container carrying the workspace's label is still there")
