package sys

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkfile(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHostDiskSize: allocated blocks, a hard link counted once, a symlink
// not followed — and a directory the walk cannot enter marks the figure
// partial, where the same tree with it readable is exact.
func TestHostDiskSize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	outside := filepath.Join(root, "outside")
	mkfile(t, filepath.Join(ws, "repo", "a"), 1<<20)
	mkfile(t, filepath.Join(ws, "repo", "private", "b"), 1<<20)
	mkfile(t, filepath.Join(outside, "big"), 8<<20)
	if err := os.Link(filepath.Join(ws, "repo", "a"), filepath.Join(ws, "repo", "a-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "repo", "to-outside")); err != nil {
		t.Fatal(err)
	}

	d := HostDisk{}
	full, partial, err := d.Size(ctx, ws)
	if err != nil || partial {
		t.Fatalf("Size = %d, %v, %v", full, partial, err)
	}
	// Two 1 MiB files, the hard link not again, the 8 MiB behind the
	// symlink not at all; plus directory blocks, a few KiB at most.
	if full < 2<<20 || full >= 3<<20 {
		t.Errorf("Size = %d, want two MiB and change", full)
	}
	if _, _, err := d.Size(ctx, filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Errorf("a missing directory: %v", err)
	}
	if _, _, err := d.Size(ctx, filepath.Join(ws, "repo", "to-outside")); err == nil {
		t.Error("a root that is a symlink was walked")
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory; the partial case needs an ordinary user")
	}
	private := filepath.Join(ws, "repo", "private")
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(private, 0o755) })
	some, partial, err := d.Size(ctx, ws)
	if err != nil || !partial {
		t.Fatalf("with an unreadable directory: %d, partial %v, %v", some, partial, err)
	}
	if some >= full || some < 1<<20 {
		t.Errorf("lower bound %d, against %d readable", some, full)
	}
}

// TestHostDiskSizeIsBounded: a tree past the entry budget, the depth budget
// or the hard-link budget, or a walk whose context ends, reports what it
// counted as partial — promptly — where the same tree under generous bounds
// is exact.
func TestHostDiskSizeIsBounded(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3000; i++ {
		mkfile(t, filepath.Join(root, "flat", fmt.Sprintf("f%04d", i)), 0)
	}
	deep := filepath.Join(root, "deep")
	for i := 0; i < 20; i++ {
		deep = filepath.Join(deep, "d")
	}
	mkfile(t, filepath.Join(deep, "bottom"), 4096)
	for i := 0; i < 10; i++ {
		p := filepath.Join(root, "links", fmt.Sprintf("l%d", i))
		mkfile(t, p, 4096)
		if err := os.Link(p, p+"-again"); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	exact, partial, err := HostDisk{}.Size(ctx, root)
	if err != nil || partial || exact == 0 {
		t.Fatalf("control: %d %v %v", exact, partial, err)
	}
	for name, d := range map[string]HostDisk{
		"entries": {MaxEntries: 500},
		"depth":   {MaxDepth: 5},
		"links":   {MaxLinks: 3},
	} {
		start := time.Now()
		got, partial, err := d.Size(ctx, root)
		if err != nil || !partial || got > exact {
			t.Errorf("%s: %d (of %d), partial %v, %v", name, got, exact, partial, err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("%s: took %s", name, time.Since(start))
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, partial, err := (HostDisk{}).Size(cancelled, root); err != nil || !partial || got >= exact {
		t.Errorf("a cancelled walk: %d, partial %v, %v", got, partial, err)
	}
}

// TestHostDiskSizeNeverFollowsASwap: a directory the container swaps for a
// symlink between the parent's listing and the descent is refused, not
// followed — the 8 MiB outside the tree is never counted — and the walk says
// it is partial. The control is the same walk with no swap.
func TestHostDiskSizeNeverFollowsASwap(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	mkfile(t, filepath.Join(ws, "sub", "small"), 4096)
	mkfile(t, filepath.Join(root, "outside", "big"), 8<<20)
	ctx := context.Background()
	control, partial, err := HostDisk{}.Size(ctx, ws)
	if err != nil || partial {
		t.Fatalf("control: %d %v %v", control, partial, err)
	}
	swapped := false
	d := HostDisk{beforeOpen: func(parent, name string) {
		if name != "sub" || swapped {
			return
		}
		swapped = true
		if err := os.RemoveAll(filepath.Join(ws, "sub")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(ws, "sub")); err != nil {
			t.Fatal(err)
		}
	}}
	got, partial, err := d.Size(ctx, ws)
	if !swapped {
		t.Fatal("the seam never ran")
	}
	if err != nil || !partial || got >= 8<<20 {
		t.Errorf("after the swap: %d, partial %v, %v — the walk followed it", got, partial, err)
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
