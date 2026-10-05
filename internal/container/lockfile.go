package container

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Lockfile is what `up` is told to do with the repository's
// devcontainer-lock.json. VS Code writes that file to pin each Feature the
// repository declares to an exact version and digest, and repositories commit
// it, so a Drydock workspace must install what it pins — or the same
// repository builds one way in VS Code and another here.
//
// Measured on CLI 0.89.0 (test/fixtures/devcontainer/lockfile-behaviour.txt):
//
//   - no flag: `up` reads the lockfile and installs the versions it pins, and
//     writes the lockfile into the clone whenever what it resolved differs
//     from it — a new untracked file when the repository has none, a rewrite
//     when the committed one is stale, blank, or merely lacks a dependency of
//     a Feature Drydock injects. Drydock's own Feature dependsOn github-cli,
//     and that dependency is written in, so in practice `up` rewrites every
//     committed lockfile;
//   - --no-lockfile: neither reads nor writes. A committed lockfile is
//     ignored and every Feature floats to its tag's newest version;
//   - --frozen-lockfile: reads, never writes, and refuses to build unless the
//     lockfile is exactly what `up` would write — which, for the dependency
//     above, no lockfile VS Code wrote ever is. So it cannot be used.
//
// Honouring a committed lockfile therefore means letting `up` write, and the
// caller putting the committed bytes back afterwards (provision does, and
// recovers them after a crash). A repository without one gets --no-lockfile,
// and nothing is ever written.
type Lockfile uint8

const (
	// LockfileIgnore is --no-lockfile: for a repository with no lockfile.
	// It is the zero value, so a spec that forgets to decide never writes.
	LockfileIgnore Lockfile = iota
	// LockfileHonour passes no lockfile flag: `up` reads the repository's
	// lockfile and installs what it pins, and may rewrite it. The caller
	// must have saved the committed bytes outside the clone first, and must
	// restore them after.
	LockfileHonour
)

// flag is the argument for a mode, "" for none.
func (l Lockfile) flag() (string, error) {
	switch l {
	case LockfileIgnore:
		return "--no-lockfile", nil
	case LockfileHonour:
		return "", nil
	}
	return "", fmt.Errorf("container: unknown lockfile mode %d", l)
}

// LockfilePath is where the CLI reads and writes the lockfile of a
// configuration file: beside it, named .devcontainer-lock.json when the
// configuration's own name starts with a dot (a root .devcontainer.json) and
// devcontainer-lock.json otherwise. That is CLI 0.89.0's rule, read from its
// source and measured for both spellings.
//
// configFile must be the path read-configuration reports. With
// --override-config that is still the repository's default path, and so is
// the lockfile — measured: a lockfile beside the override is never read.
func LockfilePath(configFile string) string {
	name := "devcontainer-lock.json"
	if strings.HasPrefix(filepath.Base(configFile), ".") {
		name = ".devcontainer-lock.json"
	}
	return filepath.Join(filepath.Dir(configFile), name)
}

// ErrLockfileUnreadable is a lockfile Drydock will not hand to `up`: not a
// regular file, too large, or not a JSON object with a "features" object.
// The CLI would fail on most of these too; failing here names the file.
var ErrLockfileUnreadable = errors.New("the repository's devcontainer lockfile is not a readable lockfile")

// ErrLockfilePinsInjected is a lockfile with an entry for a Feature Drydock
// injects. Drydock's own Feature is pinned by Drydock's configuration — the
// --feature reference — and never by a repository. Measured on CLI 0.89.0:
// `up` READS a lockfile entry for a Feature that only --additional-features
// names, and installs the version it pins, though it never writes one. So
// such an entry would let a repository choose which Drydock Feature its
// container gets; it is refused instead.
var ErrLockfilePinsInjected = errors.New("the repository's devcontainer lockfile pins a Feature Drydock injects")

// MaxLockfile bounds what Drydock reads and saves. A lockfile is a few
// hundred bytes per Feature.
const MaxLockfile = 1 << 20

// LockfileMode decides the lockfile mode for a repository whose configuration
// read-configuration reported at configFile. injected are the references
// passed as --additional-features.
//
// No lockfile, or one holding only whitespace, is LockfileIgnore. The CLI
// treats a blank lockfile as a request to fill one in — a write into the
// clone for nothing — and with nothing pinned, ignoring it installs exactly
// what filling it in would have. Anything else is LockfileHonour, after
// checking it is a lockfile at all and pins none of Drydock's own Features.
//
// A symbolic link is refused rather than followed: the clone is the
// repository's to arrange, and Drydock reads, saves and restores this file
// with its own uid.
func LockfileMode(configFile string, injected []string) (Lockfile, error) {
	path := LockfilePath(configFile)
	b, err := ReadLockfile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return LockfileIgnore, nil
	}
	if err != nil {
		return LockfileIgnore, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return LockfileIgnore, nil
	}
	var lock struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(b, &lock); err != nil || lock.Features == nil {
		return LockfileIgnore, fmt.Errorf("%w: %s does not parse as one", ErrLockfileUnreadable, path)
	}
	for _, ref := range injected {
		if _, ok := lock.Features[ref]; ok {
			return LockfileIgnore, fmt.Errorf("%w: %s has an entry for %s", ErrLockfilePinsInjected, path, ref)
		}
	}
	return LockfileHonour, nil
}

// ReadLockfile reads a lockfile that must be a regular file of at most
// MaxLockfile bytes. A missing one is an error wrapping fs.ErrNotExist.
func ReadLockfile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrLockfileUnreadable, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxLockfile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxLockfile {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrLockfileUnreadable, path, MaxLockfile)
	}
	return b, nil
}
