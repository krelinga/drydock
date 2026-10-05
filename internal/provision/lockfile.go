package provision

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/workspace"
)

// A repository's committed devcontainer-lock.json is honoured by running
// `up` with no lockfile flag, as VS Code does, and Drydock does not repair
// what `up` then writes (design §6, "The repository's lockfile"). Measured
// on CLI 0.89.0 (container.Lockfile has the matrix), `up` leaves a lockfile
// that is in sync with the repository's configuration byte for byte — the
// Feature Drydock injects is not written in, and has no dependencies that
// would be — and rewrites a stale one to exactly the bytes VS Code would
// write for the same repository.
//
// That rewrite is left in the clone on purpose. It is the repository's own
// lockfile brought up to date with the repository's own configuration, with
// nothing of Drydock's in it; it is what the next person to open the
// repository in VS Code would get anyway, and what an agent's pull request
// ought to carry. What would be wrong is for it to surprise someone at commit
// time, so the step says so: lockfileChange names the file when `up` changed
// it.
//
// The comparison is of the file's bytes before and after `up`, not `git
// status`: the clone is the agent's working tree, so it may already hold
// changes that are not `up`'s, and running git on the host against a
// .git/config the container can write would run whatever core.fsmonitor or a
// filter driver names, as Drydock.

// lockfileSnapshot is a lockfile's bytes before `up`.
type lockfileSnapshot struct {
	path    string
	present bool
	b       []byte
	err     error
}

func snapshotLockfile(path string) lockfileSnapshot {
	s := lockfileSnapshot{path: path}
	if path == "" {
		return s
	}
	s.b, s.err = container.ReadLockfile(path)
	if errors.Is(s.err, fs.ErrNotExist) {
		s.err = nil
		return s
	}
	s.present = s.err == nil
	return s
}

// lockfileChange compares the lockfile with the snapshot taken before `up`
// and returns a Note naming it when `up` changed it, or nil. clone is the
// clone's host path, which the sentence names the file relative to.
func lockfileChange(before lockfileSnapshot, clone string) error {
	if before.path == "" || before.err != nil {
		return nil
	}
	after := snapshotLockfile(before.path)
	if after.err == nil && after.present == before.present && bytes.Equal(after.b, before.b) {
		return nil
	}
	rel, err := filepath.Rel(clone, before.path)
	if err != nil {
		rel = filepath.Base(before.path)
	}
	switch {
	case !before.present && after.present:
		return workspace.Note(fmt.Sprintf("devcontainer up created %q in the clone, so it now shows as a new file there.", rel))
	case after.err != nil || !after.present:
		return workspace.Note(fmt.Sprintf("devcontainer up left %q in the clone as something other than the lockfile it found.", rel))
	}
	return workspace.Note(fmt.Sprintf("devcontainer up rewrote %q in the clone because it was out of date with the repository's devcontainer.json; the clone now holds the lockfile VS Code would write. Commit it, or every build resolves it again.", rel))
}
