package workspace

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// cloneFolderPattern is GitHub's charset for a repository name.
var cloneFolderPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// OwnDir is the directory of Drydock's own beside every clone, in
// <root>/<id>/: the guard, the per-up TMPDIR and the minimal configuration.
// No clone folder may take its name.
const OwnDir = ".drydock"

// CloneFolder is the name of a workspace's clone folder for the repository
// fullName ("owner/name"): the name without the owner, in its own case, so
// `devcontainer up` names the images it builds vsc-<name>-… and the
// container's workspace folder is /workspaces/<name> (design §6). It fails
// closed: a name outside GitHub's [A-Za-z0-9._-], ".", "..", or Drydock's own
// directory, OwnDir in any case, is refused, so no name reaches a path
// outside <root>/<id>/ or shares one with Drydock's files.
func CloneFolder(fullName string) (string, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" {
		return "", fmt.Errorf("workspace: %q is not an owner/name repository name", fullName)
	}
	if err := checkCloneFolder(name); err != nil {
		return "", err
	}
	return name, nil
}

func checkCloneFolder(name string) error {
	if !cloneFolderPattern.MatchString(name) || name == "." || name == ".." || strings.EqualFold(name, OwnDir) {
		return fmt.Errorf("workspace: %q cannot name a clone folder", name)
	}
	return nil
}

// ClonePath is where a workspace's clone lives: <root>/<id>/<CloneFolder>.
// Create records it on the row, from the repository's name as recorded then,
// so a rename on GitHub afterwards never moves the folder: everything after
// the create reads the row's host_path, never the name again.
func ClonePath(root, id, fullName string) (string, error) {
	name, err := CloneFolder(fullName)
	if err != nil {
		return "", err
	}
	if !oneElement(id) {
		return "", fmt.Errorf("workspace: %q is not a workspace id", id)
	}
	return filepath.Join(root, id, name), nil
}

// CheckClonePath proves hostPath is a clone path ClonePath could have made
// for workspace id under root: <root>/<id>/ and one valid folder name, with
// nothing else. A row's host_path, or a container's local_folder label, is
// held to it before anything is made, removed or adopted by it.
func CheckClonePath(root, id, hostPath string) error {
	if !oneElement(id) {
		return fmt.Errorf("workspace: %q is not a workspace id", id)
	}
	dir := filepath.Join(root, id)
	if filepath.Dir(hostPath) != dir || filepath.Clean(hostPath) != hostPath {
		return fmt.Errorf("workspace: the clone %q is not directly under %q", hostPath, dir)
	}
	return checkCloneFolder(filepath.Base(hostPath))
}

// oneElement is a name that is exactly one path element: not empty, not "."
// or "..", and with no separator.
func oneElement(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsRune(s, filepath.Separator)
}
