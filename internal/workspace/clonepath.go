package workspace

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// repoNamePattern is GitHub's charset for a repository name.
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// cloneFolderPattern is every folder name CloneFolder makes: GitHub's
	// charset, starting with a letter or a digit.
	cloneFolderPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// OwnDir is the directory of Drydock's own beside every clone, in
// <root>/<id>/: the guard, the per-up TMPDIR and the minimal configuration.
// No clone folder can take its name: a clone folder starts with a letter or
// a digit.
const OwnDir = ".drydock"

// fallbackFolder names the clone of a repository whose name has no letter or
// digit to start a folder with ("...", "-_").
const fallbackFolder = "repo"

// CloneFolder is the name of a workspace's clone folder for the repository
// fullName ("owner/name"): the name without the owner, in its own case, so
// `devcontainer up` names the images it builds vsc-<name>-… and the
// container's workspace folder is /workspaces/<name> (design §6 step 2).
//
// Leading characters that are not a letter or a digit are dropped
// (".github" → "github", "-x" → "x"), and a name with nothing left is
// fallbackFolder ("..." → "repo"). That is for Docker Compose: with no name:
// of its own, the CLI names a Compose configuration's project
// "<folder>_devcontainer", lowercased with everything outside [-_a-z0-9]
// dropped (or outside [a-z0-9], for a Compose before 1.21; its cg), and
// Compose refuses a project name that does not start with a letter or digit
// — measured: a folder "-x" fails up with "invalid project name", ".hub"
// becomes hub_devcontainer. So every folder this makes gives a valid project
// name (TestEveryCloneFolderIsAComposeProject). It also means no folder is
// ".", "..", or OwnDir.
//
// It fails closed on a name outside GitHub's [A-Za-z0-9._-] or with no owner,
// so no name reaches a path outside <root>/<id>/.
func CloneFolder(fullName string) (string, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || !repoNamePattern.MatchString(name) {
		return "", fmt.Errorf("workspace: %q is not an owner/name repository name in GitHub's characters", fullName)
	}
	name = strings.TrimLeft(name, "._-")
	if name == "" {
		name = fallbackFolder
	}
	return name, checkCloneFolder(name)
}

func checkCloneFolder(name string) error {
	if !cloneFolderPattern.MatchString(name) {
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
// for workspace id under root: <root>/<id>/ and one folder name of the shape
// CloneFolder makes, with nothing else. A row's host_path, or a container's
// local_folder label, is held to it before anything is made, removed or
// adopted by it.
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

// OrphanClonePath is the clone path an orphan — a container found with no
// row — is adopted with: its devcontainer.local_folder label, which the guard
// set to the row's host_path, so a repository renamed since keeps its
// folder; or, for a container with no such label (a Compose
// configuration's), ClonePath of the repository its label names. A label
// that is not a clone path under root is an error, never a reason to derive
// one instead. Reconciliation's Plan decides by it and Adopt is given its
// answer, so the two cannot disagree.
//
// The derived path assumes no rename: the repository label is written at
// each up from the name then, so a Compose workspace renamed and rebuilt
// before the row was lost is adopted at a path that does not exist. That is
// benign — a start clones afresh there, and a delete removes all of
// <root>/<id> — and needs a lost database besides.
func OrphanClonePath(root, id, localFolder, fullName string) (string, error) {
	if localFolder != "" {
		if err := CheckClonePath(root, id, localFolder); err != nil {
			return "", err
		}
		return localFolder, nil
	}
	return ClonePath(root, id, fullName)
}

// oneElement is a name that is exactly one path element: not empty, not "."
// or "..", and with no separator.
func oneElement(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsRune(s, filepath.Separator)
}
