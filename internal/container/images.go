package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/krelinga/drydock/internal/subproc"
)

// builtImageBase is the one shape of workspace folder basename Drydock gives
// the CLI ("repo"), and the only one whose image names are computed here: the
// CLI also lowercases and strips other characters, which this does not copy.
var builtImageBase = regexp.MustCompile(`^[a-z0-9]+$`)

// BuiltImages are the names `devcontainer up` gives the images it builds for
// a workspace folder, measured on CLI 0.89.0: `vsc-<basename>-<sha256 of the
// folder's path>`, which a Dockerfile build is tagged, and that name with
// `-features` (the image with Features installed, Drydock's own among them),
// `-uid` (the remote user's uid updated) and `-features-uid`. The hash is of
// --workspace-folder, not of the process's working directory, so each
// workspace's names are its own: the folder is <root>/<id>/repo, and the id
// is a ULID no other workspace has. Nothing labels these images, so the name
// is the only handle, and it is exact — never a pattern, never a prune.
func BuiltImages(folder string) ([]string, error) {
	if !filepath.IsAbs(folder) || filepath.Clean(folder) != folder {
		return nil, fmt.Errorf("container: %q is not a clean absolute workspace folder", folder)
	}
	base := filepath.Base(folder)
	if !builtImageBase.MatchString(base) {
		return nil, fmt.Errorf("container: cannot compute the CLI's image names for a folder named %q", base)
	}
	sum := sha256.Sum256([]byte(folder))
	name := "vsc-" + base + "-" + hex.EncodeToString(sum[:])
	return []string{name, name + "-features", name + "-uid", name + "-features-uid"}, nil
}

// RemoveBuiltImages removes those of a workspace folder's BuiltImages that
// exist, after its containers are gone (design §6, delete): a delete that
// left them kept every image `up` ever built, one set per workspace, on the
// daemon for good. It never forces: Docker refuses to remove an image a
// container still uses, so an image some other container was made from stays
// (that is an error here, naming docker's refusal), and an image that is also
// tagged with another name is only untagged. It lists first, by exact
// reference, so an image never built — most workspaces have two of the four
// — is not an error. It returns the names it removed.
func (m Manager) RemoveBuiltImages(ctx context.Context, folder string) ([]string, error) {
	names, err := BuiltImages(folder)
	if err != nil {
		return nil, err
	}
	args := []string{"image", "ls", "--format", "{{.Repository}}:{{.Tag}}"}
	for _, n := range names {
		args = append(args, "--filter", "reference="+n)
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: args,
		Stdout: limit(&out, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker image ls", res, &stderr); err != nil {
		return nil, err
	}
	var present []string
	for _, line := range strings.Fields(out.String()) {
		repo, tag, _ := strings.Cut(line, ":")
		// Only the exact names, at the tag the CLI gives them. Anything
		// else the filter let through is not this workspace's to remove.
		if tag == "latest" && slices.Contains(names, repo) && !slices.Contains(present, line) {
			present = append(present, line)
		}
	}
	if len(present) == 0 {
		return nil, nil
	}
	stderr.Reset()
	res = m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"image", "rm", "--"}, present...),
		Stdout: limit(&bytes.Buffer{}, 64<<10), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker image rm", res, &stderr); err != nil {
		return nil, err
	}
	return present, nil
}
