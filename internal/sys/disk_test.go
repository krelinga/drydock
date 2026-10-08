package sys

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHostDiskSize: allocated blocks, a hard link counted once, a symlink
// not followed — and a directory the walk cannot enter marks the figure
// partial, where the same tree with it readable is exact.
func TestHostDiskSize(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{ws, outside, filepath.Join(ws, "repo", "private")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, n int) {
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(ws, "repo", "a"), 1<<20)
	write(filepath.Join(ws, "repo", "private", "b"), 1<<20)
	write(filepath.Join(outside, "big"), 8<<20)
	if err := os.Link(filepath.Join(ws, "repo", "a"), filepath.Join(ws, "repo", "a-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "repo", "to-outside")); err != nil {
		t.Fatal(err)
	}

	d := HostDisk{}
	full, partial, err := d.Size(ws)
	if err != nil || partial {
		t.Fatalf("Size = %d, %v, %v", full, partial, err)
	}
	// Two 1 MiB files, the hard link not again, the 8 MiB behind the
	// symlink not at all; plus directory blocks, a few KiB at most.
	if full < 2<<20 || full >= 3<<20 {
		t.Errorf("Size = %d, want two MiB and change", full)
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory; the partial case needs an ordinary user")
	}
	private := filepath.Join(ws, "repo", "private")
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(private, 0o755) })
	some, partial, err := d.Size(ws)
	if err != nil || !partial {
		t.Fatalf("with an unreadable directory: %d, partial %v, %v", some, partial, err)
	}
	if some >= full || some < 1<<20 {
		t.Errorf("lower bound %d, against %d readable", some, full)
	}

	if _, _, err := d.Size(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Errorf("a missing directory: %v", err)
	}
}

// TestHostDiskUsageOfAPathNotYetMade: the workspace root may not exist on a
// first run; its filesystem is still the one it will be made on.
func TestHostDiskUsageOfAPathNotYetMade(t *testing.T) {
	root := t.TempDir()
	u1, t1, err := HostDisk{}.Usage(root)
	if err != nil || t1 == 0 || u1 > t1 {
		t.Fatalf("control: %d of %d, %v", u1, t1, err)
	}
	_, t2, err := HostDisk{}.Usage(filepath.Join(root, "not", "yet"))
	if err != nil || t2 != t1 {
		t.Errorf("a path not yet made: total %d (want %d), %v", t2, t1, err)
	}
}
