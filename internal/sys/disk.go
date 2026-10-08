package sys

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// HostDisk is the production DiskUsage: statfs for a filesystem, and a walk
// of lstat results for a directory.
type HostDisk struct{}

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

// Size walks path without following symlinks or leaving its filesystem,
// summing allocated blocks (st_blocks × 512, what du reports) and counting a
// hard-linked inode once. An entry it cannot read sets partial and the walk
// goes on: an exact total is impossible then, and a lower bound is still
// worth having.
func (HostDisk) Size(path string) (uint64, bool, error) {
	root, err := os.Lstat(path)
	if err != nil {
		return 0, false, err
	}
	rst, ok := root.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, errors.New("sys: no stat_t for " + path)
	}
	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	var total uint64
	partial := false
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == path {
				return err
			}
			partial = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			partial = true
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			partial = true
			return nil
		}
		if uint64(st.Dev) != uint64(rst.Dev) {
			if d.IsDir() {
				return fs.SkipDir // another filesystem mounted inside: not this tree's
			}
			return nil
		}
		if st.Nlink > 1 && !d.IsDir() {
			k := inode{uint64(st.Dev), uint64(st.Ino)}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		total += uint64(st.Blocks) * 512
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return total, partial, nil
}
