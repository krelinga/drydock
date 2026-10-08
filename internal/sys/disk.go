package sys

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// HostDisk is the production DiskUsage: statfs for a filesystem, and a bounded
// walk for a directory.
//
// The directory it walks is a workspace's, which a process in the container
// writes: its shape is whatever that process chose. So the walk is bounded in
// every dimension the tree controls — time (the caller's context), entries,
// depth, the hard-link set, and the entries held in memory at once (one batch
// per level) — and it never follows a name: each directory is opened relative
// to its parent's descriptor with O_NOFOLLOW, so a directory swapped for a
// symlink mid-walk is refused rather than followed. Any bound reached ends the
// walk with what it counted and partial set: a lower bound, which the card
// shows as one.
type HostDisk struct {
	// MaxEntries, MaxDepth and MaxLinks override the defaults when positive.
	MaxEntries, MaxDepth, MaxLinks int

	// beforeOpen, in a test, runs before each subdirectory is opened: the
	// seam that swaps a directory for a symlink between the parent's listing
	// and the descent.
	beforeOpen func(parent, name string)
}

const (
	// DefaultMaxEntries bounds the names one walk stats. Two million is far
	// beyond a real clone with its node_modules and worktrees, and is a few
	// seconds of stat calls.
	DefaultMaxEntries = 2_000_000
	// DefaultMaxDepth bounds the descent, and so the descriptors held open.
	DefaultMaxDepth = 128
	// DefaultMaxLinks bounds the set of hard-linked inodes remembered to
	// count each once. Past it, a hard-linked file not yet seen is not
	// counted at all (and the walk is partial), so the figure stays a lower
	// bound rather than counting a file twice.
	DefaultMaxLinks = 100_000
	// readBatch is how many names are read from a directory at a time.
	readBatch = 1024
)

// Usage statfs's the nearest existing ancestor of path, so a workspace root
// that has not been made yet — a first run — still reports the filesystem
// it will be made on.
func (HostDisk) Usage(path string) (used, total uint64, err error) {
	p := filepath.Clean(path)
	for {
		var st syscall.Statfs_t
		err = syscall.Statfs(p, &st)
		if err == nil {
			bs := uint64(st.Bsize)
			total = st.Blocks * bs
			// Bfree, not Bavail: "used" is what is on the disk, and the
			// root-reserved blocks are free space nobody has used.
			return total - st.Bfree*bs, total, nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return 0, 0, err
		}
		p = parent
	}
}

type inode struct{ dev, ino uint64 }

type walk struct {
	ctx     context.Context
	d       HostDisk
	dev     uint64
	entries int
	seen    map[inode]bool
	total   uint64
	partial bool
	stop    bool
}

// Size walks path within ctx and the walk's bounds, summing allocated blocks
// (st_blocks × 512, what du reports) on path's own filesystem, each
// hard-linked inode once. An entry it cannot read, a directory it will not
// enter, or a bound reached sets partial; the walk goes on where it can. A
// missing path is fs.ErrNotExist; a path that is a symlink is refused.
func (d HostDisk) Size(ctx context.Context, path string) (uint64, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return 0, false, fs.ErrNotExist
		}
		return 0, false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return 0, false, err
	}
	w := &walk{ctx: ctx, d: d, dev: uint64(st.Dev), seen: map[inode]bool{}}
	w.total = uint64(st.Blocks) * 512
	w.dir(fd, path, 1)
	return w.total, w.partial, nil
}

func (w *walk) limit(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// dir counts everything under the open directory fd, which it closes.
func (w *walk) dir(fd int, name string, depth int) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	for !w.stop {
		if w.ctx.Err() != nil {
			w.partial, w.stop = true, true
			return
		}
		batch, err := f.ReadDir(readBatch)
		for _, e := range batch {
			if w.stop {
				return
			}
			w.entry(fd, name, e.Name(), depth)
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			w.partial = true
			return
		}
	}
}

func (w *walk) entry(dirfd int, parent, name string, depth int) {
	w.entries++
	if w.entries > w.limit(w.d.MaxEntries, DefaultMaxEntries) {
		w.partial, w.stop = true, true
		return
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		w.partial = true
		return
	}
	if uint64(st.Dev) != w.dev {
		return // another filesystem mounted inside: not this tree's
	}
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if st.Nlink > 1 && !isDir {
		k := inode{uint64(st.Dev), uint64(st.Ino)}
		if w.seen[k] {
			return
		}
		if len(w.seen) >= w.limit(w.d.MaxLinks, DefaultMaxLinks) {
			w.partial = true // cannot tell whether it was counted: count it nowhere
			return
		}
		w.seen[k] = true
	}
	w.total += uint64(st.Blocks) * 512
	if !isDir {
		return
	}
	if depth >= w.limit(w.d.MaxDepth, DefaultMaxDepth) {
		w.partial = true
		return
	}
	if w.d.beforeOpen != nil {
		w.d.beforeOpen(parent, name)
	}
	// Relative to the parent's descriptor and never through a symlink: a
	// name the container swapped since the listing is refused here.
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		w.partial = true
		return
	}
	var got unix.Stat_t
	if err := unix.Fstat(fd, &got); err != nil || got.Dev != st.Dev || got.Ino != st.Ino {
		// Not the directory the stat saw: replaced in between.
		unix.Close(fd)
		w.partial = true
		return
	}
	w.dir(fd, parent+"/"+name, depth+1)
}
