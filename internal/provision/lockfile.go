package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/workspace"
)

// A repository's committed devcontainer-lock.json is honoured by running
// `up` with no lockfile flag, and that `up` rewrites it — measured on CLI
// 0.89.0, it writes in the dependency of Drydock's own Feature, so every
// committed lockfile comes back modified (container.Lockfile has the matrix).
// Drydock never leaves the clone modified, so the committed bytes are saved
// outside the clone before `up` and put back after it, whatever its outcome.
//
// The save is what makes this crash-safe. It is a file beside the clone,
// /srv/drydock/ws/<id>/.drydock/lockfile.json, written and synced before `up`
// starts and removed only once the restore is synced. So if Drydock dies
// between `up` rewriting the lockfile and the restore — a window as long as
// the image build — the saved bytes survive it, and RecoverLockfiles puts
// them back at the next boot, before anything else runs. A run that finds a
// save left over (the boot pass failed, say) restores it before doing
// anything else, too.
//
// What the restore cannot know is whether someone else changed the file
// during `up`. Nobody should: `up` runs during a create, a start or a
// rebuild, when the workspace has no session for an agent to work in.

// savedLockfile is the save file's contents.
type savedLockfile struct {
	// Path is the lockfile's absolute path, inside the workspace's clone.
	Path    string      `json:"path"`
	Mode    fs.FileMode `json:"mode"`
	Content []byte      `json:"content"`
}

func savePath(wsDir string) string { return filepath.Join(wsDir, ".drydock", "lockfile.json") }

// saveLockfile records the committed lockfile at path before `up`. It reports
// false, saving nothing, when there is no lockfile there any more — the
// caller then passes --no-lockfile, so nothing is written.
func saveLockfile(wsDir, path string) (bool, error) {
	b, err := container.ReadLockfile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	data, err := json.Marshal(savedLockfile{Path: path, Mode: fi.Mode().Perm(), Content: b})
	if err != nil {
		return false, err
	}
	dst := savePath(wsDir)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, err
	}
	if err := writeSynced(dst+".tmp", data, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		return false, err
	}
	return true, syncDir(filepath.Dir(dst))
}

// restoreLockfile puts a saved lockfile back and then removes the save. With
// no save it does nothing. The file is rewritten only if its bytes differ, and
// in place: a crash part-way leaves the save, so the next restore finishes the
// job — where a temporary file renamed over it would leave an untracked file
// in the clone instead.
func restoreLockfile(wsDir, clone string) error {
	src := savePath(wsDir)
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s savedLockfile
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("provision: %s does not parse: %w", src, err)
	}
	if !strings.HasPrefix(s.Path, clone+"/") || filepath.Clean(s.Path) != s.Path {
		return fmt.Errorf("provision: %s names %q, which is not inside the clone %s", src, s.Path, clone)
	}
	if fi, err := os.Lstat(s.Path); err == nil && !fi.Mode().IsRegular() {
		// `up` writes a regular file; anything else here is not ours to keep.
		if err := os.Remove(s.Path); err != nil {
			return err
		}
	}
	cur, err := os.ReadFile(s.Path)
	if err != nil || !bytes.Equal(cur, s.Content) {
		if err := writeSynced(s.Path, s.Content, s.Mode); err != nil {
			return err
		}
	}
	if err := os.Chmod(s.Path, s.Mode); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	return syncDir(filepath.Dir(src))
}

// RecoverLockfiles restores every lockfile a run saved and did not get to put
// back — Drydock died during `up`. Serve calls it at boot, before serving,
// so no run is in flight. Each workspace is tried; the errors are joined.
func (p *Provisioner) RecoverLockfiles() error {
	saves, err := filepath.Glob(filepath.Join(p.Workspaces.Root, "*", ".drydock", "lockfile.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range saves {
		wsDir := filepath.Dir(filepath.Dir(s))
		if err := restoreLockfile(wsDir, filepath.Join(wsDir, "repo")); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", filepath.Base(wsDir), err))
		}
	}
	return errors.Join(errs...)
}

// recoverLockfile is RecoverLockfiles for one workspace, at the start of a
// run: a save left behind is restored before the clone is read again.
func (r *runState) recoverLockfile(w workspace.Workspace) error {
	if err := restoreLockfile(r.dir(w), w.HostPath); err != nil {
		return workspace.Public("Drydock could not restore the repository's devcontainer lockfile from an earlier run.", err)
	}
	return nil
}

func writeSynced(path string, b []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
